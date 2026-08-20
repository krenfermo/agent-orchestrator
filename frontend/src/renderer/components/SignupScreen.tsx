import { type FormEvent, useState } from "react";
import { useTranslation } from "react-i18next";
import { Button } from "./ui/button";
import { Input } from "./ui/input";
import { Label } from "./ui/label";
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from "./ui/card";
import { useAuthStore } from "../stores/auth-store";

// Checkpoint 8P-E.8: rendered by _shell.tsx in place of LoginScreen when
// auth-store's status is "unauthenticated" AND setupRequired is true — i.e.
// this installation has zero users yet. Only ever reachable while that
// holds; the backend independently rejects a second registration once an
// owner exists (see authsvc.RegisterFirstUser), so this screen is not the
// only guard.
export function SignupScreen() {
	const { t } = useTranslation();
	const register = useAuthStore((state) => state.register);
	const error = useAuthStore((state) => state.error);
	const [displayName, setDisplayName] = useState("");
	const [email, setEmail] = useState("");
	const [password, setPassword] = useState("");
	const [confirmPassword, setConfirmPassword] = useState("");
	const [mismatch, setMismatch] = useState(false);
	const [submitting, setSubmitting] = useState(false);

	const handleSubmit = async (event: FormEvent<HTMLFormElement>) => {
		event.preventDefault();
		if (submitting) return;
		if (password !== confirmPassword) {
			setMismatch(true);
			return;
		}
		setMismatch(false);
		setSubmitting(true);
		try {
			await register(displayName, email, password);
		} finally {
			setSubmitting(false);
		}
	};

	return (
		<div className="flex h-screen w-screen items-center justify-center bg-background">
			<Card className="w-full max-w-sm">
				<CardHeader>
					<CardTitle>{t("auth.signup.title")}</CardTitle>
					<CardDescription>{t("auth.signup.description")}</CardDescription>
				</CardHeader>
				<CardContent>
					<form className="flex flex-col gap-4" onSubmit={handleSubmit}>
						<div className="flex flex-col gap-1.5">
							<Label htmlFor="signup-display-name">{t("auth.signup.displayNameLabel")}</Label>
							<Input
								id="signup-display-name"
								autoComplete="name"
								autoFocus
								value={displayName}
								onChange={(event) => setDisplayName(event.target.value)}
								required
							/>
						</div>
						<div className="flex flex-col gap-1.5">
							<Label htmlFor="signup-email">{t("auth.signup.emailLabel")}</Label>
							<Input
								id="signup-email"
								type="email"
								autoComplete="email"
								value={email}
								onChange={(event) => setEmail(event.target.value)}
								required
							/>
						</div>
						<div className="flex flex-col gap-1.5">
							<Label htmlFor="signup-password">{t("auth.signup.passwordLabel")}</Label>
							<Input
								id="signup-password"
								type="password"
								autoComplete="new-password"
								value={password}
								onChange={(event) => setPassword(event.target.value)}
								required
							/>
						</div>
						<div className="flex flex-col gap-1.5">
							<Label htmlFor="signup-confirm-password">{t("auth.signup.confirmPasswordLabel")}</Label>
							<Input
								id="signup-confirm-password"
								type="password"
								autoComplete="new-password"
								value={confirmPassword}
								onChange={(event) => setConfirmPassword(event.target.value)}
								required
							/>
						</div>
						{mismatch ? (
							<p role="alert" className="text-sm text-destructive">
								{t("auth.signup.passwordMismatch")}
							</p>
						) : null}
						{error ? (
							<p role="alert" className="text-sm text-destructive">
								{error}
							</p>
						) : null}
						<Button
							type="submit"
							variant="primary"
							disabled={submitting || !displayName || !email || !password || !confirmPassword}
						>
							{submitting ? t("auth.signup.submitting") : t("auth.signup.submit")}
						</Button>
					</form>
				</CardContent>
			</Card>
		</div>
	);
}
