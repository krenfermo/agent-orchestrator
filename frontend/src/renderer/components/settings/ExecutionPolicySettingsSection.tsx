import { useEffect, useState } from "react";
import { useTranslation } from "react-i18next";
import {
	DndContext,
	KeyboardSensor,
	PointerSensor,
	closestCenter,
	useSensor,
	useSensors,
	type DragEndEvent,
} from "@dnd-kit/core";
import {
	SortableContext,
	arrayMove,
	sortableKeyboardCoordinates,
	useSortable,
	verticalListSortingStrategy,
} from "@dnd-kit/sortable";
import { CSS } from "@dnd-kit/utilities";
import { GripVertical } from "lucide-react";
import { apiErrorMessage } from "../../lib/api-client";
import { useExecutionPolicy, type FallbackBehavior, type ReviewIndependence } from "../../hooks/useExecutionPolicy";
import { useProviderProfiles, type ProviderProfile } from "../../hooks/useProviderProfiles";
import { Badge } from "../ui/badge";
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "../ui/select";
import { Switch } from "../ui/switch";
import { SettingsRow } from "./SettingsRow";
import { SettingsSection } from "./SettingsSection";

type Role = "planner" | "worker" | "reviewer" | "decisionResolver";

const ROLE_CAPABILITY: Record<Role, string> = {
	planner: "planner",
	worker: "worker",
	reviewer: "reviewer",
	decisionResolver: "decision_resolver",
};

/**
 * Checkpoint 8P-C: Settings → Execution Policy. Replaces
 * RoutingPolicySettingsSection's read-only V1-defaults summary with a real
 * editable per-user routing policy: four drag-to-reorder priority lists
 * (one per role) over the current user's own ProviderProfiles, a fallback
 * behavior toggle, a review-independence toggle, and an autonomy toggle
 * (stored only -- Autonomous Mode behavior lands in 8P-D). Only shows
 * profiles owned by the current user; an unsupported provider (e.g.
 * MiniMax, no real adapter) is never selectable; disabled/unconnected
 * profiles are shown with a status badge but remain orderable (a user may
 * pre-arrange preference before actually connecting).
 */
