import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { Loader2, ShieldCheck, UserPlus, Users, UsersRound } from "lucide-react";
import { useState } from "react";
import { useTranslation } from "react-i18next";
import { apiClient, apiErrorMessage } from "../../lib/api-client";
import { useAuthStore, useCan } from "../../stores/auth-store";
import type { components } from "../../../api/schema";
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

type AdminUser = components["schemas"]["AdminUserView"];
type Team = components["schemas"]["TeamView"];
type TeamMember = components["schemas"]["TeamMemberView"];

export const usersQueryKey = ["rbac", "users"] as const;
export const teamsQueryKey = ["rbac", "teams"] as const;

/** The roles a person can be given here. Ownership is transferred, never assigned. */
const assignableRoles = ["admin", "member", "viewer"] as const;
type AssignableRole = (typeof assignableRoles)[number];

/**
 * Settings → Users & teams (P4-B). The administration surface for the
 * installation's accounts and groups.
 *
 * Everything rendered here is a convenience over an API that decides for
 * itself: the section is mounted only when the backend reports users.read or
 * teams.read in the capability list, each control is shown only when it reports
 * the matching manage capability, and calling any of these routes directly
 * without the permission is refused all the same. React hides; the daemon
 * decides.
 */
export function AccessSettingsSection({ titleHidden }: { titleHidden?: boolean } = {}) {
	const canReadUsers = useCan("users.read");
	const canReadTeams = useCan("teams.read");

	return (
		<>
			{canReadUsers ? <UsersBlock titleHidden={titleHidden} /> : null}
			{canReadTeams ? <TeamsBlock /> : null}
		</>
	);
}

function useRoleLabel() {
	const { t } = useTranslation();
	return (role: string): string => {
		switch (role) {
			case "owner":
				return t("settings.access.role.owner");
			case "admin":
				return t("settings.access.role.admin");
			case "viewer":
				return t("settings.access.role.viewer");
			default:
				return t("settings.access.role.member");
		}
	};
}

