import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { CircleAlert, GitBranch } from "lucide-react";
import { useState } from "react";
import { useTranslation } from "react-i18next";
import type { components } from "../../../api/schema";
import { apiClient, apiErrorMessage } from "../../lib/api-client";
import { Badge } from "../ui/badge";
import { Button } from "../ui/button";
import { Input } from "../ui/input";
import {
	Select,
	SelectContent,
	SelectItem,
	SelectTrigger,
	SelectValue,
} from "../ui/select";
import { SettingsSection } from "./SettingsSection";

type MovedTag = components["schemas"]["SkillMovedTagView"];
type Revocation = components["schemas"]["SkillExternalRevocationView"];
type Subject = (typeof SUBJECTS)[number];

// The three blast radii, narrowest first. The order is the guidance: during an
// incident the narrowest true statement is almost always the right one,
// because withdrawing one commit leaves everybody on a good version working
// and withdrawing an account takes a dependency away from all of them.
const SUBJECTS = ["external_commit", "external_repository", "external_owner"] as const;

/**
 * Settings → Skills → External. The two things a git forge cannot tell AO, and
 * that AO therefore has to keep track of itself.
 *
 * **Tags that moved.** A tag is a name somebody can re-point; the commit is
 * the release. AO writes down where each tag pointed and says, here, when that
 * changed — with both commits in full, because deciding two commits are
 * different needs every character and this is exactly the moment somebody goes
 * and compares them against a repository page. Nothing already installed is
 * changed by a tag moving: its provenance still records the commit its bytes
 * actually came from, because that is what happened.
 *
 * **What this installation refuses to install from.** A forge publishes NO
 * revocation feed. There is no endpoint AO could poll to learn that a
 * repository was compromised, so this list is a decision somebody here made
 * after reading an advisory — and the screen says so rather than implying AO
 * was told. Withdrawing blocks NEW installs; it uninstalls nothing, disables
 * nothing and stops no run under way, and the daemon's own sentence at the
 * bottom is what says so.
 *
 * The asymmetry with Trust is deliberate and visible here: an external
 * withdrawal can be LIFTED and a signing-key revocation cannot. A key somebody
 * else may have held is compromised forever; a repository transferred to
 * somebody who was then vetted is a judgement about a place, and places get
 * re-vetted.
 */
