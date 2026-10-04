# Connecting AO to a private Skill registry

What a company registry has to serve, how AO reaches it, and — more usefully —
what AO refuses to do on your behalf.

The trust model is unchanged from `docs/skills/registry.md` and ADR 0006:
installing verifies **integrity**, not provenance. Reaching a registry over TLS
does not change that. TLS authenticates the *server*; the question is who wrote
the *code*, and AO still verifies no signature. `trusted` remains unreachable
and a registry on the `signed` trust policy still installs nothing.

The transport decisions are ADR 0007.

---

## 1. The protocol a registry must serve

`ao.registry/v1` over HTTPS. Every path is relative to the configured base URL,
and AO builds every one of them itself — a URL that appears in a release
(`sourceUrl`, `changelogUrl`, `attestationUrl`) is reference metadata for a
human and is never fetched.

| Method & path | Answers | Media type |
| --- | --- | --- |
| `GET /v1/registry` | `{apiVersion, registryId, displayName?}` | `application/json` |
| `GET /v1/skills?q=&publisher=&capability=&includeDeprecated=&includeRevoked=&limit=` | `{apiVersion, releases[]}` | `application/json` |
| `GET /v1/skills/{skillId}/versions` | `{apiVersion, releases[]}` | `application/json` |
| `GET /v1/skills/{skillId}/versions/{version}` | `{apiVersion, release}` | `application/json` |
| `GET /v1/skills/{skillId}/versions/{version}/artifact` | the package | `application/vnd.ao.skill-package.v1+tar+gzip` |
| `GET /v1/revocations` | `{apiVersion, revocations[]}` | `application/json` |

A release is the same JSON shape a local `registry.json` entry carries, minus
`registryId` (AO stamps its own — a payload that named itself could impersonate
another registry) and minus `artifactPath`.

Two properties of this shape are load-bearing:

- **Metadata and bytes are different endpoints, reached by different provider
  methods.** A registry cannot smuggle a package into a search answer, because
  there is nowhere in the metadata schema to put one.
- **`/v1/revocations` exists separately from the release metadata.** A registry
  that deletes a compromised release and serves 404 has told AO nothing — 404
  is indistinguishable from a typo. A withdrawal has to be answerable after the
  release is gone.

### Conditional requests

Serve `ETag` (and optionally `Last-Modified`) on the metadata endpoints. AO
replays them as `If-None-Match` / `If-Modified-Since` and honours `304`. It
costs you a body per search; it costs nothing if you do not.

`ETag` is deliberately ignored on two paths: `ResolveExactRelease`, which an
install uses, and `/v1/revocations`. Neither may be answered from a copy.

---

## 2. Configuring one

Settings → Skills → Registries, or:

```bash
# The credential first: a registry that names a secret cannot be saved until
# the secret exists, because AO opens a registry before recording it.
ao skills secret add CORP_REGISTRY_TOKEN --description "private skill registry"

ao skills registry add company-private \
  --name "Company Private" \
  --type https \
  --location https://registry.corp.example \
  --auth-type bearer \
  --credential-secret CORP_REGISTRY_TOKEN \
  --trust-policy pinned_publisher --pinned-publisher acme-platform

ao skills registry test company-private
```

### The base URL is an ORIGIN

`https://host[:port][/prefix]`. HTTPS only. No credentials in the URL, no query
string, and **no IP literal** — a configuration reading "the registry is
10.4.2.9" is not one anybody can evaluate a year later. Give it a name in DNS
or in `/etc/hosts`.

Everything AO requests is that origin plus a path from the table above. A
redirect anywhere else is refused rather than followed.

### The credential is a NAME

`--credential-secret` takes the name of a sealed secret, never a value. Nothing
in `skill_registries` can hold a credential; there is no column for one. The
value is opened from `internal/secretbox` when a provider is built and dropped
when the request is done, and it redacts in every rendering path Go has.

Two auth types:

- `bearer` → `Authorization: Bearer <secret>`
- `api_key_header` → `<--api-key-header>: <secret>`, default `X-API-Key`.
  `Authorization`, `Cookie`, `Proxy-Authorization` and `Host` are refused as
  header names.

There is no `basic` and no credential-in-a-query-string. Basic is a password
with a worse cache story; a query string ends up in access logs, proxy logs and
referrers.

The auth type is explicit rather than guessed from the presence of a secret,
because "AO guessed bearer and the registry wanted a header" is a 401 nobody can
debug from a settings screen.

### An on-premises registry on a private address

AO refuses every private, loopback, link-local and carrier-NAT address by
default — the same list `internal/skillegress` enforces for skill traffic. If
your registry genuinely lives at `10.4.0.0/16`:

```bash
ao skills registry add company-private ... --permit-private-cidr 10.4.0.0/16
```

Each exception is written down, shown in Settings, and printed in the audit
line. **No exception can re-open link-local**, so none can reach the cloud
metadata address; the validator refuses an overlapping entry.

---

## 3. Test connection

Reads **one** small metadata endpoint. It downloads no package, changes no
catalog, installs nothing, enables nothing on any project and approves no
container image. It is safe to press.

