import { useQuery } from "@tanstack/react-query";
import { CircleAlert, KeyRound, ShieldCheck } from "lucide-react";
import { useTranslation } from "react-i18next";
import type { components } from "../../../api/schema";
import { apiClient, apiErrorMessage } from "../../lib/api-client";
import { Badge } from "../ui/badge";
import { SettingsSection } from "./SettingsSection";

type Install = components["schemas"]["SkillInstallView"];
type Origin = components["schemas"]["SkillInstallOriginView"];

/**
 * SkillInstalledSettingsSection — Settings → Skills → Installed.
 *
 * # Why this screen exists
 *
 * Until phase 12.1 nothing rendered what AO had verified about a package that
 * is actually on this host. The Marketplace tab lists what is AVAILABLE, and
 * every row there is honestly "nothing checked yet" because a search hashes
 * nothing. So a person could install a signed release, have AO verify the
 * whole chain, and then find no screen anywhere that distinguished it from a
 * package AO had merely hashed. The provenance existed in the API and had no
 * reader.
 *
 * # Why here and not in Project → Skills
 *
 * Installing is installation-wide; enabling is per project. Project settings
 * are where somebody grants a skill capabilities on one repository, and
 * putting provenance there would imply the verification is per project — it is
 * not, it is a property of the bytes on this host. This tab sits beside
 * Marketplace, Registries and Trust, which are the other installation-wide
 * skill decisions.
 *
 * # Why it does not duplicate the Marketplace row
 *
 * The Marketplace answers "what could I install and what does the registry
 * claim". This answers "what is here and what did AO actually check". They
 * share a skill id and nothing else: one is a claim, the other is a record.
 *
 * # An external install shows WHERE, not just WHO
 *
 * A package from a git forge carries a repository, a tag and a COMMIT, and the
 * three are not interchangeable. The tag is a label somebody can re-point; the
 * commit is what these bytes actually came from, and this row keeps saying so
 * whatever the tag does afterwards. The identity sentence beside it is the
 * daemon's, because the four different answers to "who published this" on a
 * forge -- the account, the name in the package, the key that signed it, and
 * what this installation expected -- are routinely different people, and a
 * screen that merged them would print "published by acme" because a repository
 * at github.com/acme said so.
 *
 * # The sentences are the daemon's
 *
 * `trustExplanation` is served, not written here, for the reason the rest of
 * this family is: a screen that wrote its own version of "what trusted means"
 * would eventually write a kinder one, and the kindest available lie about
 * this subject is that trusted means safe. Nothing in this file claims a
 * package is safe, secure or free of vulnerabilities, and nothing may.
 */
