import { readFileSync } from "node:fs";
import { resolve } from "node:path";
import { afterEach, describe, expect, it } from "vitest";
import { buildTerminalThemes } from "./terminal-themes";

// Resolved from the repository root rather than from import.meta.url: vitest
// does not give this module a file: URL.
const TOKENS_CSS = readFileSync(resolve(process.cwd(), "src/styles/tokens.css"), "utf8");

/**
 * Custom properties declared inside a top-level selector block, last-wins.
 * tokens.css re-declares a few names within the same block, so the parse has to
 * mirror the cascade rather than take the first hit.
 */
function tokensInBlock(selector: string): Record<string, string> {
	const lines = TOKENS_CSS.split("\n");
	const start = lines.findIndex((line) => line.trim().startsWith(selector));
	if (start === -1) throw new Error(`selector not found in tokens.css: ${selector}`);
	const out: Record<string, string> = {};
	for (let i = start; i < lines.length; i += 1) {
		const line = lines[i].trim();
		if (i > start && line === "}") break;
		const match = /^(--[a-z0-9-]+)\s*:\s*([^;]+);/.exec(line);
		if (match) out[match[1]] = match[2].trim();
	}
	return out;
}

function relativeLuminance(hex: string): number {
	const value = hex.replace("#", "");
	expect(value, `${hex} must be a 6-digit hex so the contrast maths is exact`).toMatch(/^[0-9a-f]{6}$/i);
	const channels = [0, 2, 4].map((offset) => {
		const srgb = Number.parseInt(value.slice(offset, offset + 2), 16) / 255;
		return srgb <= 0.03928 ? srgb / 12.92 : ((srgb + 0.055) / 1.055) ** 2.4;
	});
	return 0.2126 * channels[0] + 0.7152 * channels[1] + 0.0722 * channels[2];
}

function contrast(a: string, b: string): number {
	const [lighter, darker] = [relativeLuminance(a), relativeLuminance(b)].sort((x, y) => y - x);
	return (lighter + 0.05) / (darker + 0.05);
}

function setVars(vars: Record<string, string>): void {
	for (const [name, value] of Object.entries(vars)) {
		document.documentElement.style.setProperty(name, value);
	}
}

afterEach(() => {
	document.documentElement.removeAttribute("style");
	delete document.documentElement.dataset.styleTheme;
});

describe("terminal selection colours", () => {
	it("carries an explicit background/foreground/inactive triple in both variants", () => {
		setVars({
			"--color-term-selection-dark": "#3468cc",
			"--color-term-selection-foreground-dark": "#f5f8ff",
			"--color-term-selection-inactive": "#35486a",
			"--color-term-selection-light": "#1f5fbe",
			"--color-term-selection-foreground-light": "#ffffff",
			"--color-term-selection-inactive-light": "#4e7099",
		});

		const { dark, light } = buildTerminalThemes();

		expect(dark.selectionBackground).toBe("#3468cc");
		expect(dark.selectionForeground).toBe("#f5f8ff");
		expect(dark.selectionInactiveBackground).toBe("#35486a");
		expect(light.selectionBackground).toBe("#1f5fbe");
		expect(light.selectionForeground).toBe("#ffffff");
		expect(light.selectionInactiveBackground).toBe("#4e7099");
		expect(dark.selectionBackground).not.toBe(light.selectionBackground);
	});

	it("gives each variant three distinct selection roles, and the two variants distinct fills", () => {
		// The fallbacks are the values under test here: an accidental copy-paste
		// that made the focused and unfocused fills the same colour, or the dark
		// and light fills the same colour, would make the selection unreadable in
		// one state or one theme while every "is it set?" assertion stayed green.
		const { dark, light } = buildTerminalThemes();

		for (const theme of [dark, light]) {
			expect(theme.selectionBackground).not.toBe(theme.selectionForeground);
			expect(theme.selectionBackground).not.toBe(theme.selectionInactiveBackground);
			expect(theme.selectionForeground).not.toBe(theme.selectionInactiveBackground);
		}
		expect(dark.selectionBackground).not.toBe(light.selectionBackground);
		expect(dark.selectionInactiveBackground).not.toBe(light.selectionInactiveBackground);
	});

	it("never leaves a selection colour empty when the tokens are unavailable", () => {
		// An empty string here would hand xterm its built-in translucent default,
		// which is the wash this fix exists to replace.
		const { dark, light } = buildTerminalThemes();
		for (const theme of [dark, light]) {
			expect(theme.selectionBackground).toMatch(/^#[0-9a-f]{6}$/i);
			expect(theme.selectionForeground).toMatch(/^#[0-9a-f]{6}$/i);
			expect(theme.selectionInactiveBackground).toMatch(/^#[0-9a-f]{6}$/i);
		}
	});
});

describe("tokens.css selection contrast", () => {
	const darkTokens = tokensInBlock(":root,");
	const lightTokens = tokensInBlock(':root[data-theme="light"] {');

	const cases = [
		{
			name: "dark",
			plate: darkTokens["--color-bg-terminal-opaque"],
			background: darkTokens["--color-term-selection-dark"],
			foreground: darkTokens["--color-term-selection-foreground-dark"],
			inactive: darkTokens["--color-term-selection-inactive"],
		},
		{
			name: "light",
			plate: lightTokens["--color-bg-terminal-opaque"],
			background: darkTokens["--color-term-selection-light"],
			foreground: darkTokens["--color-term-selection-foreground-light"],
			inactive: darkTokens["--color-term-selection-inactive-light"],
		},
	];

	for (const variant of cases) {
		it(`${variant.name}: the selection reads clearly against the terminal plate`, () => {
			// 3:1 is the WCAG threshold for a non-text block against its surround,
			// so a drag is unmistakable rather than a faint tint.
			expect(contrast(variant.background, variant.plate)).toBeGreaterThanOrEqual(3);
			// Selected text stays readable on both the focused and unfocused fill;
			// xterm has one foreground slot for both states.
			expect(contrast(variant.foreground, variant.background)).toBeGreaterThanOrEqual(4.5);
			expect(contrast(variant.foreground, variant.inactive)).toBeGreaterThanOrEqual(4.5);
			// The unfocused fill is dimmer but must still be visible.
			expect(contrast(variant.inactive, variant.plate)).toBeGreaterThanOrEqual(1.8);
			expect(contrast(variant.inactive, variant.plate)).toBeLessThan(contrast(variant.background, variant.plate));
		});
	}
});
