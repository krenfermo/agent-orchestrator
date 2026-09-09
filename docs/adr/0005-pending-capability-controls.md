# 5. The four controls that stay unimplemented, and what each one requires

Date: 2026-09-09
Status: `scoped_secret_delivery` (phase 5), `writable_workspace` (phase 6) and
`egress_allowlist` (phase 7, packaged in phase 8) are **IMPLEMENTED** — see the
notes at the end of their sections. `arbitrary_process_execution` remains design
only.

## Context

ADR 0004 built the container boundary and proved it. Phase 4 then made one
mode — `static-code` — actually execute inside it, over a staged, scope-limited
copy, with a report whose coverage says what was and was not read.

That leaves four controls, each blocking exactly one capability:

| Control | Blocks | Status |
| --- | --- | --- |
| `writable_workspace` | `repo.write` | **built, phase 6** |
| `egress_allowlist` | `net.egress` | **built, phase 7; packaged, phase 8** |
| `scoped_secret_delivery` | `secrets.read` | **built, phase 5** |
| `arbitrary_process_execution` | `process.exec` | design only |

This ADR is the design for all four and the implementation of none. The reason
is stated plainly: each needs a negative test proving it cannot exceed its
scope, and a control whose negative test does not exist yet is a control that
should not be attested. Building four of them at once is how one of them ships
without its test.

**Do not read the remaining section as available.** The capability table
names these controls; `internal/skillrunner` attests `scoped_secret_delivery`,
`writable_workspace` and `egress_allowlist` — each only when both a container and
its own second half exist, and the last only on a build that packages the proxy
and a host whose boundary was measured — and every other affected capability is
refused with the missing control named.

## 1. `writable_workspace` — for `repo.write`

### What it must be

A run may modify files in a workspace of its own, and AO can review the result
before anything reaches the operator's checkout. What it must never be is a
writable mount of the checkout: the operator's working tree is not AO's to
change, and a container that could write it would make "read-only audit" a
setting rather than a property.

### Design

- Stage as `static-code` already does, but into a writable copy: the container
  gets `/work` read-only and `/workspace` read-write, both under AO's staging
  root, never the checkout.
- `/workspace` is a `tmpfs` or a bind mount with a **size cap**. An unbounded
  writable mount is a disk-exhaustion primitive.
- On completion, AO diffs `/workspace` against the staged inputs and produces a
  patch. The patch is the output; the files are not applied.
- Applying a patch is a **separate, human-approved act** through AO's existing
  review path, not something the runner does.

### Negative tests it needs

1. A run cannot write the project checkout, by path or through `..`.
2. A run cannot write outside `/workspace`, including `/work` and `/tmp` beyond
   its cap.
3. Exceeding the workspace size cap fails the run rather than the host.
4. A produced patch that touches a path outside the staged scope is refused,
   not applied.
5. A symlink created inside `/workspace` pointing outside it does not let the
   diff read or write through it.

### Built, phase 6 — and what changed from this design

The design said "a `tmpfs` or a bind mount with a size cap". Measurement made
that a real choice rather than an either/or:

**A writable bind mount has no cap AO can enforce, and `--memory` does not help**
— pages written to a bind mount are page cache, not the cgroup's. **A tmpfs is
charged to the memory cgroup and its `size=` is kernel-enforced**: a 4 MiB write
into a 1 MiB tmpfs stopped at exactly 1 MiB. So `/workspace` is a tmpfs.

The cost, also measured: **a tmpfs dies with the container and `docker cp`
cannot reach it afterwards** — the copy comes back empty. So AO's own wrapper
transfers `/workspace` into a bind-mounted `/out` as its last act. What reaches
the host is bounded by the tmpfs cap by construction, whatever the workload did.

Two things the design did not say:

**The transfer deliberately preserves file types.** A symlink the run created
arrives on the host pointing at the *host's* filesystem, and a FIFO arrives as a
FIFO. Dropping them in the transfer would have left the host-side validator
untested, and the validator is the actual defense. Everything that arrives is
quarantined, not trusted: `Collect` uses `Lstat` and never follows a link,
refuses symlinks, hardlinks (an artifact whose bytes another name can change is
not an artifact), special files, traversing or over-long paths, oversize files
and over-budget counts — and **reports every refusal** rather than dropping it,
because a caller who sees three files and expected four needs to know which one
was refused and why. A refused file stays in quarantine so a person can look at
it.

**Cleanup fails closed and KEEPS the directory.** A per-run marker records the
owner, the run, the attempt and a token minted at preparation. A missing,
unreadable or mismatched marker refuses the removal: an orphan somebody deletes
by hand is recoverable, and deleting a directory that turned out to be somebody
else's is not. A leftover from a crashed attempt is likewise kept, never reused.

The result carries `Applied: false` as a field rather than an omission, so a
consumer reading it sees the answer instead of assuming one. Applying a patch
remains a separate, human-approved act through AO's review path — nothing here
writes to the operator's checkout, and a test fingerprints the whole checkout
before and after to prove it.

### Still open

The patch-review surface. `WorkspaceResult` is the input it needs — artifacts by
digest, a diff against the staged inputs, deletions, and the inputs actually
used — but there is no HTTP route, CLI verb or UI, so nothing here is reachable
from the app. Base/HEAD checking at apply time belongs there, not in the runner.

## 2. `egress_allowlist` — for `net.egress` and `net.active_scan`

### What it must be

Outbound traffic limited to declared destinations. Not "the network is off"
(that is `egress_deny_all`, which exists), and not "we asked the skill nicely".

### Design

The evidence in ADR 0004 already shows the substrate: a container on a Docker
`--internal` network reaches other containers and cannot reach the internet.

- AO starts a forward proxy container on an internal network, and the skill
  container joins **only** that network. It has no default route to anywhere
  else, so there is no direct path to bypass.
- `HTTP_PROXY`/`HTTPS_PROXY` point at the proxy. Those variables are a
  convenience for well-behaved clients, **not** the control — the control is
  that no other route exists.
- The proxy enforces the manifest's `scope.network.allow` plus the run's
  explicitly authorized targets, by host **and** port, on `CONNECT` and on
  plain requests.
- Every allowed and refused destination is logged as run evidence.

### The two bypasses that must be closed, and how

- **DNS.** The skill container gets no resolver of its own; name resolution
  happens at the proxy, against the allowlist. A container that could resolve
  arbitrary names could encode data in the queries even with no TCP egress.
- **Raw IP.** The allowlist is enforced on the connection the proxy makes, so
  an IP literal is checked the same as a name. Reaching an IP directly is not
  possible because there is no route.

### Negative tests it needs

1. A host not on the allowlist is refused, by name.
2. The same host by raw IP is refused.
3. A DNS query for a non-allowlisted name does not resolve.
4. Unsetting `HTTP_PROXY` inside the container does not restore egress — the
   variable is not the boundary.
5. An allowlisted host on a non-allowlisted port is refused.
6. A redirect from an allowlisted host to a non-allowlisted one is not followed.
7. The proxy container is torn down with the run and leaves no network behind.

### Built, phase 7 — and what changed from this design

The design held in outline. Four things it did not say:

**The topology is the control; the proxy only decides.** Measured on the
implemented design, from inside the skill container: an internet IP is "Network
unreachable", the cloud-metadata address is "Network unreachable", the embedded
resolver answers SERVFAIL for external names, and a container on the neighbouring
network is not resolvable. A live test unsets `HTTP_PROXY`, `http_proxy` and
`HTTPS_PROXY` and confirms that restores nothing — the variables are a
convenience for well-behaved clients, and there is no route to bypass.

**Rebinding is answered by resolving once and dialling the address.** The proxy
looks the host up itself, checks **every** returned address against the blocked
ranges, and then dials the checked IP rather than the name — so the second
lookup that would have answered differently never happens. A name that resolves
to both a usable address and a blocked one is refused outright: allowing it
would leave which one gets used to chance.

