import { type FormEvent, useEffect, useState } from "react";
import { useTranslation } from "react-i18next";
import { Button } from "./ui/button";
import { Input } from "./ui/input";
import { Label } from "./ui/label";
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from "./ui/card";
import { useAuthStore } from "../stores/auth-store";

// Checkpoint 8P-A: rendered by __root.tsx/_shell.tsx in place of the normal
// shell only when auth-store's status is "unauthenticated" (AO_TRUSTED_LOCAL_MODE
// off, no valid session). It must never appear in "trusted-local" status —
// that is today's zero-friction desktop default and stays completely
// unchanged.
export function LoginScreen() {
	const { t } = useTranslation();
	const login = useAuthStore((state) => state.login);
	const error = useAuthStore((state) => state.error);
	// P4-A: the SSO button renders only when the backend says a provider is
	// configured. The renderer never learns the issuer, the client id, or any
	// constraint — only a label and that a provider exists.
	const providers = useAuthStore((state) => state.providers);
	const loadProviders = useAuthStore((state) => state.loadProviders);
	const startSso = useAuthStore((state) => state.startSso);
	const ssoPending = useAuthStore((state) => state.ssoPending);
	const [usernameOrEmail, setUsernameOrEmail] = useState("");
	const [password, setPassword] = useState("");
	const [submitting, setSubmitting] = useState(false);

	useEffect(() => {
		void loadProviders();
	}, [loadProviders]);

	const oidc = providers?.oidc;

	const handleSubmit = async (event: FormEvent<HTMLFormElement>) => {
		event.preventDefault();
		if (submitting) return;
		setSubmitting(true);
		try {
			await login(usernameOrEmail, password);
		} finally {
			setSubmitting(false);
		}
	};

	return (
		<div className="flex h-screen w-screen items-center justify-center bg-background">
			<Card className="w-full max-w-sm">
				<CardHeader>
					<CardTitle>{t("auth.login.title")}</CardTitle>
					<CardDescription>{t("auth.login.description")}</CardDescription>
				</CardHeader>
				<CardContent>
					{oidc ? (
						<div className="mb-4 flex flex-col gap-4">
							<Button
								type="button"
								variant="primary"
								className="w-full"
								disabled={ssoPending}
								onClick={() => void startSso()}
							>
								{ssoPending
									? t("auth.login.ssoSubmitting", { provider: oidc.displayName })
									: t("auth.login.ssoSubmit", { provider: oidc.displayName })}
							</Button>
							{ssoPending ? (
								<p className="text-center text-sm text-muted-foreground">{t("auth.login.ssoWaiting")}</p>
							) : null}
							<div className="flex items-center gap-3">
								<span className="h-px flex-1 bg-border" />
								<span className="text-xs uppercase text-muted-foreground">{t("auth.login.ssoDivider")}</span>
								<span className="h-px flex-1 bg-border" />
							</div>
						</div>
					) : null}
					<form className="flex flex-col gap-4" onSubmit={handleSubmit}>
						<div className="flex flex-col gap-1.5">
							<Label htmlFor="auth-username">{t("auth.login.usernameOrEmailLabel")}</Label>
							<Input
								id="auth-username"
								autoComplete="username"
								autoFocus
								value={usernameOrEmail}
								onChange={(event) => setUsernameOrEmail(event.target.value)}
								required
							/>
						</div>
						<div className="flex flex-col gap-1.5">
							<Label htmlFor="auth-password">{t("auth.login.passwordLabel")}</Label>
							<Input
								id="auth-password"
								type="password"
								autoComplete="current-password"
								value={password}
								onChange={(event) => setPassword(event.target.value)}
								required
							/>
						</div>
						{error ? (
							<p role="alert" className="text-sm text-destructive">
								{error}
							</p>
						) : null}
						<Button type="submit" variant="primary" disabled={submitting || !usernameOrEmail || !password}>
							{submitting ? t("auth.login.submitting") : t("auth.login.submit")}
						</Button>
					</form>
				</CardContent>
			</Card>
		</div>
	);
}
