# Single sign-on (OIDC) — P4-A

The as-built reference for AO's OIDC/SSO support. The 8P-E.8 design notes live
in [auth-sso-design.md](auth-sso-design.md); its final section records where
this implementation deliberately diverges and why.

P4-A answers exactly one question: **who authenticated?** What they may do is
P4-B's question, and nothing here decides it.

## The two auth modes

`AuthMode` is a closed set, not a pile of booleans:

| Mode | What a request with no session cookie resolves to | When |
| --- | --- | --- |
| `trusted_local` | the bootstrap admin, synthesized — no login screen, ever | the default, and every installation that configures no provider |
| `oidc` | nothing; owner-scoped endpoints answer 401 | set automatically the moment a provider is configured |

Configuring `AO_OIDC_ISSUER` + `AO_OIDC_CLIENT_ID` moves the whole
installation to `oidc` **and turns trusted-local synthesis off**, derived
rather than left to a second switch. There is no configuration in which AO
both demands SSO and hands an admin identity to anyone who omits a cookie. An
operator who wants SSO offered as an *option* on a trusted desktop sets
`AO_AUTH_MODE=trusted_local` explicitly; the provider stays usable and the
synthesized identity stays on.

Nothing about `trusted_local` changed in P4-A. An install that sets no
`AO_OIDC_*` variable is indistinguishable from one built before this
checkpoint: same routes, same behavior, same absence of a login screen.

## Configuration (backend-owned)

Read once at boot from the environment. No API route sets any of it, and no
response shape renders `AO_OIDC_CLIENT_SECRET`.

| Variable | Meaning |
| --- | --- |
| `AO_OIDC_ISSUER` | issuer identifier; must equal the `iss` in the discovery document and in every ID token. https, except a loopback issuer |
| `AO_OIDC_CLIENT_ID` | client id |
| `AO_OIDC_CLIENT_SECRET_FILE` | **preferred.** Path to a `0600` file holding the client secret. Keeps it out of the process environment — see "Where the client secret lives" |
| `AO_OIDC_CLIENT_SECRET` | client secret inline; omit for a public (PKCE-only) client. Legacy: readable with `ps eww`, and unset from the process as soon as it is read |
| `AO_OIDC_REDIRECT_URL` | callback registered with the provider (default `http://127.0.0.1:<port>/api/v1/auth/oidc/callback`) |
| `AO_OIDC_SCOPES` | requested scopes (default `openid profile email`; `openid` is prepended if omitted) |
| `AO_OIDC_DISPLAY_NAME` | the sign-in button's label |
| `AO_OIDC_ALLOWED_DOMAINS` | comma-separated email domains permitted to sign in |
| `AO_OIDC_REQUIRED_CLAIM` | `claim=value` a signing-in identity must carry (matches a string claim or an element of a string array) |
| `AO_OIDC_LINK_VERIFIED_EMAIL` | link a first federated login to an existing local account on a verified email match (default on) |
| `AO_AUTH_MODE` | `trusted_local` or `oidc`; overrides the derived default |
| `AO_SESSION_COOKIE_SAMESITE` | `lax` (default) or `none`; the desktop supervisor sets `none` |

A configuration that is half-set is rejected at boot rather than silently
disabling SSO: an issuer with no client id, `AO_AUTH_MODE=oidc` with no
provider, a non-loopback `http` issuer, and a malformed required claim are all
startup errors.

## Where the client secret lives

A process environment is not a private place. On macOS any process running as
the same user can read another's with `ps eww`, and AO spawns every agent,
reviewer, planner and verify subprocess with a copy of its own environment — so
a secret passed as `AO_OIDC_CLIENT_SECRET` was handed to every agent AO ran.

Point `AO_OIDC_CLIENT_SECRET_FILE` at a `0600` file instead:

```bash
umask 077
printf %s 'THE-SECRET' > ~/.ao/oidc-client-secret   # no trailing newline needed; one is trimmed
chmod 600 ~/.ao/oidc-client-secret
export AO_OIDC_CLIENT_SECRET_FILE=~/.ao/oidc-client-secret
```

AO refuses a secret file that is readable by group or other, refuses an empty
one, and refuses having both variables set at once — during a rotation you must
be able to say which secret is live.

**What this does and does not buy you.** Measured, not assumed:

| | secret in `AO_OIDC_CLIENT_SECRET` | secret in a `0600` file |
| --- | --- | --- |
| Visible in `ps eww` on the daemon | **yes, for the process's whole life** | no |
| Inherited by agent/reviewer/planner subprocesses | no (AO unsets it after reading) | no |
| Readable by another process running as the same user | yes | yes |

The middle row is the fix that also covers the legacy variable: AO unsets it
once it is read, and `os.Environ()` no longer carries it, so children do not
inherit it. The top row is why that is not sufficient on its own — macOS
snapshots a process's environment at `exec`, so unsetting afterwards does not
change what `ps` prints for the daemon itself. Only never putting it there does.

