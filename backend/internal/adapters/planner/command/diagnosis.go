package command

import (
	"errors"
	"fmt"
	"os/exec"
	"strings"

	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	workflowcore "github.com/aoagents/agent-orchestrator/backend/internal/workflow"
)

// diagnosis.go — the planner's launch contract, part two: what a failed
// invocation actually said.
//
// See preflight.go for the incident. The half fixed here is the one that made
// wf-7f8cc736 undiagnosable after the fact: when the subprocess exited
// non-zero, the adapter attached `snippet(output)` to the error and stopped.
// snippet took the first 500 bytes; the Claude Code print-mode envelope puts
// `is_error` at ~1476, `subtype` at ~1507, `api_error_status` at ~1527 and
// `result` — the human-readable reason — at ~1551, behind a fixed
// usage/iterations block. The reason was therefore discarded on every failure,
// which is why AO's own provider-failure classifier (which reads text) could
// never see a rate limit, an auth rejection or an overload, and every planner
// failure became the same nonrecoverable stop.
//
// The envelope is emitted on the failure path too. So it is parsed on the
// failure path too, and the fields that carry the verdict are turned into a
// typed sentinel plus a sentence that names the provider, the binary AO
// resolved and what the provider said.

// providerReasonLimit bounds how much of the provider's own error prose is
// carried into a durable stop. Provider error text is short by nature (a
// rate-limit banner, a "run /login" instruction, an HTTP status line); this is
// generous enough for all of them and far too small for a leaked plan body.
const providerReasonLimit = 400

// failureDiagnosis is what one failed invocation is understood to mean.
type failureDiagnosis struct {
	// Sentinel is the typed claim the coordinator's retry policy reads.
	Sentinel error
	// Classification is the attempt-evidence class recorded durably.
	Classification string
	// Reason is the provider's own words, bounded, or a description of the
	// process outcome when the provider said nothing.
	Reason string
	// Subtype and APIErrorStatus are the envelope's own machine-readable
	// verdict fields, empty when it carried none.
	Subtype        string
	APIErrorStatus string
	// ExitCode is the subprocess exit status, or -1 when it did not exit
	// normally (killed by a signal, or never started).
	ExitCode int
}

// diagnoseFailure interprets a planner subprocess that exited non-zero.
//
// out is the subprocess's combined output and runErr the error exec returned.
// Neither is optional: the exit status alone never says why, and the envelope
// alone cannot distinguish "exited 1 having explained itself" from "was killed".
func diagnoseFailure(out []byte, runErr error) failureDiagnosis {
	d := failureDiagnosis{
		Sentinel:       ports.ErrPlannerLaunchFailed,
		Classification: workflowcore.PlannerAttemptExitedEarly,
		ExitCode:       exitCodeOf(runErr),
	}
	envelope, err := extractEnvelope(out)
	if err != nil {
		// No envelope at all: the CLI died before it could report anything, or
		// what came back is not this CLI's output. The raw output is the only
		// evidence there is, so it is carried through head-and-tail rather
		// than head-only — a provider that prints a banner then fails puts the
		// failure at the END.
		d.Reason = boundedOutput(out)
		if d.Reason == "" {
			d.Reason = fmt.Sprintf("no output (%v)", runErr)
		}
		d.Classification, d.Sentinel = sentinelForText(d.Reason)
		return d
	}
	d.Subtype = envelope.Subtype
	d.APIErrorStatus = envelope.APIErrorStatus
	d.Reason = providerReason(envelope, out)
	d.Classification, d.Sentinel = sentinelForText(strings.Join([]string{
		envelope.Subtype, envelope.APIErrorStatus, d.Reason,
	}, " "))
	return d
}

// providerReason picks the most specific human-readable text the envelope
// carries, preferring the CLI's own `result` prose over anything AO would have
// to infer, and falling back to the raw output when the envelope is an error
// envelope with no message in it.
func providerReason(env plannerEnvelope, out []byte) string {
	if r := strings.TrimSpace(env.Result); r != "" {
		return bound(r, providerReasonLimit)
	}
	if env.Subtype != "" || env.APIErrorStatus != "" {
		return strings.TrimSpace(env.Subtype + " " + env.APIErrorStatus)
	}
	return boundedOutput(out)
}

