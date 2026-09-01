import { describe, expect, it, vi } from "vitest";
import { createWindowComposition } from "./window-composition";

function setup() {
	let bounds = { x: 0, y: 0, width: 900, height: 640 };
	let boundsChanged: (() => void) | undefined;
	let windowDestroyed = false;
	let webContentsDestroyed = false;
	const addChildView = vi.fn();
	const removeChildView = vi.fn();
	const removeListener = vi.fn();
	const close = vi.fn(() => {
		webContentsDestroyed = true;
	});
	const view = {
		webContents: { close, isDestroyed: () => webContentsDestroyed },
		setBackgroundColor: vi.fn(),
		setBounds: vi.fn(),
		setVisible: vi.fn(),
	};

	// Electron's real behaviour, which the previous fake did not model and
	// which is why this bug shipped: once a BaseWindow is destroyed, READING
	// `.contentView` throws -- before any method on it is called. A fake with a
	// permanently live contentView can never reproduce the failure.
	const liveContentView = {
		addChildView,
		getBounds: () => bounds,
		on: vi.fn((event: string, listener: () => void) => {
			if (event === "bounds-changed") boundsChanged = listener;
		}),
		removeChildView,
		removeListener,
	};
	const mainWindow = {
		get contentView() {
			if (windowDestroyed) throw new TypeError("Object has been destroyed");
			return liveContentView;
		},
		isDestroyed: () => windowDestroyed,
	};

	function FakeWebContentsView() {
		return view;
	}
	const composition = createWindowComposition({
		mainWindow: mainWindow as never,
		WebContentsView: FakeWebContentsView as never,
		preload: "/preload.js",
	});
	return {
		addChildView,
		bounds: () => bounds,
		close,
		composition,
		/** Destroy the window exactly as Electron does before "closed" fires. */
		destroyWindow: () => {
			windowDestroyed = true;
			webContentsDestroyed = true;
		},
		/** Destroy only the window, leaving this module's WebContents alive. */
		destroyWindowOnly: () => {
			windowDestroyed = true;
		},
		emitBoundsChanged: () => boundsChanged?.(),
		mainWindow,
		removeChildView,
		removeListener,
		setBounds: (next: typeof bounds) => {
			bounds = next;
		},
		view,
	};
}