export function SkillExternalSettingsSection() {
	const { t } = useTranslation();
	const queryClient = useQueryClient();
	const [error, setError] = useState<string | null>(null);
	const [form, setForm] = useState({
		subject: "external_repository" as (typeof SUBJECTS)[number],
		subjectId: "",
		reason: "",
	});

	const revocationsKey = ["skills", "external", "revocations"] as const;
	const revocations = useQuery({
		queryKey: revocationsKey,
		queryFn: async () => {
			const { data, error: apiError } = await apiClient.GET(
				"/api/v1/skills/external/revocations",
				{ credentials: "include" },
			);
			if (apiError || !data) throw new Error(apiErrorMessage(apiError));
			return data;
		},
	});

	const tagsKey = ["skills", "external", "tags"] as const;
	const tags = useQuery({
		queryKey: tagsKey,
		queryFn: async () => {
			const { data, error: apiError } = await apiClient.GET("/api/v1/skills/external/tags", {
				credentials: "include",
			});
			if (apiError || !data) throw new Error(apiErrorMessage(apiError));
			return data;
		},
	});

	const invalidate = () => {
		void queryClient.invalidateQueries({ queryKey: ["skills"] });
	};

	const revoke = useMutation({
		mutationFn: async () => {
			const { error: apiError } = await apiClient.POST("/api/v1/skills/external/revocations", {
				credentials: "include",
				body: {
					subject: form.subject,
					subjectId: form.subjectId.trim(),
					reason: form.reason.trim(),
				},
			});
			if (apiError) throw new Error(apiErrorMessage(apiError));
		},
		onSuccess: () => {
			setError(null);
			setForm({ subject: "external_repository", subjectId: "", reason: "" });
			invalidate();
		},
		onError: (err: Error) => setError(err.message),
	});

	const lift = useMutation({
		mutationFn: async (rev: Revocation) => {
			const { error: apiError } = await apiClient.POST(
				"/api/v1/skills/external/revocations/lift",
				{
					credentials: "include",
					body: { subject: rev.subject, subjectId: rev.subjectId },
				},
			);
			if (apiError) throw new Error(apiErrorMessage(apiError));
		},
		onSuccess: () => {
			setError(null);
			invalidate();
		},
		onError: (err: Error) => setError(err.message),
	});

	const movedRows = tags.data?.tags ?? [];
	const revocationRows = revocations.data?.revocations ?? [];
	const complete = form.subjectId.trim() !== "" && form.reason.trim() !== "";

	return (
		<SettingsSection
			title={t("settings.skillExternal.title")}
			data-testid="skill-external-settings"
		>
			<p className="text-caption text-settings-muted">{t("settings.skillExternal.intro")}</p>

			{error ? (
				<p className="flex items-start gap-2 text-caption text-error" role="alert">
					<CircleAlert className="mt-0.5 size-3 shrink-0" aria-hidden="true" />
					{error}
				</p>
			) : null}

			{/* Tags that moved. */}
			<div className="flex flex-col gap-2" data-testid="skill-external-moved-tags">
				<p className="text-caption font-medium">{t("settings.skillExternal.movedTitle")}</p>
				{movedRows.length === 0 ? (
					// "No tag has moved" is a real and good state, and it is said
					// plainly rather than rendered as an empty list.
					<p className="text-caption text-settings-muted">
						{t("settings.skillExternal.movedEmpty")}
					</p>
				) : (
					<ul className="flex flex-col gap-2">
						{movedRows.map((row: MovedTag) => (
							<li
								className="flex flex-col gap-1 rounded-(--radius-settings-dialog-lg) border border-[var(--color-border-settings-input)] p-3"
								key={`${row.registryId}/${row.owner}/${row.repository}#${row.tag}`}
							>
								<div className="flex flex-wrap items-center gap-2">
									<GitBranch className="size-3 shrink-0 text-settings-muted" aria-hidden="true" />
									<span className="text-caption font-medium">
										{row.owner}/{row.repository}
									</span>
									<span className="text-caption font-mono text-settings-muted">{row.tag}</span>
									<Badge variant="error">{t("settings.skillExternal.movedBadge")}</Badge>
								</div>
								{/* The daemon's sentence, with both SHAs in full. */}
								<p className="break-all text-caption text-warning">{row.explanation}</p>
							</li>
						))}
					</ul>
				)}
				{/* What AO does about it, from the daemon. */}
				{tags.data?.policy ? (
					<p className="text-caption text-settings-muted" data-testid="skill-external-tag-policy">
						{tags.data.policy}
					</p>
				) : null}
			</div>

			{/* What this installation will not install from. */}
			<div className="flex flex-col gap-2" data-testid="skill-external-revocations">
				<p className="text-caption font-medium">{t("settings.skillExternal.revocationsTitle")}</p>
				<p className="text-caption text-settings-muted">
					{t("settings.skillExternal.revocationsNote")}
				</p>
				{revocationRows.length === 0 ? (
					<p className="text-caption text-settings-muted">
						{t("settings.skillExternal.revocationsEmpty")}
					</p>
				) : (
					<ul className="flex flex-col gap-2">
						{revocationRows.map((rev: Revocation) => (
							<li
								className="flex flex-col gap-1 rounded-(--radius-settings-dialog-lg) border border-[var(--color-border-settings-input)] p-3"
								key={`${rev.subject}/${rev.subjectId}`}
							>
								<div className="flex flex-wrap items-center gap-2">
									<Badge variant="outline">
										{t(`settings.skillExternal.subjectOption.${rev.subject as Subject}`)}
									</Badge>
									<span className="break-all text-caption font-mono">{rev.subjectId}</span>
								</div>
								<p className="text-caption text-settings-muted">{rev.reason}</p>
								<p className="text-caption text-settings-muted">
									{t("settings.skillExternal.revokedBy", {
										actor: rev.revokedBy ?? "",
										at: new Date(rev.revokedAt).toLocaleString(),
									})}
								</p>
								<Button
									variant="secondary"
									size="sm"
									className="self-start"
									disabled={lift.isPending}
									onClick={() => lift.mutate(rev)}
									data-testid={`skill-external-lift-${rev.subjectId}`}
								>
									{lift.isPending
										? t("settings.skillExternal.lifting")
										: t("settings.skillExternal.liftAction")}
								</Button>
							</li>
						))}
					</ul>
				)}
				{/* The non-promises matter as much as the promise, and they are the
				    daemon's words. */}
				{revocations.data?.policy ? (
					<p
						className="text-caption text-warning"
						data-testid="skill-external-revocation-policy"
					>
						{revocations.data.policy}
					</p>
				) : null}
			</div>

			{/* Adding one. */}
			<div className="flex flex-col gap-2" data-testid="skill-external-revoke-form">
				<p className="text-caption font-medium">{t("settings.skillExternal.addTitle")}</p>
				<div className="grid grid-cols-2 gap-2">
					<label className="flex flex-col gap-1 text-caption">
						{t("settings.skillExternal.subjectLabel")}
						<Select
							value={form.subject}
							onValueChange={(value) =>
								setForm({ ...form, subject: value as (typeof SUBJECTS)[number] })
							}
						>
							<SelectTrigger aria-label={t("settings.skillExternal.subjectLabel")}>
								<SelectValue />
							</SelectTrigger>
							<SelectContent>
								{SUBJECTS.map((subject) => (
									<SelectItem key={subject} value={subject}>
										{t(`settings.skillExternal.subjectOption.${subject}`)}
									</SelectItem>
								))}
							</SelectContent>
						</Select>
					</label>
					<label className="flex flex-col gap-1 text-caption">
						{t("settings.skillExternal.subjectIdLabel")}
						<Input
							value={form.subjectId}
							placeholder={
								form.subject === "external_owner"
									? "acme"
									: form.subject === "external_repository"
										? "acme/skills"
										: "acme/skills@0f1e2d3c4b5a69788796a5b4c3d2e1f001122334"
							}
							onChange={(e) => setForm({ ...form, subjectId: e.target.value })}
						/>
					</label>
				</div>
				{/* Outside the label: a label's text IS the field's accessible name,
				    and guidance folded into one renames the field for anybody using
				    a screen reader. */}
				<p className="text-caption text-settings-muted">
					{t(`settings.skillExternal.subjectIdHelp.${form.subject}`)}
				</p>
				<label className="flex flex-col gap-1 text-caption">
					{t("settings.skillExternal.reasonLabel")}
					<Input
						value={form.reason}
						onChange={(e) => setForm({ ...form, reason: e.target.value })}
					/>
				</label>
				{/* A revocation that does not say why is indistinguishable from a
				    mistake, and it is about to block every install under it. */}
				<p className="text-caption text-settings-muted">
					{t("settings.skillExternal.reasonHelp")}
				</p>
				<Button
					variant="primary"
					size="sm"
					className="self-start"
					disabled={!complete || revoke.isPending}
					onClick={() => revoke.mutate()}
				>
					{revoke.isPending
						? t("settings.skillExternal.adding")
						: t("settings.skillExternal.addAction")}
				</Button>
			</div>
		</SettingsSection>
	);
}
