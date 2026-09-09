import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { useState } from "react";
import { useTranslation } from "react-i18next";
import { CircleAlert } from "lucide-react";
import type { components } from "../../../api/schema";
import { apiClient, apiErrorMessage } from "../../lib/api-client";
import { Badge } from "../ui/badge";
import { Button } from "../ui/button";
import { Input } from "../ui/input";
import { SettingsSection } from "./SettingsSection";

type Approval = components["schemas"]["SkillImageApprovalView"];

/**
 * SkillImagesSettingsSection — AO's trust root for container images, as a
 * screen.
 *
 * The one thing this must never imply is that AO verified anything about the
 * bytes. It did not: an approval records that a named administrator looked at
 * an exact digest and decided this scope may execute it. It is not a publisher
 * signature, AO checks none, and the daemon says so in its own response — which
 * is why the trust model is rendered from the API rather than written here.
 *
 * Revoked and expired approvals stay on screen. They are the history of what
 * this installation once allowed, and a list that hid them would answer "what
 * did we approve" with "what is approved right now".
 *
 * Approving is a form with a digest in it, and the digest is the whole
 * decision. The daemon refuses one this host cannot show it — no pull, no
 * guess — so a well-formed digest that is not present here is rejected, and the
 * refusal is shown verbatim instead of being softened.
 */