**IP literals are refused as destinations.** An allowlist entry is meant to be
read by whoever approves it, and "may reach 34.117.x.y" is not a statement
anybody can evaluate. Wildcards are refused for the same reason — a grant
covering hosts that do not exist yet is one nobody can enumerate.

**The blanket refusal of private addresses needed an escape valve, and it is
written down.** An installation whose artifact registry genuinely lives at 10.x
would otherwise have to turn the control off, and a control nobody can use is a
control that gets turned off. So a policy may list `permittedPrivateCidrs`. Each
entry is validated, appears in the proxy's startup line and in the audit summary,
and **can never re-open link-local**: 169.254.0.0/16 is where cloud metadata
hands out the host's own credentials, and no exception may reach it. That
refusal is asserted directly.

The proxy runs with the same posture as a skill container — non-root, immutable
root filesystem, no capabilities, one bounded tmpfs for its decision log — and
refuses to start on a policy it cannot accept rather than falling back to
anything permissive.

### net.active_scan is deliberately still blocked

`egress_allowlist` unblocks `net.egress` and **not** `net.active_scan`. A
forward proxy can express "may open a connection to this host"; it cannot
express "may probe this host for weaknesses", because it does not inspect
payloads and would not know the difference. The capability table keeps them
apart by requiring `arbitrary_process_execution` for the scan as well — a test
asserts that granting egress leaves the scan refused, and names the control it
still lacks.

Active testing also needs what a network control cannot provide: named targets,
a time window, rate limits, and an approval that says which system may be
attacked. Those belong with the capability, not with the proxy.

### Packaged, phase 8 — and what the packaging decided

Phase 7 left the proxy as a binary the TEST cross-compiled and bind-mounted:
enforcement proven, distribution unsolved. Phase 8 makes it an artifact AO
carries. Nothing is compiled while a skill runs, and no new capability is
unblocked.

**The binaries are built, not committed; the provenance is committed, not
built.** `scripts/build-egress-proxy.sh` cross-compiles `linux/amd64` and
`linux/arm64` with `CGO_ENABLED=0 -trimpath -buildvcs=false -ldflags="-s -w
-buildid="`, which removes the build machine's paths, the commit, the dirty bit
and the content-derived build id — everything that varies between two people
building the same source. `--check` builds twice into different directories and
compares: **measured byte-identical for both architectures.** The digests, the
sizes, the toolchain and the flag set land in `provenance.json`, which is 30
lines of reviewable text; the artifacts are 6 MiB each and are reviewable by
nobody, so they stay out of git. `--record` is the only thing that rewrites the
provenance, and the diff it produces is exactly "these bytes changed, on this
toolchain".

The cost, stated plainly: **a Go version change moves every digest.** The verify
path detects that case and says so — naming the recorded toolchain and the one
in use — rather than failing with an opaque mismatch.

**`go:embed`, behind a build tag, is the answer.** A release is built with
`-tags ao_embed_egress_proxy`; the embed directive names both artifacts, so a
release that would have shipped without one **does not compile**. Every other
build — every `go build ./...`, every `go test ./...` — carries no proxy at all,
`Packaged()` is false, and the runner attests no allowlist. That is not a
degraded mode to work around: a development daemon that reported an allowlist it
could not enforce would be worse than one that refuses.

**The runner now MEASURES instead of being told.** `WithEgressAllowlist(bool)`
is gone. `VerifyEgressBoundary` checks two things and attests only when both
hold: the right proxy is present (packaged, matching the architecture the
**container runtime** reports — on macOS the VM's, not the daemon's — and
hashing to the recorded digest), and the boundary works (AO creates an
`--internal` network, confirms the runtime reports it as internal, and watches a
container on it **fail** to reach `192.0.2.1` and `169.254.169.254`). Neither
address is an external service: on an internal network there is no default
route, so the kernel refuses locally. The artifact check runs first and
short-circuits, because a build with no proxy cannot enforce anything however
good the host's topology is.

