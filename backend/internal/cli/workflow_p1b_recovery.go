package cli

import (
	"errors"
	"fmt"
	"io"
	"net/url"
	"strings"

	"github.com/spf13/cobra"
)

// workflow_p1b_recovery.go — P1-B §K's CLI parity for the recovery surface.
//
// The existing `ao workflow recover` group stays exactly as it is: those two
// subcommands each discard something AO could not prove, and they are not the
// ordinary way back. What was missing is the ordinary way back — asking what to
// do, doing it, deciding about the plan, and asking for a repair — and every
// one of those answers comes from the daemon. The CLI never re-derives whether
// something is safe; it prints what the daemon decided.
//
//	ao workflow recover status <id>       what should I do about this run
//	ao workflow resume <id>               discharge the outstanding obligation
//	ao workflow repair <id>               launch a bounded Repair Agent
//	ao workflow plan reuse <id>           execute the existing plan revision
//	ao workflow plan regenerate <id>      mint a new plan revision

// workflowRecoveryEnvelope mirrors the recovery response's shape. Hand-mirrored
// on purpose: the CLI is a thin client over the daemon's HTTP routes and does
// not import controller packages (AGENTS.md).
type workflowRecoveryEnvelope struct {
	Recovery struct {
		RecommendedAction string `json:"recommendedAction"`
		ReasonCode        string `json:"reasonCode"`
		Explanation       string `json:"explanation"`
		AutomaticAllowed  bool   `json:"automaticAllowed"`
		PlanReusable      string `json:"planReusable"`
		RepairAvailable   bool   `json:"repairAvailable"`
		RepairEligibility string `json:"repairEligibility"`
		BlockingCondition string `json:"blockingCondition"`
		Obligation        string `json:"obligation"`
		ObligationDetail  string `json:"obligationDetail"`
		Strategy          string `json:"strategy"`
		TargetRunID       string `json:"targetRunId"`
		// Execution is P3-D's technical detail. Absent from an older daemon's
		// response, in which case the technical block simply is not printed.
		Execution *struct {
			StepID         string `json:"stepId"`
			StepKind       string `json:"stepKind"`
			AttemptID      string `json:"attemptId"`
			AttemptNumber  int64  `json:"attemptNumber"`
			Provider       string `json:"provider"`
			SessionID      string `json:"sessionId"`
			LifecycleState string `json:"lifecycleState"`
			Authority      string `json:"authority"`
			StartedAt      string `json:"startedAt"`
			FinishedAt     string `json:"finishedAt"`
			Outcome        string `json:"outcome"`
			ErrorClass     string `json:"errorClass"`
			LastEventPhase string `json:"lastEventPhase"`
			LastEventAt    string `json:"lastEventAt"`
		} `json:"execution"`
	} `json:"recovery"`
	Repair struct {
		Eligibility string `json:"eligibility"`
		Mode        string `json:"mode"`
		Spent       int    `json:"spent"`
		Budget      int    `json:"budget"`
		Reason      string `json:"reason"`
	} `json:"repair"`
	// Status is P3-D's recovery projection. Absent from an older daemon, in
	// which case the recovery block below prints exactly as it always did.
	Status *struct {
		State      string `json:"state"`
		Summary    string `json:"summary"`
		AOIsActing bool   `json:"aoIsActing"`
		Waiting    bool   `json:"waiting"`
		StopReason string `json:"stopReason"`
		Repair     struct {
			Active            bool   `json:"active"`
			Attempt           int    `json:"attempt"`
			Budget            int    `json:"budget"`
			Exhausted         bool   `json:"exhausted"`
			NextRetryPossible bool   `json:"nextRetryPossible"`
			Quiescent         bool   `json:"quiescent"`
			WhyStarted        string `json:"whyStarted"`
		} `json:"repair"`
		Failover []struct {
			AttemptNumber int64  `json:"attemptNumber"`
			Provider      string `json:"provider"`
			Outcome       string `json:"outcome"`
			ErrorClass    string `json:"errorClass"`
		} `json:"failover"`
		Capacity struct {
			Read            bool     `json:"read"`
			Waiting         int      `json:"waiting"`
			Held            int      `json:"held"`
			Kinds           []string `json:"kinds"`
			FossilSuspected bool     `json:"fossilSuspected"`
		} `json:"capacity"`
		Branch struct {
			Branch      string `json:"branch"`
			HeldByRunID string `json:"heldByRunId"`
			Waiting     bool   `json:"waiting"`
		} `json:"branch"`
		Dialog struct {
			State      string `json:"state"`
			Source     string `json:"source"`
			Unreadable bool   `json:"unreadable"`
		} `json:"dialog"`
		RetryCount int64 `json:"retryCount"`
		Timeline   []struct {
			Kind string `json:"kind"`
			At   string `json:"at"`
		} `json:"timeline"`
	} `json:"status"`
}