Neither option is a security boundary against a same-user attacker, and AO does
not claim one: a process running as the operator can read a `0600` file exactly
as it can read `~/.ao/data/cli-credentials.json`. What the file removes is
casual and incidental exposure — a process listing, a pasted bug report, a
screen share, and every agent subprocess's own environment.

macOS Keychain was evaluated and is deliberately **not** the default. Reading an
item at daemon start needs the keychain unlocked and the ACL to admit the
caller; a daemon launched non-interactively either blocks on a GUI prompt or
fails, which is the failure mode `internal/runtimehome` was already burned by
(see `keychain_darwin.go`). An operator who wants at-rest protection can keep
the secret in Keychain and have a login-time launcher materialize the `0600`
file, accepting that the file then exists for the daemon's lifetime.

### Rotating the client secret

Google (and every provider that allows more than one active secret) lets a new
secret exist alongside the old one, so rotation costs no downtime and needs no
change to the client ID, the issuer, the redirect URL, the allowed domains or
any authorized account. The existing sessions are AO's own cookies and are not
re-validated against the provider, so they survive.

1. In Google Cloud Console → **APIs & Services → Credentials**, open the OAuth
   2.0 Client ID AO uses (match it against `AO_OIDC_CLIENT_ID`; the client ID is
   not secret). Choose **Add secret**. Do not delete the old one yet.
2. Write the new secret straight into the file, without it passing through a
   shell history, a chat, or a shared terminal:

   ```bash
   umask 077
   pbpaste > ~/.ao/oidc-client-secret     # after copying it from the console
   chmod 600 ~/.ao/oidc-client-secret
   ```

   If the daemon still uses the inline variable, switch it to
   `AO_OIDC_CLIENT_SECRET_FILE` in the same step and remove the export.
3. Restart the daemon so it re-reads the configuration. A sign-in performed
   after the restart exercises the token endpoint with the new secret.
4. Only once a real sign-in has succeeded, **disable** the old secret in the
   console, then delete it. Disabling first is the rollback: if a sign-in fails,
   re-enable the old secret and put it back in the file.

**Rollback.** Keep the previous secret in the console until step 4 completes.
The rollback is re-enabling it and restoring the file — no schema, no session
and no account state is involved in a rotation.

**If a secret was exposed** (a process listing, a screen share, a transcript),
treat it as compromised and rotate it, then delete the old one rather than
leaving it disabled. A Google OAuth client secret does not by itself grant
access to any account: an attacker also needs a valid authorization code for
your redirect URL, which is loopback-only here. That lowers the urgency; it does
not remove the need to rotate.

## The flow

```
                 ┌──────────────────── browser deployment ───────────────────┐
 POST /auth/oidc/start ──▶ mint state+nonce+PKCE, persist an oidc_login_flows
                           row, return the provider authorization URL
        │
        ▼  the person signs in at the provider
 GET /auth/oidc/callback?code&state
        │  state → flow row (unknown / expired / already consumed ⇒ refused)
        │  exchange code + code_verifier ⇒ ID token
        │  verify signature (JWKS), iss, aud/azp, exp/iat/nbf, nonce
        │  enforce the operator's domain / claim constraints
        │  resolve (issuer, sub) ⇒ AO user
        └─▶ Set-Cookie: ao_session · 302 to a bounded in-app path
```

The desktop differs only in the last step:

```
                 ┌──────────────────── desktop (Electron) ───────────────────┐
 main process mints a HANDOFF SECRET, posts it with clientKind=desktop
        │                                (the secret never leaves loopback,
        ▼                                 and never goes to the provider)
 shell.openExternal(authorizationUrl) ──▶ the SYSTEM browser, never a webview
        │
        ▼
 GET /auth/oidc/callback  ──▶ every check above runs, the resolved user is
                              stamped on the flow, and the browser gets a
                              terminal page carrying nothing at all
        │
        ▼
 POST /auth/oidc/claim {flowId, handoffSecret}  (loopback, main process)
        └─▶ NOW the session is minted, returned as Set-Cookie on that
            response, written straight into the renderer's cookie jar by
            Electron's net module
```

No token, no authorization code and no session ever transits a renderer URL, a
query parameter, or a deep link. The session is minted at pickup, so the raw
token never rests in the database either — only its SHA-256, in
`auth_sessions`, exactly as the password flow already did.

**`ao auth login` is a second client of this same handoff.** The `ao` CLI mints
its own handoff secret, posts `clientKind=desktop`, opens the authorization URL
in the system browser, and polls `/auth/oidc/claim` over loopback until the
person has finished. It stores the `Set-Cookie` session at
`<AO_DATA_DIR>/cli-credentials.json` (mode 0600) and presents it as the
ordinary `ao_session` cookie on every daemon call. It is the same flow with the
same guarantees; the only difference is which loopback process holds the
secret. Without it, a cookie-less CLI on an SSO installation resolves no
principal and every permission-gated command answers 401 — see
[`docs/cli/README.md`](cli/README.md).