**Extraction is its own control.** `proxybin.Stage` writes the binary and the
policy — and nothing else — into a fresh per-run directory, `0711`, mounted
read-only at `/aoproxy`. The binary is `0555` and the policy `0444`: the
container runs as uid 65534 and owns nothing, so it needs other-execute and
other-read; what is absent is a write bit for anybody. The digest is verified
**twice** — on the embedded bytes, and again by reading the file back after it
is written — because "AO carried the right binary" and "the right binary is what
the container will mount" are different claims and only the second one is the
mount. Neither the home directory nor AO's data dir may be the mount source: AO's
data dir holds the database and every project's state, and mounting it to
deliver one binary would put all of that inside a container. A leftover
directory from a crashed attempt is kept, never reused.

**Two things the design did not anticipate:**

*A caller must not be able to name the architecture.* `StageRequest` takes what
the runtime **reported** — `"aarch64"`, `"x86_64"` — and maps it itself. A field
called `GOARCH` is a field somebody fills in with the daemon's own architecture,
which on macOS is the wrong one.

*The kernel is not the safeguard.* A `linux/amd64` proxy on a `linux/arm64`
runtime does **not** produce an exec-format error on Docker Desktop: binfmt
emulation is registered inside the VM, and the foreign binary starts and serves
normally (measured — it ran for five minutes until the test was killed). So the
architecture check is AO's control, and the live test asserts it by hashing the
mounted binary from inside the container rather than by waiting for a failure
that does not come.

### Still open after phase 8

- **Where the release build runs.** The build script and the tag exist; wiring
  them into the desktop release pipeline is a separate change, and until it
  happens every shipped daemon is an unpackaged one that refuses egress.
- **The trust root.** `provenance.json` says which bytes AO's own build produced.
  It is not a signature, and it is not evidence to anybody who did not run the
  build. See section 4.

## 3. `scoped_secret_delivery` — for `secrets.read`

### What it must be

AO hands one run exactly the named secrets its activation authorized, for that
run only, and the daemon's own credentials never come near it.

### Design

- Follow the rule `internal/agentcred` already documents: the secret rests in a
  file, and only the **filename** travels in the environment. An environment
  variable is readable from the process table, inherited by every child, and
  lands in crash dumps.
- The file lives on a `tmpfs` mount for that container alone, is written 0600,
  and is unlinked when the run ends.
- The set of names comes from the activation's grant, intersected with the
  manifest's `scope.secrets.requested`. A name in neither is refused.
- The value comes from AO's secret store (`internal/secretbox`), decrypted at
  run start, never logged and never written to the report.
- The report records the **names** delivered, never values, and the redaction
  rule already stated for reports applies to every field.

### Negative tests it needs

1. A secret the activation did not grant is not delivered, even when the
   manifest requests it.
2. No secret value appears in the report, stdout, stderr or the audit trail.
3. The daemon's own environment is still absent (the existing test must keep
   passing with secret delivery on).
4. The secret file is gone after the run, including after a timeout kill.
5. A run cannot read another run's secret file.

### Built, phase 5 — and what changed from this design

The design above held. Three things it did not say, which the implementation
had to decide:

**A grant needs a full scope, and a lease needs an attempt.** The design said
"the activation's grant". That is not narrow enough: a grant bound to a project
is still usable by a different package version, or by the same package's pentest
mode. `skillsecrets.Scope` binds tenant + project + skill + version + mode, with
no wildcard, and every field must match exactly. On top of it a `Lease` binds to
one run AND one attempt and is redeemable exactly once, which is what makes
replay answerable: attempt N-1 cannot redeem attempt N's lease, and a redeemed
lease is spent.

**Revocation is re-checked at redemption, not only at minting.** A lease is a
right to ask, not a right to receive. Somebody who revokes a grant between
launch and delivery expects the delivery to stop, and "verifiable revocation"
has to mean that.

