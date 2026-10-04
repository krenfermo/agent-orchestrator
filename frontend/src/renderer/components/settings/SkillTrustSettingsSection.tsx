import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { CircleAlert, KeyRound, ShieldCheck } from "lucide-react";
import { useState } from "react";
import { useTranslation } from "react-i18next";
import type { components } from "../../../api/schema";
import { apiClient, apiErrorMessage } from "../../lib/api-client";
import { classifyTrustValidity, trustValidityTone } from "../../lib/trust-validity";
import { Badge } from "../ui/badge";
import { Button } from "../ui/button";
import { Input } from "../ui/input";
import { SettingsSection } from "./SettingsSection";

type TrustRoot = components["schemas"]["SkillTrustRootView"];
type SigningKey = components["schemas"]["SkillSigningKeyView"];
type TrustRevocation = components["schemas"]["SkillTrustRevocationView"];

/**
 * SkillTrustSettingsSection — Settings → Skills → Trust.
 *
 * # What this screen is
 *
 * The list of anchors this installation will accept a signature from, and the
 * public keys under them. A release becomes TRUSTED only by chaining to one of
 * these, so this is the screen that decides what "trusted" is allowed to mean
 * here.
 *
 * # The three things it must never do
 *
 * **Never accept a private key.** The only key field takes 32 bytes of public
 * key. There is no "generate" button, because the daemon verifies and does not
 * sign: a consumer that could sign could mint its own trusted releases. The
 * daemon refuses a 64-byte value — somebody pasting the wrong half of a
 * keypair — and the refusal is shown verbatim rather than softened.
 *
 * **Never let AO Official be edited.** Built-in roots render with their edit
 * and revoke affordances absent, not merely disabled. The reserved id and
 * publisher are refused by the daemon too, so this is the visible half of a
 * rule enforced somewhere a form cannot reach.
 *
 * **Never describe the trust model.** Every sentence about what trusted means
 * comes from the daemon (`trustModel`, `officialRootNote`), for the same reason
 * the marketplace's does: a screen that wrote its own version would eventually
 * write a nicer one, and the nicest available lie about this subject is
 * "trusted means safe".
 *
 * # Why fingerprints are shown twice
 *
 * Grouped for recognition, and in full beside it. A truncated fingerprint is
 * for recognising a key you already know; deciding two keys are the same one
 * needs every character, and a screen that only offered the short form would be
 * inviting the comparison it cannot support.
 *
 * # Why the validity window is shown, not just the status
 *
 * A key's window is what decides whether a HISTORICAL signature verifies: the
 * backend checks a signature against the moment it was made, not against now.
 * So "active" alone is not enough to read a provenance record — a release
 * signed last year by a key that expired in March is still legitimately
 * trusted, and a screen showing only "Expired" would imply the opposite.
 * Every root and key therefore prints validFrom, validUntil (or, explicitly,
 * that there is none) and a state that distinguishes closing a window on
 * schedule from repudiating it.
 *
 * # Why revoked and retired keys stay on screen
 *
 * They are the history of what this installation once accepted. A list that
 * hid them would answer "whose signatures have we trusted" with "whose
 * signatures do we trust right now", and an incident review needs the first.
 */