## Identity

The canonical external identity is **`issuer` + `sub`**, and nothing else.
Email is a snapshot of the last login's claim, kept for display and for the
operator's domain constraint. Per OIDC Core, `sub` is the only claim
guaranteed stable and unique within an issuer; email can change at the
provider and may be unverified.

First login for an unknown `(issuer, sub)`:

1. operator constraints are enforced **before** anything is created, so a
   refused identity leaves no account behind;
2. if the provider **verified** the email and it matches an existing account,
   and `AO_OIDC_LINK_VERIFIED_EMAIL` is on, the identity is linked to it;
3. if the email matches an existing account but will not be linked (not
   verified, or linking off), the login is refused — never a silent second
   account;
4. otherwise a password-less account is provisioned. Its `password_hash` is
   empty, which no password can ever match; `Authenticate` refuses it
   explicitly rather than relying on bcrypt erroring on a malformed hash.

The very first identity to sign in to an installation with no users at all
becomes its owner, guarded by the same `ux_users_single_owner` index
`Bootstrap` and `RegisterFirstUser` use; a lost race becomes a member rather
than a failed login.

## Principal

One canonical request identity, attached once by `httpd/identity.Middleware`:

```go
type Principal struct {
    User       User        // the durable AO account
    AuthMethod AuthMethod  // trusted_local | password | oidc
    SessionID  string      // empty for a synthesized trusted-local identity
    Issuer     string      // federated sessions only
    Subject    string      //   "
}
```

Controllers read `identity.RequirePrincipal(r)` (or `identity.Require(r)` when
the user alone suffices). **No handler parses an OIDC claim.** P4-B consumes
these three axes — identity, method, and the `role` 8P-E.8 already stored — to
answer "what may they do?".

## Security controls

- **State**: 256 bits of entropy, and the primary key of a durable
  `oidc_login_flows` row. Unknown, expired and replayed states are one answer
  on the wire ("start again"), so a prober learns nothing about which states
  have existed. Consumption is a SQL fact, so a replayed callback loses a race
  rather than minting a second session.
- **Nonce**: required. An ID token with no nonce, or a different one, is
  rejected — that binding is the only thing preventing a token obtained in one
  login from being replayed into another.
- **PKCE**: S256 only. `plain` is not offered.
- **Signature**: RS/PS/ES 256/384/512 against the provider's JWKS, HS* against
  the client secret. The two never mix: a symmetric key is never taken from a
  public JWKS (algorithm confusion), and `alg: none` is rejected before any
  key lookup. An unknown `kid` triggers exactly one JWKS refetch, so a key
  rotation costs a round trip rather than an outage.
- **Issuer**: checked twice — the discovery document's own `issuer` must equal
  the configured one (otherwise a redirected discovery URL silently
  substitutes a whole identity provider), and so must every ID token's `iss`.
- **Audience**: `aud` must contain the client id; with multiple audiences
  `azp` must additionally be this client, or a token minted for another
  relying party at the same issuer would be accepted.
- **Freshness**: `exp`, `nbf` and a `iat` replay bound, each with a 2-minute
  clock skew.
- **Redirect**: the post-login destination is validated to a same-origin
  absolute path at Start and stored only in validated form, so the callback
  has no attacker-supplied redirect left to trust. Anything else becomes `/`.
- **Cookie**: `HttpOnly` always — the token is never a JavaScript value, in
  any deployment, so it is out of reach of XSS and never in `localStorage`.
  `SameSite=Lax` by default; the desktop sets `none` (with `Secure`, honored
  on a loopback origin) because its renderer (`app://`) and the daemon
  (`http://127.0.0.1`) are different origins and a Lax cookie would never be
  sent between them.

## Logout

`POST /api/v1/auth/logout` invalidates the **AO** session. When the session
was federated and the provider advertises an `end_session_endpoint`, the
response additionally carries `providerEndSessionUrl` — an **offer**, not a
claim. AO never states that it ended a session at the provider, because it
cannot.

## Audit

Emitted to the daemon log and, when a telemetry sink is wired, to it:
`ao.auth.login.succeeded`, `ao.auth.login.failed`, `ao.auth.logout`,
`ao.auth.session.expired`. Each carries the AO user id, the auth method, the
issuer, the email **domain**, a stable outcome code, and the caller's source
address.

Never logged, anywhere on this path: access tokens, refresh tokens, client
secrets, authorization codes, raw ID tokens. The federated `sub` is not logged
either — the AO user id already identifies the actor.

## Tests

Everything runs offline against `internal/oidc/oidctest`, a deterministic
in-process OpenID Provider with real RS256 signatures, a real JWKS, a real
PKCE check and a real code exchange. Every deviation a test needs (wrong
issuer, foreign audience, expired/unsigned token, mismatched nonce, declined
authorization, a provider that only releases email from userinfo) is a field
on it, so each test states the one thing it varies. No network, no container,
no vendor account, no credentials.