// sentinelForText maps a provider's own words onto the typed launch-failure
// claim they support.
//
// The order is deliberate and is the order of consequence, not of likelihood:
// an auth rejection must never be read as a generic exit (it would be retried
// into the same login prompt), and a capacity/rate signal must be left
// UNCLAIMED so it reaches the coordinator's shared provider-failure classifier
// and parks the plan for capacity like every other provider call does. Claiming
// it here with a launch-failure sentinel would take a retryable wait and turn
// it into a bounded retry that burns budget.
func sentinelForText(text string) (string, error) {
	t := strings.ToLower(text)
	switch {
	case mentionsAny(t, "rate limit", "rate-limit", "429", "too many requests",
		"usage limit", "quota", "overloaded", "capacity", "529", "503"):
		// Left to classifyProviderFailure: this is a wait, not a launch fault.
		return workflowcore.PlannerAttemptExitedEarly, ports.ErrPlannerLaunchFailed
	case mentionsAny(t, "invalid api key", "invalid_api_key", "authentication_error", "authentication failed",
		"unauthorized", "401", "403", "not logged in", "logged out", "log in", "/login",
		"oauth token", "expired token", "credential"):
		return workflowcore.PlannerAttemptAuthUnavailable, ports.ErrPlannerAuthRequired
	case mentionsAny(t, "unknown option", "unknown argument", "unrecognized option",
		"invalid option", "unsupported", "is not a valid", "usage: claude", "error: unknown command"):
		return workflowcore.PlannerAttemptUnsupported, ports.ErrPlannerUnsupportedInvocation
	case mentionsAny(t, "command not found", "executable file not found", "no such file or directory"):
		return workflowcore.PlannerAttemptBinaryMissing, ports.ErrPlannerBinaryMissing
	}
	return workflowcore.PlannerAttemptExitedEarly, ports.ErrPlannerLaunchFailed
}

// mentionsAny is local to this package on purpose: workflow's classifier has a
// similar helper, and importing across that boundary to share four lines would
// couple the adapter to the coordinator's vocabulary.
func mentionsAny(haystack string, needles ...string) bool {
	for _, n := range needles {
		if strings.Contains(haystack, n) {
			return true
		}
	}
	return false
}

// launchError renders a diagnosis as the error the coordinator receives: the
// typed sentinel first (so errors.Is decides policy), then the facts a person
// needs, then the provider's own words. Every element is safe to persist — a
// binary path, an exit code, a provider status, and bounded provider prose.
func (d failureDiagnosis) launchError(plan launchPlan) error {
	parts := []string{fmt.Sprintf("provider %s (%s) exited %d", plan.Binary, plan.BinaryPath, d.ExitCode)}
	if d.Subtype != "" {
		parts = append(parts, "subtype="+d.Subtype)
	}
	if d.APIErrorStatus != "" {
		parts = append(parts, "apiErrorStatus="+d.APIErrorStatus)
	}
	if plan.ProfileVar != "" {
		parts = append(parts, fmt.Sprintf("%s=%s", plan.ProfileVar, plan.ProfileDir))
	}
	if d.Reason != "" {
		parts = append(parts, "reason: "+d.Reason)
	}
	return fmt.Errorf("%w: %s", d.Sentinel, strings.Join(parts, "; "))
}

// exitCodeOf extracts a subprocess exit status, or -1 when the process did not
// exit normally. -1 is a fact ("killed, or never ran"), not a missing value.
func exitCodeOf(err error) int {
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return exitErr.ExitCode()
	}
	return -1
}

// boundedOutput renders raw subprocess output for a diagnostic, keeping BOTH
// ends of it.
//
// Head-only is what the original snippet() did and is exactly wrong for this
// job: a CLI that prints a large structured envelope and then fails puts its
// metrics at the front and its verdict at the back, so a head-only window
// records the least informative bytes available and discards the rest. Keeping
// a head and a tail costs the same budget and cannot miss a trailing stderr
// line.
func boundedOutput(b []byte) string {
	s := strings.TrimSpace(string(b))
	if len(s) <= providerReasonLimit {
		return s
	}
	half := providerReasonLimit / 2
	head := s[:half]
	for head != "" && !isRuneStart(s[len(head)]) {
		head = head[:len(head)-1]
	}
	tailStart := len(s) - half
	for tailStart < len(s) && !isRuneStart(s[tailStart]) {
		tailStart++
	}
	return head + "…[" + fmt.Sprintf("%d", len(s)-len(head)-(len(s)-tailStart)) + " bytes elided]…" + s[tailStart:]
}

// bound truncates s to limit bytes on a rune boundary, marking the cut.
func bound(s string, limit int) string {
	s = strings.TrimSpace(s)
	if len(s) <= limit {
		return s
	}
	cut := limit
	for cut > 0 && !isRuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + "…"
}

func isRuneStart(b byte) bool { return b&0xC0 != 0x80 }

// classificationForPreflight names the durable class for a launch AO refused
// before spawning anything. A refused launch spends nothing and leaves no
// process, which is precisely why the class must say which check refused it:
// "the planner never ran" is not a diagnosis.
func classificationForPreflight(err error) string {
	switch {
	case errors.Is(err, ports.ErrPlannerBinaryMissing):
		return workflowcore.PlannerAttemptBinaryMissing
	case errors.Is(err, ports.ErrPlannerAuthInteractive):
		return workflowcore.PlannerAttemptAuthInteractive
	case errors.Is(err, ports.ErrPlannerRuntimeHomeUnreadable):
		return workflowcore.PlannerAttemptProfileUnreadable
	}
	return workflowcore.PlannerAttemptExitedEarly
}
