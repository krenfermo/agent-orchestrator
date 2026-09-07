# Agent identity (P4-I)

## The defect

On an installation running `AO_AUTH_MODE=oidc`, trusted-local synthesis is off by
construction: a request with no `ao_session` cookie resolves **no principal**, and
every permission-gated route answers `401 NOT_AUTHENTICATED` (see
[rbac.md](rbac.md), "trusted-local compatibility").

AO launches agents itself. A reviewer runs in a tmux pane, is told to record its
verdict with `ao review submit`, and `POST /sessions/{id}/reviews/submit` sits
behind `AuthorizeSessionAccess` like every other session-scoped route. The pane
holds no cookie, and before this change it could hold nothing else — an OIDC
Authorization Code flow needs a browser and a person, and a pane has neither.

So the reviewer was refused by the one route it exists to call. Observed on
`wf-98ab416c-fddd-4c1e-98b9-89a7e3eaae00`:

```
• Ran ao review submit agent-orchestrator-59 --run bf660d26-… --verdict approved
  └ authentication required (NOT_AUTHENTICATED)

• Review outcome: approved.
  The required verdict could not be recorded because the AO daemon returned:
  authentication required (NOT_AUTHENTICATED)
```

The reviewer then idled at its prompt. Its `review_run` stayed `running` with no
verdict, and thirty minutes later `reviewStalenessThreshold` parked the workflow
on `review_state_ambiguous` — AO reporting that it could not prove what the
review concluded, about a review it had itself refused to listen to. Twenty-three
hours later the reviewer was still alive, still owned, still finished.

## Two shortcuts, both refused

**Exempting the route**, the way `/reviews/{reviewSessionID}/activity` is
exempted. Activity is a heartbeat; a verdict decides whether work merges. An
unauthenticated write of a verdict is a worse defect than the one it fixes.

**Letting the agent present the operator's CLI credential.** It works — the
credential is a 0600 file under `AO_DATA_DIR`, readable by the same OS user the
pane runs as — and it silently promotes every agent to whatever the operator may
do, across every project, in every organization, for as long as that session
lives. To record one verdict.

## What was built instead

A credential that is **for the agent**: minted by the daemon at launch, acting on
behalf of the run's owner but capped by the agent's role, and bound to the one
project, session, workflow run, step and launch generation it was created for.

| | |
|---|---|
| Durable row | `agent_credentials` (migration `0159`). SHA-256 of the token only; the raw token never rests in the database, exactly as `auth_sessions` has done since `0107`. |
| Service | `internal/service/agentauth` — `Issue`, `ResolveAgentPrincipal`, `RevokeForReviewRun`, `EverIssuedForReviewRun`. |
| Transport | the `X-AO-Agent-Token` header, never a cookie. An agent is not a browser, and the two credential kinds must be impossible to confuse. |
| Handoff | `internal/agentcred` writes a 0600 JSON file under `<AO_DATA_DIR>/agent-credentials/` and the pane is given its path in `AO_AGENT_CREDENTIAL_FILE`. The token is never an environment variable: env is inherited by every tool the agent shells out to and shows up in `ps eww`. |
| Lifetime | 72h, revoked the moment the review run it was minted for is closed out, and revoked again when the reviewer is terminated. |

### Authorization is an INTERSECTION, never a substitute

`authz.Subject.Allows` evaluates the agent's own grant **and** the account's, and
grants only what both permit:

```go
if s.Agent != nil && !s.Agent.Allows(perm, res) {
    return false
}
```

`domain.AgentAuthority.Allows` refuses every non-project scope outright and every
project but the bound one, so an agent acting for the installation owner still
cannot manage users, read a second project, or touch an organization. In the
other direction the account's rules run unchanged, so revoking somebody's project
access revokes it for their agents in the same instant, and disabling an account
stops its agents at the next request rather than at the credential's expiry.

The role ceiling (`domain.AgentRoleCeiling`) is a closed table: a reviewer holds
`session.read`, `session.write` and `workflow.read`, and nothing else.

### Bindings are enforced at the canonical boundary

Project-scoped RBAC is the right model for a person, who legitimately works
across the project, and the wrong one for a credential minted for a single
launch. So `AuthorizeSessionAccess` and `Guard.AllowWorkflowRun` check the
session and run bindings **first** — before the Guard branch, before the
scoping-disabled early return, and before trusted-local mode — because the
binding is a property of the credential, not of the installation's authorization
posture. A denial is the same 404 the ownership gates report.

### A failed agent claim never falls back onto a person

Presenting the header is a claim about what the request *is*. Once made, it is
the only identity considered: a bad agent token resolves to no principal rather
than to whatever cookie happens to be attached. It still reaches trusted-local
synthesis, so a desktop install behaves exactly as it did before.

The CLI enforces the mirror rule. Inside an AO-launched runtime — detected by
`AO_AGENT_CREDENTIAL_FILE` or the runtime's own `AO_SESSION_OWNER` — it presents
the agent credential and **never** the person's, and its 401 hint says so rather
than telling a pane with no browser to run `ao auth login`.

### Refusing to launch a reviewer that cannot answer

Where an identity is required and one cannot be minted, the launch is refused.
Starting a reviewer that provably cannot record its verdict only moves the dead
end thirty minutes later, to the staleness threshold. On trusted-local the
reviewer's cookie-less call resolves the bootstrap admin anyway, so a missing
credential costs nothing and must not cost a launch.

## Recovering runs already stranded

`review_dispatch.go`'s ambiguous-review recovery previously acted only on
`reviewerRuntimeGone` — proof the reviewer is **absent**. `wf-98ab416c`'s reviewer
was present, so the refusal was correct and the run stayed parked forever.

`reviewerCannotDeliverVerdict` (`review_identity_recovery.go`) adds a second
proof, and it deliberately observes nothing about the agent. It reads two durable
facts AO owns:

1. this installation resolves no identity for a cookie-less request, so the only
   route by which a verdict reaches AO is an authenticated `reviews/submit`; and
2. AO never minted a credential for this review run, so every submit it makes is
   refused before it is read.

Both feed the **same** recovery: the same termination, the same CAS-guarded
close-out with no verdict, the same single bounded replacement over the same
target, the same provenance. Two proofs in front of one implementation, not two
paths that could drift.

It runs only on an explicit human resume — never a poll, a wake or a boot
reconcile — and it is **self-extinguishing**: every reviewer launched by this
build holds a credential, so fact 2 is false for all of them and the rule answers
false forever after. It fires for exactly the population stranded by the defect
above. Every uncertain answer is false: a probe that errored, a launch that was
never confirmed, a presence AO cannot correlate to its own launch (`foreign`), one
it could not read (`unknown`), a trusted-local installation, and an unreadable
ledger all decline.

## What is never done

- No verdict is ever fabricated. A closed-out review run carries no verdict, and
  a verdict landing in the same instant still wins the CAS.
- No route is exempted from authorization.
- No agent presents a person's credential.
- An `unknown` or `foreign` probe is never read as absence or as death.
