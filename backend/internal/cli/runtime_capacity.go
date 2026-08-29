package cli

import (
	"errors"
	"fmt"
	"strings"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

// runtime_capacity.go — P1-C's operator commands (§Q).
//
//	ao capacity status        what is running, what is queued, and under which limits
//	ao runtime gc --dry-run   what a sweep WOULD reclaim
//	ao runtime gc             reclaim it
//
// Two deliberate absences. There is no force-delete: every destructive answer
// comes from the daemon's own proofs (ownership, exact incarnation,
// terminality), and a flag that skipped them would be a flag that destroys a
// live session. And there is no client-side classification: the CLI prints
// what the daemon decided, so `--dry-run` and a real sweep run the identical
// predicates and a preview is a true preview.

type capacityStatusEnvelope struct {
	Global struct {
		Limit  int `json:"limit"`
		Held   int `json:"held"`
		Queued int `json:"queued"`
	} `json:"global"`
	PerKind []struct {
		Kind   string `json:"kind"`
		Limit  int    `json:"limit"`
		Held   int    `json:"held"`
		Queued int    `json:"queued"`
	} `json:"perKind"`
	PerWorkflowLimit int                     `json:"perWorkflowLimit"`
	Held             []capacityClaimEnvelope `json:"held"`
	Queued           []capacityClaimEnvelope `json:"queued"`
}

type capacityClaimEnvelope struct {
	Kind                string `json:"kind"`
	WorkflowRunID       string `json:"workflowRunId"`
	WorkflowStepID      string `json:"workflowStepId"`
	LifecycleGeneration int64  `json:"lifecycleGeneration"`
	Priority            int64  `json:"priority"`
	RuntimeHandle       string `json:"runtimeHandle"`
}

type runtimeGCEnvelope struct {
	DryRun            bool `json:"dryRun"`
	Candidates        int  `json:"candidates"`
	Cleaned           int  `json:"cleaned"`
	SkippedLive       int  `json:"skippedLive"`
	SkippedUnprovable int  `json:"skippedUnprovable"`
	SkippedForeign    int  `json:"skippedForeign"`
	Absent            int  `json:"absent"`
	Errors            int  `json:"errors"`
	Findings          []struct {
		Handle        string `json:"handle"`
		InstanceID    string `json:"instanceId"`
		Class         string `json:"class"`
		Disposition   string `json:"disposition"`
		Reason        string `json:"reason"`
		WorkflowRunID string `json:"workflowRunId"`
		Error         string `json:"error"`
	} `json:"findings"`
}

func newCapacityCommand(ctx *commandContext) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "capacity",
		Short: "Inspect AO's runtime execution capacity",
		Long: "This is the MACHINE's capacity — how many agent runtimes AO may run at once — not a provider's rate\n" +
			"limit. A run that is waiting here is waiting for room on this computer.",
	}
	cmd.AddCommand(&cobra.Command{
		Use:   "status",
		Short: "Show configured limits, what holds each slot, and what is queued",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			var res capacityStatusEnvelope
			if err := ctx.getJSON(cmd.Context(), "runtime/capacity", &res); err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			_, _ = fmt.Fprintf(out, "global:       %d/%d held, %d queued\n", res.Global.Held, res.Global.Limit, res.Global.Queued)
			for _, k := range res.PerKind {
				_, _ = fmt.Fprintf(out, "  %-9s %d/%d held, %d queued\n", k.Kind+":", k.Held, k.Limit, k.Queued)
			}
			_, _ = fmt.Fprintf(out, "per-workflow: at most %d slots per workflow (the fairness bound)\n", res.PerWorkflowLimit)
			if len(res.Held) > 0 {
				_, _ = fmt.Fprintln(out, "\nholding:")
				for _, c := range res.Held {
					_, _ = fmt.Fprintf(out, "  %-9s run %s step %s gen %d %s\n",
						c.Kind, c.WorkflowRunID, c.WorkflowStepID, c.LifecycleGeneration, c.RuntimeHandle)
				}
			}
			if len(res.Queued) > 0 {
				_, _ = fmt.Fprintln(out, "\nqueued (in scheduling order):")
				for _, c := range res.Queued {
					_, _ = fmt.Fprintf(out, "  %-9s run %s step %s priority %d\n",
						c.Kind, c.WorkflowRunID, c.WorkflowStepID, c.Priority)
				}
			}
			return nil
		},
	})
	return cmd
}

func newRuntimeCommand(ctx *commandContext) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "runtime",
		Short: "Operate on the runtimes AO owns",
	}
	cmd.AddCommand(newRuntimeGCCommand(ctx))
	return cmd
}

