# macOS provider credentials: the unattended auth contract

Status: implemented
Scope: every provider CLI launch AO makes (planner, worker, reviewer, repair,
decision resolver) on macOS

## The incident (wf-4e3d187b, 2026-09-06)

A real workflow stopped at `planner_auth_unavailable`. While it was failing,
macOS displayed:

> `security` quiere usar el llavero "Inicio de sesión"
> (`security` wants to use the keychain "login")

The same `claude --print` command run by hand from a terminal worked every time.
Resetting the login keychain, re-authenticating Claude, and granting
`/usr/bin/security` and the Claude binary access to the keychain all changed
nothing.

AO's own durable evidence held the first real clue:

```
planner: provider credentials are unavailable: provider claude
(/Users/…/.local/bin/claude) exited 1;
CLAUDE_CONFIG_DIR=/Users/…/.ao/data/users/<uid>/providers/claude-code;
reason: … "Failed to authenticate. API Error: 401 OAuth access token has
expired. Re-authenticate to continue."
```

The planner was not running against the person's home directory at all.

## Root cause

Three defects stacked. Each is individually survivable; together they produce an
unattended process blocked on a dialog nobody can answer.

### 1. `login` is a reserved keychain name, and AO used it

`runtimehome` provisions a per-user runtime home for provider subprocesses and,
on macOS, a keychain inside it — because Claude Code stores its OAuth credential
in the OS keychain, and macOS resolves the default keychain and the user search
list from `$HOME`, not from `CLAUDE_CONFIG_DIR`. AO named that keychain
`login.keychain-db`.

macOS reserves that name. Verified directly, on Darwin 25.6:

```
security create-keychain -p PW …/login.keychain-db        -> ok
security unlock-keychain -p PW …/login.keychain-db        -> "The user name or
                                                              passphrase you
                                                              entered is not
                                                              correct."

security create-keychain -p PW …/ao-provider.keychain-db  -> ok
security unlock-keychain -p PW …/ao-provider.keychain-db  -> ok
```

So AO could create its own keychain and could never reopen it.

The bug stayed invisible for weeks because `create-keychain` leaves the new
keychain **unlocked for the rest of the securityd session**. Writes and reads
worked normally until the first sleep, logout or reboot. After that the keychain
was permanently unopenable, and any provider process reaching for a credential
in it raised the OS unlock dialog — titled with the keychain's *file* name,
`login`, which is exactly why it looked like the user's real login keychain and
why their login password never worked. The password was a random secret in
`~/.ao/data/users/<uid>/.keychain-secret`.

`ensureIsolatedKeychain` discarded every `security(1)` error with `_ =`, so AO
never noticed.

### 2. Isolation was a side effect of the identity mode

`providerruntime.Resolver` decided whether to substitute the runtime home from
`config.TrustedLocalMode`. Enabling OIDC sign-in derives `TrustedLocalMode =
false` (see `config/oidc.go`), so **turning on Google sign-in silently moved
every provider launch onto the isolated runtime home** — and therefore onto the
broken keychain. The user had changed how they log in to AO and lost the
credential their planner ran on.

### 3. The preflight was asking about a different process

`providerpreflight` answered the credentials question by calling the claude-code
adapter's `AuthStatus(ctx)`, which reads the **daemon's** `~/.claude.json` and
the **daemon's** `ANTHROPIC_API_KEY`. Under isolation the launch resolves a
different `HOME`, a different config dir and a different keychain, so the
preflight reported `auth: ok` for a launch that could not authenticate — and had
no vocabulary for "reaching the credential needs a person" in any case.

An unattended subprocess that hits a GUI dialog does not exit. It blocks until a
deadline kills it, so every attempt cost a full planner budget and learned
nothing.

## The contract

`internal/providerauth` is now the single answer to *from where will this
subprocess get its credential, and can it get it with nobody present?* It is
read by the planner's launch contract
(`adapters/planner/command/preflight.go`) and by the worker/reviewer/repair
dispatch preflight (`providerpreflight`), so no role can be fixed while another
still depends on the popup.

