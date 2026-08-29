package cli

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"
)

// workflow_placement.go — P1-D §P's operator commands.
//
//	ao workflow placement <id>   where this run's work happens, and why it has not launched
//	ao provider attempts <id>    which providers have been tried, and what each proved
//
// Both are read-only, and deliberately so. There is no command that re-points a
// placement, forces a failover, or clears a wait: every one of those would be a
// way to aim a running agent at a different checkout, or to start a second
// provider over a state AO refused to call safe. The operator affordance this
// phase adds is being able to SEE the refusal, not to override it.
//
// No client-side classification either. The waiting reason printed here is the
// one the daemon decided, from the same durable rows admission used — so what
// an operator reads is what actually happened, not a second opinion computed
// from a projection.

type workflowPlacementEnvelope struct {
	Placements []struct {
		Type                string `json:"type"`
		PlacementGeneration int64  `json:"placementGeneration"`
		LifecycleGeneration int64  `json:"lifecycleGeneration"`
		State               string `json:"state"`
		Provenance          string `json:"provenance"`
		TaskID              string `json:"taskId"`
		RepoPath            string `json:"repoPath"`
		BaseBranch          string `json:"baseBranch"`
		BaseSHA             string `json:"baseSha"`
		ExecutionBranch     string `json:"executionBranch"`
		WorktreePath        string `json:"worktreePath"`
		MergeTarget         string `json:"mergeTarget"`
		IntegratedSHA       string `json:"integratedSha"`
		Detail              string `json:"detail"`
		Current             bool   `json:"current"`
	} `json:"placements"`
	ProviderAttempts []struct {
		ID                  string `json:"id"`
		Ordinal             int64  `json:"ordinal"`
		Provider            string `json:"provider"`
		State               string `json:"state"`
		Safety              string `json:"safety"`
		FailureClass        string `json:"failureClass"`
		FailureReason       string `json:"failureReason"`
		LifecycleGeneration int64  `json:"lifecycleGeneration"`
		PlacementGeneration int64  `json:"placementGeneration"`
		RuntimeSessionID    string `json:"runtimeSessionId"`
		MutationEvidence    string `json:"mutationEvidence"`
		SuccessorAttemptID  string `json:"successorAttemptId"`
		Authoritative       bool   `json:"authoritative"`
	} `json:"providerAttempts"`
	Admission struct {
		WaitingReason       string `json:"waitingReason"`
		Detail              string `json:"detail"`
		AutoResume          bool   `json:"autoResume"`
		SpendsRetryBudget   bool   `json:"spendsRetryBudget"`
		PlacementReady      bool   `json:"placementReady"`
		PlacementState      string `json:"placementState"`
		PlacementGeneration int64  `json:"placementGeneration"`
		CapacityClaimID     string `json:"capacityClaimId"`
		CurrentAttemptID    string `json:"currentAttemptId"`
	} `json:"admission"`
}