// workflowAdviceEnvelope mirrors P3-C's advice response. Hand-mirrored for the
// same reason every other envelope in this file is: the CLI is a thin client
// over the daemon's HTTP routes and does not import controller packages.
type workflowAdviceEnvelope struct {
	Advice struct {
		Category                     string `json:"category"`
		Summary                      string `json:"summary"`
		Explanation                  string `json:"explanation"`
		RequiresHuman                bool   `json:"requiresHuman"`
		AutomaticAction              string `json:"automaticAction"`
		AutomaticActionActive        bool   `json:"automaticActionActive"`
		AutomaticActionBlockedReason string `json:"automaticActionBlockedReason"`
		RecommendedAction            string `json:"recommendedAction"`
		ExpectedNextStage            string `json:"expectedNextStage"`
	} `json:"advice"`
}

// workflowResumeEnvelope mirrors the resume response.
type workflowResumeEnvelope struct {
	Workflow struct {
		Run struct {
			ID       string `json:"id"`
			State    string `json:"state"`
			Recovery *struct {
				RecommendedAction string `json:"recommendedAction"`
				Explanation       string `json:"explanation"`
			} `json:"recovery"`
		} `json:"run"`
		Resume *struct {
			Obligation       string `json:"obligation"`
			ObligationDetail string `json:"obligationDetail"`
			Performed        bool   `json:"performed"`
		} `json:"resume"`
		PlanReuse *struct {
			Reusability string `json:"reusability"`
			Revision    int64  `json:"revision"`
			TaskCount   int    `json:"taskCount"`
			Reason      string `json:"reason"`
		} `json:"planReuse"`
	} `json:"workflow"`
}

// workflowRepairEnvelope mirrors the repair response.
type workflowRepairEnvelope struct {
	Repair struct {
		Eligibility string `json:"eligibility"`
		Mode        string `json:"mode"`
		Spent       int    `json:"spent"`
		Budget      int    `json:"budget"`
	} `json:"repair"`
	Intent *struct {
		ID              string `json:"id"`
		TargetRunID     string `json:"targetRunId"`
		ConditionReason string `json:"conditionReason"`
		Generation      int    `json:"generation"`
		RepairRunID     string `json:"repairRunId"`
	} `json:"intent"`
}

func workflowIDArg(args []string) (string, error) {
	runID := strings.TrimSpace(args[0])
	if runID == "" {
		return "", usageError{errors.New("usage: workflow id is required")}
	}
	return runID, nil
}