It resolves against the **launch** environment, never the daemon's.

Modes, in preference order:

| Mode | Source | Unattended |
| --- | --- | --- |
| `environment` | `ANTHROPIC_API_KEY` / `ANTHROPIC_AUTH_TOKEN` in the launch env | yes |
| `cloud_provider` | `CLAUDE_CODE_USE_BEDROCK` / `CLAUDE_CODE_USE_VERTEX` | not AO's to check |
| `helper` | `apiKeyHelper` in the provider's `settings.json` | yes, if the program runs |
| `keychain` | the provider's own OAuth login in the OS credential store | only if the store opens |

Statuses are four-valued on purpose:

- `available` — a source exists and its store opens without a person.
- `requires_interaction` — launching would open a dialog. **Refused**, because
  attempting it hangs rather than fails.
- `unavailable` — there is affirmatively no credential source.
- `unknown` — AO could not tell. **Always ready**: the cost of a wrong "unknown"
  is a warning AO fails to give, while the cost of a wrong refusal is AO
  refusing to work.

Nothing in the package reads, returns or logs a credential. It reports the
*name* of the variable carrying one, the *path* of a helper or a keychain, and
an OS status sentence. It deliberately never reads a keychain item — that
operation prompts on a locked keychain, so a probe that verified the credential
could cause the very dialog it exists to predict.

### New stop reasons

- `provider_auth_interactive` (worker/reviewer/repair)
- `planner_auth_interactive` (planner)

Both are distinct from `provider_auth_required` / `planner_auth_unavailable`,
which mean "sign in again" — advice that fixed nothing here. The planner's
refusal happens **before** the subprocess starts, so it spends no provider time.

## Configuration

Two new environment variables. Both default to the behavior AO already had.

### `AO_PROVIDER_AUTH_MODE`

`auto` (default) | `environment` | `helper` | `cloud_provider` | `keychain`

Pins the credential mechanism. A pinned mode that the launch environment cannot
satisfy is a refusal, not a fallback: an operator who declares "credentials come
from the environment" must not silently get a launch that reaches for somebody's
keychain instead.

### `AO_PROVIDER_RUNTIME_ISOLATION`

`auto` (default) | `host` | `strict`

Separates "who is signed in to AO" from "where AO's provider CLIs keep their
credentials".

- `auto` — derive from the identity mode, exactly as before.
- `host` — provider launches always keep the desktop user's own `HOME`,
  `CLAUDE_CONFIG_DIR` and OS credential store. **This is the supported posture
  for a single-user desktop that signs in to AO with SSO**, and the direct
  remedy for the incident. Every AO user on the instance shares the host's
  provider credentials, which is correct for one desktop and wrong for a shared
  deployment.
- `strict` — always prepare an AO-owned per-user runtime home, whatever the
  identity mode.

## Keychain repair and upgrade

`runtimehome` now:

- names its keychain `ao-provider.keychain-db` (never `login`);
- creates `$HOME/Library/Preferences` before writing the keychain domain —
  without it `security default-keychain -s` exits 0 and persists nothing, so the
  next process resolves *no* default keychain and every credential write blocks
  on a chooser. The old code got away without it only because a keychain named
  `login` is found by convention, the same reserved name that made it
  impossible to unlock;
- **quarantines** (renames, never deletes) any keychain it cannot open with its
  stored secret — including a legacy `login.keychain-db` left by an earlier
  version — and provisions a working replacement;
- reports the outcome on `runtimehome.Environment.Keychain` instead of
  discarding it, so the daemon logs a repair and the preflight can refuse a
  launch that would prompt.

A quarantined keychain's credentials are gone (they were unreadable anyway). The
provider must be re-connected for that user, or given an unattended credential.

## Update safety

The Claude binary path is versioned (`~/.local/share/claude/versions/<version>`)
and changes on every update. Nothing in this contract depends on it: the fix is
not an ACL grant tied to one binary, and `providerauth.Probe` never resolves or
reads the provider executable, so a provider update cannot change the answer.
There is a test asserting exactly that.
