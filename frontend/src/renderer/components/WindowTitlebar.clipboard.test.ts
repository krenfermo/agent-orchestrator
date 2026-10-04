import { beforeEach, describe, expect, it, vi } from "vitest";
import {
	noteTerminalClipboardFocus,
	registerTerminalClipboardTarget,
	resetTerminalClipboardTargetsForTest,
} from "../lib/terminal-clipboard";
import { runTerminalClipboardAction } from "./WindowTitlebar";

// The Edit menu's clipboard items must reach the focused terminal instead of the
// main process's DOM edit commands, which cannot see an xterm selection.
describe("title-bar Edit menu clipboard routing", () => {
	beforeEach(() => {
		resetTerminalClipboardTargetsForTest();
		document.body.innerHTML = "";
	});

	function registerFocusedTerminal(copy = vi.fn(() => true)) {
		const target = { copy, paste: vi.fn(), selectAll: vi.fn(), ownsFocus: () => true };
		registerTerminalClipboardTarget(target);
		noteTerminalClipboardFocus(target);
		return target;
	}

	it("leaves non-clipboard actions to the main process", () => {
		registerFocusedTerminal();

		for (const action of ["edit.undo", "edit.redo", "edit.cut", "view.reload"]) {
			expect(runTerminalClipboardAction(action)).toBe(false);
		}
	});

	it("routes copy, paste, and select all to the focused terminal", () => {
		const target = registerFocusedTerminal();

		expect(runTerminalClipboardAction("edit.copy")).toBe(true);
		expect(runTerminalClipboardAction("edit.paste")).toBe(true);
		expect(runTerminalClipboardAction("edit.selectAll")).toBe(true);
		expect(target.copy).toHaveBeenCalledTimes(1);
		expect(target.paste).toHaveBeenCalledTimes(1);
		expect(target.selectAll).toHaveBeenCalledTimes(1);
	});

	// Nothing selected in the terminal: the DOM copy is still the right handler
	// for whatever else on the page may hold a selection.
	it("falls through to the main process when the terminal has no selection", () => {
		const target = registerFocusedTerminal(vi.fn(() => false));

		expect(runTerminalClipboardAction("edit.copy")).toBe(false);
		expect(target.copy).toHaveBeenCalledTimes(1);
	});

	it("falls through entirely when no terminal is mounted", () => {
		expect(runTerminalClipboardAction("edit.copy")).toBe(false);
		expect(runTerminalClipboardAction("edit.paste")).toBe(false);
		expect(runTerminalClipboardAction("edit.selectAll")).toBe(false);
	});
});
