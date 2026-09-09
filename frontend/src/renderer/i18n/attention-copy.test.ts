import { describe, expect, it } from "vitest";
import { createAppI18n } from "./instance";
import {
	attentionActionLabelKeys,
	attentionActionMessageKey,
	attentionNextActionLabelKeys,
	attentionNextActionMessageKey,
} from "./key-maps";
import { APP_LOCALES } from "./locales";
import { catalogFor, type MessageKey } from "./messages";

// The MEDUSA stop this file exists for: the run went to needs_attention with
// reason `worker_turn_produced_nothing` / error class `ambiguous_worker_state`,
// and a Spanish install read the daemon's English sentence verbatim.
const incidentReason = "worker_turn_produced_nothing";
const incidentErrorClass = "ambiguous_worker_state";

// The exact strings the daemon composes, which the UI used to print directly.
const daemonHumanAction =
	"The worker reported its turn finished and left no change in its workspace. Decide whether that is correct for this task, then continue this run or cancel it.";
const daemonNextAction =
	"worker reported its turn finished, but AO can see no change in its workspace — nothing verifiable was produced";

function translate(locale: string, key: MessageKey): string {
	const i18n = createAppI18n(locale as never);
	return i18n.t(key);
}

describe("workflow attention copy", () => {
	it("renders the incident's stop in Spanish rather than the daemon's English", () => {
		const key = attentionActionMessageKey(incidentReason);
		expect(key).toBeDefined();
		const spanish = translate("es", key as MessageKey);
		expect(spanish).toContain("espacio de trabajo");
		expect(spanish).not.toBe(daemonHumanAction);
		// The English catalog still says exactly what the daemon says, so the
		// two surfaces cannot drift apart for an English reader.
		expect(translate("en", key as MessageKey)).toBe(daemonHumanAction);
	});

	it("renders the run's next action in Spanish for the same stop", () => {
		const key = attentionNextActionMessageKey(incidentReason);
		expect(key).toBeDefined();
		const spanish = translate("es", key as MessageKey);
		expect(spanish).toContain("no se produjo nada verificable");
		expect(spanish).not.toBe(daemonNextAction);
	});

	it("covers the ambiguous_worker_state error class in both directions", () => {
		const action = attentionActionMessageKey(incidentErrorClass);
		const next = attentionNextActionMessageKey(incidentErrorClass);
		expect(action).toBeDefined();
		expect(next).toBeDefined();
		expect(translate("es", action as MessageKey)).toContain("no pudo probar");
		expect(translate("es", next as MessageKey)).toContain("no pudo probar");
	});

	// The information-loss guard. A daemon newer than this build can emit a
	// reason this map has never heard of; the UI must then show the sentence the
	// daemon sent rather than a blank or a raw key.
	it("keeps the daemon's own sentence for an unmapped reason", () => {
		expect(attentionActionMessageKey("a_reason_from_a_newer_daemon")).toBeUndefined();
		expect(attentionNextActionMessageKey("a_reason_from_a_newer_daemon")).toBeUndefined();
		expect(attentionActionMessageKey(undefined)).toBeUndefined();
		expect(attentionNextActionMessageKey(undefined)).toBeUndefined();
	});

	// Reason codes are diagnostic identifiers and are never translated: they
	// stay beside the copy so an operator and a log line say the same word.
	it("does not translate the reason code itself", () => {
		for (const locale of APP_LOCALES) {
			const catalog = catalogFor(locale);
			expect(catalog[incidentReason as never]).toBeUndefined();
			expect(catalog[incidentErrorClass as never]).toBeUndefined();
		}
	});

	it("resolves every mapped key to non-empty copy in every locale", () => {
		const keys = [
			...Object.values(attentionActionLabelKeys),
			...Object.values(attentionNextActionLabelKeys),
		];
		for (const locale of APP_LOCALES) {
			for (const key of keys) {
				const value = translate(locale, key);
				expect(value, `${locale}/${key}`).toBeTruthy();
				expect(value, `${locale}/${key}`).not.toBe(key);
			}
		}
	});
});
