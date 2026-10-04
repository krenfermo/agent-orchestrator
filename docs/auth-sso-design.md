# AO-identity SSO design (GitHub / Google / Apple)

Status: **superseded in part by P4-A, which shipped.** This page remains the
record of the 8P-E.8 design thinking, and most of it held. Where the shipped
implementation deliberately differs, see
[**sso-oidc.md**](sso-oidc.md) — the as-built reference — and the
"What P4-A changed about this design" section at the end of this file, which
names each divergence and why.

The original status line read: design only — Checkpoint 8P-E.8. No provider
code ships in this checkpoint. This documents the architecture the
local-password foundation (users/auth_sessions/role, `authsvc`,
`/api/v1/auth/*`) is meant to grow into, and the deployment question that has
to be answered before any provider is wired up for real.

## Why design-only, this checkpoint

Wiring three real OAuth integrations means real client IDs/secrets, a
callback story that works identically in a packaged Electron app on three
OSes and in a self-hosted web deployment, and a decision about whether AO
depends on a hosted identity broker. Getting any of that wrong is expensive
to unwind (linked accounts, leaked tokens, a public repo with baked-in
credentials). The local-password path (`RegisterFirstUser`, `authsvc`,
`ux_users_single_owner`) is a complete, secure product on its own and doesn't
need to wait on these decisions — see `AGENTS.md`/checkpoint 8P-E.8 for that
half.

## Provider abstraction

One narrow interface, three implementations:

```go
type OAuthProvider interface {
    Name() string // "github" | "google" | "apple"
    AuthorizationURL(state, codeChallenge string) string
    Exchange(ctx context.Context, code, codeVerifier string) (ExternalIdentity, error)
}

type ExternalIdentity struct {
    ProviderSubject string // stable id from the provider, never email
    Email           string
    DisplayName     string
}
```

`authsvc` gains one more entry point built on top of the existing
`CreateUser`/session machinery:

```go
// ResolveOrLinkExternalIdentity: first-run -> creates the owner (same
// ux_users_single_owner guard RegisterFirstUser uses); post-setup with no
// linked identity and no admin-enabled auto-provisioning -> rejected, not
// silently provisioned.
ResolveOrLinkExternalIdentity(ctx, provider string, ident ExternalIdentity) (domain.User, error)
```

## Schema: `user_external_identities`

```sql
CREATE TABLE user_external_identities (
    id               TEXT PRIMARY KEY,
    user_id          TEXT NOT NULL REFERENCES users(id),
    provider         TEXT NOT NULL CHECK (provider IN ('github','google','apple')),
    provider_subject TEXT NOT NULL,
    email            TEXT,
    created_at       TIMESTAMP NOT NULL,
    updated_at       TIMESTAMP NOT NULL
);
CREATE UNIQUE INDEX ux_external_identity_provider_subject
    ON user_external_identities (provider, provider_subject);
```

Keyed on `(provider, provider_subject)` — the provider's stable account id —
**never** on email. Two identities that happen to share an email must not
silently merge into one AO user (account-takeover vector: someone controls
`bob@example.com` on Google but not GitHub). Linking a second provider to an
existing AO user is an explicit action taken from Settings → Account while
already signed in, not an automatic side effect of a matching email at
login time.

