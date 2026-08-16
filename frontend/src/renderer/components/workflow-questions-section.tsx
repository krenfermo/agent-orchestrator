import { useState } from "react";
import { useTranslation } from "react-i18next";
import type { TFunction } from "i18next";
import type { components } from "../../api/schema";
import { Badge } from "./ui/badge";
import { Button } from "./ui/button";

// Same rationale as workflow-usage-section.tsx's `translate` helper: the
// classification/certainty values reaching this function are one of a
// small, finite, hardcoded set already declared in en.json, but the
// generated TFunction key union can't express that without enumerating
// them.
function translate(t: TFunction, key: string): string {
	const untypedT = t as unknown as (key: string) => string;
	return untypedT(key);
}

type WorkflowQuestionResponse = components["schemas"]["WorkflowQuestionResponse"];

const CLASSIFICATION_KEYS: Record<string, string> = {
	policy_resolvable: "shell.workflowQuestions.classification.policy_resolvable",
	auto_resolvable: "shell.workflowQuestions.classification.auto_resolvable",
	human_required: "shell.workflowQuestions.classification.human_required",
	ambiguous: "shell.workflowQuestions.classification.ambiguous",
};

const CLASSIFICATION_BADGE_VARIANT: Record<string, "neutral" | "outline" | "accent" | "success" | "warning" | "error"> = {
	policy_resolvable: "success",
	auto_resolvable: "success",
	human_required: "warning",
	ambiguous: "warning",
};

const STATE_KEYS: Record<string, string> = {
	pending: "shell.workflowQuestions.state.pending",
	resolving: "shell.workflowQuestions.state.resolving",
	answered: "shell.workflowQuestions.state.answered",
	human_required: "shell.workflowQuestions.state.human_required",
	cancelled: "shell.workflowQuestions.state.cancelled",
};

function ageText(t: TFunction, createdAt: string): string {
	const createdMs = Date.parse(createdAt);
	if (Number.isNaN(createdMs)) return "";
	const minutes = Math.max(0, Math.round((Date.now() - createdMs) / 60000));
	if (minutes < 1) return t("shell.workflowQuestions.ageJustNow");
	if (minutes < 60) return t("shell.workflowQuestions.ageMinutes", { count: minutes });
	const hours = Math.round(minutes / 60);
	if (hours < 24) return t("shell.workflowQuestions.ageHours", { count: hours });
	const days = Math.round(hours / 24);
	return t("shell.workflowQuestions.ageDays", { count: days });
}

interface QuestionCardProps {
	question: WorkflowQuestionResponse;
	onAnswer: (args: { questionId: string; choiceId?: string; customText?: string }) => Promise<unknown>;
	answering: boolean;
}

