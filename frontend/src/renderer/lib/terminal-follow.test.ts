import { describe, expect, it, vi } from "vitest";
import {
	createTailFollowTracker,
	FOLLOW_TAIL_SLACK_LINES,
	isAtTail,
	linesFromTail,
	NEW_LINE_COUNT_CAP,
	type TailPosition,
} from "./terminal-follow";

/**
 * A buffer the way xterm keeps one, with no xterm: `baseY` is the top of the
 * live tail's screen and `viewportY` the top of what is rendered. The
 * user-scrolling lock is modelled rather than mocked — it is the whole reason
 * "near the bottom" is not the same as "following", so a fake that skipped it
 * would let a regression through while looking green.
 */
function fakeBuffer(baseY = 100) {
	const position: TailPosition = { baseY, viewportY: baseY };
	// Mirrors BufferService.scrollLines: an upward scroll takes the lock, and
	// only a downward scroll that actually reaches baseY releases it.
	let userScrolling = false;
	return {
		position,
		get userScrolling() {
			return userScrolling;
		},
		scrollLines(amount: number) {
			if (amount < 0) {
				if (position.viewportY === 0) return;
				userScrolling = true;
			} else if (amount + position.viewportY >= position.baseY) {
				userScrolling = false;
			}
			position.viewportY = Math.max(0, Math.min(position.viewportY + amount, position.baseY));
		},
		scrollToBottom() {
			this.scrollLines(position.baseY - position.viewportY);
		},
		/**
		 * Append one line of output the way a live PTY would. Mirrors
		 * BufferService.scroll: the viewport follows the buffer only while the
		 * user-scrolling lock is clear.
		 */
		feedLine() {
			position.baseY += 1;
			if (!userScrolling) position.viewportY = position.baseY;
		},
		/** Append a line once the bounded scrollback is full: the top is trimmed. */
		feedTrimmedLine() {
			if (!userScrolling) position.viewportY = position.baseY;
		},
	};
}

function trackerOn(buffer: ReturnType<typeof fakeBuffer>) {
	const onChange = vi.fn();
	const tracker = createTailFollowTracker({ readPosition: () => buffer.position, onChange });
	return { onChange, tracker };
}

describe("tail position helpers", () => {
	it("measures the distance from the live tail without going negative", () => {
		expect(linesFromTail({ baseY: 100, viewportY: 60 })).toBe(40);
		expect(linesFromTail({ baseY: 100, viewportY: 100 })).toBe(0);
		// A viewport past baseY is not a thing xterm produces, but the arithmetic
		// that feeds scrollLines() must not hand back a negative distance.
		expect(linesFromTail({ baseY: 100, viewportY: 120 })).toBe(0);
	});

	it("counts a small gap as still riding the tail, and a larger one as not", () => {
		expect(isAtTail({ baseY: 100, viewportY: 100 })).toBe(true);
		expect(isAtTail({ baseY: 100, viewportY: 100 - FOLLOW_TAIL_SLACK_LINES })).toBe(true);
		expect(isAtTail({ baseY: 100, viewportY: 100 - FOLLOW_TAIL_SLACK_LINES - 1 })).toBe(false);
	});

	it("reads an alt-buffer pane, which cannot scroll at all, as following", () => {
		expect(isAtTail({ baseY: 0, viewportY: 0 })).toBe(true);
	});
});