export function ExecutionPolicySettingsSection({ titleHidden }: { titleHidden?: boolean } = {}) {
	const { t } = useTranslation();
	const { policy, isLoading, error, save, isSaving } = useExecutionPolicy();
	const { registry, profiles, isLoading: profilesLoading } = useProviderProfiles();

	const [draft, setDraft] = useState<{
		autonomousMode: boolean;
		priorities: Record<Role, string[]>;
		fallbackBehavior: FallbackBehavior;
		reviewIndependence: ReviewIndependence;
	} | null>(null);
	const [saveError, setSaveError] = useState<string | undefined>();

	useEffect(() => {
		if (!policy) return;
		setDraft({
			autonomousMode: policy.autonomousMode,
			priorities: {
				planner: policy.plannerPriority,
				worker: policy.workerPriority,
				reviewer: policy.reviewerPriority,
				decisionResolver: policy.decisionResolverPriority,
			},
			fallbackBehavior: policy.fallbackBehavior,
			reviewIndependence: policy.reviewIndependence,
		});
	}, [policy]);

	const persist = async (next: NonNullable<typeof draft>) => {
		setDraft(next);
		setSaveError(undefined);
		try {
			await save({
				autonomousMode: next.autonomousMode,
				plannerPriority: next.priorities.planner,
				workerPriority: next.priorities.worker,
				reviewerPriority: next.priorities.reviewer,
				decisionResolverPriority: next.priorities.decisionResolver,
				fallbackBehavior: next.fallbackBehavior,
				reviewIndependence: next.reviewIndependence,
			});
		} catch (err) {
			setSaveError(apiErrorMessage(err));
		}
	};

	return (
		<SettingsSection title={t("settings.executionPolicy.title", "Execution Policy")} sectionId="executionPolicy" titleHidden={titleHidden} grouped>
			{(error || saveError) && <p className="px-(--size-settings-row-padding) text-xs text-error">{error ?? saveError}</p>}
			{(isLoading || profilesLoading || !draft) && (
				<p className="px-(--size-settings-row-padding) text-xs text-settings-muted">{t("settings.common.checking")}</p>
			)}
			{draft && registry && profiles && (
				<>
					<SettingsRow label={t("settings.executionPolicy.autonomy", "Autonomy")}>
						<div className="flex items-center gap-1.5">
							<span className="text-xs text-settings-muted">
								{draft.autonomousMode
									? t("settings.executionPolicy.autonomous", "Autonomous")
									: t("settings.executionPolicy.manual", "Manual")}
							</span>
							<Switch
								checked={draft.autonomousMode}
								disabled={isSaving}
								onCheckedChange={(next) => void persist({ ...draft, autonomousMode: next })}
							/>
						</div>
					</SettingsRow>

					{(["planner", "worker", "reviewer", "decisionResolver"] as Role[]).map((role) => (
						<PriorityListRow
							key={role}
							role={role}
							profiles={profiles}
							available={registry.filter((d) => d.available).map((d) => `${d.provider}:${d.harness}`)}
							order={draft.priorities[role]}
							onChange={(order) => void persist({ ...draft, priorities: { ...draft.priorities, [role]: order } })}
						/>
					))}

					<SettingsRow label={t("settings.executionPolicy.fallback", "Fallback")}>
						<Select
							value={draft.fallbackBehavior}
							onValueChange={(value) => void persist({ ...draft, fallbackBehavior: value as FallbackBehavior })}
						>
							<SelectTrigger aria-label={t("settings.executionPolicy.fallback", "Fallback")} className="settings-field-control w-auto min-w-48">
								<SelectValue />
							</SelectTrigger>
							<SelectContent align="end">
								<SelectItem value="use_next_available">
									{t("settings.executionPolicy.fallback.useNextAvailable", "Use next available")}
								</SelectItem>
								<SelectItem value="wait_for_preferred">
									{t("settings.executionPolicy.fallback.waitForPreferred", "Wait for preferred")}
								</SelectItem>
							</SelectContent>
						</Select>
					</SettingsRow>

					<SettingsRow label={t("settings.executionPolicy.reviewIndependence", "Review independence")}>
						<Select
							value={draft.reviewIndependence}
							onValueChange={(value) => void persist({ ...draft, reviewIndependence: value as ReviewIndependence })}
						>
							<SelectTrigger aria-label={t("settings.executionPolicy.reviewIndependence", "Review independence")} className="settings-field-control w-auto min-w-48">
								<SelectValue />
							</SelectTrigger>
							<SelectContent align="end">
								<SelectItem value="require_different_provider">
									{t("settings.executionPolicy.reviewIndependence.requireDifferent", "Require different provider")}
								</SelectItem>
								<SelectItem value="allow_same_provider_fallback">
									{t("settings.executionPolicy.reviewIndependence.allowSame", "Allow same-provider fallback")}
								</SelectItem>
							</SelectContent>
						</Select>
					</SettingsRow>
				</>
			)}
		</SettingsSection>
	);
}

const ROLE_LABEL_KEY: Record<Role, string> = {
	planner: "settings.executionPolicy.role.planner",
	worker: "settings.executionPolicy.role.worker",
	reviewer: "settings.executionPolicy.role.reviewer",
	decisionResolver: "settings.executionPolicy.role.decisionResolver",
};
const ROLE_LABEL_DEFAULT: Record<Role, string> = {
	planner: "Planner priority",
	worker: "Worker priority",
	reviewer: "Reviewer priority",
	decisionResolver: "Decision resolver priority",
};