| Verdict | Means |
| --- | --- |
| `CONNECTED` | The registry answered **as itself**, over verified TLS, speaking a protocol version this build knows. |
| `AUTH_FAILED` | 401/403. The detail names your `secretRef`, never its value. |
| `TLS_FAILED` | The certificate did not verify. Misconfiguration, or somebody in the middle. |
| `UNREACHABLE` | Nothing answered: DNS, connection, timeout, 5xx. |
| `INVALID_RESPONSE` | Something answered and it is not this registry speaking this protocol. |
| `POLICY_BLOCKED` | AO refused before sending anything: a blocked address, a base URL it will not accept, a credential it could not resolve. |

`CONNECTED` is the hardest of the six to reach, on purpose. A socket opening,
TLS verifying and a 200 arriving are each necessary and none is sufficient — a
load balancer, a captive portal and an unrelated service on the right port all
produce a 200. If the endpoint calls itself something other than the id you
configured, the answer is `INVALID_RESPONSE`, and it says which name it gave.

Testing requires `settings.manage`, not `settings.read`: it makes AO open a
connection and present a stored credential, which is not a read of AO's own
state.

---

## 4. What an install actually does

1. Re-resolve **one exact version** from the registry. Never from cache.
2. Refuse if the registry says it is revoked, **or** if AO has already recorded
   a revocation for it.
3. Apply the registry's trust policy (`digest`, `pinned_publisher`, `signed`).
4. Check compatibility against the running AO version.
5. Refuse a skill id already installed from a different registry.
6. Fetch into a quarantine directory — or reuse a cached tree, re-measured.
7. Recompute **both** digests over the bytes that landed. Load the manifest.
8. Refuse if the manifest disagrees with the listing about the id, the version,
   the publisher, or the capabilities **in either direction**.
9. Install through the ordinary catalog path.
10. Record provenance: registry, publisher, both digests, trust state, trust
    policy, compatibility verdict, who and when.
11. Keep the verified tree in the artifact cache.
12. Remove the quarantine, whichever way it went.

Nothing downloaded is ever executed. Installing enables the skill on no
project, grants no capability and approves no image.

### Limits, none of them configurable

| | |
| --- | --- |
| metadata response | 8 MiB |
| artifact download | 64 MiB |
| unpacked total | 256 MiB |
| one file | 32 MiB |
| entries | 4096 |
| redirects | 3, same-origin only |
| metadata request | 20s |
| artifact request | 5m |

They are constants because a per-registry override is a per-registry way to
turn a limit off. A link, a device node, or an entry naming a path above the
package root is refused before anything is written.

---

## 5. Cache, and what "offline" is allowed to mean

**Metadata cache**: a record of an answer, with the moment it was fetched. Past
its TTL it is STALE, and the UI says `OFFLINE / STALE METADATA` rather than
showing it as current. No install acts on it.

**Artifact cache**: content-addressed by **both** digests, per registry,
immutable, and **re-measured on every read**. A cache directory is a file on
this host, and "we checked it when we wrote it" is not a check that holds now.
A tampered entry is detected and removed.

> Why both digests: `ComputePackageDigest` deliberately excludes the manifest,
> so two releases whose only difference is the version line hash to the same
> artifact digest. That is every ordinary version bump of a skill whose code
> did not change.

An **offline install** is possible only when you ask for it explicitly, and
only when:

- AO already fetched and verified those exact bytes,
- both digests recompute over the cached tree,
- the release AO verified them against is the one being installed, and
- AO holds no recorded revocation for it.

A release AO has never downloaded **cannot** be installed offline. Cache is
evidence, not a substitute for it.

Garbage collection is explicit and bounded by entries, bytes and retention. AO
deletes nothing on a schedule nobody asked for.

---

## 6. Revocation

```bash
ao skills registry revocations company-private
```

AO polls nothing on its own. A sync you ask for records what the registry has
withdrawn and marks any installed copy.

A recorded revocation **blocks new installs of that exact release, including
while the registry is unreachable.** Silence is not consent. A registry AO
cannot reach is not a registry with nothing withdrawn.

What it still does not do, unchanged:

- it does not uninstall the package,
- it does not delete any files,
- it does not disable the skill on any project,
- it does not stop a run already under way.

The screen says *"Installed release has been revoked by registry."* and the
decision is yours, one package at a time.

---

## 7. What lands in the audit trail

`registry_added`, `registry_updated`, `registry_removed`, `registry_enabled`,
`registry_disabled`, `registry_connection_tested`, `registry_auth_failed`,
`skill_fetch_started`, `skill_fetch_refused`, `cached_artifact_used`,
`install`, `install_refused`, `update_available`, `update_installed`,
`release_revoked_seen`, `registry_revocations_synced`.

`cached_artifact_used` exists so an install served from cache is
distinguishable from one that went to the network. They verify identically;
"where did these bytes come from" is what an incident review asks.

**Not audited, deliberately:** a row per search. A search query is text a person
typed — it can name an internal package or a vulnerability they are hunting —
and reaching a private registry does not make it worth keeping, it makes it more
sensitive. Also never audited: a token, a full header set, or an artifact body.

---

## 8. Still missing

- **A public marketplace, AO Official, and a GitHub provider.** ADR 0007 lists
  what each would need; none is transport work.
- **Signature verification.** The only thing that could make `trusted`
  reachable.
- **Publishing.** AO reads registries and has no verb that writes to one.
- **A tenant-scoped secret store.** The tenant boundary today is drawn around
  the *registry*, not the secret. See ADR 0007, Decision 2.
