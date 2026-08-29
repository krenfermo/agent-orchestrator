package cli

import (
	"errors"
	"fmt"
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
	} `json:"recovery"`
	Repair struct {
		Eligibility string `json:"eligibility"`
		Mode        string `json:"mode"`
		Spent       int    `json:"spent"`
		Budget      int    `json:"budget"`
		Reason      string `json:"reason"`
	} `json:"repair"`
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
			_, err = fmt.Fprintln(out)
			return err
		},
	}
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
