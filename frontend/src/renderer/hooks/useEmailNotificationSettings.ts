import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import type { components } from "../../api/schema";
import { apiClient, apiErrorMessage, hasTrustedApiBaseUrl } from "../lib/api-client";

export type EmailNotificationSettings = components["schemas"]["ControllersEmailNotificationSettingsResponse"];
export type EmailNotificationTLS = EmailNotificationSettings["tls"];

/**
 * The shape the form saves. `password` is deliberately optional: the server
 * never sends the stored password back, so the form has nothing to echo, and
 * omitting the field is what tells the server to keep what it already has.
 * Sending `""` is the explicit clear.
 */
export type EmailNotificationSettingsInput = {
	enabled: boolean;
	recipient: string;
	host: string;
	port: number;
	username: string;
	tls: EmailNotificationTLS;
	password?: string;
};

export const emailNotificationSettingsQueryKey = ["settings", "email-notifications"] as const;

async function fetchEmailNotificationSettings(): Promise<EmailNotificationSettings> {
	const { data, error } = await apiClient.GET("/api/v1/settings/email-notifications");
	if (error) throw new Error(apiErrorMessage(error));
	return data.emailNotifications;
}

/**
 * Settings → Completion emails. Daemon-owned rather than renderer-held: the
 * sender runs in the daemon and a task can finish while no window is open, so
 * a preference kept here would simply never be consulted.
 */
export function useEmailNotificationSettings() {
	const queryClient = useQueryClient();

	const settings = useQuery({
		queryKey: emailNotificationSettingsQueryKey,
		enabled: hasTrustedApiBaseUrl(),
		queryFn: fetchEmailNotificationSettings,
		staleTime: 30 * 1000,
	});

	const save = useMutation({
		mutationFn: async (input: EmailNotificationSettingsInput) => {
			const { data, error } = await apiClient.PATCH("/api/v1/settings/email-notifications", { body: input });
			if (error) throw new Error(apiErrorMessage(error));
			return data.emailNotifications;
		},
		onSuccess: (saved) => queryClient.setQueryData(emailNotificationSettingsQueryKey, saved),
	});

	const sendTest = useMutation({
		mutationFn: async () => {
			const { data, error } = await apiClient.POST("/api/v1/settings/email-notifications/test", {});
			if (error) throw new Error(apiErrorMessage(error));
			return data;
		},
	});

	return {
		settings: settings.data,
		isLoading: settings.isLoading,
		error: settings.error ? apiErrorMessage(settings.error) : undefined,
		save: save.mutateAsync,
		isSaving: save.isPending,
		sendTest: sendTest.mutateAsync,
		isSendingTest: sendTest.isPending,
	};
}
