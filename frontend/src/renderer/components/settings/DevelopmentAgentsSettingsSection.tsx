import { useState } from "react";
import { useTranslation } from "react-i18next";
import { apiClient, apiErrorMessage } from "../../lib/api-client";
import { useEnvironmentStatus, type EnvironmentStatus } from "../../hooks/useEnvironmentStatus";
import { useCapacity, type CapacitySnapshot } from "../../hooks/useCapacity";
import { Button } from "../ui/button";
import { SettingsRow } from "./SettingsRow";
import { SettingsSection } from "./SettingsSection";
import { AuthStatusBadge, InstalledBadge } from "./StatusBadge";

type AgentCapability = EnvironmentStatus["codex"];

/**
 * Settings → Development Agents: Codex and Claude Code capability cards
 * (checkpoint 8H.5 §3). Every field is either a real probe result or an
 * explicit "—" placeholder — never a guessed value. "Test connection" reuses
 * the existing cheap POST /api/v1/agents/{agent}/probe route rather than
 * running a real task.
 */
export function DevelopmentAgentsSettingsSection({ titleHidden }: { titleHidden?: boolean } = {}) {
	const { t } = useTranslation();
	const { status, isLoading, error, refetch } = useEnvironmentStatus();
	const { capacity } = useCapacity();
	const capacityFor = (harness: string) => capacity?.find((c) => c.harness === harness);

	return (
		<SettingsSection title={t("settings.agents")} sectionId="agents" titleHidden={titleHidden} grouped>
			{error && <p className="px-(--size-settings-row-padding) text-xs text-error">{error}</p>}
			{isLoading && !status && (
				<p className="px-(--size-settings-row-padding) text-xs text-settings-muted">{t("settings.common.checking")}</p>
			)}
			{status && (
				<>
					<AgentCard
						title={t("settings.environment.codex")}
						agentId="codex"
						capability={status.codex}
						capacity={capacityFor("codex")}
						onTested={() => void refetch()}
					/>
					<AgentCard
						title={t("settings.environment.claudeCode")}
						agentId="claude-code"
						capability={status.claude}
						capacity={capacityFor("claude-code")}
						onTested={() => void refetch()}
					/>
				</>
			)}
		</SettingsSection>
	);
}

function AgentCard({
	title,
	agentId,
	capability,
	capacity,
	onTested,
}: {
	title: string;
	agentId: string;
	capability: AgentCapability;
	capacity?: CapacitySnapshot;
	onTested: () => void;
}) {
	const { t } = useTranslation();
	const [testing, setTesting] = useState(false);
	const [testError, setTestError] = useState<string | undefined>();

	const runTest = async () => {
		setTesting(true);
		setTestError(undefined);
		try {
			const { error } = await apiClient.POST("/api/v1/agents/{agent}/probe", {
				params: { path: { agent: agentId } },
			});
			if (error) throw new Error(apiErrorMessage(error));
			onTested();
		} catch (err) {
			setTestError(apiErrorMessage(err));
		} finally {
			setTesting(false);
		}
	};

	return (
		<div className="flex flex-col gap-1.5 border-t border-(--color-border-settings-dialog-header) pt-3 first:border-t-0 first:pt-0">
			<div className="settings-row-bar h-auto min-h-(--size-settings-row) flex-wrap gap-2">
				<span className="min-w-0 flex-1 text-sm font-medium leading-5 text-settings-label">{title}</span>
				<InstalledBadge installed={capability.installed} />
				<AuthStatusBadge status={capability.authState} />
				<Button type="button" variant="outline" size="sm" onClick={() => void runTest()} disabled={testing}>
					{testing ? t("settings.agents.testing") : t("settings.agents.testConnection")}
				</Button>
			</div>
			<SettingsRow label={t("settings.agents.binaryPath")}>
				<span className="truncate text-xs text-settings-muted" title={capability.binaryPath}>
					{capability.binaryPath || "—"}
				</span>
			</SettingsRow>
			<SettingsRow label={t("settings.agents.version")}>
				<span className="text-xs text-settings-muted">{capability.version || "—"}</span>
			</SettingsRow>
			<SettingsRow label={t("settings.agents.configuredRoles")}>
				<span className="text-xs text-settings-muted">
					{capability.configuredRoles && capability.configuredRoles.length > 0 ? capability.configuredRoles.join(", ") : "—"}
				</span>
			</SettingsRow>
			<SettingsRow label={t("settings.agents.model")}>
				<span className="text-xs text-settings-muted">{capability.model || "—"}</span>
			</SettingsRow>
			<SettingsRow label={t("settings.agents.source")}>
				<span className="text-xs text-settings-muted">{capability.source}</span>
			</SettingsRow>
			<SettingsRow label={t("settings.agents.capacity")}>
				<span className="text-xs text-settings-muted">{capacity ? capacity.state : "unknown"}</span>
			</SettingsRow>
			<SettingsRow label={t("settings.agents.lastLimitEvent")}>
				<span className="text-xs text-settings-muted">
					{capacity?.detectedAt ? `${capacity.reason || capacity.state} · ${new Date(capacity.detectedAt).toLocaleString()}` : "—"}
				</span>
			</SettingsRow>
			<SettingsRow label={t("settings.agents.resetIfKnown")}>
				<span className="text-xs text-settings-muted">{capacity?.resetAt ? new Date(capacity.resetAt).toLocaleString() : "Unknown"}</span>
			</SettingsRow>
			{testError && <p className="px-(--size-settings-row-padding) text-xs text-error">{testError}</p>}
		</div>
	);
}
