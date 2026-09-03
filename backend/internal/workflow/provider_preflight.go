package workflow

import (
	stdctx "context"
	"errors"
	"fmt"
	"strings"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

// provider_preflight.go — the readiness check before an UNATTENDED dispatch.
//
// The incident: unattended Claude sessions launched by AO sat forever at
// interactive prompts nobody was there to answer —
//
//	"Yes, I accept" (bypass permissions)
//	"Yes, I trust this folder" (a workspace the provider had not seen before)
//
// From AO's side the spawn succeeded: a process existed, a pane existed, and
// nothing was wrong except that the agent was never going to do anything. That
// is a failure AO can detect BEFORE spending a dispatch, and it must be named
// precisely, because "the agent could not start" and "the agent is waiting for
// someone to type y" send a person to two completely different places.
//
// What this deliberately does NOT do is answer the prompts. Piping "yes" into
// whatever a provider happens to ask is how an unattended agent ends up
// accepting something nobody read. Trust and permission mode are configured
// through the provider's OWN supported configuration, ahead of time; this
// checks that they were, and refuses the dispatch with a named reason when they
// were not.

// WorkerPreflight is the narrow port that answers "can this provider start,
// right now, in this workspace, without asking an operator anything?".
//
// Optional. A nil implementation keeps AO's pre-existing behavior exactly: the
// dispatch proceeds and any interactive block is discovered afterwards, by the
// worker observation, as it always was.
type WorkerPreflight interface {
	// Preflight reports readiness for one unattended launch. An error means the
	// check itself could not be run, which is unknown — never a refusal.
	Preflight(ctx stdctx.Context, req WorkerPreflightRequest) (WorkerPreflightResult, error)
}

// WorkerPreflightRequest is one readiness question.
type WorkerPreflightRequest struct {
	Harness domain.AgentHarness
	// WorkspacePath is the directory the agent will be launched in. It is the
	// whole point of the trust question: providers scope trust to a path, so a
	// brand-new incident/task worktree is exactly the case that prompts.
	WorkspacePath string
	ProjectID     string
	RunID         string
	StepID        string
	// RuntimeEnv is the owner-scoped provider subprocess env the dispatch will
	// actually use, so the probe reads the same credentials the launch will.
	RuntimeEnv map[string]string
	Owner      domain.UserID
	ProfileID  domain.ProviderProfileID
	// TrustRecordedAtLaunch says the launch this preflight is for will record
	// the provider's workspace trust ITSELF, for the workspace it actually
	// launches in, before the agent starts.
	//
	// It exists because the trust check would otherwise refuse a launch on a
	// condition AO resolves moments later, and did (P3-D, smoke A): every
	// session spawn runs the adapter's PreLaunch, and claude-code's PreLaunch
	// writes projects[workspace].hasTrustDialogAccepted into the very config
	// this preflight reads. A worker dispatch therefore cannot stop at the
	// trust dialog however the config reads beforehand — but the check fired
	// anyway and grounded every claude-code worker on any repository the person
	// had never opened in Claude Code themselves.
	//
	// It is a field rather than an assumption because the exemption is NOT
	// universal: the case the trust check was written for is a workspace that
	// will not get that write (the package comment names an incident or repair
	// workspace), and a future caller in that shape must still be refused. The
	// caller states which it is; the checker never guesses.
	//
	// Only the trust answer is affected. Binary, credentials and the
	// bypass-permissions acceptance are untouched — nothing about a launch
	// writes those, and accepting a permissions posture on somebody's behalf is
	// exactly what this package refuses to do.
	TrustRecordedAtLaunch bool
}

// WorkerPreflightResult is the answer. Every field is a fact the checker
// obtained; nothing here is inferred from the absence of another field.
type WorkerPreflightResult struct {
	// BinaryOK is false when the provider's CLI could not be resolved at all.
	BinaryOK bool
	// AuthOK is false only when the provider AFFIRMATIVELY reported that its
	// credentials are not usable. An auth probe that could not tell leaves this
	// true and sets AuthUnknown — "AO could not check" is never a refusal.
	AuthOK      bool
	AuthUnknown bool
	// TrustOK is false when the provider's own configuration does not already
	// record this workspace as trusted, so a launch there would ask.
	TrustOK bool
	// TrustUnknown is set when the provider has no trust concept AO can read.
	// Treated as ready: refusing on an unreadable check would ground every
	// provider that simply does not have one.
	TrustUnknown bool
	// PermissionModeOK is false when the configured permission mode cannot run
	// unattended (it would open an accept/deny dialog on the first tool use).
	PermissionModeOK bool
	// PermissionModeUnknown is set when AO cannot read the mode.
	PermissionModeUnknown bool
	// Detail is the checker's own one-line explanation, recorded verbatim.
	Detail string
}

// Provider preflight failure classes. They are attempt error classes rather
// than free text so the Board, the incident pack and the notification all name
// the same thing, and so a person reading "provider_workspace_trust_required"
// knows to go and trust the folder rather than to check their login.
const (
	// WorkflowErrorProviderAuthRequired: the provider says its credentials are
	// not usable. Nothing about retrying changes that.
	WorkflowErrorProviderAuthRequired domain.WorkflowErrorClass = "provider_auth_required"
	// WorkflowErrorProviderWorkspaceTrustRequired: launching in this directory
	// would open the provider's "do you trust this folder?" prompt, and there is
	// nobody to answer it.
	WorkflowErrorProviderWorkspaceTrustRequired domain.WorkflowErrorClass = "provider_workspace_trust_required"
	// WorkflowErrorProviderPreflightFailed: the provider would ask the operator
	// something else before it could work — an unusable permission mode, or a
	// readiness check that failed for a reason of its own.
	WorkflowErrorProviderPreflightFailed domain.WorkflowErrorClass = "provider_preflight_failed"
)

// Canonical attention reasons for the three. Each names a different thing to do,
// which is the entire reason they are not one reason.
const (
	ReasonProviderAuthRequired           = string(WorkflowErrorProviderAuthRequired)
	ReasonProviderWorkspaceTrustRequired = string(WorkflowErrorProviderWorkspaceTrustRequired)
	ReasonProviderPreflightFailed        = string(WorkflowErrorProviderPreflightFailed)
)

// preflightVerdict is the decision derived from one result.
type preflightVerdict struct {
	// Ready is true when the dispatch may proceed.
	Ready bool
	Class domain.WorkflowErrorClass
	// Reason is the canonical attention reason; empty when Ready.
	Reason string
	// Detail is the human-readable refusal.
	Detail string
}

// evaluateWorkerPreflight turns a result into a verdict. Pure, so the whole
// policy — including every "unknown is not a refusal" rule — is testable
// without a provider.
//
// Order matters and is the order a person would fix them in: an unusable binary
// makes every other question moot, credentials come next, then the two things
// that make a launch INTERACTIVE rather than broken.
func evaluateWorkerPreflight(harness domain.AgentHarness, workspace string, res WorkerPreflightResult) preflightVerdict {
	detail := strings.TrimSpace(res.Detail)
	suffix := ""
	if detail != "" {
		suffix = ": " + detail
	}
	switch {
	case !res.BinaryOK:
		return preflightVerdict{
			Class:  domain.WorkflowErrorBinaryMissing,
			Reason: ReasonDispatchFailed,
			Detail: fmt.Sprintf("provider preflight: %s's CLI could not be resolved%s", harness, suffix),
		}
	case !res.AuthOK && !res.AuthUnknown:
		return preflightVerdict{
			Class:  WorkflowErrorProviderAuthRequired,
			Reason: ReasonProviderAuthRequired,
			Detail: fmt.Sprintf("provider preflight: %s reported its credentials are not usable, so an unattended launch would stop at a login%s", harness, suffix),
		}
	case !res.TrustOK && !res.TrustUnknown:
		return preflightVerdict{
			Class:  WorkflowErrorProviderWorkspaceTrustRequired,
			Reason: ReasonProviderWorkspaceTrustRequired,
			Detail: fmt.Sprintf("provider preflight: %s has no recorded trust for %q, so an unattended launch would stop at its \"do you trust this folder?\" prompt%s", harness, workspace, suffix),
		}
	case !res.PermissionModeOK && !res.PermissionModeUnknown:
		return preflightVerdict{
			Class:  WorkflowErrorProviderPreflightFailed,
			Reason: ReasonProviderPreflightFailed,
			Detail: fmt.Sprintf("provider preflight: %s's configured permission mode cannot run unattended — it would open an accept/deny dialog on the first tool use%s", harness, suffix),
		}
	}
	return preflightVerdict{Ready: true}
}

// ErrProviderPreflight is the typed error a refused preflight becomes, so the
// existing launch-failure machinery (classifyWorkerLaunchFailure,
// recordWorkerLaunchFailure) carries it without a second failure path.
type ErrProviderPreflight struct {
	Class  domain.WorkflowErrorClass
	Reason string
	Detail string
}

func (e *ErrProviderPreflight) Error() string { return e.Detail }

// preflightWorkerDispatch runs the check for one dispatch attempt.
//
// Three properties are load-bearing:
//
//   - no checker wired is not a refusal. It is exactly today's behavior.
//   - a checker that ERRORS is not a refusal either. "AO could not run the
//     readiness check" is unknown, and grounding every dispatch on an unknown
//     would be strictly worse than the incident this exists for.
//   - it never answers a prompt. It reports that one would be asked.
func (c *Coordinator) preflightWorkerDispatch(ctx stdctx.Context, req WorkerPreflightRequest) error {
	if c.workerPreflight == nil {
		return nil
	}
	res, err := c.workerPreflight.Preflight(ctx, req)
	if err != nil {
		if c.log != nil {
			c.log.Warn("workflow: provider preflight could not be run; dispatching anyway",
				"harness", req.Harness, "run", req.RunID, "err", err)
		}
		return nil
	}
	verdict := evaluateWorkerPreflight(req.Harness, req.WorkspacePath, res)
	if verdict.Ready {
		return nil
	}
	if c.log != nil {
		c.log.Warn("workflow: provider preflight refused an unattended dispatch",
			"harness", req.Harness, "run", req.RunID, "step", req.StepID,
			"class", verdict.Class, "reason", verdict.Reason, "detail", verdict.Detail)
	}
	return &ErrProviderPreflight{Class: verdict.Class, Reason: verdict.Reason, Detail: verdict.Detail}
}

// classifyPreflightRefusal maps a preflight refusal onto the launch-recovery
// vocabulary: never retryable (nothing about waiting installs a credential or
// trusts a folder) and always carrying its own precise attention reason.
func classifyPreflightRefusal(err error) (workerLaunchClassification, bool) {
	var pf *ErrProviderPreflight
	if !errors.As(err, &pf) {
		return workerLaunchClassification{}, false
	}
	return workerLaunchClassification{
		Class:     pf.Class,
		Certainty: CertaintyActual,
		Retryable: false,
		Reason:    pf.Reason,
	}, true
}