func newRecoverStatusCommand(ctx *commandContext) *cobra.Command {
	return &cobra.Command{
		Use:   "status <workflow-id>",
		Short: "Ask what should be done about a stopped run, and whether AO may do it itself",
		Long: "Prints the daemon's deterministic recovery assessment: the one recommended action, the canonical stop\n" +
			"reason behind it, what is blocking the run, whether the existing plan can be reused, and whether a\n" +
			"bounded Repair Agent is available.\n\n" +
			"It reads only — nothing here starts, resumes or repairs anything.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			runID, err := workflowIDArg(args)
			if err != nil {
				return err
			}
			var res workflowRecoveryEnvelope
			if err := ctx.getJSON(cmd.Context(), "workflows/"+url.PathEscape(runID)+"/recovery", &res); err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			// P3-C §25: lead with the Advisor's answer to "what do I do now",
			// because that is the question somebody typing this actually has.
			// The P1-B assessment below it is unchanged and still printed in
			// full — it is the operator detail behind the headline, not a
			// competing answer. A daemon that does not serve /advice (an older
			// build) simply prints the assessment as it always did.
			// P3-D §16: the recovery state leads, because "what is AO doing"
			// is the question somebody typing this at a run that will not
			// finish actually has. The Advisor's "what do I do" follows it, and
			// the P1-B assessment follows that — three answers to three
			// different questions, in the order they get asked.
			// Terminal is the one state this line is silent on: the Advisor
			// below already says the run ended, and two sentences saying the
			// same thing is how a status page stops being read.
			if res.Status != nil && res.Status.Summary != "" && res.Status.State != "terminal" {
				_, _ = fmt.Fprintf(out, "%s\n", res.Status.Summary)
			}
			var advice workflowAdviceEnvelope
			if aerr := ctx.getJSON(cmd.Context(), "workflows/"+url.PathEscape(runID)+"/advice", &advice); aerr == nil {
				printWorkflowAdvice(out, advice)
			}
			_, _ = fmt.Fprintf(out, "recommended: %s", res.Recovery.RecommendedAction)
			if res.Recovery.AutomaticAllowed {
				_, _ = fmt.Fprint(out, " (AO can do this itself)")
			} else {
				_, _ = fmt.Fprint(out, " (needs you)")
			}
			_, _ = fmt.Fprintln(out)
			if res.Recovery.ReasonCode != "" {
				_, _ = fmt.Fprintf(out, "reason:      %s\n", res.Recovery.ReasonCode)
			}
			if res.Recovery.Explanation != "" {
				_, _ = fmt.Fprintf(out, "what to do:  %s\n", res.Recovery.Explanation)
			}
			if res.Recovery.BlockingCondition != "" && res.Recovery.BlockingCondition != res.Recovery.Explanation {
				_, _ = fmt.Fprintf(out, "blocked on:  %s\n", res.Recovery.BlockingCondition)
			}
			if res.Recovery.TargetRunID != "" && res.Recovery.TargetRunID != runID {
				_, _ = fmt.Fprintf(out, "act on:      %s (this run is mirroring its stop)\n", res.Recovery.TargetRunID)
			}
			_, _ = fmt.Fprintf(out, "obligation:  %s", res.Recovery.Obligation)
			if res.Recovery.ObligationDetail != "" {
				_, _ = fmt.Fprintf(out, " — %s", res.Recovery.ObligationDetail)
			}
			_, _ = fmt.Fprintln(out)
			_, _ = fmt.Fprintf(out, "plan:        %s\n", res.Recovery.PlanReusable)
			_, _ = fmt.Fprintf(out, "repair:      %s (policy %s, %d of %d spent)",
				res.Repair.Eligibility, res.Repair.Mode, res.Repair.Spent, res.Repair.Budget)
			if res.Repair.Reason != "" {
				_, _ = fmt.Fprintf(out, " — %s", res.Repair.Reason)
			}
			_, _ = fmt.Fprintln(out)
			// P3-D §14/§22: the technical block, under the human answer rather
			// than instead of it. It names the execution the recommendation is
			// about — which attempt, whose provider, on what session, holding
			// what authority — so a stuck run is diagnosable without opening
			// ao.db (§24). Identities and classifications only (§35).
			printWorkflowExecution(out, res)
			printWorkflowRecoveryStatus(out, res)
			return nil
		},
	}
}