No provider access/refresh token is persisted by this table. If a future
feature needs continued API access to the provider (not just "who is this
person"), that's a separate, explicitly-scoped credential store — the
existing per-project SCM credentials (`medusa -> HTTPS/krenfermo`) are the
model for that, not this table.

## AO identity vs. source-control credentials — stay independent

"Sign in with GitHub" answers "who is this person" for AO's own session.
It must never become the credential AO uses to push/pull a project's git
remote. Those live in the existing SCM-adapter credential system
(`backend/internal/adapters/scm/github`, per-project), keyed by project, not
by AO user. A user can be signed into AO via GitHub OAuth and still use a
completely different GitHub identity (or SSH key, or GitLab) for a given
project's remote — the two systems share no code path and no token. This is
the same distinction the checkpoint brief draws between "Joaquin signed in
through GitHub" and "backend_node -> SSH -> github-nuevo".

## Desktop callback strategy (Electron)

Reuse the existing pattern from `frontend/src/main/cloud-auth.ts` (built for
a different flow — the WorkOS-based "AO Cloud account" — but the plumbing is
exactly right):

- Authorization Code + PKCE (`getAuthorizationUrlWithPKCE`-equivalent):
  generate `codeVerifier`/`codeChallenge` and a random `state` before
  opening the browser; store them (in-memory + short TTL, see
  `PKCE_TTL_MS`) keyed by `state`.
- Never embed the provider's login page in an Electron `BrowserWindow`.
  Open the system browser (`shell.openExternal`).
- Redirect URI is a custom protocol, `ao-app://callback` — already
  registered in `frontend/src/main.ts` (`registerCloudProtocol`,
  `app.on("open-url", ...)` for macOS, the `second-instance` argv-scan for
  Windows/Linux since those platforms redirect a second launch into the
  running instance rather than firing `open-url`). AO-identity OAuth reuses
  this exact registration rather than adding a second custom scheme.
- On callback: verify `state` matches the pending PKCE record, exchange
  `code` + `codeVerifier` for the identity, then call the AO backend to
  resolve/link the identity and mint a normal `ao_session` cookie — the
  desktop app never sees or stores a provider access token itself; the
  token exchange with the AO backend happens over the same loopback API
  every other CLI/Electron call uses.
- No localhost-callback-server assumption (`http://127.0.0.1:<port>/callback`
  patterns break in some packaged-app sandboxes and are unnecessary here
  since the custom protocol is already proven working for cloud-auth).

## Web / self-hosted callback strategy

A standard server-side OAuth callback: `GET /api/v1/auth/oauth/{provider}/callback`,
using a configurable public base URL (`AO_PUBLIC_BASE_URL` or similar — never
a hardcoded domain) to build the redirect URI registered with each provider.
Same state/PKCE verification, same `authsvc.ResolveOrLinkExternalIdentity`
call, same `ao_session` cookie issuance — Electron and web converge on the
identical backend code path once the browser hands back an authorization
code, which is the point: the desktop/web split is only in how the browser
gets opened and how the callback is received, not in how the identity is
resolved.

## Self-hosting: who owns the OAuth client ID/secret?

This is the open architectural question, and it has three distinct answers
depending on deployment:

1. **Official AO desktop/web builds.** If Anthropic/the AO project ships a
   build with working "Continue with GitHub" out of the box, that build
   needs its own registered OAuth app per provider. Those client
   IDs/secrets must **not** be committed to this public repository — they'd
   be injected at build time (CI secret, or a build-time config service),
   the same way any public open-source project with a hosted SaaS
   companion keeps its production credentials out of the tree.

2. **Self-hosted / community builds.** A self-hoster who runs their own AO
   instance (e.g. on Hetzner) registers their *own* OAuth app with each
   provider they want to enable, and supplies the client ID/secret via env
   vars (`AO_OAUTH_GITHUB_CLIENT_ID`/`_SECRET`, etc.) — the same shape as
   `AO_BOOTSTRAP_ADMIN_EMAIL`/`_PASSWORD` today. No SSO provider is enabled
   unless its env vars are present; local password auth keeps working
   regardless. This is the only model that doesn't require AO itself to
   run any shared infrastructure.

3. **A hosted identity broker (WorkOS/Auth0/Clerk/etc.), evaluated and
   rejected for now.** A broker would let every AO installation (including
   self-hosted ones) get zero-config "Continue with Google" without each
   operator registering their own OAuth app. The tradeoff: it introduces a
   mandatory third-party dependency and a centralized point that every
   self-hosted, allegedly-independent AO installation would silently rely
   on — in tension with AO's stated model of "one AO installation, no
   SaaS multi-tenant coupling" (see the 8P-E.8 checkpoint's own framing).
   `cloud-auth.ts`'s existing WorkOS integration is scoped to the optional
   "AO Cloud account" feature, not general AO login, and that scoping
   should stay intentional rather than expand by default. **Recommendation:
   do not adopt a hosted broker for AO-identity SSO.** Ship the
   env-var-supplied-credentials model (option 2) and let official builds
   layer their own build-time credentials (option 1) on top of the same
   code path. Revisit only if self-hosters report the per-provider OAuth
   app registration step is a real adoption blocker.

## What's already true, unaffected by any of the above

Local password auth (`RegisterFirstUser`, `ResetPassword`, `role`,
`ux_users_single_owner`) needs none of this. It is the default, always-
available path regardless of which OAuth providers (if any) an installation
later turns on.

## What P4-A changed about this design

P4-A shipped SSO as **standards-based OIDC against one operator-configured
provider**, not as three named vendor integrations. Three concrete
divergences, each deliberate:

**1. The identity key is `(issuer, subject)`, not `(provider, subject)`.**
The table is `external_identities` (migration 0151), not
`user_external_identities`, and it has no `provider` column with a CHECK
listing github/google/apple. The reason is the P4-A brief's own constraint —
"do not hardcode one identity vendor" — and the fact that a `provider` enum
cannot name a self-hosted Keycloak, Dex, Authentik, or any corporate IdP. The
`iss` claim already is the vendor-neutral name for exactly this, and OIDC
guarantees `sub` is unique within it. Everything the original design said
about **never keying on email** carries over unchanged and is enforced by
`ux_external_identities_issuer_subject`.

**2. A first federated login DOES link to a matching local account, when the
provider verified the email — and it is a toggle.** The original design said
linking must be an explicit action from Settings while already signed in. That
reasoning is sound for a MULTI-provider deployment (control `bob@example.com`
at Google but not at GitHub, and automatic linking is a takeover). P4-A
configures exactly one issuer, chosen by the operator, and ships no
account-linking UI — so refusing to link would strand every account an
installation already has, with no remedy, the moment SSO is switched on. The
shipped rule is: link only on `email_verified`, and only when
`AO_OIDC_LINK_VERIFIED_EMAIL` is on (the default). An operator running an
issuer they do not fully trust to verify addresses turns it off and gets the
original design's behavior — a refusal, with a distinct
`SSO_LINKING_DISABLED` code. An UNVERIFIED email never links, in either
setting.

**3. The desktop callback is the daemon's own loopback route, not
`ao-app://callback`.** The original design chose the custom protocol to avoid
"a localhost callback server", and warned that packaged-app sandboxes break
such patterns. That warning is about an app spawning its *own* callback
listener. AO does not have to: the daemon is already bound on loopback and is
already the app's entire API surface — if it were unreachable the app would
not run at all. Using it means Electron and web converge on **one** backend
code path for the whole exchange, which is what this document itself called
"the point"; and it keeps the authorization code out of a deep link entirely.
The desktop session still never crosses into a renderer URL: the callback
records who signed in but issues nothing, and the supervisor redeems it over
loopback with a handoff secret that never left the machine. See
[sso-oidc.md](sso-oidc.md) for the full sequence.

**Unchanged and still true:** AO identity stays completely independent of
source-control credentials; no provider access or refresh token is persisted;
local password auth remains the always-available default; and the
self-hosting answer is option 2 — the operator supplies the client
credentials via environment variables, and AO depends on no hosted identity
broker.
