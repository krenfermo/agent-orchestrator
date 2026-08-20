import { fireEvent, render, screen, waitFor } from "@testing-library/react";
import { afterEach, expect, test, vi } from "vitest";
import { SignupScreen } from "./SignupScreen";
import { useAuthStore } from "../stores/auth-store";

afterEach(() => {
	useAuthStore.setState({ user: null, status: "unauthenticated", error: null, setupRequired: true });
});

test("submits displayName/email/password to register()", async () => {
	const register = vi.fn().mockResolvedValue(true);
	useAuthStore.setState({ register });

	render(<SignupScreen />);

	fireEvent.change(screen.getByLabelText("Display name"), { target: { value: "Ada" } });
	fireEvent.change(screen.getByLabelText("Email"), { target: { value: "ada@example.com" } });
	fireEvent.change(screen.getByLabelText("Password"), { target: { value: "supersecret1" } });
	fireEvent.change(screen.getByLabelText("Confirm password"), { target: { value: "supersecret1" } });
	fireEvent.click(screen.getByRole("button", { name: "Create account" }));

	await waitFor(() => {
		expect(register).toHaveBeenCalledWith("Ada", "ada@example.com", "supersecret1");
	});
});

test("blocks submission and shows an error when passwords do not match", () => {
	const register = vi.fn();
	useAuthStore.setState({ register });

	render(<SignupScreen />);

	fireEvent.change(screen.getByLabelText("Display name"), { target: { value: "Ada" } });
	fireEvent.change(screen.getByLabelText("Email"), { target: { value: "ada@example.com" } });
	fireEvent.change(screen.getByLabelText("Password"), { target: { value: "supersecret1" } });
	fireEvent.change(screen.getByLabelText("Confirm password"), { target: { value: "different1" } });
	fireEvent.click(screen.getByRole("button", { name: "Create account" }));

	expect(screen.getByRole("alert")).toHaveTextContent("Passwords do not match.");
	expect(register).not.toHaveBeenCalled();
});