export function SkillImagesSettingsSection() {
	const { t } = useTranslation();
	const queryClient = useQueryClient();
	const key = ["skills", "images"] as const;
	const [error, setError] = useState<string | null>(null);
	const [form, setForm] = useState({
		tenantId: "",
		projectId: "",
		skillId: "",
		version: "",
		modeId: "",
		tool: "",
		reference: "",
		digest: "",
		note: "",
	});

	const approvals = useQuery({
		queryKey: key,
		queryFn: async () => {
			const { data, error: apiError } = await apiClient.GET("/api/v1/skills/images", {
				credentials: "include",
			});
			if (apiError || !data) throw new Error(apiErrorMessage(apiError));
			return data;
		},
	});

	const invalidate = () => void queryClient.invalidateQueries({ queryKey: key });

	const approve = useMutation({
		mutationFn: async () => {
			const { error: apiError } = await apiClient.POST("/api/v1/skills/images", {
				credentials: "include",
				// confirm is set HERE, by the act of submitting this form, and is
				// not a checkbox with a default. The daemon refuses without it.
				body: { ...form, confirm: true },
			});
			if (apiError) throw new Error(apiErrorMessage(apiError));
		},
		onSuccess: () => {
			setError(null);
			setForm({
				tenantId: "", projectId: "", skillId: "", version: "",
				modeId: "", tool: "", reference: "", digest: "", note: "",
			});
			invalidate();
		},
		onError: (err: Error) => setError(err.message),
	});

	const revoke = useMutation({
		mutationFn: async (id: string) => {
			const { error: apiError } = await apiClient.DELETE("/api/v1/skills/images/{approvalId}", {
				credentials: "include",
				params: { path: { approvalId: id } },
			});
			if (apiError) throw new Error(apiErrorMessage(apiError));
		},
		onSuccess: () => {
			setError(null);
			invalidate();
		},
		onError: (err: Error) => setError(err.message),
	});

	const rows = approvals.data?.approvals ?? [];
	const complete = Object.values(form).every((v) => v.trim() !== "");

	const field = (name: keyof typeof form, label: string, placeholder?: string) => (
		<label className="flex flex-col gap-1 text-caption" key={name}>
			{label}
			<Input
				value={form[name]}
				placeholder={placeholder}
				onChange={(e) => setForm({ ...form, [name]: e.target.value })}
			/>
		</label>
	);

	return (
		<SettingsSection title={t("settings.skillImages.title")} data-testid="skill-images-settings">
			<p className="text-caption text-settings-muted">{t("settings.skillImages.intro")}</p>

			{error ? (
				<p className="flex items-start gap-2 text-caption text-error">
					<CircleAlert className="mt-0.5 size-3 shrink-0" aria-hidden="true" />
					{error}
				</p>
			) : null}

			{rows.length === 0 ? (
				// An empty trust root says what that MEANS. "No approvals" alone
				// reads like a setup step nobody got to; "nothing may execute" is
				// the actual state.
				<p className="text-caption text-settings-muted">{t("settings.skillImages.empty")}</p>
			) : (
				<ul className="flex flex-col gap-2" data-testid="skill-image-approvals">
					{rows.map((a: Approval) => (
						<li
							className="flex flex-col gap-1 rounded-(--radius-settings-dialog-lg) border border-[var(--color-border-settings-input)] p-3"
							key={a.id}
						>
							<div className="flex items-center gap-2">
								<Badge variant={a.active ? "success" : "warning"}>
									{a.active
										? t("settings.skillImages.active")
										: t("settings.skillImages.inactive", {
												reason: a.inactiveReason || t("settings.skillImages.unknownReason"),
											})}
								</Badge>
								<span className="text-caption text-settings-muted">{a.id}</span>
							</div>
							<p className="text-caption">
								{t("settings.skillImages.scope", {
									tenant: a.tenantId, project: a.projectId,
									skill: a.skillId, version: a.version,
									mode: a.modeId, tool: a.tool,
								})}
							</p>
							<p className="break-all text-caption text-settings-muted">
								{t("settings.skillImages.image", { digest: a.digest, reference: a.reference })}
							</p>
							{/* Who authorized it and when. An approval nobody signed
							    is one nobody can be asked about. */}
							<p className="text-caption text-settings-muted">
								{t("settings.skillImages.approvedBy", { actor: a.approvedBy, at: a.approvedAt })}
							</p>
							{a.expiresAt ? (
								<p className="text-caption text-settings-muted">
									{t("settings.skillImages.expires", { at: a.expiresAt })}
								</p>
							) : null}
							{a.revokedAt ? (
								<p className="text-caption text-settings-muted">
									{t("settings.skillImages.revoked", { at: a.revokedAt })}
								</p>
							) : null}
							{a.note ? <p className="text-caption text-settings-muted">{a.note}</p> : null}
							{a.active ? (
								<Button
									variant="secondary"
									size="sm"
									className="self-start"
									disabled={revoke.isPending}
									onClick={() => revoke.mutate(a.id)}
								>
									{t("settings.skillImages.revokeAction")}
								</Button>
							) : null}
						</li>
					))}
				</ul>
			)}

			{/* The non-promises come from the daemon's own response, so this
			    screen cannot describe the trust model more optimistically than
			    the thing enforcing it. */}
			{approvals.data?.trustModel ? (
				<p className="text-caption text-settings-muted">{approvals.data.trustModel}</p>
			) : null}
			{approvals.data?.revocationPolicy ? (
				<p className="text-caption text-settings-muted">{approvals.data.revocationPolicy}</p>
			) : null}

			<div className="flex flex-col gap-2" data-testid="skill-image-approve-form">
				<p className="text-caption font-medium">{t("settings.skillImages.approveTitle")}</p>
				<p className="text-caption text-settings-muted">{t("settings.skillImages.approveNote")}</p>
				<div className="grid grid-cols-2 gap-2">
					{field("tenantId", t("settings.skillImages.tenant"))}
					{field("projectId", t("settings.skillImages.project"))}
					{field("skillId", t("settings.skillImages.skill"))}
					{field("version", t("settings.skillImages.version"))}
					{field("modeId", t("settings.skillImages.mode"))}
					{field("tool", t("settings.skillImages.tool"), "ao.static-scan/v1")}
					{field("reference", t("settings.skillImages.reference"), "alpine")}
					{field("digest", t("settings.skillImages.digest"), "sha256:…")}
				</div>
				{field("note", t("settings.skillImages.note"))}
				<Button
					variant="primary"
					size="sm"
					className="self-start"
					// Every scope field is required: an approval missing one of
					// them authorizes somebody, somewhere, to run something.
					disabled={!complete || approve.isPending}
					onClick={() => approve.mutate()}
				>
					{approve.isPending
						? t("settings.skillImages.approving")
						: t("settings.skillImages.approveAction")}
				</Button>
			</div>
		</SettingsSection>
	);
}
