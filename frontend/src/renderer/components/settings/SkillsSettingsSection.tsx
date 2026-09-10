import { useState } from "react";
import { useTranslation } from "react-i18next";
import { Tabs, TabsContent, TabsList, TabsTrigger } from "../ui/tabs";
import { SkillImagesSettingsSection } from "./SkillImagesSettingsSection";
import { SkillInstalledSettingsSection } from "./SkillInstalledSettingsSection";
import { SkillMarketplaceSettingsSection } from "./SkillMarketplaceSettingsSection";
import { SkillRegistriesSettingsSection } from "./SkillRegistriesSettingsSection";
import { SkillTrustSettingsSection } from "./SkillTrustSettingsSection";

/**
 * Settings → Skills. The three installation-wide skill surfaces, as tabs.
 *
 * They are tabs rather than one long page because they are three separate
 * decisions with three separate consequences, taken at different times:
 *
 * **Marketplace** — find a package and put its bytes on this host. It reaches
 * no project.
 * **Installed** — what is actually on this host and what AO verified about it.
 * It is a separate tab from Marketplace because those answer two different
 * questions: the Marketplace shows what a registry CLAIMS about something you
 * could install, and this shows what AO RECORDED about something it already
 * fetched and checked. A row in one is a claim; a row in the other is evidence.
 * **Registries** — decide which outside sources this installation may install
 * from at all, and see what those sources now say about what is already here.
 * **Trust** — decide whose signatures this installation will accept at all.
 * It is what makes a release reach "trusted" rather than stop at "verified",
 * and it is a separate tab from Registries because a registry is WHERE a
 * package comes from and a trust root is WHO signed it. Conflating them is
 * exactly the mistake TLS invites: authenticating the server says nothing
 * about who wrote the code.
 * **Container images** — decide which bytes this installation may EXECUTE, per
 * digest and per exact scope.
 *
 * What is deliberately NOT here is the per-project Skills panel, which lives in
 * Project Settings under a different permission. Installing a skill and letting
 * a project use it are the two halves this whole design keeps apart, and
 * putting them on one screen would be the first step to collapsing them.
 */
export function SkillsSettingsSection({ titleHidden }: { titleHidden?: boolean }) {
	const { t } = useTranslation();
	// Per-viewer convenience only; nothing here needs to persist.
	const [tab, setTab] = useState("marketplace");

	return (
		<div
			className="flex w-full flex-col gap-(--size-settings-section-gap)"
			data-testid="skills-settings"
		>
			{!titleHidden ? (
				<h2 className="px-3 text-xs font-medium leading-4 text-settings-muted">
					{t("settings.skills.title")}
				</h2>
			) : null}
			<Tabs value={tab} onValueChange={setTab}>
				<TabsList>
					<TabsTrigger value="marketplace">{t("settings.skills.tab.marketplace")}</TabsTrigger>
					<TabsTrigger value="installed">{t("settings.skills.tab.installed")}</TabsTrigger>
					<TabsTrigger value="registries">{t("settings.skills.tab.registries")}</TabsTrigger>
					<TabsTrigger value="trust">{t("settings.skills.tab.trust")}</TabsTrigger>
					<TabsTrigger value="images">{t("settings.skills.tab.images")}</TabsTrigger>
				</TabsList>
				<TabsContent value="marketplace" className="pt-3">
					<SkillMarketplaceSettingsSection />
				</TabsContent>
				<TabsContent value="installed" className="pt-3">
					<SkillInstalledSettingsSection />
				</TabsContent>
				<TabsContent value="registries" className="pt-3">
					<SkillRegistriesSettingsSection />
				</TabsContent>
				<TabsContent value="trust" className="pt-3">
					<SkillTrustSettingsSection />
				</TabsContent>
				<TabsContent value="images" className="pt-3">
					<SkillImagesSettingsSection />
				</TabsContent>
			</Tabs>
			{/* The sentence that keeps the two halves apart, on the screen where
			    somebody is about to install something. */}
			<p className="px-3 text-caption text-settings-muted">{t("settings.skills.separationNote")}</p>
		</div>
	);
}
