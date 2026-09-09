import { useEffect, useRef, useState } from "react";
import { useTranslation } from "react-i18next";
import { ClipboardCheck, ClipboardCopy } from "lucide-react";
import type { components } from "../../api/schema";
import { aoBridge } from "../lib/bridge";
import { buildRunDiagnostics, formatRunDiagnostics } from "../lib/run-diagnostics";

type WorkflowRunDetailView = components["schemas"]["WorkflowRunDetailView"];

/**
 * The exportable diagnostic bundle, as one button.
 *
 * Its whole reason for existing is the recovery workflow it replaces: read the
 * run id off the page, open a terminal, query SQLite for the attempt, the
 * reason code and the evidence. Every one of those facts was already on this
 * screen and none of it could be copied out in one piece.
 *
 * The redaction rules live in run-diagnostics.ts, which is an allowlist, so
 * what this button copies cannot quietly grow when the API does.
 */
export function WorkflowDiagnosticsButton({ detail }: { detail: WorkflowRunDetailView }) {
	const { t } = useTranslation();
	const [copied, setCopied] = useState(false);
	const [failed, setFailed] = useState(false);
	const timeout = useRef<ReturnType<typeof setTimeout> | null>(null);

	useEffect(
		() => () => {
			if (timeout.current !== null) clearTimeout(timeout.current);
		},
		[],
	);

	const copy = async () => {
		const text = formatRunDiagnostics(buildRunDiagnostics(detail));
		try {
			await aoBridge.clipboard.writeText(text);
			setCopied(true);
			setFailed(false);
		} catch {
			// A clipboard the platform refused is worth saying out loud: the
			// silent version of this leaves somebody pasting stale content.
			setCopied(false);
			setFailed(true);
		}
		if (timeout.current !== null) clearTimeout(timeout.current);
		timeout.current = setTimeout(() => {
			setCopied(false);
			setFailed(false);
			timeout.current = null;
		}, 2_000);
	};

	return (
		<div className="flex flex-col gap-1">
			<button
				className="flex items-center gap-1.5 rounded border border-border px-3 py-1.5 text-sm"
				onClick={() => void copy()}
				type="button"
			>
				{copied ? (
					<ClipboardCheck aria-hidden="true" className="size-icon-sm shrink-0" />
				) : (
					<ClipboardCopy aria-hidden="true" className="size-icon-sm shrink-0" />
				)}
				{copied ? t("wf.diagnostics.copied") : t("wf.diagnostics.copy")}
			</button>
			{failed ? <p className="text-xs text-destructive">{t("wf.diagnostics.copyFailed")}</p> : null}
		</div>
	);
}
