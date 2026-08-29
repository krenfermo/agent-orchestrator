package cli

import (
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

// workflow_recover.go — the operator's two P0-B recovery actions.
//
// Both exist because AO deliberately FAILS CLOSED somewhere, and a fail-closed
// stop with no way out is a run that is permanently useless. Neither of them
// weakens the refusal that produced the stop: each discards something AO could
// not prove and re-enters the ordinary path, on a person's explicit say-so, with
// a bound so that even a person holding the button cannot loop.
//
//	ao workflow recover review-provenance <workflow-id>
//	    AO cannot prove which commit the approved review target was read at, and
//	    could not reconstruct it from the branch's history. This DISCARDS that
//	    approval and asks for exactly one fresh independent review of what is in
//	    the worktree now. It never attests a commit and never verifies code no
//	    reviewer has read.
//
//	ao workflow recover plan <workflow-id> --observed-plan-updated-at <ts>
//	    The planner was in flight when the daemon restarted, so AO cannot say
//	    whether it produced a plan. This reopens planning FROM SCRATCH; nothing
//	    is adopted from the discarded planner. The timestamp is the plan row
//	    version your view was rendered from, and a reopen of a state that has
//	    since changed is refused rather than accepted.
//
// They are deliberately not folded into `ao workflow continue`: continue is also
// what the daemon's own wake poller drives, and an automatic caller reaching
// either of these turns a fail-closed stop into an unattended loop.

// workflowRunEnvelope is the shared response shape both routes return.
type workflowRunEnvelope struct {
	Workflow struct {
		ID    string `json:"id"`
		State string `json:"state"`
		Plan  *struct {
			Status        string `json:"status"`
			CommandStatus string `json:"commandStatus"`
			UpdatedAt     string `json:"updatedAt"`
		} `json:"plan"`
	} `json:"workflow"`
}

// newWorkflowCommand is the operator-facing workflow group. It carries only the
// recovery actions today; the ordinary lifecycle is driven from the app.
func newWorkflowCommand(ctx *commandContext) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "workflow",
		Short: "Operate on AO workflow runs",
	}
	cmd.AddCommand(newWorkflowRecoverCommand(ctx))
	// P1-B: the ordinary way back, alongside the two fail-closed recoveries.
	cmd.AddCommand(newWorkflowResumeCommand(ctx))
	cmd.AddCommand(newWorkflowRepairCommand(ctx))
	cmd.AddCommand(newWorkflowPlanCommand(ctx))
	// P1-D: where the work happens, and why it has not launched.
	cmd.AddCommand(newWorkflowPlacementCommand(ctx))
	return cmd
}

func newWorkflowRecoverCommand(ctx *commandContext) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "recover",
		Short: "Recover a workflow run AO has deliberately stopped on something it cannot prove",
		Long: "Each subcommand discards exactly one unprovable fact and re-enters the ordinary path.\n" +
			"None of them infers the missing fact, and none of them skips a review or a verification.",
	}
	cmd.AddCommand(newRecoverStatusCommand(ctx))
	cmd.AddCommand(newRecoverReviewProvenanceCommand(ctx))
	cmd.AddCommand(newRecoverPlanCommand(ctx))
	return cmd
}

func newRecoverReviewProvenanceCommand(ctx *commandContext) *cobra.Command {
	return &cobra.Command{
		Use:   "review-provenance <workflow-id>",
		Short: "Discard an approval whose commit AO cannot prove, and ask for one fresh review of the live workspace",
		Long: "Use this when a run is stopped on verify_approved_head_unprovable: AO holds an approved review target\n" +
			"but no durable record of the commit it was read at, and could not recover one from the branch's history.\n\n" +
			"AO will NOT invent that commit. This discards the unlocatable approval and asks for exactly one fresh,\n" +
			"independent review of whatever is in the worktree now — the code is reviewed before it is verified,\n" +
			"exactly as it would have been otherwise. Bounded: AO refuses after a small number of these per run.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			runID := strings.TrimSpace(args[0])
			if runID == "" {
				return usageError{errors.New("usage: workflow id is required")}
			}
			var res workflowRunEnvelope
			path := "workflows/" + url.PathEscape(runID) + "/recover/review-provenance"
			if err := ctx.postJSON(cmd.Context(), path, struct{}{}, &res); err != nil {
				return err
			}
			_, err := fmt.Fprintf(cmd.OutOrStdout(),
				"workflow %s is %s — the unlocatable approval was discarded and one fresh review of the current workspace is due\n",
				res.Workflow.ID, res.Workflow.State)
			return err
		},
	}
}

type recoverPlanOptions struct {
	observedUpdatedAt string
}

func newRecoverPlanCommand(ctx *commandContext) *cobra.Command {
	var opts recoverPlanOptions
	cmd := &cobra.Command{
		Use:   "plan <workflow-id>",
		Short: "Reopen planning for an objective whose planner was interrupted by a daemon restart",
		Long: "Use this when an objective is stopped on planner_ambiguous: the planner command was running when the\n" +
			"daemon restarted, so AO cannot prove whether it produced a plan.\n\n" +
			"Planning starts over from scratch — nothing is adopted from the discarded planner, because adopting a\n" +
			"plan AO cannot verify would put fabricated work under a real objective.\n\n" +
			"--observed-plan-updated-at is REQUIRED and is the plan's updatedAt as you read it (`ao workflow get`\n" +
			"shows it). It says \"this is the row I looked at\": if anything has written to the plan since, the reopen\n" +
			"is refused and you re-read and try again. Bounded: AO refuses after a small number of these per run.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			runID := strings.TrimSpace(args[0])
			if runID == "" {
				return usageError{errors.New("usage: workflow id is required")}
			}
			observed := strings.TrimSpace(opts.observedUpdatedAt)
			if observed == "" {
				return usageError{errors.New("usage: --observed-plan-updated-at is required (the plan's updatedAt as your view read it)")}
			}
			if _, err := time.Parse(time.RFC3339Nano, observed); err != nil {
				return usageError{fmt.Errorf("usage: --observed-plan-updated-at must be an RFC3339 timestamp: %w", err)}
			}
			var res workflowRunEnvelope
			path := "workflows/" + url.PathEscape(runID) + "/plan/reopen"
			body := struct {
				ObservedPlanUpdatedAt string `json:"observedPlanUpdatedAt"`
			}{ObservedPlanUpdatedAt: observed}
			if err := ctx.postJSON(cmd.Context(), path, body, &res); err != nil {
				return err
			}
			status := "unknown"
			if res.Workflow.Plan != nil {
				status = res.Workflow.Plan.Status
			}
			_, err := fmt.Fprintf(cmd.OutOrStdout(),
				"workflow %s is %s — planning reopened (plan %s); nothing was adopted from the interrupted planner\n",
				res.Workflow.ID, res.Workflow.State, status)
			return err
		},
	}
	// Agents routinely spell flags with underscores; normalize both spellings,
	// mirroring incident.go and decision.go.
	cmd.Flags().SetNormalizeFunc(func(_ *pflag.FlagSet, name string) pflag.NormalizedName {
		return pflag.NormalizedName(strings.ReplaceAll(name, "_", "-"))
	})
	cmd.Flags().StringVar(&opts.observedUpdatedAt, "observed-plan-updated-at", "",
		"The plan's updatedAt as your view read it, RFC3339 (required)")
	return cmd
}