export function SkillTrustSettingsSection() {
	const { t } = useTranslation();
	const queryClient = useQueryClient();
	const key = ["skills", "trust"] as const;
	const [error, setError] = useState<string | null>(null);
	const [rootForm, setRootForm] = useState({ id: "", displayName: "", publisher: "" });
	const [keyForm, setKeyForm] = useState({ keyId: "", trustRootId: "", publicKey: "" });

	const trust = useQuery({
		queryKey: key,
		queryFn: async () => {
			const { data, error: apiError } = await apiClient.GET("/api/v1/skills/trust", {
				credentials: "include",
			});
			if (apiError || !data) throw new Error(apiErrorMessage(apiError));
			return data;
		},
	});

	const invalidate = () => void queryClient.invalidateQueries({ queryKey: key });

	const addRoot = useMutation({
		mutationFn: async () => {
			const { error: apiError } = await apiClient.PUT("/api/v1/skills/trust/roots/{trustRootId}", {
				credentials: "include",
				params: { path: { trustRootId: rootForm.id } },
				body: { displayName: rootForm.displayName, publisher: rootForm.publisher },
			});
			if (apiError) throw new Error(apiErrorMessage(apiError));
		},
		onSuccess: () => {
			setError(null);
			setRootForm({ id: "", displayName: "", publisher: "" });
			invalidate();
		},
		onError: (err: Error) => setError(err.message),
	});

	const addKey = useMutation({
		mutationFn: async () => {
			const { error: apiError } = await apiClient.PUT("/api/v1/skills/trust/keys/{keyId}", {
				credentials: "include",
				params: { path: { keyId: keyForm.keyId } },
				body: { trustRootId: keyForm.trustRootId, publicKey: keyForm.publicKey },
			});
			if (apiError) throw new Error(apiErrorMessage(apiError));
		},
		onSuccess: () => {
			setError(null);
			setKeyForm({ keyId: "", trustRootId: "", publicKey: "" });
			invalidate();
		},
		onError: (err: Error) => setError(err.message),
	});

	// The three revocable subjects, as the API spells them. A RELEASE is
	// deliberately absent: a release is withdrawn by the registry that
	// published it, and a second place to do it would be a second place to
	// look.
	type RevocableSubject = "signing_key" | "publisher" | "trust_root";

	const revoke = useMutation({
		mutationFn: async (input: { subject: RevocableSubject; subjectId: string; reason: string }) => {
			const { error: apiError } = await apiClient.POST("/api/v1/skills/trust/revocations", {
				credentials: "include",
				body: input,
			});
			if (apiError) throw new Error(apiErrorMessage(apiError));
		},
		onSuccess: () => {
			setError(null);
			invalidate();
		},
		onError: (err: Error) => setError(err.message),
	});

	const roots = trust.data?.roots ?? [];
	const revocations = trust.data?.revocations ?? [];

	// A revocation needs a reason, and the reason is typed at the moment of
	// revoking rather than defaulted. A stored default would put the same
	// sentence on every incident.
	const askAndRevoke = (subject: RevocableSubject, subjectId: string) => {
		const reason = window.prompt(t("settings.skillTrust.revokeReasonPrompt", { subject: subjectId }));
		if (reason === null || reason.trim() === "") return;
		revoke.mutate({ subject, subjectId, reason });
	};

	// One clock for the whole render, so two rows cannot disagree about "now".
	const now = new Date();

	const formatDate = (value?: string | null) =>
		value ? new Date(value).toLocaleString() : null;

	// The validity block every root and key gets. It prints the window in full
	// -- including saying so when there is no expiry -- because an absent
	// validUntil and an unknown one look identical if you only omit the line.
	const validity = (row: { status: string; validFrom?: string; validUntil?: string | null }) => {
		const verdict = classifyTrustValidity(
			{ status: row.status, validFrom: row.validFrom, validUntil: row.validUntil },
			now,
		);
		return (
			<div className="flex flex-col gap-0.5">
				<div className="flex flex-wrap items-center gap-2">
					{/* The state is a WORD as well as a colour. A state carried by
					    colour alone is a state some readers cannot read. */}
					<Badge variant={trustValidityTone(verdict.state)}>
						{t(`settings.skillTrust.validity.${verdict.state}`)}
					</Badge>
					<span className="text-caption text-settings-muted">
						{t("settings.skillTrust.validFrom", { from: formatDate(row.validFrom) ?? "—" })}
					</span>
					<span className="text-caption text-settings-muted">
						{row.validUntil
							? t("settings.skillTrust.validUntil", { until: formatDate(row.validUntil) })
							: t("settings.skillTrust.noExpiry")}
					</span>
				</div>
				{/* Closing a window on schedule keeps history; revoking repudiates
				    it. Saying which is the difference between "this old release is
				    still fine" and "re-check everything this key ever signed". */}
				{verdict.state === "expired" || verdict.state === "retired" ? (
					<p className="text-caption text-settings-muted">
						{t("settings.skillTrust.stillVerifiesHistory")}
					</p>
				) : null}
				{verdict.state === "revoked" ? (
					<p className="text-caption text-error">
						{t("settings.skillTrust.historyRepudiated")}
					</p>
				) : null}
				{verdict.state === "not-yet-valid" ? (
					<p className="text-caption text-settings-muted">
						{t("settings.skillTrust.notYetValidNote")}
					</p>
				) : null}
			</div>
		);
	};

	return (
		<SettingsSection title={t("settings.skillTrust.title")} data-testid="skill-trust-settings">
			<p className="text-caption text-settings-muted">{t("settings.skillTrust.intro")}</p>

			{/* From the daemon, so this screen cannot describe the trust model
			    more optimistically than the thing enforcing it. */}
			{trust.data?.trustModel ? (
				<p className="text-caption text-settings-muted">{trust.data.trustModel}</p>
			) : null}

			{/* The absence of an official root is stated, not left as a greyed
			    out control a person has to guess about. */}
			{trust.data && !trust.data.officialRootAvailable && trust.data.officialRootNote ? (
				<p
					className="flex items-start gap-2 text-caption text-warning"
					data-testid="skill-trust-official-note"
				>
					<CircleAlert className="mt-0.5 size-3 shrink-0" aria-hidden="true" />
					{trust.data.officialRootNote}
				</p>
			) : null}

			{error ? (
				<p className="flex items-start gap-2 text-caption text-error">
					<CircleAlert className="mt-0.5 size-3 shrink-0" aria-hidden="true" />
					{error}
				</p>
			) : null}

			{roots.length === 0 ? (
				// An empty trust store says what that MEANS. "No roots" alone
				// reads like a setup step nobody got to; "nothing can reach
				// trusted" is the actual state.
				<p className="text-caption text-settings-muted">{t("settings.skillTrust.empty")}</p>
			) : (
				<ul className="flex flex-col gap-2" data-testid="skill-trust-roots">
					{roots.map((root: TrustRoot) => (
						<li
							className="flex flex-col gap-2 rounded-(--radius-settings-dialog-lg) border border-[var(--color-border-settings-input)] p-3"
							key={root.trustRootId}
						>
							<div className="flex flex-wrap items-center gap-2">
								<ShieldCheck className="size-3.5 shrink-0 text-settings-muted" aria-hidden="true" />
								<span className="text-caption font-medium">{root.displayName}</span>
								<Badge variant="outline">{t(`settings.skillTrust.tier.${root.tier}`)}</Badge>
								<Badge variant="outline">
									{t(`settings.skillTrust.status.${root.status}`)}
								</Badge>
								{root.builtIn ? (
									<Badge variant="outline" data-testid="skill-trust-builtin">
										{t("settings.skillTrust.builtIn")}
									</Badge>
								) : null}
							</div>
							<p className="text-caption text-settings-muted">
								{t("settings.skillTrust.rootIdentity", {
									id: root.trustRootId,
									publisher: root.publisher,
								})}
							</p>
							{validity(root)}
							{root.revocationReason ? (
								<p className="text-caption text-error">
									{t("settings.skillTrust.revokedReason", { reason: root.revocationReason })}
								</p>
							) : null}

							{root.keys.length === 0 ? (
								<p className="text-caption text-settings-muted">
									{t("settings.skillTrust.noKeys")}
								</p>
							) : (
								<ul className="flex flex-col gap-1">
									{root.keys.map((k: SigningKey) => (
										<li className="flex flex-col gap-0.5" key={k.keyId}>
											<div className="flex flex-wrap items-center gap-2 text-caption">
												<KeyRound className="size-3 shrink-0 text-settings-muted" aria-hidden="true" />
												<span className="font-mono">{k.keyId}</span>
												<Badge variant="outline">
													{t(`settings.skillTrust.status.${k.status}`)}
												</Badge>
												<Badge variant="outline">
													{t(`settings.skillTrust.origin.${k.origin}`)}
												</Badge>
												{k.isRootKey ? (
													<Badge variant="outline">{t("settings.skillTrust.rootKey")}</Badge>
												) : null}
											</div>
											{/* Grouped for recognition, in full for comparison.
											    Deciding two keys are the same one needs every
											    character. */}
											<p className="text-caption font-mono text-settings-muted">
												{t("settings.skillTrust.fingerprint", {
													short: k.fingerprintShort,
													full: k.fingerprint,
												})}
											</p>
											{/* The publisher a key signs for, beside the key, so a
											    reader does not have to infer it from the root. */}
											<p className="text-caption text-settings-muted">
												{t("settings.skillTrust.keyPublisher", {
													publisher: k.publisher,
													algorithm: k.algorithm,
												})}
											</p>
											{validity(k)}
											{k.rotatedFromKeyId ? (
												<p className="text-caption text-settings-muted">
													{t("settings.skillTrust.rotatedFrom", { keyId: k.rotatedFromKeyId })}
												</p>
											) : null}
											{k.revocationReason ? (
												<p className="text-caption text-error">
													{t("settings.skillTrust.revokedReason", { reason: k.revocationReason })}
												</p>
											) : null}
											{/* A built-in key's revoke affordance is ABSENT rather
											    than disabled: it is not a thing this host may do,
											    and a greyed-out button invites somebody to look
											    for the way round it. */}
											{!root.builtIn && k.status !== "revoked" ? (
												<div>
													<Button
														variant="ghost"
														size="sm"
														onClick={() => askAndRevoke("signing_key", k.keyId)}
													>
														{t("settings.skillTrust.revokeKeyAction")}
													</Button>
												</div>
											) : null}
										</li>
									))}
								</ul>
							)}

							{!root.builtIn && root.status !== "revoked" ? (
								<div>
									<Button
										variant="ghost"
										size="sm"
										onClick={() => askAndRevoke("trust_root", root.trustRootId)}
									>
										{t("settings.skillTrust.revokeRootAction")}
									</Button>
								</div>
							) : null}
						</li>
					))}
				</ul>
			)}

			{revocations.length > 0 ? (
				<div className="flex flex-col gap-1" data-testid="skill-trust-revocations">
					<h3 className="text-caption font-medium">{t("settings.skillTrust.revocationsTitle")}</h3>
					{revocations.map((r: TrustRevocation) => (
						<p className="text-caption text-settings-muted" key={`${r.subject}:${r.subjectId}`}>
							{t("settings.skillTrust.revocationRow", {
								subject: t(`settings.skillTrust.subject.${r.subject}`),
								subjectId: r.subjectId,
								reason: r.reason,
								time: new Date(r.revokedAt).toLocaleString(),
							})}
						</p>
					))}
				</div>
			) : null}

			<div className="flex flex-col gap-2 rounded-(--radius-settings-dialog-lg) border border-[var(--color-border-settings-input)] p-3">
				<h3 className="text-caption font-medium">{t("settings.skillTrust.addRootTitle")}</h3>
				<p className="text-caption text-settings-muted">{t("settings.skillTrust.addRootNote")}</p>
				<label className="flex flex-col gap-1 text-caption">
					{t("settings.skillTrust.rootIdLabel")}
					<Input
						value={rootForm.id}
						onChange={(e) => setRootForm({ ...rootForm, id: e.target.value })}
					/>
				</label>
				<label className="flex flex-col gap-1 text-caption">
					{t("settings.skillTrust.rootNameLabel")}
					<Input
						value={rootForm.displayName}
						onChange={(e) => setRootForm({ ...rootForm, displayName: e.target.value })}
					/>
				</label>
				<label className="flex flex-col gap-1 text-caption">
					{t("settings.skillTrust.rootPublisherLabel")}
					<Input
						value={rootForm.publisher}
						onChange={(e) => setRootForm({ ...rootForm, publisher: e.target.value })}
					/>
				</label>
				<div>
					<Button
						size="sm"
						disabled={
							addRoot.isPending ||
							!rootForm.id.trim() ||
							!rootForm.displayName.trim() ||
							!rootForm.publisher.trim()
						}
						onClick={() => addRoot.mutate()}
					>
						{t("settings.skillTrust.addRootAction")}
					</Button>
				</div>
			</div>

			<div className="flex flex-col gap-2 rounded-(--radius-settings-dialog-lg) border border-[var(--color-border-settings-input)] p-3">
				<h3 className="text-caption font-medium">{t("settings.skillTrust.addKeyTitle")}</h3>
				{/* The sentence that stops somebody pasting the wrong half of a
				    keypair, on the screen where they would do it. */}
				<p className="text-caption text-settings-muted">{t("settings.skillTrust.addKeyNote")}</p>
				<label className="flex flex-col gap-1 text-caption">
					{t("settings.skillTrust.keyIdLabel")}
					<Input
						value={keyForm.keyId}
						onChange={(e) => setKeyForm({ ...keyForm, keyId: e.target.value })}
					/>
				</label>
				<label className="flex flex-col gap-1 text-caption">
					{t("settings.skillTrust.keyRootLabel")}
					<Input
						value={keyForm.trustRootId}
						onChange={(e) => setKeyForm({ ...keyForm, trustRootId: e.target.value })}
					/>
				</label>
				<label className="flex flex-col gap-1 text-caption">
					{t("settings.skillTrust.publicKeyLabel")}
					<Input
						value={keyForm.publicKey}
						onChange={(e) => setKeyForm({ ...keyForm, publicKey: e.target.value })}
					/>
				</label>
				<div>
					<Button
						size="sm"
						disabled={
							addKey.isPending ||
							!keyForm.keyId.trim() ||
							!keyForm.trustRootId.trim() ||
							!keyForm.publicKey.trim()
						}
						onClick={() => addKey.mutate()}
					>
						{t("settings.skillTrust.addKeyAction")}
					</Button>
				</div>
			</div>
		</SettingsSection>
	);
}