describe("createWindowComposition", () => {
	it("creates a transparent shell at window bounds and reorders it for overlays", () => {
		const { addChildView, composition, view } = setup();

		expect(view.setBackgroundColor).toHaveBeenCalledWith("#00000000");
		expect(addChildView).toHaveBeenNthCalledWith(1, view, 0);
		expect(view.setBounds).toHaveBeenCalledWith({ x: 0, y: 0, width: 900, height: 640 });

		composition.setOverlayOpen(true);
		expect(addChildView).toHaveBeenLastCalledWith(view);
		composition.setOverlayOpen(false);
		expect(addChildView).toHaveBeenLastCalledWith(view, 0);
	});

	it("resizes and disposes the explicit shell without recreating it", () => {
		const { bounds, close, composition, emitBoundsChanged, removeChildView, removeListener, setBounds, view } = setup();

		(view.setBounds as ReturnType<typeof vi.fn>).mockClear();
		setBounds({ x: 0, y: 0, width: 1920, height: 1080 });
		emitBoundsChanged();
		expect(view.setBounds).toHaveBeenCalledWith({ x: 0, y: 0, width: 1920, height: 1080 });
		expect(bounds()).toEqual({ x: 0, y: 0, width: 1920, height: 1080 });

		composition.dispose();
		expect(removeListener).toHaveBeenCalledWith("bounds-changed", composition.resize);
		expect(removeChildView).toHaveBeenCalledWith(view);
		expect(close).toHaveBeenCalledOnce();
	});

	// --- teardown safety (the "Object has been destroyed" incident) ---------
	//
	// AO's Electron app logged, twice during real sessions:
	//
	//   UnhandledPromiseRejectionWarning: TypeError: Object has been destroyed
	//       at Object.dispose (main.js:41163)
	//       at (main.js:46817)
	//       at process.processTicksAndRejections
	//
	// `Object.dispose` is this composition's dispose; the second frame is the
	// `.finally()` in main.ts's `mainWindow.on("closed")` handler. The window is
	// already destroyed when "closed" fires, and dispose ran a further tick
	// later because it waits on the asynchronous browser-runtime teardown --
	// so its first statement, a bare read of `mainWindow.contentView`, threw.

	it("disposes without throwing after the window is destroyed", () => {
		const { composition, destroyWindow, removeChildView, removeListener } = setup();

		destroyWindow();

		// The regression, exactly: before the fix this threw
		// "Object has been destroyed" on its very first statement.
		expect(() => composition.dispose()).not.toThrow();

		// And it did not merely swallow the error: it skipped the window half
		// deliberately, because Electron already tore that hierarchy down.
		expect(removeListener).not.toHaveBeenCalled();
		expect(removeChildView).not.toHaveBeenCalled();
	});

	it("reproduces the incident: a late dispose in a promise chain rejects nothing", async () => {
		const { composition, destroyWindow } = setup();
		const rejections: unknown[] = [];
		const onRejection = (reason: unknown) => rejections.push(reason);
		process.on("unhandledRejection", onRejection);

		try {
			// The production sequence: the window is destroyed, then the async
			// browser-runtime teardown settles, and only then does dispose run --
			// inside a .finally() on a chain nobody awaits.
			destroyWindow();
			const browserRuntimeTeardown = new Promise<void>((resolve) => setTimeout(resolve, 0));
			void browserRuntimeTeardown.finally(() => composition.dispose());

			// Two macrotask turns: enough for the chain to settle and for Node to
			// have reported an unhandled rejection if one had been created.
			await new Promise((resolve) => setTimeout(resolve, 0));
			await new Promise((resolve) => setTimeout(resolve, 0));

			expect(rejections).toEqual([]);
		} finally {
			process.off("unhandledRejection", onRejection);
		}
	});

	it("closes its own WebContents even when the window is already gone", () => {
		const { close, composition, destroyWindowOnly } = setup();

		// The WebContents is this module's own object and outlives the window.
		// Before the fix dispose threw one statement earlier, so this close --
		// the only cleanup that actually mattered -- never happened at all.
		destroyWindowOnly();
		composition.dispose();

		expect(close).toHaveBeenCalledOnce();
	});

	it("is idempotent: a second dispose closes nothing twice", () => {
		const { close, composition, removeChildView } = setup();

		composition.dispose();
		composition.dispose();
		composition.dispose();

		expect(close).toHaveBeenCalledOnce();
		expect(removeChildView).toHaveBeenCalledOnce();
	});

	it("survives a bounds-changed callback that fires after destruction", () => {
		const { composition, destroyWindow, emitBoundsChanged, view } = setup();

		destroyWindow();
		(view.setBounds as ReturnType<typeof vi.fn>).mockClear();

		// A resize already queued when the window went away.
		expect(() => emitBoundsChanged()).not.toThrow();
		expect(view.setBounds).not.toHaveBeenCalled();

		// And after dispose, for the same reason.
		composition.dispose();
		expect(() => emitBoundsChanged()).not.toThrow();
	});

	it("survives a late overlay toggle after destruction", () => {
		const { addChildView, composition, destroyWindow } = setup();

		destroyWindow();
		(addChildView as ReturnType<typeof vi.fn>).mockClear();

		// An IPC overlay toggle already in flight when the window closed.
		expect(() => composition.setOverlayOpen(true)).not.toThrow();
		expect(() => composition.setOverlayOpen(false)).not.toThrow();
		expect(addChildView).not.toHaveBeenCalled();
	});

	it("still tears the shell down normally while the window is alive", () => {
		const { close, composition, removeChildView, removeListener } = setup();

		// The ordinary path is unchanged: nothing about the destroyed-window
		// guards may weaken cleanup when there IS something to clean up.
		composition.dispose();

		expect(removeListener).toHaveBeenCalledWith("bounds-changed", composition.resize);
		expect(removeChildView).toHaveBeenCalledOnce();
		expect(close).toHaveBeenCalledOnce();
	});
});
