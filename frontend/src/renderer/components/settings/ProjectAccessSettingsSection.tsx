import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { Loader2, UserRound, UsersRound } from "lucide-react";
import { useState } from "react";
import { useTranslation } from "react-i18next";
import { apiClient, apiErrorMessage } from "../../lib/api-client";
import type { components } from "../../../api/schema";
import { Badge } from "../ui/badge";
import { Button } from "../ui/button";
import {
	Select,
	SelectContent,
	SelectItem,
	SelectTrigger,
	SelectValue,
} from "../ui/select";
import { SettingsSection } from "./SettingsSection";

type ProjectAccess = components["schemas"]["ProjectAccessResponse"];
type AdminUser = components["schemas"]["AdminUserView"];
type Team = components["schemas"]["TeamView"];

const projectRoles = ["admin", "member", "viewer"] as const;
type ProjectRole = (typeof projectRoles)[number];

/**
 * Project settings → Access (P4-B). Who can reach THIS project.
 *
 * The controls render from `permissions` in the response — the caller's own
 * effective permissions on this project, computed by the daemon — rather than
 * from a role name this component would have to interpret. That is what lets a
 * project administrator manage their own project's access without holding any
 * installation-wide authority, and what keeps a change to the role tables from
 * needing a matching change in React.
 */
