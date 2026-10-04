// Registry that lets app chrome reach the focused terminal's clipboard path.
//
// Why this exists: xterm draws its own selection (WebGL/canvas renderer), so the
// selected text is NOT a DOM document selection. `webContents.copy()` — what the
// title-bar Edit menu dispatches to the main process — therefore copies nothing
// while a terminal selection is up, even though Ctrl+C and the terminal's own
// right-click Copy both work. Terminals register their real copy/paste actions
// here so the Edit menu can route through the SAME code path instead of the
// no-op DOM one.

export type TerminalClipboardTarget = {
	// Returns false when there is nothing selected, so the caller can fall back
	// to the DOM copy for whatever else on the page may hold a selection.
	copy: () => boolean;
	paste: () => void;
	selectAll: () => void;
	ownsFocus: () => boolean;
};

const targets = new Set<TerminalClipboardTarget>();
let lastFocused: TerminalClipboardTarget | null = null;

export function registerTerminalClipboardTarget(target: TerminalClipboardTarget): () => void {
	targets.add(target);
	return () => {
		targets.delete(target);
		if (lastFocused === target) lastFocused = null;
	};
}

export function noteTerminalClipboardFocus(target: TerminalClipboardTarget): void {
	if (targets.has(target)) lastFocused = target;
}

function isEditableElement(node: Element | null): boolean {
	if (!node) return false;
	if (node instanceof HTMLTextAreaElement || node instanceof HTMLInputElement) return true;
	return node instanceof HTMLElement && node.isContentEditable;
}

// The terminal an Edit-menu action should act on, or null to let the main
// process handle it as an ordinary DOM edit.
//
// Opening the title-bar Edit menu moves DOM focus onto the menu item, so by the
// time "Copy" is clicked no terminal owns focus any more. Hence the fallback to
// the terminal that held focus last — but only while focus sits on non-editable
// chrome (a menu item). If focus has genuinely moved into a text field, that
// field owns the action and the DOM path is the correct one.
export function activeTerminalClipboardTarget(): TerminalClipboardTarget | null {
	for (const target of targets) {
		if (target.ownsFocus()) return target;
	}
	if (!lastFocused || !targets.has(lastFocused)) return null;
	if (isEditableElement(typeof document === "undefined" ? null : document.activeElement)) return null;
	return lastFocused;
}

// Test-only reset so suites don't leak targets between cases.
export function resetTerminalClipboardTargetsForTest(): void {
	targets.clear();
	lastFocused = null;
}