func newRuntimeGCCommand(ctx *commandContext) *cobra.Command {
	var dryRun bool
	cmd := &cobra.Command{
		Use:   "gc",
		Short: "Reclaim the runtimes AO can prove are its own and finished",
		Long: "Destroys only a runtime for which AO holds all three proofs: it owns it (an ownership token, or a\n" +
			"durable capacity claim naming the exact incarnation), it can address that exact incarnation, and the\n" +
			"authority that could still be using it is finished.\n\n" +
			"Anything else is reported and left alone — a session AO cannot attribute, one whose state it could not\n" +
			"read, or one whose name has since been taken by a different session. Unknown is not dead.\n\n" +
			"It destroys runtime resources only. No durable row is deleted: a finished run stays explainable.\n\n" +
			"--dry-run classifies everything and destroys nothing, using the identical predicates.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runRuntimeGC(ctx, cmd, dryRun, nil)
		},
	}
	cmd.Flags().SetNormalizeFunc(func(_ *pflag.FlagSet, name string) pflag.NormalizedName {
		return pflag.NormalizedName(strings.ReplaceAll(name, "_", "-"))
	})
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "Classify every candidate and destroy nothing")
	return cmd
}

// boolCount reports candidates as the "would clean" figure on a dry run, where
// nothing was actually cleaned.
func boolCount(dryRun bool, candidates int) int {
	if dryRun {
		return candidates
	}
	return 0
}

// newWorktreeCommand is P1-D §AE's placement surface.
//
// `gc` here is the SAME sweep `ao runtime gc` runs, against the same endpoint,
// with the same proofs -- exposed under the noun an operator thinking about
// checkouts will reach for. Two commands, one decision: a second sweep with its
// own rules is how two safety models end up disagreeing.
func newWorktreeCommand(ctx *commandContext) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "worktree",
		Short: "Inspect and reclaim the git worktrees AO manages",
		Long: "AO-managed worktrees live under the AO data dir, never inside a user project, and each one belongs to\n" +
			"exactly one task of one workflow.",
	}
	cmd.AddCommand(&cobra.Command{
		Use:   "list",
		Short: "Show every AO-managed worktree and whether it is reclaimable",
		Long: "Runs the placement sweep in dry-run mode and prints what it found. Nothing is removed, and the\n" +
			"classification is the identical one a real sweep uses.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runRuntimeGC(ctx, cmd, true, isWorktreeFinding)
		},
	})
	cmd.AddCommand(newWorktreeGCCommand(ctx))
	return cmd
}

func newWorktreeGCCommand(ctx *commandContext) *cobra.Command {
	var dryRun bool
	cmd := &cobra.Command{
		Use:   "gc",
		Short: "Reclaim AO-managed worktrees whose work provably landed",
		Long: "Removes a worktree only when AO can prove its work is safe elsewhere: the record says integrated, it\n" +
			"names the commit the work landed at, and the workflow run has ended.\n\n" +
			"It never removes a worktree that is active, preserved, failed, or that belongs to a run still going --\n" +
			"a checkout can hold the only copy of an agent's work. Human worktrees are not merely spared: the sweep\n" +
			"walks AO's own records, so it never sees them.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runRuntimeGC(ctx, cmd, dryRun, isWorktreeFinding)
		},
	}
	cmd.Flags().SetNormalizeFunc(func(_ *pflag.FlagSet, name string) pflag.NormalizedName {
		return pflag.NormalizedName(strings.ReplaceAll(name, "_", "-"))
	})
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "Classify every worktree and remove nothing")
	return cmd
}

// isWorktreeFinding narrows which findings are PRINTED under `ao worktree`.
//
// It never narrows what the sweep did. The daemon decides what is reclaimable;
// a filter that changed that would be the CLI deciding safety, which is exactly
// what must not happen.
func isWorktreeFinding(class string) bool {
	return strings.HasSuffix(class, "_worktree")
}

// runRuntimeGC drives the one sweep both `ao runtime gc` and `ao worktree gc`
// use. Having a single caller is the point: a preview and a real sweep, and a
// runtime view and a worktree view, are the same decision seen through
// different filters rather than different code that could disagree.
func runRuntimeGC(ctx *commandContext, cmd *cobra.Command, dryRun bool, include func(class string) bool) error {
	var res runtimeGCEnvelope
	body := struct {
		DryRun bool `json:"dryRun"`
	}{DryRun: dryRun}
	if err := ctx.postJSON(cmd.Context(), "runtime/gc", body, &res); err != nil {
		return err
	}
	out := cmd.OutOrStdout()
	verb := "cleaned"
	if res.DryRun {
		verb = "would clean"
	}
	_, _ = fmt.Fprintf(out, "%s %d of %d candidates\n", verb, res.Cleaned+boolCount(res.DryRun, res.Candidates), res.Candidates)
	_, _ = fmt.Fprintf(out, "skipped: %d live, %d unprovable, %d foreign, %d already gone, %d errors\n",
		res.SkippedLive, res.SkippedUnprovable, res.SkippedForeign, res.Absent, res.Errors)
	for _, f := range res.Findings {
		if include != nil && !include(f.Class) {
			continue
		}
		line := fmt.Sprintf("  %-10s %-28s %s", f.Disposition, f.Handle, f.Reason)
		if f.Error != "" {
			line += " (" + f.Error + ")"
		}
		_, _ = fmt.Fprintln(out, strings.TrimRight(line, " "))
	}
	if res.Errors > 0 {
		return errors.New("runtime gc: some candidates could not be handled; see the findings above")
	}
	return nil
}