**A value cannot become text.** `SecretValue.String`, `GoString` and
`MarshalJSON` all redact, and `UnmarshalJSON` refuses outright. That is the
control doing the most work here: `%v`, `%+v`, `%#v`, `fmt.Errorf`, `slog` and
`json.Marshal` are the six routes a real leak takes, and all six are closed by
construction rather than by remembering. `Reveal` is the one door, and it is
greppable.

Partial authorization delivers **nothing**: a run written against two secrets
and handed one behaves in ways nobody designed.

The mechanism is a per-run directory, 0700, one 0600 file per reference,
bind-mounted read-only at `/run/secrets`. Never an environment variable, for the
reasons `internal/agentcred` already records. Never the home directory, the
Keychain, a credential file or AO's data dir. Cleanup overwrites and removes on
every exit path including timeout, and refuses any path outside AO's own
namespace so a bug in root selection cannot become a delete of somebody's files.

**The residual risk, stated plainly:** the value transits the host filesystem.
A container runtime cannot pre-fill a tmpfs, so a bind mount is the mechanism
available. The file exists for the length of the run with restrictive
permissions and is overwritten before unlinking — which removes it from
everything that reads by path, but is not a guarantee about the medium on a
copy-on-write or journaling filesystem. A real vault delivers over a channel the
value never leaves; AO is not one.

**Still open, deliberately:** the product question this section originally
raised. There is no HTTP route, no CLI verb and no UI for registering a secret
or granting one, so nothing here is reachable from the app. Which secrets a
skill may request, and who approves each name, is a policy decision that should
be made before the surface exists — otherwise the mechanism decides the policy.

## 4. `arbitrary_process_execution` — for `process.exec`

### What it must be

A skill may run commands it authored. Today AO authors every command:
`ao.static-scan/v1` is a script in `internal/skillrunner`, and a manifest
contributes two validated integers and nothing else.

### Design

This is not a sandbox problem — the sandbox already holds. It is a supply-chain
and provenance problem, and it needs the answer AO has deliberately deferred
twice (ADR 0003 rejects `provenance.signature`; the roadmap's open question 4
is the trust root).

- A skill ships a **tool image**, referenced by digest, built from a declared
  source, with a declared entrypoint.
- AO verifies the digest against a **trust root it decided in advance**:
  signature verification, or an AO-operated registry, or a manual
  administrator approval recorded per digest. Any of the three is a decision;
  none of them is a default.
- The manifest may still not build a command line. It selects a declared
  entrypoint and supplies typed, validated parameters — the same shape as
  today's `ToolParams`, generalized.
- Every execution records image digest, entrypoint and parameters in the audit
  trail, so "what ran as AO" is answerable after the fact.

### Negative tests it needs

1. An unapproved image digest is refused.
2. A digest whose signature/approval does not verify is refused.
3. A parameter cannot inject a shell metacharacter or an extra argument.
4. An entrypoint not declared by the verified image is refused.
5. A skill cannot cause a pull; an absent image is a refusal, as today.

### Why not now

It requires the trust-root decision, which is a product and operational choice
about who may publish a skill image for this installation. Implementing before
that produces a mechanism that trusts whatever is on the host, which is
indistinguishable from no control at all.

## Ordering recommendation

1. **`scoped_secret_delivery`** — smallest, well-precedented by `agentcred`,
   and its blocker is a policy question rather than an architecture one.
2. **`writable_workspace`** — medium, but its output (a patch) plugs into a
   review surface AO already has.
3. **`egress_allowlist`** — large, and the only one needing a second container.
   It unblocks two capabilities, including the dependency audit that is the most
   asked-for mode after static analysis.
4. **`arbitrary_process_execution`** — last, because it is gated on a trust-root
   decision nobody has made, and because the closed tool contract makes it the
   least urgent: AO can add approved tools without it.

## What this ADR does not change

`internal/skillrunner` attests five controls and no more.
`repo.write`, `process.exec`, `secrets.read`, `net.egress` and
`net.active_scan` are refused with the missing control named. Nothing above is
reachable by a person clicking through the app, and nothing above should be
described to anyone as available.
