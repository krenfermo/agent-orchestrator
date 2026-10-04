import type { components } from "../../api/schema";
import { useTranslation } from "react-i18next";

type VerifyResult = components["schemas"]["WorkflowVerifyResult"];

export function WorkflowVerifyDetails({ result }: { result: VerifyResult }) {
	const { t } = useTranslation();
	return (
		<div className="mt-2 border-t border-border pt-2 text-xs text-muted-foreground" data-testid="workflow-verify-details">
			<div className="grid grid-cols-[auto_1fr] gap-x-2 gap-y-1">
				<span>{t("shell.workflowsVerifyResult")}</span><span>{result.passed ? t("shell.workflowsVerifyPass") : t("shell.workflowsVerifyFail")}</span>
				<span>{t("shell.workflowsVerifyFingerprint")}</span><span className="break-all font-mono">{result.postFingerprint || result.preFingerprint}</span>
				{result.scope && <><span>{t("shell.workflowsVerifyScope")}</span><span>{result.scope.scope}{result.scope.packageDir ? ` (${result.scope.packageDir})` : ""}</span></>}
				{result.errorClass && <><span>{t("shell.workflowsVerifyFailure")}</span><span>{result.errorClass}</span></>}
			</div>
			<ul className="mt-2 space-y-1">
				{result.checks.map((check, index) => (
					<li key={`${check.kind}-${check.label}-${index}`}>
						{check.passed ? t("shell.workflowsVerifyPass") : t("shell.workflowsVerifyFail")} · {check.label}
						{check.exitCode !== undefined && ` · ${t("shell.workflowsVerifyExit", { code: check.exitCode })}`}
						{check.durationMs !== undefined && ` · ${t("shell.workflowsVerifyDuration", { duration: check.durationMs })}`}
						{check.failureReason && ` · ${check.failureReason}`}
					</li>
				))}
			</ul>
		</div>
	);
}