function QuestionCard({ question, onAnswer, answering }: QuestionCardProps) {
	const { t } = useTranslation();
	const [customText, setCustomText] = useState("");
	const [submittingChoice, setSubmittingChoice] = useState<string | null>(null);
	const [submitError, setSubmitError] = useState<string | undefined>(undefined);

	const isAnswerable = question.state === "human_required";
	const hasChoices = Boolean(question.structuredChoices && question.structuredChoices.length > 0);
	const placeholderText = t("shell.workflowQuestions.unreconstructedFallback");

	async function submitChoice(choiceId: string) {
		setSubmitError(undefined);
		setSubmittingChoice(choiceId);
		try {
			await onAnswer({ questionId: question.id, choiceId });
		} catch (err) {
			setSubmitError(err instanceof Error ? err.message : String(err));
		} finally {
			setSubmittingChoice(null);
		}
	}

	async function submitCustom() {
		if (!customText.trim()) return;
		setSubmitError(undefined);
		try {
			await onAnswer({ questionId: question.id, customText: customText.trim() });
			setCustomText("");
		} catch (err) {
			setSubmitError(err instanceof Error ? err.message : String(err));
		}
	}

	return (
		<div className="flex flex-col gap-3 rounded-lg border border-border p-4 text-sm">
			<div className="flex flex-wrap items-center gap-2">
				<Badge variant={CLASSIFICATION_BADGE_VARIANT[question.classification] ?? "neutral"}>
					{translate(t, CLASSIFICATION_KEYS[question.classification] ?? question.classification)}
				</Badge>
				<Badge variant="outline">{translate(t, STATE_KEYS[question.state] ?? question.state)}</Badge>
				<span className="text-xs text-muted-foreground">{ageText(t, question.createdAt)}</span>
				{question.askingHarness && <span className="text-xs text-muted-foreground">{question.askingHarness}</span>}
			</div>

			<p className="text-sm font-medium text-foreground">{question.questionText || placeholderText}</p>

			{question.classificationReason && !question.questionText && (
				<p className="text-xs text-muted-foreground">{question.classificationReason}</p>
			)}

			{question.state === "answered" && (
				<div className="rounded border border-border bg-muted/50 p-2 text-xs">
					<div className="font-medium text-foreground">{t("shell.workflowQuestions.answered")}</div>
					<p className="mt-1 text-muted-foreground">{question.answerText}</p>
					{question.answerSource && (
						<p className="mt-1 text-muted-foreground">
							{t("shell.workflowQuestions.answerSource", { source: question.answerSource })}
						</p>
					)}
				</div>
			)}

			{isAnswerable && (
				<div className="flex flex-col gap-3">
					{hasChoices && (
						<div className="flex flex-col gap-2">
							{question.structuredChoices?.map((choice) => (
								<Button
									key={choice.id}
									variant="outline"
									className="h-12 w-full justify-start text-left text-sm"
									disabled={answering}
									onClick={() => void submitChoice(choice.id)}
								>
									{submittingChoice === choice.id ? t("shell.workflowQuestions.submitting") : choice.label}
								</Button>
							))}
						</div>
					)}

					<div className="flex flex-col gap-2">
						<textarea
							className="min-h-20 w-full rounded-md border border-border bg-background p-3 text-sm text-foreground placeholder:text-muted-foreground focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring/50"
							placeholder={t("shell.workflowQuestions.customResponsePlaceholder")}
							value={customText}
							onChange={(e) => setCustomText(e.target.value)}
							disabled={answering}
						/>
						<Button
							variant="primary"
							className="h-11 w-full"
							disabled={answering || !customText.trim()}
							onClick={() => void submitCustom()}
						>
							{t("shell.workflowQuestions.submitCustomResponse")}
						</Button>
					</div>

					{submitError && <p className="text-xs text-destructive">{submitError}</p>}
				</div>
			)}
		</div>
	);
}

/**
 * WorkflowQuestionsSection is Checkpoint 8K-A's durable question surface:
 * one stacked, mobile-first card per captured question (open or recent),
 * with large tap-friendly choice buttons, a custom-text fallback, a
 * classification badge, and the exact fixed fallback string when the
 * question text could not be reconstructed reliably. No global tray/badge —
 * scoped to this one run's detail page only.
 */
export function WorkflowQuestionsSection({
	questions,
	onAnswer,
	answering,
}: {
	questions: WorkflowQuestionResponse[];
	onAnswer: (args: { questionId: string; choiceId?: string; customText?: string }) => Promise<unknown>;
	answering: boolean;
}) {
	const { t } = useTranslation();
	if (questions.length === 0) return null;

	// Most recent first: an operator scanning the run wants to see what's
	// blocking it right now.
	const sorted = [...questions].sort((a, b) => Date.parse(b.createdAt) - Date.parse(a.createdAt));

	return (
		<section className="flex flex-col gap-3">
			<h2 className="text-sm font-semibold text-muted-foreground">{t("shell.workflowQuestions.title")}</h2>
			<div className="flex flex-col gap-3">
				{sorted.map((q) => (
					<QuestionCard key={q.id} question={q} onAnswer={onAnswer} answering={answering} />
				))}
			</div>
		</section>
	);
}