function UsersBlock({ titleHidden }: { titleHidden?: boolean }) {
	const { t } = useTranslation();
	const queryClient = useQueryClient();
	const roleLabel = useRoleLabel();
	const canManage = useCan("users.manage");
	const currentUserId = useAuthStore((state) => state.user?.id ?? null);
	const [creating, setCreating] = useState(false);
	const [actionError, setActionError] = useState<string | null>(null);

	const query = useQuery({
		queryKey: usersQueryKey,
		queryFn: async (): Promise<AdminUser[]> => {
			const { data, error } = await apiClient.GET("/api/v1/users", { credentials: "include" });
			if (error || !data) throw new Error(apiErrorMessage(error));
			return data.users;
		},
	});

	const invalidate = () => {
		void queryClient.invalidateQueries({ queryKey: usersQueryKey });
	};

	const setStatus = useMutation({
		mutationFn: async ({ id, status }: { id: string; status: "active" | "disabled" }) => {
			const { error } = await apiClient.PATCH("/api/v1/users/{id}/status", {
				credentials: "include",
				params: { path: { id } },
				body: { status },
			});
			// The daemon refuses to disable the owner or the caller's own
			// account. Surfacing its message verbatim is the point: the reason
			// is a real rule, not a validation quibble the UI should paraphrase.
			if (error) throw new Error(apiErrorMessage(error));
		},
		onSuccess: () => {
			setActionError(null);
			invalidate();
		},
		onError: (err: Error) => setActionError(err.message),
	});

	const setRole = useMutation({
		mutationFn: async ({ id, role }: { id: string; role: string }) => {
			const { error } = await apiClient.PATCH("/api/v1/users/{id}/role", {
				credentials: "include",
				params: { path: { id } },
				body: { role: role as AssignableRole },
			});
			if (error) throw new Error(apiErrorMessage(error));
		},
		onSuccess: () => {
			setActionError(null);
			invalidate();
		},
		onError: (err: Error) => setActionError(err.message),
	});

	const users = query.data ?? [];

	return (
		<SettingsSection title={t("settings.access.users")} sectionId="access-users" titleHidden={titleHidden}>
			<p className="px-3 text-caption text-settings-muted">{t("settings.access.users.description")}</p>

			{query.isLoading ? (
				<div className="flex items-center gap-2 px-3 py-2 text-caption text-settings-muted">
					<Loader2 className="size-3 animate-spin" aria-hidden="true" />
					{t("settings.access.users.loading")}
				</div>
			) : query.error ? (
				<p className="px-3 text-caption text-error">{(query.error as Error).message}</p>
			) : users.length === 0 ? (
				<p className="px-3 text-caption text-settings-muted">{t("settings.access.users.empty")}</p>
			) : (
				<ul className="flex flex-col gap-1.5">
					{users.map((user) => {
						const isOwner = user.role === "owner";
						const isSelf = user.id === currentUserId;
						return (
							<li
								key={user.id}
								className="flex items-center gap-3 rounded-(--radius-settings-dialog-lg) border border-[var(--color-border-settings-input)] bg-[var(--color-bg-settings-input)] p-3"
								data-testid="access-user-row"
							>
								<Users className="size-4 shrink-0 text-settings-muted" aria-hidden="true" />
								<div className="min-w-0 flex-1">
									<div className="truncate text-sm text-settings-label">{user.displayName}</div>
									<div className="truncate text-caption text-settings-muted">{user.email}</div>
								</div>
								{user.federated ? (
									<Badge variant="neutral">{t("settings.access.users.federated")}</Badge>
								) : null}
								{user.status === "disabled" ? (
									<Badge variant="neutral">{t("settings.access.users.disabled")}</Badge>
								) : null}
								{/* The owner's role is not editable here: ownership moves by
								    transfer, and the daemon refuses anything else. */}
								{canManage && !isOwner ? (
									<Select
										value={user.role}
										onValueChange={(role) => setRole.mutate({ id: user.id, role })}
									>
										<SelectTrigger className="w-36" aria-label={t("settings.access.users.role")}>
											<SelectValue />
										</SelectTrigger>
										<SelectContent>
											{assignableRoles.map((role) => (
												<SelectItem key={role} value={role}>
													{roleLabel(role)}
												</SelectItem>
											))}
										</SelectContent>
									</Select>
								) : (
									<Badge variant="neutral">{roleLabel(user.role)}</Badge>
								)}
								{canManage && !isOwner && !isSelf ? (
									<Button
										variant="secondary"
										size="sm"
										disabled={setStatus.isPending}
										onClick={() =>
											setStatus.mutate({
												id: user.id,
												status: user.status === "disabled" ? "active" : "disabled",
											})
										}
									>
										{user.status === "disabled"
											? t("settings.access.users.enable")
											: t("settings.access.users.disable")}
									</Button>
								) : null}
							</li>
						);
					})}
				</ul>
			)}

			{actionError ? <p className="px-3 text-caption text-error">{actionError}</p> : null}

			{canManage ? (
				creating ? (
					<CreateUserForm
						onDone={() => {
							setCreating(false);
							invalidate();
						}}
						onCancel={() => setCreating(false)}
					/>
				) : (
					<div className="px-3">
						<Button variant="secondary" size="sm" onClick={() => setCreating(true)}>
							<UserPlus className="size-3.5" aria-hidden="true" />
							{t("settings.access.users.create")}
						</Button>
					</div>
				)
			) : null}
		</SettingsSection>
	);
}

function CreateUserForm({ onDone, onCancel }: { onDone: () => void; onCancel: () => void }) {
	const { t } = useTranslation();
	const roleLabel = useRoleLabel();
	const [displayName, setDisplayName] = useState("");
	const [email, setEmail] = useState("");
	const [password, setPassword] = useState("");
	const [role, setRole] = useState<AssignableRole>("member");
	const [error, setError] = useState<string | null>(null);

	const create = useMutation({
		mutationFn: async () => {
			const { error: apiError } = await apiClient.POST("/api/v1/users", {
				credentials: "include",
				body: { displayName, email, username: email, password, role },
			});
			if (apiError) throw new Error(apiErrorMessage(apiError));
		},
		onSuccess: onDone,
		onError: (err: Error) => setError(err.message),
	});

	return (
		<form
			className="mx-3 flex flex-col gap-2 rounded-(--radius-settings-dialog-lg) border border-[var(--color-border-settings-input)] bg-[var(--color-bg-settings-input)] p-3"
			onSubmit={(event) => {
				event.preventDefault();
				setError(null);
				create.mutate();
			}}
		>
			<Input
				value={displayName}
				onChange={(event) => setDisplayName(event.target.value)}
				aria-label={t("settings.access.users.displayName")}
				placeholder={t("settings.access.users.displayName")}
			/>
			<Input
				type="email"
				value={email}
				onChange={(event) => setEmail(event.target.value)}
				aria-label={t("settings.access.users.email")}
				placeholder={t("settings.access.users.email")}
			/>
			<Input
				type="password"
				value={password}
				onChange={(event) => setPassword(event.target.value)}
				aria-label={t("settings.access.users.password")}
				placeholder={t("settings.access.users.password")}
			/>
			<Select value={role} onValueChange={(next) => setRole(next as AssignableRole)}>
				<SelectTrigger aria-label={t("settings.access.users.role")}>
					<SelectValue />
				</SelectTrigger>
				<SelectContent>
					{assignableRoles.map((option) => (
						<SelectItem key={option} value={option}>
							{roleLabel(option)}
						</SelectItem>
					))}
				</SelectContent>
			</Select>
			{error ? <p className="text-caption text-error">{error}</p> : null}
			<div className="flex items-center gap-2">
				<Button type="submit" size="sm" disabled={create.isPending}>
					{t("settings.access.users.submit")}
				</Button>
				<Button type="button" variant="secondary" size="sm" onClick={onCancel}>
					{t("settings.access.cancel")}
				</Button>
			</div>
		</form>
	);
}

