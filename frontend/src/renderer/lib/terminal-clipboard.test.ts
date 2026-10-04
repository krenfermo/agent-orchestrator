import { beforeEach, describe, expect, it, vi } from "vitest";
import {
	activeTerminalClipboardTarget,
	noteTerminalClipboardFocus,
	registerTerminalClipboardTarget,
	resetTerminalClipboardTargetsForTest,
	type TerminalClipboardTarget,
} from "./terminal-clipboard";

function makeTarget(overrides: Partial<TerminalClipboardTarget> = {}): TerminalClipboardTarget {
	return {
		copy: vi.fn(() => true),
		paste: vi.fn(),
		selectAll: vi.fn(),
		ownsFocus: () => false,
		...overrides,
	};
}

describe("terminal clipboard registry", () => {
	beforeEach(() => {
		resetTerminalClipboardTargetsForTest();
		document.body.innerHTML = "";
	});

	it("has no active target until a terminal registers", () => {
		expect(activeTerminalClipboardTarget()).toBeNull();
	});

	it("prefers the terminal that currently owns focus", () => {
		const unfocused = makeTarget();
		const focused = makeTarget({ ownsFocus: () => true });
		registerTerminalClipboardTarget(unfocused);
		registerTerminalClipboardTarget(focused);

		expect(activeTerminalClipboardTarget()).toBe(focused);
	});

	// Opening the title-bar Edit menu pulls DOM focus off the terminal, so by the
	// time "Copy" is clicked nothing owns focus. The menu must still reach the
	// terminal the user was working in.
	it("falls back to the last focused terminal while focus sits on non-editable chrome", () => {
		const target = makeTarget();
		registerTerminalClipboardTarget(target);
		noteTerminalClipboardFocus(target);

		const menuItem = document.createElement("div");
		menuItem.tabIndex = 0;
		document.body.append(menuItem);
		menuItem.focus();

		expect(activeTerminalClipboardTarget()).toBe(target);
	});

	it("yields to a real text field so its own selection copies as before", () => {
		const target = makeTarget();
		registerTerminalClipboardTarget(target);
		noteTerminalClipboardFocus(target);

		const input = document.createElement("input");
		document.body.append(input);
		input.focus();

		expect(activeTerminalClipboardTarget()).toBeNull();
	});

	it("forgets a terminal once it unregisters", () => {
		const target = makeTarget();
		const dispose = registerTerminalClipboardTarget(target);
		noteTerminalClipboardFocus(target);
		dispose();

		expect(activeTerminalClipboardTarget()).toBeNull();
	});

	it("ignores focus notes from a terminal that never registered", () => {
		noteTerminalClipboardFocus(makeTarget());

		expect(activeTerminalClipboardTarget()).toBeNull();
	});
});
