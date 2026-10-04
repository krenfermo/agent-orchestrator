// Follow / scroll-lock state for a terminal viewport.
//
// Extracted from XtermTerminal's mount effect so the rules that decide "is the
// reader on the live tail?", "how much output have they missed?" and "where do
// we put them back after a reflow?" can be exercised without a live xterm, a
// WebGL context, or a laid-out DOM. The module knows nothing about xterm: it
// reads a {baseY, viewportY} pair through an injected reader and returns
// decisions for the caller to apply to whatever terminal it owns.
//
// Follow is DERIVED from that pair, never asserted. Every path that moves the
// viewport — wheel, page keys, the activation scroll, the resize anchor — ends
// in a scroll event, so a tracker driven from those events cannot drift out of
// sync with what is on screen.

/**
 * How far off the live tail still counts as following it. Landing exactly on
 * the last row is not something a trackpad flick or a page key reliably does,
 * and re-arming auto-follow only on an exact match leaves the terminal feeling
 * stuck one line short of the bottom.
 */
export const FOLLOW_TAIL_SLACK_LINES = 2;

/**
 * Stop counting new lines here. Past a few hundred the exact number is no
 * longer something the reader acts on, and an uncapped counter would relabel
 * (and resize) the control on every line of a `git log`.
 */
export const NEW_LINE_COUNT_CAP = 999;

/** The two buffer coordinates that decide everything in this module. */
export type TailPosition = {
	/** Row index of the top of the live tail's screen. */
	baseY: number;
	/** Row index of the top of the rendered viewport. */
	viewportY: number;
};

/** What the "jump to latest" control renders from. */
export type TailState = {
	following: boolean;
	newLines: number;
};

/** Where the reader was sitting before a fit reflowed the buffer. */
export type ViewportAnchor = {
	following: boolean;
	linesFromTail: number;
};

/**
 * How to put the reader back after a reflow: either ride the tail again, or
 * scroll by `delta` lines to land the same distance above it as before.
 */
export type AnchorRestore = { toBottom: true } | { toBottom: false; delta: number };

/** Distance between the viewport and the live tail, in lines. */
export function linesFromTail(position: TailPosition): number {
	return Math.max(0, position.baseY - position.viewportY);
}

/**
 * Is the viewport parked at the live tail? Alt-buffer panes have no scrollback,
 * so baseY and viewportY are both 0 there and they always read as following —
 * which is right: nothing about them can scroll away from the tail.
 */
export function isAtTail(position: TailPosition): boolean {
	return position.baseY - position.viewportY <= FOLLOW_TAIL_SLACK_LINES;
}

export type TailFollowTracker = {
	/** Current published state. Read it, do not mutate it. */
	readonly state: TailState;
	/** Re-derive follow from the buffer. Call from the terminal's scroll event. */
	syncFromViewport: () => void;
	/**
	 * Account for one line of output. Call from the terminal's line-feed event.
	 *
	 * Deliberately not derived from the buffer's tail distance: once the bounded
	 * scrollback is full, the terminal trims from the top as it appends, so
	 * `baseY - viewportY` stops growing even though output keeps coming. Line
	 * feeds keep counting through that. (A scroll event cannot carry this either
	 * — output landing under a scrolled-up reader moves no viewport, so it fires
	 * no scroll event.)
	 */
	noteLineFeed: () => void;
	/**
	 * Should the caller snap the viewport to the exact bottom?
	 *
	 * Landing inside the slack window is not, on its own, following — not as far
	 * as xterm is concerned. It keeps a private user-scrolling lock that any
	 * upward scroll sets and only a downward scroll actually REACHING ybase
	 * clears (BufferService.scrollLines), and while that lock is set, incoming
	 * output advances the buffer without advancing the viewport
	 * (BufferService.scroll). A reader who comes to rest one line short of the
	 * bottom would therefore be told they are on the tail while the terminal
	 * quietly leaves them a line further behind with every line that arrives —
	 * and those lines would go uncounted, because we believed we were following
	 * while they landed.
	 *
	 * So the caller closes the gap: whenever the viewport settles inside the
	 * slack window, it scrolls to the exact bottom, which is also what clears the
	 * lock. Asked from the paths that move the viewport rather than from the
	 * scroll listener, so it never re-enters the terminal's scroll dispatch from
	 * inside the terminal's own scroll event.
	 */
	needsTailSettle: () => boolean;
	/** Remember where the reader sits, before a fit reflows the buffer. */
	captureAnchor: () => ViewportAnchor;
	/** Where to move the viewport to honour `anchor` after the reflow. */
	anchorRestore: (anchor: ViewportAnchor) => AnchorRestore;
};

export type TailFollowTrackerOptions = {
	/** Reads the live buffer coordinates. */
	readPosition: () => TailPosition;
	/**
	 * Called whenever `state` has changed. Output arrives a line at a time, so a
	 * burst would otherwise re-render the control once per line: callers are
	 * expected to coalesce (one publish per frame) and read `state` then.
	 */
	onChange: () => void;
};

export function createTailFollowTracker(options: TailFollowTrackerOptions): TailFollowTracker {
	const state: TailState = { following: true, newLines: 0 };

	const syncFromViewport = () => {
		const next = isAtTail(options.readPosition());
		if (next === state.following) return;
		state.following = next;
		// Coming back to the tail means the reader has seen everything; the
		// counter starts over from the next time they leave it.
		if (next) state.newLines = 0;
		options.onChange();
	};

	return {
		state,
		syncFromViewport,
		noteLineFeed: () => {
			syncFromViewport();
			if (state.following || state.newLines >= NEW_LINE_COUNT_CAP) return;
			state.newLines += 1;
			options.onChange();
		},
		needsTailSettle: () => {
			const position = options.readPosition();
			return position.viewportY !== position.baseY && isAtTail(position);
		},
		captureAnchor: () => ({
			following: state.following,
			linesFromTail: linesFromTail(options.readPosition()),
		}),
		anchorRestore: (anchor) => {
			if (anchor.following) return { toBottom: true };
			const position = options.readPosition();
			return {
				toBottom: false,
				delta: Math.max(0, position.baseY - anchor.linesFromTail) - position.viewportY,
			};
		},
	};
}
