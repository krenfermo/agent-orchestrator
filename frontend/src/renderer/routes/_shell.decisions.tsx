import { createFileRoute, Link } from "@tanstack/react-router";
import { useTranslation } from "react-i18next";
import { usePendingDecisions } from "../hooks/usePendingDecisions";
import { QuestionCard } from "../components/workflow-questions-section";

export const Route = createFileRoute("/_shell/decisions")({
	component: PendingDecisionsRoute,
});

/**
 * PendingDecisionsRoute is Checkpoint 8K-B pass 3's global "Pending
 * Decisions" inbox: every open/in-flight question across ALL workflow runs
 * (human_required, resolving, waiting_for_capacity), one stacked mobile-
 * first QuestionCard per question — the exact same card the per-run
 * questions section (workflow-questions-section.tsx) uses, reused verbatim
 * rather than duplicated. No dashboard, no charts, no filters beyond the
 * three states the backend already exposes, no wide tables: single column,
 * ≥44px tap targets, mirroring _shell.workflows.tsx's simple list-page
 * structure (`mx-auto max-w-2xl`).
 */
export function PendingDecisionsRoute() {
	const { t } = useTranslation();
	const { questions, isLoading, error, answerQuestion, answeringQuestion } = usePendingDecisions();

	// Most recent first, same convention as the per-run section.
	const sorted = [...questions].sort((a, b) => Date.parse(b.createdAt) - Date.parse(a.createdAt));

	return (
		<div className="mx-auto flex max-w-2xl flex-col gap-6 p-6">
			<div className="flex flex-col gap-1">
				<h1 className="text-lg font-semibold">{t("shell.decisions.title")}</h1>
				<p className="text-sm text-muted-foreground">{t("shell.decisions.subtitle")}</p>
			</div>

			{isLoading && <p className="text-sm text-muted-foreground">{t("shell.decisions.loading")}</p>}
			{error && <p className="text-sm text-destructive">{error}</p>}

			<div className="flex flex-col gap-3">
				{sorted.map((q) => (
					<div className="flex flex-col gap-1" key={q.id}>
						<QuestionCard
							question={q}
							answering={answeringQuestion}
							onAnswer={(args) => answerQuestion({ workflowRunId: q.workflowRunId, ...args })}
						/>
						<Link
							className="self-start text-xs text-muted-foreground underline underline-offset-2 hover:text-foreground"
							params={{ workflowId: q.workflowRunId }}
							to="/workflows/$workflowId"
						>
							{t("shell.decisions.viewRun")}
						</Link>
					</div>
				))}
				{!isLoading && sorted.length === 0 && (
					<p className="text-sm text-muted-foreground">{t("shell.decisions.empty")}</p>
				)}
			</div>
		</div>
	);
}