// printWorkflowExecution renders the recovery answer's technical detail.
//
// Every line is omitted when the fact behind it is absent, because a printed
// "unknown" for something AO simply did not look at reads as a finding. What it
// never omits is the authority line: "which attempt is allowed to speak for
// this step" is the question the whole projection exists for, and a blank there
// would be the one silence a reader could misread as "fine".
func printWorkflowExecution(out io.Writer, res workflowRecoveryEnvelope) {
	e := res.Recovery.Execution
	if e == nil || e.StepID == "" {
		return
	}
	_, _ = fmt.Fprintln(out, "\nexecution:")
	_, _ = fmt.Fprintf(out, "  step:      %s", e.StepID)
	if e.StepKind != "" {
		_, _ = fmt.Fprintf(out, " (%s)", e.StepKind)
	}
	if e.LifecycleState != "" {
		_, _ = fmt.Fprintf(out, " — %s", e.LifecycleState)
	}
	_, _ = fmt.Fprintln(out)
	if e.AttemptID != "" {
		_, _ = fmt.Fprintf(out, "  attempt:   %s", e.AttemptID)
		if e.AttemptNumber > 0 {
			_, _ = fmt.Fprintf(out, " (#%d)", e.AttemptNumber)
		}
		if e.Provider != "" {
			_, _ = fmt.Fprintf(out, " via %s", e.Provider)
		}
		_, _ = fmt.Fprintln(out)
	}
	_, _ = fmt.Fprintf(out, "  authority: %s\n", orUnknown(e.Authority))
	if e.SessionID != "" {
		_, _ = fmt.Fprintf(out, "  session:   %s\n", e.SessionID)
	}
	if e.StartedAt != "" {
		_, _ = fmt.Fprintf(out, "  dispatched:%s\n", " "+e.StartedAt)
	}
	if e.Outcome != "" {
		_, _ = fmt.Fprintf(out, "  outcome:   %s", e.Outcome)
		if e.ErrorClass != "" {
			_, _ = fmt.Fprintf(out, " (%s)", e.ErrorClass)
		}
		if e.FinishedAt != "" {
			_, _ = fmt.Fprintf(out, " at %s", e.FinishedAt)
		}
		_, _ = fmt.Fprintln(out)
	}
	if e.LastEventPhase != "" {
		_, _ = fmt.Fprintf(out, "  last event:%s", " "+e.LastEventPhase)
		if e.LastEventAt != "" {
			_, _ = fmt.Fprintf(out, " at %s", e.LastEventAt)
		}
		_, _ = fmt.Fprintln(out)
	}
	if res.Repair.Budget > 0 {
		_, _ = fmt.Fprintf(out, "  repair:    attempt %d of %d\n", res.Repair.Spent, res.Repair.Budget)
	}
}