describe("tail follow tracker", () => {
	it("starts on the tail with nothing missed", () => {
		const { tracker } = trackerOn(fakeBuffer());

		expect(tracker.state).toEqual({ following: true, newLines: 0 });
	});

	it("stops following once the reader scrolls up, and publishes the change once", () => {
		const buffer = fakeBuffer();
		const { onChange, tracker } = trackerOn(buffer);

		buffer.scrollLines(-8);
		tracker.syncFromViewport();

		expect(tracker.state.following).toBe(false);
		expect(onChange).toHaveBeenCalledOnce();

		// A further scroll inside the same "not following" run is not a state
		// change, so the control is not re-rendered for it.
		buffer.scrollLines(-4);
		tracker.syncFromViewport();

		expect(onChange).toHaveBeenCalledOnce();
	});

	it("re-arms auto-follow on its own when the reader scrolls back near the bottom", () => {
		const buffer = fakeBuffer();
		const { tracker } = trackerOn(buffer);
		buffer.scrollLines(-8);
		tracker.syncFromViewport();

		// One line short of the bottom: inside the slack window, so this reads as
		// following without the reader having to press anything.
		buffer.scrollLines(7);
		tracker.syncFromViewport();

		expect(tracker.state.following).toBe(true);
	});

	it("counts output that lands while scrolled up, and does not move the viewport for it", () => {
		const buffer = fakeBuffer();
		const { tracker } = trackerOn(buffer);
		buffer.scrollLines(-8);
		tracker.syncFromViewport();
		buffer.scrollToBottom();
		// Back on the tail, so the lock is clear and the counter is reset.
		tracker.syncFromViewport();
		buffer.scrollLines(-8);
		tracker.syncFromViewport();
		const parked = buffer.position.viewportY;

		for (let index = 0; index < 3; index += 1) {
			buffer.feedLine();
			tracker.noteLineFeed();
		}

		// The whole point: new output is announced, not scrolled to.
		expect(buffer.position.viewportY).toBe(parked);
		expect(tracker.state).toEqual({ following: false, newLines: 3 });
	});

	it("does not count output that lands while the reader is on the tail", () => {
		const buffer = fakeBuffer();
		const { tracker } = trackerOn(buffer);

		buffer.feedLine();
		tracker.noteLineFeed();

		expect(tracker.state).toEqual({ following: true, newLines: 0 });
	});

	it("keeps counting once the bounded scrollback is full and the tail distance stops growing", () => {
		const buffer = fakeBuffer();
		const { tracker } = trackerOn(buffer);
		buffer.scrollLines(-8);
		tracker.syncFromViewport();
		const distance = linesFromTail(buffer.position);

		// Trimmed appends: xterm drops a line off the top for every line added, so
		// baseY - viewportY does not move. Line feeds still have to be counted.
		for (let index = 0; index < 4; index += 1) {
			buffer.feedTrimmedLine();
			tracker.noteLineFeed();
		}

		expect(linesFromTail(buffer.position)).toBe(distance);
		expect(tracker.state.newLines).toBe(4);
	});

	it("caps the counter instead of relabelling the control on every line of a git log", () => {
		const buffer = fakeBuffer();
		const { onChange, tracker } = trackerOn(buffer);
		buffer.scrollLines(-8);
		tracker.syncFromViewport();
		onChange.mockClear();

		for (let index = 0; index < NEW_LINE_COUNT_CAP + 50; index += 1) {
			buffer.feedLine();
			tracker.noteLineFeed();
		}

		expect(tracker.state.newLines).toBe(NEW_LINE_COUNT_CAP);
		// Nothing is published once the label has stopped changing.
		expect(onChange).toHaveBeenCalledTimes(NEW_LINE_COUNT_CAP);
	});

	it("clears the missed-output count when the reader returns to the tail", () => {
		const buffer = fakeBuffer();
		const { tracker } = trackerOn(buffer);
		buffer.scrollLines(-8);
		tracker.syncFromViewport();
		buffer.feedLine();
		tracker.noteLineFeed();
		expect(tracker.state.newLines).toBe(1);

		buffer.scrollToBottom();
		tracker.syncFromViewport();

		expect(tracker.state).toEqual({ following: true, newLines: 0 });
	});

	it("asks for a settling scroll when the reader parks inside the slack window", () => {
		const buffer = fakeBuffer();
		const { tracker } = trackerOn(buffer);

		buffer.scrollLines(-8);
		tracker.syncFromViewport();
		expect(tracker.needsTailSettle()).toBe(false);

		// One line short of the bottom: reads as following, so it must also BE
		// following as far as the terminal's own scroll lock is concerned.
		buffer.scrollLines(7);
		expect(buffer.userScrolling).toBe(true);
		expect(tracker.needsTailSettle()).toBe(true);

		buffer.scrollToBottom();

		expect(buffer.userScrolling).toBe(false);
		expect(tracker.needsTailSettle()).toBe(false);
	});

	it("puts a following reader back on the tail after a reflow", () => {
		const buffer = fakeBuffer();
		const { tracker } = trackerOn(buffer);

		const anchor = tracker.captureAnchor();
		// Stand in for xterm's post-reflow viewport, which lands wherever the
		// resize leaves it rather than where the reader was.
		buffer.position.viewportY = 0;

		expect(anchor).toEqual({ following: true, linesFromTail: 0 });
		expect(tracker.anchorRestore(anchor)).toEqual({ toBottom: true });
	});

	it("puts a reviewing reader back the same distance above the tail after a reflow", () => {
		const buffer = fakeBuffer();
		const { tracker } = trackerOn(buffer);
		buffer.scrollLines(-40);
		tracker.syncFromViewport();

		const anchor = tracker.captureAnchor();
		expect(anchor).toEqual({ following: false, linesFromTail: 40 });

		// The reflow both moves the viewport and changes how many rows the buffer
		// holds; the restore is relative to the tail, not to an absolute row.
		buffer.position.baseY = 120;
		buffer.position.viewportY = 120;
		const restore = tracker.anchorRestore(anchor);

		expect(restore).toEqual({ toBottom: false, delta: -40 });
		if (restore.toBottom) throw new Error("unreachable");
		buffer.scrollLines(restore.delta);
		expect(linesFromTail(buffer.position)).toBe(40);
	});

	it("asks for no scroll when a reflow left the reviewing reader exactly where they were", () => {
		const buffer = fakeBuffer();
		const { tracker } = trackerOn(buffer);
		buffer.scrollLines(-40);
		tracker.syncFromViewport();

		// A fit that does not change the grid is a no-op inside FitAddon, and the
		// restore has to be a no-op too — that is what keeps an active selection
		// alive across layout changes that leave the cell count untouched.
		expect(tracker.anchorRestore(tracker.captureAnchor())).toEqual({ toBottom: false, delta: 0 });
	});

	it("clamps the restore to the top when the reflow left less history than the anchor", () => {
		const buffer = fakeBuffer();
		const { tracker } = trackerOn(buffer);
		buffer.scrollLines(-40);
		tracker.syncFromViewport();
		const anchor = tracker.captureAnchor();

		buffer.position.baseY = 10;
		buffer.position.viewportY = 10;
		const restore = tracker.anchorRestore(anchor);

		if (restore.toBottom) throw new Error("unreachable");
		buffer.scrollLines(restore.delta);
		expect(buffer.position.viewportY).toBe(0);
	});
});
