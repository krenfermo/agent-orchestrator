import type { ITheme } from "@xterm/xterm";

/** Read a CSS custom property from :root (tokens.css). */
function cssVar(name: string): string {
	if (typeof document === "undefined") return "";
	return getComputedStyle(document.documentElement).getPropertyValue(name).trim();
}

/**
 * Selection colours must never fall back to xterm's built-in default: that
 * default is a translucent wash that disappears on the AO terminal plates, and
 * an empty string here would silently restore it. Fall back to the literal
 * token value instead so a stylesheet that has not been applied yet (or a test
 * renderer without tokens.css) still gets a visible selection.
 */
const SELECTION_FALLBACK = {
	darkBackground: "#3468cc",
	darkForeground: "#f5f8ff",
	darkInactive: "#35486a",
	lightBackground: "#1f5fbe",
	lightForeground: "#ffffff",
	lightInactive: "#4e7099",
} as const;

function cssVarOr(name: string, fallback: string): string {
	return cssVar(name) || fallback;
}

/** xterm palettes harmonized to tokens.css (--color-term-* / semantic colors). */
export function buildTerminalThemes(): { dark: ITheme; light: ITheme } {
	// Opaque plate — xterm cells must not be translucent or the p-2 gutter and
	// the glyph grid pick up different composites against app chrome.
	const namedThemeActive = typeof document !== "undefined" && Boolean(document.documentElement.dataset.styleTheme);
	const terminalBg = namedThemeActive
		? cssVar("--background")
		: cssVar("--color-bg-terminal-opaque") || cssVar("--color-bg-terminal");
	const terminalForeground = namedThemeActive ? cssVar("--foreground") : cssVar("--color-text-terminal");
	const terminalCursor = namedThemeActive ? cssVar("--primary") : cssVar("--color-working");
	const dark: ITheme = {
		background: terminalBg,
		foreground: terminalForeground,
		cursor: terminalCursor,
		cursorAccent: terminalBg,
		selectionBackground: cssVarOr("--color-term-selection-dark", SELECTION_FALLBACK.darkBackground),
		selectionForeground: cssVarOr("--color-term-selection-foreground-dark", SELECTION_FALLBACK.darkForeground),
		selectionInactiveBackground: cssVarOr("--color-term-selection-inactive", SELECTION_FALLBACK.darkInactive),
		black: cssVar("--color-term-black"),
		red: cssVar("--color-term-red"),
		green: cssVar("--color-term-green"),
		yellow: cssVar("--color-term-yellow"),
		blue: cssVar("--color-term-blue"),
		magenta: cssVar("--color-term-magenta"),
		cyan: cssVar("--color-term-cyan"),
		white: cssVar("--color-term-white"),
		brightBlack: cssVar("--color-term-bright-black"),
		brightRed: cssVar("--color-term-bright-red"),
		brightGreen: cssVar("--color-term-bright-green"),
		brightYellow: cssVar("--color-term-bright-yellow"),
		brightBlue: cssVar("--color-term-bright-blue"),
		brightMagenta: cssVar("--color-term-bright-magenta"),
		brightCyan: cssVar("--color-term-bright-cyan"),
		brightWhite: cssVar("--color-term-bright-white"),
	};

	const light: ITheme = {
		background: terminalBg,
		foreground: terminalForeground,
		// xterm block cursor fills with `cursor` and paints cell text in
		// `cursorAccent`. --color-working is fine on dark plates but reads as a
		// low-contrast wash on the light terminal bg (#f5f5f4), especially while
		// blinking. Use the terminal foreground so the block stays visible.
		cursor: terminalForeground,
		cursorAccent: terminalBg,
		selectionBackground: cssVarOr("--color-term-selection-light", SELECTION_FALLBACK.lightBackground),
		selectionForeground: cssVarOr("--color-term-selection-foreground-light", SELECTION_FALLBACK.lightForeground),
		selectionInactiveBackground: cssVarOr("--color-term-selection-inactive-light", SELECTION_FALLBACK.lightInactive),
		black: cssVar("--color-term-black"),
		red: cssVar("--color-term-red"),
		green: cssVar("--color-term-green"),
		yellow: cssVar("--color-term-yellow"),
		blue: cssVar("--color-term-blue"),
		magenta: cssVar("--color-term-magenta"),
		cyan: cssVar("--color-term-cyan"),
		white: cssVar("--color-term-white"),
		brightBlack: cssVar("--color-term-bright-black"),
		brightRed: cssVar("--color-term-bright-red"),
		brightGreen: cssVar("--color-term-bright-green"),
		brightYellow: cssVar("--color-term-bright-yellow"),
		brightBlue: cssVar("--color-term-bright-blue"),
		brightMagenta: cssVar("--color-term-bright-magenta"),
		brightCyan: cssVar("--color-term-bright-cyan"),
		brightWhite: cssVar("--color-term-bright-white"),
	};

	return { dark, light };
}