// printWorkflowRecoveryStatus renders the recovery projection: the state, what
// it is waiting for, and the bounded history that got it there.
//
// Every block is omitted when the daemon holds nothing for it. A "capacity: 0
// waiting" line on a run that never queued reads as a finding; its absence
// reads as what it is.
func printWorkflowRecoveryStatus(out io.Writer, res workflowRecoveryEnvelope) {
	s := res.Status
	if s == nil || s.State == "" {
		return
	}
	_, _ = fmt.Fprintln(out, "\nrecovery:")
	_, _ = fmt.Fprintf(out, "  state:     %s", s.State)
	switch {
	case s.State == "terminal":
		// Nobody is the next actor on a run that ended, and saying "you are"
		// would invite somebody to go and do something about it.
		_, _ = fmt.Fprint(out, " (nothing further to do)")
	case s.Waiting:
		_, _ = fmt.Fprint(out, " (a legitimate wait, not a fault)")
	case s.AOIsActing:
		_, _ = fmt.Fprint(out, " (AO is the next actor)")
	default:
		_, _ = fmt.Fprint(out, " (you are the next actor)")
	}
	_, _ = fmt.Fprintln(out)
	if s.StopReason != "" {
		_, _ = fmt.Fprintf(out, "  stopped:   %s\n", s.StopReason)
	}
	if s.Repair.Attempt > 0 || s.Repair.Active {
		_, _ = fmt.Fprintf(out, "  repair:    attempt %d of %d", s.Repair.Attempt, s.Repair.Budget)
		switch {
		case s.Repair.Active:
			_, _ = fmt.Fprint(out, " · running")
		case s.Repair.Quiescent:
			_, _ = fmt.Fprint(out, " · parked (proven unable to write)")
		case s.Repair.Exhausted:
			_, _ = fmt.Fprint(out, " · exhausted")
		}
		if s.Repair.NextRetryPossible {
			_, _ = fmt.Fprint(out, " · another attempt remains")
		}
		if s.Repair.WhyStarted != "" {
			_, _ = fmt.Fprintf(out, " · started for %s", s.Repair.WhyStarted)
		}
		_, _ = fmt.Fprintln(out)
	}
	if len(s.Failover) > 1 {
		for _, a := range s.Failover {
			_, _ = fmt.Fprintf(out, "  provider:  attempt %d %s → %s", a.AttemptNumber, a.Provider,
				orValueCLI(a.Outcome, "running"))
			if a.ErrorClass != "" {
				_, _ = fmt.Fprintf(out, " (%s)", a.ErrorClass)
			}
			_, _ = fmt.Fprintln(out)
		}
	}
	if s.Capacity.Read && (s.Capacity.Waiting > 0 || s.Capacity.Held > 0) {
		_, _ = fmt.Fprintf(out, "  capacity:  %d waiting, %d held", s.Capacity.Waiting, s.Capacity.Held)
		if len(s.Capacity.Kinds) > 0 {
			_, _ = fmt.Fprintf(out, " (%s)", strings.Join(s.Capacity.Kinds, ", "))
		}
		if s.Capacity.FossilSuspected {
			_, _ = fmt.Fprint(out, " · a held claim looks like a fossil")
		}
		_, _ = fmt.Fprintln(out)
	}
	if s.Branch.Branch != "" {
		_, _ = fmt.Fprintf(out, "  branch:    %s", s.Branch.Branch)
		if s.Branch.Waiting && s.Branch.HeldByRunID != "" {
			_, _ = fmt.Fprintf(out, " · waiting, held by %s", s.Branch.HeldByRunID)
		}
		_, _ = fmt.Fprintln(out)
	}
	if s.Dialog.State != "" {
		_, _ = fmt.Fprintf(out, "  question:  %s", s.Dialog.State)
		if s.Dialog.Source != "" {
			_, _ = fmt.Fprintf(out, " (answered by %s)", s.Dialog.Source)
		}
		_, _ = fmt.Fprintln(out)
	}
	if s.RetryCount > 0 {
		_, _ = fmt.Fprintf(out, "  retries:   %d\n", s.RetryCount)
	}
	if len(s.Timeline) > 0 {
		_, _ = fmt.Fprint(out, "  history:  ")
		for i, e := range s.Timeline {
			if i > 0 {
				_, _ = fmt.Fprint(out, " →")
			}
			_, _ = fmt.Fprintf(out, " %s", e.Kind)
		}
		_, _ = fmt.Fprintln(out)
	}
}

// orValueCLI is orUnknown for a caller that has its own fallback word.
func orValueCLI(s, fallback string) string {
	if strings.TrimSpace(s) == "" {
		return fallback
	}
	return s
}

// orUnknown is the one place this file says "unknown", so a missing fact reads
// as a missing fact rather than as a value.
func orUnknown(s string) string {
	if strings.TrimSpace(s) == "" {
		return "unknown"
	}
	return s
}