function TeamsBlock() {
	const { t } = useTranslation();
	const queryClient = useQueryClient();
	const canManage = useCan("teams.manage");
	const [name, setName] = useState("");
	const [error, setError] = useState<string | null>(null);
	const [expanded, setExpanded] = useState<string | null>(null);

	const query = useQuery({
		queryKey: teamsQueryKey,
		queryFn: async (): Promise<Team[]> => {
			const { data, error: apiError } = await apiClient.GET("/api/v1/teams", { credentials: "include" });
			if (apiError || !data) throw new Error(apiErrorMessage(apiError));
			return data.teams;
		},
	});

	const invalidate = () => {
		void queryClient.invalidateQueries({ queryKey: teamsQueryKey });
	};

	const create = useMutation({
		mutationFn: async () => {
			const { error: apiError } = await apiClient.POST("/api/v1/teams", {
				credentials: "include",
				body: { name, description: "" },
			});
			if (apiError) throw new Error(apiErrorMessage(apiError));
		},
		onSuccess: () => {
			setName("");
			setError(null);
			invalidate();
		},
		onError: (err: Error) => setError(err.message),
	});

	const remove = useMutation({
		mutationFn: async (id: string) => {
			const { error: apiError } = await apiClient.DELETE("/api/v1/teams/{id}", {
				credentials: "include",
				params: { path: { id } },
			});
			if (apiError) throw new Error(apiErrorMessage(apiError));
		},
		onSuccess: () => {
			setError(null);
			invalidate();
		},
		onError: (err: Error) => setError(err.message),
	});

	const teams = query.data ?? [];

	return (
		<SettingsSection title={t("settings.access.teams")} sectionId="access-teams">
			<p className="px-3 text-caption text-settings-muted">{t("settings.access.teams.description")}</p>

			{query.isLoading ? (
				<div className="flex items-center gap-2 px-3 py-2 text-caption text-settings-muted">
					<Loader2 className="size-3 animate-spin" aria-hidden="true" />
					{t("settings.access.teams.loading")}
				</div>
			) : teams.length === 0 ? (
				<p className="px-3 text-caption text-settings-muted">{t("settings.access.teams.empty")}</p>
			) : (
				<ul className="flex flex-col gap-1.5">
					{teams.map((team) => (
						<li
							key={team.id}
							className="flex flex-col gap-2 rounded-(--radius-settings-dialog-lg) border border-[var(--color-border-settings-input)] bg-[var(--color-bg-settings-input)] p-3"
							data-testid="access-team-row"
						>
							<div className="flex items-center gap-3">
								<UsersRound className="size-4 shrink-0 text-settings-muted" aria-hidden="true" />
								<div className="min-w-0 flex-1">
									<div className="truncate text-sm text-settings-label">{team.name}</div>
									<div className="truncate text-caption text-settings-muted">{team.slug}</div>
								</div>
								{team.status === "archived" ? (
									<Badge variant="neutral">{t("settings.access.teams.archived")}</Badge>
								) : null}
								<Button
									variant="secondary"
									size="sm"
									onClick={() => setExpanded(expanded === team.id ? null : team.id)}
								>
									{t("settings.access.teams.members")}
								</Button>
								{canManage ? (
									<Button
										variant="secondary"
										size="sm"
										disabled={remove.isPending}
										onClick={() => remove.mutate(team.id)}
									>
										{t("settings.access.teams.delete")}
									</Button>
								) : null}
							</div>
							{expanded === team.id ? <TeamMembers teamId={team.id} canManage={canManage} /> : null}
						</li>
					))}
				</ul>
			)}

			{error ? <p className="px-3 text-caption text-error">{error}</p> : null}

			{canManage ? (
				<form
					className="flex items-center gap-2 px-3"
					onSubmit={(event) => {
						event.preventDefault();
						setError(null);
						create.mutate();
					}}
				>
					<Input
						value={name}
						onChange={(event) => setName(event.target.value)}
						aria-label={t("settings.access.teams.name")}
						placeholder={t("settings.access.teams.name")}
					/>
					<Button type="submit" size="sm" disabled={create.isPending || name.trim() === ""}>
						{t("settings.access.teams.create")}
					</Button>
				</form>
			) : null}
		</SettingsSection>
	);
}