export function SkillInstalledSettingsSection() {
	const { t } = useTranslation();

	const installed = useQuery({
		queryKey: ["skills", "installed"] as const,
		queryFn: async () => {
			const { data, error } = await apiClient.GET("/api/v1/skills", { credentials: "include" });
			if (error || !data) throw new Error(apiErrorMessage(error));
			return data;
		},
	});

	const rows = installed.data?.skills ?? [];

	const trustTone = (trust: string) =>
		trust === "trusted"
			? "success"
			: trust === "revoked"
				? "error"
				: trust === "verified"
					? "outline"
					: "warning";

	const line = (label: string, value?: string | null, mono = false) =>
		value ? (
			<p className={`text-caption text-settings-muted${mono ? " font-mono break-all" : ""}`}>
				{label}: {value}
			</p>
		) : null;

	const provenance = (origin: Origin) => {
		const p = origin.provenance;
		// No provenance object at all means AO checked no signature -- an
		// unsigned release from a digest-policy registry. That is a different
		// fact from a signature that was checked and refused, and the two must
		// not render the same.
		if (!p) {
			return (
				<p className="text-caption text-settings-muted">
					{t("settings.skillInstalled.noSignatureChecked")}
				</p>
			);
		}
		if (!p.verified) {
			return (
				<div className="flex flex-col gap-0.5" data-testid="skill-installed-signature-refused">
					<Badge variant="error">{t("settings.skillInstalled.signatureRefused")}</Badge>
					{line(t("settings.skillInstalled.refusalCode"), p.refusalCode)}
					{p.refusal ? <p className="text-caption text-error">{p.refusal}</p> : null}
				</div>
			);
		}
		return (
			<div className="flex flex-col gap-0.5" data-testid="skill-installed-provenance">
				<div className="flex flex-wrap items-center gap-2 text-caption">
					<KeyRound className="size-3 shrink-0 text-settings-muted" aria-hidden="true" />
					<span className="font-mono">{p.keyId}</span>
					{p.keyOrigin ? (
						<Badge variant="outline">{t(`settings.skillTrust.origin.${p.keyOrigin}`)}</Badge>
					) : null}
					{p.trustRootTier ? (
						<Badge variant="outline">{t(`settings.skillTrust.tier.${p.trustRootTier}`)}</Badge>
					) : null}
				</div>
				{/* Short for recognition, full for comparison -- deciding two keys
				    are the same one needs every character. */}
				<p className="text-caption font-mono text-settings-muted break-all">
					{t("settings.skillTrust.fingerprint", {
						short: p.keyFingerprintShort ?? "",
						full: p.keyFingerprint ?? "",
					})}
				</p>
				{line(
					t("settings.skillInstalled.trustRoot"),
					p.trustRootName ? `${p.trustRootName} (${p.trustRootId})` : p.trustRootId,
				)}
				{line(t("settings.skillInstalled.signedBy"), p.publisher)}
				{line(t("settings.skillInstalled.algorithm"), p.algorithm)}
				{line(
					t("settings.skillInstalled.verifiedAt"),
					p.verifiedAt ? new Date(p.verifiedAt).toLocaleString() : null,
				)}
				{line(
					t("settings.skillInstalled.signedAt"),
					p.signedAt ? new Date(p.signedAt).toLocaleString() : null,
				)}
			</div>
		);
	};

	return (
		<SettingsSection
			title={t("settings.skillInstalled.title")}
			data-testid="skill-installed-settings"
		>
			<p className="text-caption text-settings-muted">{t("settings.skillInstalled.intro")}</p>

			{installed.error ? (
				<p className="flex items-start gap-2 text-caption text-error">
					<CircleAlert className="mt-0.5 size-3 shrink-0" aria-hidden="true" />
					{(installed.error as Error).message}
				</p>
			) : null}

			{rows.length === 0 && !installed.isLoading ? (
				<p className="text-caption text-settings-muted">{t("settings.skillInstalled.empty")}</p>
			) : (
				<ul className="flex flex-col gap-2" data-testid="skill-installed-list">
					{rows.map((s: Install) => {
						const origin = s.origin;
						return (
							<li
								className="flex flex-col gap-1 rounded-(--radius-settings-dialog-lg) border border-[var(--color-border-settings-input)] p-3"
								key={`${s.id}@${s.version}`}
							>
								<div className="flex flex-wrap items-center gap-2">
									<ShieldCheck
										className="size-3.5 shrink-0 text-settings-muted"
										aria-hidden="true"
									/>
									<span className="text-caption font-medium">{s.name}</span>
									<span className="text-caption font-mono text-settings-muted">
										{s.id}@{s.version}
									</span>
									{/* The four states, as WORDS. A package with no origin
									    row has no registry provenance at all, which is what
									    a hand-vetted local install genuinely is. */}
									{origin ? (
										<Badge variant={trustTone(origin.trust)}>
											{t(`settings.skillMarketplace.trust.${origin.trust}`)}
										</Badge>
									) : (
										<Badge variant="outline">
											{t("settings.skillInstalled.noRegistryOrigin")}
										</Badge>
									)}
								</div>

								{origin ? (
									<>
										{/* Served by the daemon so this screen cannot describe
										    the model more generously than the thing enforcing it. */}
										<p className="text-caption text-settings-muted">
											{origin.trustExplanation}
										</p>
										{line(t("settings.skillInstalled.publisher"), origin.publisher)}
										{line(
											t("settings.skillInstalled.registry"),
											origin.registryName
												? `${origin.registryName} (${origin.registryId}, ${origin.registryType})`
												: origin.registryId,
										)}
										{line(t("settings.skillInstalled.trustPolicy"), origin.trustPolicy)}
										{/* The forge provenance, when there is one. The commit
										    is shown short AND full: recognising a commit and
										    deciding two are the same one are different jobs,
										    and only the second needs every character. */}
										{origin.sourceProvider ? (
											<div
												className="flex flex-col gap-0.5"
												data-testid="skill-installed-source"
											>
												{line(
													t("settings.skillInstalled.sourceRepository"),
													`${origin.sourceOwner}/${origin.sourceRepository} (${origin.sourceProvider}, ${t(
														origin.sourceVisibility === "private"
															? "settings.skillMarketplace.sourcePrivate"
															: "settings.skillMarketplace.sourcePublic",
													)})`,
												)}
												{line(t("settings.skillInstalled.sourceTag"), origin.sourceTag)}
												{line(
													t("settings.skillInstalled.sourceCommit"),
													origin.sourceCommit
														? `${origin.sourceShortCommit} (${origin.sourceCommit})`
														: null,
													true,
												)}
												{line(t("settings.skillInstalled.sourcePath"), origin.sourcePath)}
												{line(
													t("settings.skillInstalled.sourceFetchedAt"),
													origin.sourceFetchedAt
														? new Date(origin.sourceFetchedAt).toLocaleString()
														: null,
												)}
												{/* The daemon's sentence about identity. It says
												    what is a claim and what was checked, and it
												    never says the code is safe. */}
												{origin.identityExplanation ? (
													<p className="text-caption text-settings-muted">
														{origin.identityExplanation}
													</p>
												) : null}
											</div>
										) : null}
										{line(t("settings.skillInstalled.artifactDigest"), origin.artifactDigest, true)}
										{line(t("settings.skillInstalled.manifestDigest"), origin.manifestDigest, true)}
										{provenance(origin)}
										{/* What the revocation picture looked like when AO
										    installed it: "nobody had withdrawn this" and "AO
										    could not ask" are different facts. */}
										{line(
											t("settings.skillInstalled.revocationObserved"),
											origin.revocationStateObserved,
										)}
										{origin.revoked ? (
											<p className="text-caption text-error" data-testid="skill-installed-revoked">
												{t("settings.skillInstalled.revokedNote", {
													reason: origin.revocationReason ?? "",
												})}
											</p>
										) : null}
									</>
								) : (
									<p className="text-caption text-settings-muted">
										{t("settings.skillInstalled.noRegistryOriginNote")}
									</p>
								)}
							</li>
						);
					})}
				</ul>
			)}
		</SettingsSection>
	);
}