export function ProjectAccessSettingsSection({ projectId }: { projectId: string }) {
	const { t } = useTranslation();
	const queryClient = useQueryClient();
	const accessKey = ["rbac", "project-access", projectId] as const;
	const [subjectKind, setSubjectKind] = useState<"user" | "team">("user");
	const [subjectId, setSubjectId] = useState("");
	const [role, setRole] = useState<ProjectRole>("member");
	const [error, setError] = useState<string | null>(null);

	const access = useQuery({
		queryKey: accessKey,
		queryFn: async (): Promise<ProjectAccess> => {
			const { data, error: apiError } = await apiClient.GET("/api/v1/projects/{id}/access", {
				credentials: "include",
				params: { path: { id: projectId } },
			});
			if (apiError || !data) throw new Error(apiErrorMessage(apiError));
			return data;
		},
	});

	const canManage = (access.data?.permissions ?? []).includes("project.access.manage");

	// The list needs names, not ids, whether or not the caller may change it --
	// an access list rendered as raw uuids is not an access list anybody can
	// read. A project administrator may hold no installation-wide read
	// permission at all, so a refused listing simply leaves the rows showing
	// the id they already carry rather than breaking the screen.
	const users = useQuery({
		queryKey: ["rbac", "users"] as const,
		queryFn: async (): Promise<AdminUser[]> => {
			const { data, error: apiError } = await apiClient.GET("/api/v1/users", { credentials: "include" });
			if (apiError || !data) throw new Error(apiErrorMessage(apiError));
			return data.users;
		},
		enabled: access.isSuccess,
		retry: false,
	});
	const teams = useQuery({
		queryKey: ["rbac", "teams"] as const,
		queryFn: async (): Promise<Team[]> => {
			const { data, error: apiError } = await apiClient.GET("/api/v1/teams", { credentials: "include" });
			if (apiError || !data) throw new Error(apiErrorMessage(apiError));
			return data.teams;
		},
		enabled: access.isSuccess,
		retry: false,
	});

	const invalidate = () => {
		void queryClient.invalidateQueries({ queryKey: accessKey });
	};

	const grant = useMutation({
		mutationFn: async () => {
			const { error: apiError } = await apiClient.PUT("/api/v1/projects/{id}/access", {
				credentials: "include",
				params: { path: { id: projectId } },
				body: { subjectKind, subjectId, role },
			});
			if (apiError) throw new Error(apiErrorMessage(apiError));
		},
		onSuccess: () => {
			setSubjectId("");
			setError(null);
			invalidate();
		},
		onError: (err: Error) => setError(err.message),
	});

	const revoke = useMutation({
		mutationFn: async ({ kind, id }: { kind: string; id: string }) => {
			const { error: apiError } = await apiClient.DELETE(
				"/api/v1/projects/{id}/access/{subjectKind}/{subjectId}",
				{
					credentials: "include",
					params: { path: { id: projectId, subjectKind: kind, subjectId: id } },
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

	const roleLabel = (value: string) => {
		switch (value) {
			case "admin":
				return t("settings.access.role.admin");
			case "viewer":
				return t("settings.access.role.viewer");
			default:
				return t("settings.access.role.member");
		}
	};

	const nameOf = (kind: string, id: string): string => {
		if (kind === "team") return teams.data?.find((team) => team.id === id)?.name ?? id;
		return users.data?.find((user) => user.id === id)?.displayName ?? id;
	};

	const grants = access.data?.grants ?? [];
	const candidates =
		subjectKind === "team"
			? (teams.data ?? []).map((team) => ({ id: team.id, label: team.name }))
			: (users.data ?? []).map((user) => ({ id: user.id, label: user.displayName }));

	return (
		<SettingsSection title={t("settings.project.access")} sectionId="project-access">
			<p className="px-3 text-caption text-settings-muted">{t("settings.project.access.description")}</p>

			{access.isLoading ? (
				<div className="flex items-center gap-2 px-3 py-2 text-caption text-settings-muted">
					<Loader2 className="size-3 animate-spin" aria-hidden="true" />
					{t("settings.project.access.loading")}
				</div>
			) : access.error ? (
				<p className="px-3 text-caption text-error">{(access.error as Error).message}</p>
			) : (
				<ul className="flex flex-col gap-1.5">
					{access.data?.ownerUserId ? (
						<li className="flex items-center gap-3 rounded-(--radius-settings-dialog-lg) border border-[var(--color-border-settings-input)] bg-[var(--color-bg-settings-input)] p-3">
							<UserRound className="size-4 shrink-0 text-settings-muted" aria-hidden="true" />
							<span className="min-w-0 flex-1 truncate text-sm text-settings-label">
								{nameOf("user", access.data.ownerUserId)}
							</span>
							<Badge variant="neutral">{t("settings.project.access.owner")}</Badge>
						</li>
					) : null}
					{grants.length === 0 && !access.data?.ownerUserId ? (
						<p className="px-3 text-caption text-settings-muted">{t("settings.project.access.empty")}</p>
					) : null}
					{grants.map((row) => (
						<li
							key={`${row.subjectKind}:${row.subjectId}`}
							className="flex items-center gap-3 rounded-(--radius-settings-dialog-lg) border border-[var(--color-border-settings-input)] bg-[var(--color-bg-settings-input)] p-3"
							data-testid="project-access-row"
						>
							{row.subjectKind === "team" ? (
								<UsersRound className="size-4 shrink-0 text-settings-muted" aria-hidden="true" />
							) : (
								<UserRound className="size-4 shrink-0 text-settings-muted" aria-hidden="true" />
							)}
							<span className="min-w-0 flex-1 truncate text-sm text-settings-label">
								{nameOf(row.subjectKind, row.subjectId)}
							</span>
							<Badge variant="neutral">{roleLabel(row.role)}</Badge>
							{canManage ? (
								<Button
									variant="secondary"
									size="sm"
									disabled={revoke.isPending}
									onClick={() => revoke.mutate({ kind: row.subjectKind, id: row.subjectId })}
								>
									{t("settings.project.access.revoke")}
								</Button>
							) : null}
						</li>
					))}
				</ul>
			)}

			{error ? <p className="px-3 text-caption text-error">{error}</p> : null}

			{canManage ? (
				<form
					className="flex flex-wrap items-center gap-2 px-3"
					onSubmit={(event) => {
						event.preventDefault();
						setError(null);
						grant.mutate();
					}}
				>
					<Select
						value={subjectKind}
						onValueChange={(next) => {
							setSubjectKind(next as "user" | "team");
							setSubjectId("");
						}}
					>
						<SelectTrigger className="w-32" aria-label={t("settings.project.access.subjectKind")}>
							<SelectValue />
						</SelectTrigger>
						<SelectContent>
							<SelectItem value="user">{t("settings.project.access.subjectUser")}</SelectItem>
							<SelectItem value="team">{t("settings.project.access.subjectTeam")}</SelectItem>
						</SelectContent>
					</Select>
					<Select value={subjectId} onValueChange={setSubjectId}>
						<SelectTrigger className="min-w-44 flex-1" aria-label={t("settings.project.access.subject")}>
							<SelectValue placeholder={t("settings.project.access.subject")} />
						</SelectTrigger>
						<SelectContent>
							{candidates.map((candidate) => (
								<SelectItem key={candidate.id} value={candidate.id}>
									{candidate.label}
								</SelectItem>
							))}
						</SelectContent>
					</Select>
					<Select value={role} onValueChange={(next) => setRole(next as ProjectRole)}>
						<SelectTrigger className="w-32" aria-label={t("settings.access.users.role")}>
							<SelectValue />
						</SelectTrigger>
						<SelectContent>
							{projectRoles.map((option) => (
								<SelectItem key={option} value={option}>
									{roleLabel(option)}
								</SelectItem>
							))}
						</SelectContent>
					</Select>
					<Button type="submit" size="sm" disabled={subjectId === "" || grant.isPending}>
						{t("settings.project.access.grant")}
					</Button>
				</form>
			) : (
				<p className="px-3 text-caption text-settings-muted">{t("settings.project.access.readOnly")}</p>
			)}
		</SettingsSection>
	);
}