func newWorkflowResumeCommand(ctx *commandContext) *cobra.Command {
	return &cobra.Command{
		Use:   "resume <workflow-id>",
		Short: "Discharge exactly the run's outstanding durable obligation",
		Long: "Asks the daemon what the run still owes — a worker to dispatch, a review to start, a fix cycle to\n" +
			"deliver, a verification to run, children to converge — and performs only that.\n\n" +
			"Idempotent: repeated calls converge. A worker already running is observed, never launched again; a fix\n" +
			"prompt already delivered is never re-sent. An obligation only a person can discharge (a plan awaiting\n" +
			"manual approval) is reported rather than driven.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			runID, err := workflowIDArg(args)
			if err != nil {
				return err
			}
			var res workflowResumeEnvelope
			if err := ctx.postJSON(cmd.Context(), "workflows/"+url.PathEscape(runID)+"/resume", struct{}{}, &res); err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			obligation, detail, performed := "unknown", "", false
			if res.Workflow.Resume != nil {
				obligation, detail, performed = res.Workflow.Resume.Obligation, res.Workflow.Resume.ObligationDetail, res.Workflow.Resume.Performed
			}
			verb := "was not driven (it is yours to discharge)"
			if performed {
				verb = "was resumed"
			}
			_, _ = fmt.Fprintf(out, "workflow %s is %s — obligation %q %s\n", res.Workflow.Run.ID, res.Workflow.Run.State, obligation, verb)
			if detail != "" {
				_, _ = fmt.Fprintf(out, "  %s\n", detail)
			}
			if r := res.Workflow.Run.Recovery; r != nil && !performed {
				_, err = fmt.Fprintf(out, "  next: %s — %s\n", r.RecommendedAction, r.Explanation)
			}
			return err
		},
	}
}

func newWorkflowRepairCommand(ctx *commandContext) *cobra.Command {
	return &cobra.Command{
		Use:   "repair <workflow-id>",
		Short: "Launch a bounded Repair Agent for a repairable technical stop",
		Long: "Creates a bounded task run against the stopped run's own project, carrying the failing condition and\n" +
			"acceptance criteria AO wrote from it. The repair's output goes through the ordinary independent review\n" +
			"and deterministic verification before anything is adopted.\n\n" +
			"It is refused — with the reason — for any condition AO must not aim a code-writing agent at: unprovable\n" +
			"provenance, an unknown approved HEAD, credentials, external permissions, destructive ambiguity, a policy\n" +
			"refusal, or a stop AO cannot name at all. It is also refused once the run's repair budget is spent.\n\n" +
			"Use `ao workflow recover status <id>` first to see whether it is available.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			runID, err := workflowIDArg(args)
			if err != nil {
				return err
			}
			var res workflowRepairEnvelope
			if err := ctx.postJSON(cmd.Context(), "workflows/"+url.PathEscape(runID)+"/repair", struct{}{}, &res); err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			if res.Intent == nil {
				_, err = fmt.Fprintf(out, "no repair was launched (%s)\n", res.Repair.Eligibility)
				return err
			}
			_, err = fmt.Fprintf(out,
				"repair %d of %d launched for %s (%s) as run %s\n",
				res.Intent.Generation, res.Repair.Budget, res.Intent.TargetRunID, res.Intent.ConditionReason, res.Intent.RepairRunID)
			return err
		},
	}
}