function PriorityListRow({
	role,
	profiles,
	available,
	order,
	onChange,
}: {
	role: Role;
	profiles: ProviderProfile[];
	available: string[];
	order: string[];
	onChange: (order: string[]) => void;
}) {
	const { t } = useTranslation();
	const capability = ROLE_CAPABILITY[role];
	// Only profiles owned by the current user, whose provider has a real
	// adapter, and that support this role's required capability -- an
	// unsupported provider (e.g. MiniMax) never appears here.
	const eligible = profiles.filter(
		(p) => available.includes(`${p.provider}:${p.harness}`) && p.capabilities.includes(capability),
	);
	const eligibleIds = new Set(eligible.map((p) => p.id));
	const byId = new Map(eligible.map((p) => [p.id, p]));
	const orderedIds = [...order.filter((id) => eligibleIds.has(id)), ...eligible.map((p) => p.id).filter((id) => !order.includes(id))];

	const sensors = useSensors(useSensor(PointerSensor), useSensor(KeyboardSensor, { coordinateGetter: sortableKeyboardCoordinates }));
	const handleDragEnd = (event: DragEndEvent) => {
		const { active, over } = event;
		if (!over || active.id === over.id) return;
		const oldIndex = orderedIds.indexOf(String(active.id));
		const newIndex = orderedIds.indexOf(String(over.id));
		if (oldIndex === -1 || newIndex === -1) return;
		onChange(arrayMove(orderedIds, oldIndex, newIndex));
	};

	return (
		<div className="flex flex-col gap-1.5 border-t border-(--color-border-settings-dialog-header) py-3 first:border-t-0">
			<span className="px-(--size-settings-row-padding) text-sm font-medium leading-5 text-settings-label">
				{t(ROLE_LABEL_KEY[role], ROLE_LABEL_DEFAULT[role])}
			</span>
			{orderedIds.length === 0 ? (
				<p className="px-(--size-settings-row-padding) text-xs text-settings-muted">
					{t("settings.executionPolicy.noProfiles", "No connected profiles support this role yet.")}
				</p>
			) : (
				<DndContext collisionDetection={closestCenter} onDragEnd={handleDragEnd} sensors={sensors}>
					<SortableContext items={orderedIds} strategy={verticalListSortingStrategy}>
						<ul className="flex flex-col gap-1 px-(--size-settings-row-padding)">
							{orderedIds.map((id, index) => {
								const profile = byId.get(id);
								if (!profile) return null;
								return <PriorityListItem key={id} id={id} rank={index + 1} profile={profile} />;
							})}
						</ul>
					</SortableContext>
				</DndContext>
			)}
		</div>
	);
}

function PriorityListItem({ id, rank, profile }: { id: string; rank: number; profile: ProviderProfile }) {
	const { t } = useTranslation();
	const { attributes, listeners, setNodeRef, transform, transition } = useSortable({ id });
	const style = { transform: CSS.Transform.toString(transform), transition };
	const connected = profile.authState === "authenticated";

	return (
		<li
			ref={setNodeRef}
			style={style}
			className="flex items-center gap-2 rounded-md border border-(--color-border-settings-dialog-header) bg-(--color-bg-settings-row) px-2 py-1.5"
		>
			<button
				type="button"
				className="flex shrink-0 cursor-grab touch-none items-center text-settings-muted active:cursor-grabbing"
				aria-label={t("settings.executionPolicy.reorder", "Reorder")}
				{...attributes}
				{...listeners}
			>
				<GripVertical className="size-icon-lg" aria-hidden="true" />
			</button>
			<span className="w-5 shrink-0 text-xs text-settings-muted">{rank}</span>
			<span className="min-w-0 flex-1 truncate text-sm text-settings-label">{profile.displayName}</span>
			{!profile.enabled ? (
				<Badge variant="neutral">{t("settings.agents.disabled", "Disabled")}</Badge>
			) : connected ? (
				<Badge variant="success">{t("settings.agents.connected", "Connected")}</Badge>
			) : (
				<Badge variant="neutral">{t("settings.agents.notConnected", "Not connected")}</Badge>
			)}
		</li>
	);
}
