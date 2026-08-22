import { render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { beforeEach, describe, expect, it, vi } from "vitest";
import { EmailNotificationsSettingsSection } from "./EmailNotificationsSettingsSection";
import type { EmailNotificationSettings } from "../../hooks/useEmailNotificationSettings";

const { useEmailNotificationSettingsMock } = vi.hoisted(() => ({
	useEmailNotificationSettingsMock: vi.fn(),
}));

vi.mock("../../hooks/useEmailNotificationSettings", () => ({
	useEmailNotificationSettings: useEmailNotificationSettingsMock,
}));

const STORED_PASSWORD_MARKER = "gmail-app-password";

function settings(overrides: Partial<EmailNotificationSettings> = {}): EmailNotificationSettings {
	return {
		enabled: true,
		recipient: "someone@example.com",
		host: "smtp.gmail.com",
		port: 587,
		username: "someone@gmail.com",
		tls: "starttls",
		passwordSet: true,
		...overrides,
	} as EmailNotificationSettings;
}

describe("EmailNotificationsSettingsSection", () => {
	const save = vi.fn();
	const sendTest = vi.fn();

	function mockHook(overrides: Record<string, unknown> = {}) {
		useEmailNotificationSettingsMock.mockReturnValue({
			settings: settings(),
			isLoading: false,
			error: undefined,
			save,
			isSaving: false,
			sendTest,
			isSendingTest: false,
			...overrides,
		});
	}

	beforeEach(() => {
		save.mockReset().mockResolvedValue(settings());
		sendTest.mockReset().mockResolvedValue({ sent: true, recipient: "someone@example.com" });
		useEmailNotificationSettingsMock.mockReset();
		mockHook();
	});

	it("renders the stored destination", () => {
		render(<EmailNotificationsSettingsSection />);
		expect(screen.getByLabelText("Send to")).toHaveValue("someone@example.com");
		expect(screen.getByLabelText("SMTP server")).toHaveValue("smtp.gmail.com");
		expect(screen.getByLabelText("Port")).toHaveValue("587");
		expect(screen.getByLabelText("Username")).toHaveValue("someone@gmail.com");
	});

	// The server never sends the stored password back, so the field starts
	// empty and only says that one exists.
	it("never prefills the password field", () => {
		render(<EmailNotificationsSettingsSection />);
		const password = screen.getByLabelText("Password");
		expect(password).toHaveValue("");
		expect(password).toHaveAttribute("type", "password");
		expect(password).toHaveAttribute("placeholder", "A password is saved");
		expect(document.body.textContent).not.toContain(STORED_PASSWORD_MARKER);
	});

	it("says when no password is stored", () => {
		mockHook({ settings: settings({ passwordSet: false }) });
		render(<EmailNotificationsSettingsSection />);
		expect(screen.getByLabelText("Password")).toHaveAttribute("placeholder", "No password saved");
	});

	// The decisive one: saving an untouched form must not erase the stored
	// credential, which is exactly what sending an empty password would do.
	it("omits the password when the user did not type one", async () => {
		const user = userEvent.setup();
		render(<EmailNotificationsSettingsSection />);

		await user.clear(screen.getByLabelText("Send to"));
		await user.type(screen.getByLabelText("Send to"), "other@example.com");
		await user.click(screen.getByRole("button", { name: "Save" }));

		await waitFor(() => expect(save).toHaveBeenCalledTimes(1));
		const sent = save.mock.calls[0][0];
		expect(sent).not.toHaveProperty("password");
		expect(sent.recipient).toBe("other@example.com");
	});

	it("sends a newly typed password", async () => {
		const user = userEvent.setup();
		render(<EmailNotificationsSettingsSection />);

		await user.type(screen.getByLabelText("Password"), STORED_PASSWORD_MARKER);
		await user.click(screen.getByRole("button", { name: "Save" }));

		await waitFor(() => expect(save).toHaveBeenCalledTimes(1));
		expect(save.mock.calls[0][0].password).toBe(STORED_PASSWORD_MARKER);
	});

	it("clears the typed password from the form once it is saved", async () => {
		const user = userEvent.setup();
		render(<EmailNotificationsSettingsSection />);

		await user.type(screen.getByLabelText("Password"), STORED_PASSWORD_MARKER);
		await user.click(screen.getByRole("button", { name: "Save" }));

		await waitFor(() => expect(screen.getByLabelText("Password")).toHaveValue(""));
	});

	it("toggles the feature on and off", async () => {
		const user = userEvent.setup();
		render(<EmailNotificationsSettingsSection />);

		await user.click(screen.getByRole("switch", { name: "Send completion emails" }));
		await user.click(screen.getByRole("button", { name: "Save" }));

		await waitFor(() => expect(save).toHaveBeenCalledTimes(1));
		expect(save.mock.calls[0][0].enabled).toBe(false);
	});

	// Saving first means the test exercises the settings the user is looking
	// at, not whatever was stored before they started editing.
	it("saves the form before sending a test email", async () => {
		const user = userEvent.setup();
		render(<EmailNotificationsSettingsSection />);

		await user.click(screen.getByRole("button", { name: "Send test email" }));

		await waitFor(() => expect(sendTest).toHaveBeenCalledTimes(1));
		expect(save).toHaveBeenCalledTimes(1);
		expect(save.mock.invocationCallOrder[0]).toBeLessThan(sendTest.mock.invocationCallOrder[0]);
		expect(await screen.findByRole("status")).toHaveTextContent("someone@example.com");
	});

	// The whole point of a test button is that the real reason reaches the user.
	it("surfaces why a test email failed", async () => {
		sendTest.mockRejectedValue(new Error("535 5.7.8 Username and Password not accepted"));
		const user = userEvent.setup();
		render(<EmailNotificationsSettingsSection />);

		await user.click(screen.getByRole("button", { name: "Send test email" }));

		expect(await screen.findByRole("alert")).toHaveTextContent("535 5.7.8");
	});

	it("surfaces a save failure", async () => {
		save.mockRejectedValue(new Error("recipient is required"));
		const user = userEvent.setup();
		render(<EmailNotificationsSettingsSection />);

		await user.click(screen.getByRole("button", { name: "Save" }));

		expect(await screen.findByRole("alert")).toHaveTextContent("recipient is required");
	});
});