func newWorkflowPlanCommand(ctx *commandContext) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "plan",
		Short: "Decide what happens to an objective's durable plan",
		Long: "Reuse executes the plan that already exists; regenerate replaces it with a new durable revision.\n" +
			"They are separate commands because they are separate decisions, and conflating them is how a stale plan\n" +
			"ends up executing without anybody choosing that.",
	}
	cmd.AddCommand(&cobra.Command{
		Use:   "reuse <workflow-id>",
		Short: "Execute this objective's existing plan revision as it stands",
		Long: "Refused unless the plan's identity and its project context both still hold. A plan whose bytes no longer\n" +
			"match the hash it was approved under cannot be reused at all; one whose project context has moved must be\n" +
			"regenerated or explicitly revalidated first. Task identities, acceptance criteria and write intents are\n" +
			"preserved exactly, and every reused task still goes through review and verification.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			runID, err := workflowIDArg(args)
			if err != nil {
				return err
			}
			var res workflowResumeEnvelope
			if err := ctx.postJSON(cmd.Context(), "workflows/"+url.PathEscape(runID)+"/plan/reuse", struct{}{}, &res); err != nil {
				return err
			}
			revision, tasks := int64(0), 0
			if res.Workflow.PlanReuse != nil {
				revision, tasks = res.Workflow.PlanReuse.Revision, res.Workflow.PlanReuse.TaskCount
			}
			_, err = fmt.Fprintf(cmd.OutOrStdout(),
				"workflow %s is %s — plan revision %d reused as-is (%d tasks)\n",
				res.Workflow.Run.ID, res.Workflow.Run.State, revision, tasks)
			return err
		},
	})
	cmd.AddCommand(&cobra.Command{
		Use:   "regenerate <workflow-id>",
		Short: "Mint a new durable plan revision for this objective",
		Long: "The superseded revision stays on disk and stays auditable; its tasks stop being authoritative, so a child\n" +
			"run bound to one of them can never advance this objective again.\n\n" +
			"Bounded: AO refuses after a small number of revisions per objective, because a fourth decomposition\n" +
			"discovers nothing the third did not. Operator-initiated only — nothing automatic reaches it.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			runID, err := workflowIDArg(args)
			if err != nil {
				return err
			}
			var res workflowResumeEnvelope
			if err := ctx.postJSON(cmd.Context(), "workflows/"+url.PathEscape(runID)+"/plan/regenerate", struct{}{}, &res); err != nil {
				return err
			}
			revision := int64(0)
			if res.Workflow.PlanReuse != nil {
				revision = res.Workflow.PlanReuse.Revision
			}
			_, err = fmt.Fprintf(cmd.OutOrStdout(),
				"workflow %s is %s — plan revision %d is now current; the superseded revision is retained and no longer authoritative\n",
				res.Workflow.Run.ID, res.Workflow.Run.State, revision)
			return err
		},
	})
	return cmd
}

// printWorkflowAdvice writes the Advisor's headline: whether anybody is needed,
// what AO is already doing, and — only when a person really is needed — what
// they have to do.
//
// The three shapes it deliberately keeps apart are the whole point of P3-C: a
// run AO is repairing prints "no action required" and NOT a Repair suggestion,
// a run waiting on capacity prints what it is waiting for and offers nothing,
// and a run that genuinely needs a person prints the imperative.
func printWorkflowAdvice(out io.Writer, env workflowAdviceEnvelope) {
	a := env.Advice
	if a.Summary == "" && a.Category == "" {
		return
	}
	if a.Summary != "" {
		_, _ = fmt.Fprintf(out, "%s\n", a.Summary)
	}
	switch {
	case a.RequiresHuman:
		_, _ = fmt.Fprintln(out, "Human action required.")
		if a.Explanation != "" {
			_, _ = fmt.Fprintf(out, "  %s\n", a.Explanation)
		}
	case a.AutomaticActionActive:
		_, _ = fmt.Fprintf(out, "AO is handling it (%s). No action required.\n", a.AutomaticAction)
	case a.AutomaticAction != "":
		_, _ = fmt.Fprintf(out, "AO will handle it by itself (%s). No action required.\n", a.AutomaticAction)
	default:
		_, _ = fmt.Fprintln(out, "No action required.")
	}
	if a.AutomaticActionBlockedReason != "" {
		_, _ = fmt.Fprintf(out, "  AO is not doing it automatically: %s\n", a.AutomaticActionBlockedReason)
	}
	if a.ExpectedNextStage != "" {
		_, _ = fmt.Fprintf(out, "  next expected stage: %s\n", a.ExpectedNextStage)
	}
	_, _ = fmt.Fprintln(out)
}
