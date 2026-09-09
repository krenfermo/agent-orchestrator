# 5. The four controls that stay unimplemented, and what each one requires

Date: 2026-09-09
Status: **Design only. Nothing in this ADR is implemented.**

## Context

ADR 0004 built the container boundary and proved it. Phase 4 then made one
mode — `static-code` — actually execute inside it, over a staged, scope-limited
copy, with a report whose coverage says what was and was not read.

That leaves four controls, each blocking exactly one capability:

| Control | Blocks |
| --- | --- |
| `writable_workspace` | `repo.write` |
| `egress_allowlist` | `net.egress`, `net.active_scan` |
| `scoped_secret_delivery` | `secrets.read` |
| `arbitrary_process_execution` | `process.exec` |

This ADR is the design for all four and the implementation of none. The reason
is stated plainly: each needs a negative test proving it cannot exceed its
scope, and a control whose negative test does not exist yet is a control that
should not be attested. Building four of them at once is how one of them ships
without its test.

**Do not read any section below as available.** The capability table names
these controls, `internal/skillrunner` attests none of them, and every affected
capability is refused with the missing control named.

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

### Why not now

The patch-review path is a product surface, not a runner detail: somebody has
to see the diff and approve it, and that is the review UI's job. Building the
runner half first would produce writes nobody can inspect.

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

### Why not now

This is the largest of the four and the only one that needs a second container
with its own lifecycle. It is also the one whose failure is most expensive: a
partial allowlist reads as a control while leaving exfiltration open, which is
worse than the current honest refusal.

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

### Why not now

It is small, and it is gated on the product question it implies: which secrets
may a skill request at all, and who approves each name? Shipping delivery
before that answer means the mechanism decides the policy.

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
