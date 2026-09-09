import { useTranslation } from "react-i18next";
import { AlertTriangle } from "lucide-react";
import type { components } from "../../api/schema";
import { translateDynamic } from "./workflow-activity";

type ReviewRiskFacts = components["schemas"]["WorkflowReviewRiskFacts"];

/**
 * workflow-change-set.tsx — what AO observed the work change, and how it knows.
 *
 * The daemon has always sent this. `WorkflowStepView.reviewPolicy.facts` carries
 * the changed-file count, the committed subset, the base/head pair the diff was
 * taken between, the provenance of the set, and — when AO could not establish
 * it at all — a sentence explaining that "in terms a person can act on"
 * (review_changed_files.go). The renderer showed the review *decision* and its
 * reasons and dropped every one of those facts.
 *
 * The consequence is the failure this file exists to end: a task whose only
 * output landed somewhere Git ignores, or which recorded no base commit, read on
 * screen as **a run where nothing happened**. AO knew the difference between
 * "no changes" and "no answer" — `taskChangeSet.Proven()` is exactly that
 * distinction — and the product refused to pass it on, so the user went looking
 * in SQLite for a reason AO had already written down.
 *
 * Nothing here is derived. `changedFilesUnprovable` is the daemon's own
 * fail-closed verdict and `unprovableChangeSetReason` is the daemon's own
 * sentence; this component only makes sure neither is silent.
 */
export function WorkflowChangeSet({ facts }: { facts: ReviewRiskFacts | undefined }) {
	const { t } = useTranslation();
	if (!facts) return null;

	const unprovable = facts.changedFilesUnprovable === true;
	const count = facts.changedFileCount ?? 0;
	const committed = facts.committedChangedFileCount;
	const range =
		facts.changeSetBaseSha && facts.changeSetHeadSha
			? `${facts.changeSetBaseSha.slice(0, 12)}..${facts.changeSetHeadSha.slice(0, 12)}`
			: "";

	return (
		<div
			className="mt-2 flex flex-col gap-1 border-t border-border pt-2 text-xs text-muted-foreground"
			data-testid="workflow-change-set"
		>
			{/* The headline is the honest one of three, never a bare "0 files".
			    "AO could not establish" and "AO established that nothing
			    changed" are different facts and must not render alike. */}
			{unprovable ? (
				<p className="flex items-start gap-1.5 font-medium text-status-needs-you">
					<AlertTriangle aria-hidden="true" className="mt-px size-3 shrink-0" />
					<span>{t("wf.changeSet.unprovable")}</span>
				</p>
			) : (
				<p className="font-medium">
					{count === 0 ? t("wf.changeSet.none") : t("wf.changeSet.count", { count })}
				</p>
			)}

			{/* The daemon's own explanation of why it could not prove the set.
			    It is composed in English on the server, exactly like
			    presentation.technical.attentionDetail, and is shown verbatim
			    rather than replaced by a guess. */}
			{unprovable && facts.unprovableChangeSetReason ? (
				<p>{facts.unprovableChangeSetReason}</p>
			) : null}

			<dl className="grid grid-cols-[auto_1fr] gap-x-3 gap-y-0.5">
				{!unprovable && committed !== undefined && count > 0 ? (
					<>
						<dt>{t("wf.changeSet.committed")}</dt>
						<dd>{t("wf.changeSet.committedOf", { committed, count })}</dd>
					</>
				) : null}
				{facts.changedFilesSource ? (
					<>
						<dt>{t("wf.changeSet.source")}</dt>
						<dd>
							{translateDynamic(
								t,
								`wf.changeSet.sourceValue.${facts.changedFilesSource}`,
								facts.changedFilesSource,
							)}
						</dd>
					</>
				) : null}
				{range ? (
					<>
						<dt>{t("wf.changeSet.range")}</dt>
						<dd className="break-all font-mono">{range}</dd>
					</>
				) : null}
			</dl>

			{/* The paths themselves, when AO has them. A collapsed list keeps a
			    large change from burying the verdict above it. */}
			{facts.changedFilePaths && facts.changedFilePaths.length > 0 ? (
				<details>
					<summary className="cursor-pointer">{t("wf.changeSet.showPaths")}</summary>
					<ul className="mt-1 flex flex-col gap-0.5">
						{facts.changedFilePaths.map((path) => (
							<li className="break-all font-mono" key={path}>
								{path}
							</li>
						))}
					</ul>
				</details>
			) : null}
		</div>
	);
}