func newWorkflowPlacementCommand(ctx *commandContext) *cobra.Command {
	return &cobra.Command{
		Use:   "placement <workflow-id>",
		Short: "Show where this run's work happens, and why it has not launched",
		Long: "The execution placement is FROZEN: AO decided it once, before anything was mutated, and does not\n" +
			"re-derive it from the project's configuration afterwards. Changing the project's execution mode\n" +
			"therefore does not move a run that is already going.\n\n" +
			"The waiting reason names the authority that is withholding the launch — capacity, branch, placement,\n" +
			"provider or dependency — never a generic 'waiting'. None of them spends the run's retry budget.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			id, err := workflowIDArg(args)
			if err != nil {
				return err
			}
			var res workflowPlacementEnvelope
			if err := ctx.getJSON(cmd.Context(), "workflows/"+id+"/placement", &res); err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			if len(res.Placements) == 0 {
				_, _ = fmt.Fprintln(out, "no execution placement has been frozen for this run yet")
			}
			for _, p := range res.Placements {
				marker := "  "
				if p.Current {
					marker = "* "
				}
				_, _ = fmt.Fprintf(out, "%splacement gen %d  %-18s %-11s (%s)\n",
					marker, p.PlacementGeneration, p.Type, p.State, p.Provenance)
				if p.TaskID != "" {
					_, _ = fmt.Fprintf(out, "    task:      %s\n", p.TaskID)
				}
				_, _ = fmt.Fprintf(out, "    repo:      %s\n", p.RepoPath)
				_, _ = fmt.Fprintf(out, "    branch:    %s  (base %s", p.ExecutionBranch, p.BaseBranch)
				if p.BaseSHA != "" {
					_, _ = fmt.Fprintf(out, " @ %s", shortSHAForDisplay(p.BaseSHA))
				}
				_, _ = fmt.Fprintf(out, ")\n")
				if p.WorktreePath != "" {
					_, _ = fmt.Fprintf(out, "    worktree:  %s\n", p.WorktreePath)
				}
				if p.MergeTarget != "" {
					_, _ = fmt.Fprintf(out, "    target:    %s\n", p.MergeTarget)
				}
				if p.IntegratedSHA != "" {
					_, _ = fmt.Fprintf(out, "    landed at: %s\n", shortSHAForDisplay(p.IntegratedSHA))
				}
				if p.Detail != "" {
					_, _ = fmt.Fprintf(out, "    detail:    %s\n", p.Detail)
				}
			}

			_, _ = fmt.Fprintln(out, "\nadmission:")
			if res.Admission.WaitingReason == "" {
				_, _ = fmt.Fprintln(out, "  not waiting on admission")
			} else {
				_, _ = fmt.Fprintf(out, "  waiting:   %s\n", res.Admission.WaitingReason)
				if res.Admission.Detail != "" {
					_, _ = fmt.Fprintf(out, "  because:   %s\n", res.Admission.Detail)
				}
				if res.Admission.AutoResume {
					_, _ = fmt.Fprintln(out, "  resumes:   automatically, once that clears")
				} else {
					_, _ = fmt.Fprintln(out, "  resumes:   only after somebody acts — waiting will not clear this")
				}
				_, _ = fmt.Fprintln(out, "  retries:   none spent (waiting is never charged against the retry budget)")
			}
			_, _ = fmt.Fprintf(out, "  placement: ready=%t state=%s gen=%d\n",
				res.Admission.PlacementReady, orDash(res.Admission.PlacementState), res.Admission.PlacementGeneration)
			if res.Admission.CapacityClaimID != "" {
				_, _ = fmt.Fprintf(out, "  capacity:  claim %s\n", res.Admission.CapacityClaimID)
			}
			if res.Admission.CurrentAttemptID != "" {
				_, _ = fmt.Fprintf(out, "  provider:  attempt %s\n", res.Admission.CurrentAttemptID)
			}
			return nil
		},
	}
}

func newProviderCommand(ctx *commandContext) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "provider",
		Short: "Inspect how AO is trying to discharge a run's work",
	}
	cmd.AddCommand(&cobra.Command{
		Use:   "attempts <workflow-id>",
		Short: "Show which providers have been tried for a run, and what each one proved",
		Long: "A provider attempt is not a task generation. Failing over from one provider to another keeps the\n" +
			"same obligation, the same lifecycle generation and the same frozen placement — only the attempt\n" +
			"changes. The safety class on each attempt is what AO could PROVE about whether that provider\n" +
			"touched anything; AO only ever fails over from the two proven classes.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			id, err := workflowIDArg(args)
			if err != nil {
				return err
			}
			var res workflowPlacementEnvelope
			if err := ctx.getJSON(cmd.Context(), "workflows/"+id+"/placement", &res); err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			if len(res.ProviderAttempts) == 0 {
				_, _ = fmt.Fprintln(out, "no provider attempts recorded for this run")
				return nil
			}
			for _, a := range res.ProviderAttempts {
				marker := "  "
				if a.Authoritative {
					marker = "* "
				}
				_, _ = fmt.Fprintf(out, "%s#%d %-8s %-18s lifecycle gen %d, placement gen %d\n",
					marker, a.Ordinal, a.Provider, a.State, a.LifecycleGeneration, a.PlacementGeneration)
				if a.Safety != "" {
					_, _ = fmt.Fprintf(out, "     safety:   %s\n", a.Safety)
				}
				if a.FailureReason != "" {
					_, _ = fmt.Fprintf(out, "     failure:  %s (%s)\n", a.FailureReason, orDash(a.FailureClass))
				}
				if a.MutationEvidence != "" {
					_, _ = fmt.Fprintf(out, "     proof:    %s\n", a.MutationEvidence)
				}
				if a.RuntimeSessionID != "" {
					_, _ = fmt.Fprintf(out, "     runtime:  %s\n", a.RuntimeSessionID)
				}
				if a.SuccessorAttemptID != "" {
					_, _ = fmt.Fprintf(out, "     failed over to: %s\n", a.SuccessorAttemptID)
				}
			}
			return nil
		},
	})
	return cmd
}

func orDash(s string) string {
	if strings.TrimSpace(s) == "" {
		return "-"
	}
	return s
}

func shortSHAForDisplay(sha string) string {
	if len(sha) > 12 {
		return sha[:12]
	}
	return sha
}
