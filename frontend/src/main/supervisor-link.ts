import net from "node:net";

// ponytail: no heartbeat. The open socket IS the liveness signal. When the
// Electron process dies the kernel closes the fd and the daemon detects EOF
// immediately (proven against the real daemon with a write-free held
// connection). A heartbeat adds nothing for a Unix domain socket or named
// pipe and is omitted deliberately.

const BACKOFF_INIT_MS = 200;
const BACKOFF_MAX_MS = 2_000;

/**
 * Whether the daemon behind the supervisor socket is still the one this link
 * was made for (P9): "proven" connect; "unproven" (not answering right now)
 * wait and ask again without connecting; "foreign" (another daemon, another
 * incarnation, or gone) end the link for good. The socket path alone proves
 * nothing -- any daemon that binds it later would otherwise be linked and would
 * stop itself when this app quits.
 */
export type SupervisorLinkVerdict = "proven" | "unproven" | "foreign";

export interface SupervisorLinkHandle {
	readonly connected: boolean;
	dispose(): void;
}

/**
 * Hold one client connection to the daemon's supervisor socket for the
 * lifetime of the Electron process. When this process exits for any reason
 * (Cmd+Q, crash, SIGKILL), the OS closes the fd. The daemon detects EOF and
 * self-stops after its ~5s grace period, leaving tmux/ConPTY sessions alive
 * for the next boot to adopt.
 *
 * Retry semantics: if the daemon has not created the socket yet (or restarts),
 * we reconnect with bounded exponential backoff so the link re-establishes
 * automatically. dispose() cancels any pending retry and destroys the socket.
 */
export function connectSupervisor(
	addr: string,
	opts?: { log?: (msg: string) => void; verify?: () => Promise<SupervisorLinkVerdict> },
): SupervisorLinkHandle {
	const log = opts?.log ?? (() => undefined);
	const verify = opts?.verify;

	let disposed = false;
	let connected = false;
	let socket: net.Socket | null = null;
	let retryTimer: ReturnType<typeof setTimeout> | null = null;
	let backoff = BACKOFF_INIT_MS;

	function clearRetry() {
		if (retryTimer !== null) {
			clearTimeout(retryTimer);
			retryTimer = null;
		}
	}

	function destroySocket() {
		if (socket !== null) {
			socket.removeAllListeners();
			socket.destroy();
			socket = null;
		}
	}

	function scheduleReconnect() {
		if (disposed) return;
		clearRetry();
		const delay = backoff;
		backoff = Math.min(backoff * 2, BACKOFF_MAX_MS);
		log(`supervisor-link: reconnecting in ${delay}ms`);
		retryTimer = setTimeout(() => {
			retryTimer = null;
			if (!disposed) connect();
		}, delay);
	}

	function connect() {
		if (disposed) return;
		if (!verify) {
			open();
			return;
		}
		// Every (re)connect is gated: a drop is re-linked only to the same daemon.
		verify()
			.catch((): SupervisorLinkVerdict => "unproven")
			.then((verdict) => {
				if (disposed) return;
				if (verdict === "proven") {
					open();
				} else if (verdict === "foreign") {
					log("supervisor-link: the daemon behind the socket is not the one this link was made for; link ended");
					disposed = true;
					connected = false;
					clearRetry();
					destroySocket();
				} else {
					scheduleReconnect();
				}
			});
	}

	function open() {
		if (disposed) return;

		destroySocket();

		const s = net.connect(addr);
		socket = s;

		s.on("connect", () => {
			if (disposed) {
				s.destroy();
				return;
			}
			connected = true;
			log("supervisor-link: connected");
			// Reset backoff on successful connection.
			backoff = BACKOFF_INIT_MS;
		});

		// Drain inbound data: the daemon never sends payload; discard so the
		// socket buffer never stalls. ponytail: no payload to process.
		s.on("data", () => undefined);

		s.on("error", (err) => {
			log(`supervisor-link: error: ${err.message}`);
			// close fires after error, which schedules the reconnect.
		});

		s.on("close", () => {
			connected = false;
			if (disposed) return;
			log("supervisor-link: connection closed, will retry");
			scheduleReconnect();
		});
	}

	connect();

	return {
		get connected() {
			return connected;
		},
		dispose() {
			disposed = true;
			connected = false;
			clearRetry();
			destroySocket();
		},
	};
}