function TeamMembers({ teamId, canManage }: { teamId: string; canManage: boolean }) {
	const { t } = useTranslation();
	const queryClient = useQueryClient();
	const membersKey = ["rbac", "teams", teamId, "members"] as const;
	const [userId, setUserId] = useState("");
	const [error, setError] = useState<string | null>(null);

	const members = useQuery({
		queryKey: membersKey,
		queryFn: async (): Promise<TeamMember[]> => {
			const { data, error: apiError } = await apiClient.GET("/api/v1/teams/{id}/members", {
				credentials: "include",
				params: { path: { id: teamId } },
			});
			if (apiError || !data) throw new Error(apiErrorMessage(apiError));
			return data.members;
		},
	});

	const users = useQuery({
		queryKey: usersQueryKey,
		queryFn: async (): Promise<AdminUser[]> => {
			const { data, error: apiError } = await apiClient.GET("/api/v1/users", { credentials: "include" });
			if (apiError || !data) throw new Error(apiErrorMessage(apiError));
			return data.users;
		},
		// A team maintainer may not hold users.read; an empty account list then
		// simply means the picker has nothing to offer, not that the screen broke.
		retry: false,
	});

	const invalidate = () => {
		void queryClient.invalidateQueries({ queryKey: membersKey });
	};

	const add = useMutation({
		mutationFn: async () => {
			const { error: apiError } = await apiClient.POST("/api/v1/teams/{id}/members", {
				credentials: "include",
				params: { path: { id: teamId } },
				body: { userId, role: "member" },
			});
			if (apiError) throw new Error(apiErrorMessage(apiError));
		},
		onSuccess: () => {
			setUserId("");
			setError(null);
			invalidate();
		},
		onError: (err: Error) => setError(err.message),
	});

	const remove = useMutation({
		mutationFn: async (member: string) => {
			const { error: apiError } = await apiClient.DELETE("/api/v1/teams/{id}/members/{userId}", {
				credentials: "include",
				params: { path: { id: teamId, userId: member } },
			});
			if (apiError) throw new Error(apiErrorMessage(apiError));
		},
		onSuccess: () => {
			setError(null);
			invalidate();
		},
		onError: (err: Error) => setError(err.message),
	});

	const rows = members.data ?? [];
	const accounts = users.data ?? [];
	const nameOf = (id: string) => accounts.find((u) => u.id === id)?.displayName ?? id;
	const candidates = accounts.filter((account) => !rows.some((row) => row.userId === account.id));

	return (
		<div className="flex flex-col gap-2 border-t border-[var(--color-border-settings-input)] pt-2">
			{rows.length === 0 ? (
				<p className="text-caption text-settings-muted">{t("settings.access.teams.noMembers")}</p>
			) : (
				<ul className="flex flex-col gap-1">
					{rows.map((member) => (
						<li key={member.userId} className="flex items-center gap-2">
							<span className="min-w-0 flex-1 truncate text-caption text-settings-label">
								{nameOf(member.userId)}
							</span>
							{canManage ? (
								<Button
									variant="secondary"
									size="sm"
									disabled={remove.isPending}
									onClick={() => remove.mutate(member.userId)}
								>
									{t("settings.access.teams.remove")}
								</Button>
							) : null}
						</li>
					))}
				</ul>
			)}
			{error ? <p className="text-caption text-error">{error}</p> : null}
			{canManage && candidates.length > 0 ? (
				<div className="flex items-center gap-2">
					<Select value={userId} onValueChange={setUserId}>
						<SelectTrigger className="flex-1" aria-label={t("settings.access.teams.addMember")}>
							<SelectValue placeholder={t("settings.access.teams.addMember")} />
						</SelectTrigger>
						<SelectContent>
							{candidates.map((account) => (
								<SelectItem key={account.id} value={account.id}>
									{account.displayName}
								</SelectItem>
							))}
						</SelectContent>
					</Select>
					<Button size="sm" disabled={userId === "" || add.isPending} onClick={() => add.mutate()}>
						<ShieldCheck className="size-3.5" aria-hidden="true" />
						{t("settings.access.teams.addMember")}
					</Button>
				</div>
			) : null}
		</div>
	);
}
