package cli

import (
	"fmt"
	"sort"
	"strings"

	"github.com/spf13/cobra"

	"github.com/aoagents/agent-orchestrator/backend/internal/config"
	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite"
	"github.com/aoagents/agent-orchestrator/backend/internal/workflow"
)

// usage_compaction_verdicts.go -- reading P7.2B2's shadow cohort back.
//
// The shadow economic gate writes one durable verdict per COMPACT decision and
// nothing acts on any of them. A cohort nobody can interrogate is a table rather
// than evidence, so this is the question-answering side: how many evaluations,
// how many of each verdict, why, what break-even was computed, what saving was
// estimated, and whether the whole cohort was priced in the right currency.
//
// WHY IT OPENS THE DATABASE AND WHY THAT IS SAFE HERE. Every other command in
// this package talks to the running daemon's loopback API, and that rule exists
// because the daemon is the sole WRITER. This reads: sqlite.OpenReadOnly takes no
// writable connection, creates no directory and runs no migration, so it is safe
// beside a live daemon and safe with none -- which matters, because the operator
// asking "what would the gate have done" is usually asking after the run, when
// there may be no daemon to ask.
//
// It is a read and only a read. Nothing here evaluates a verdict, writes one,
// re-computes one, or changes anything about a run.

func newUsageCompactionVerdictsCommand(ctx *commandContext) *cobra.Command {
	var jsonOut bool
	cmd := &cobra.Command{
		Use:   "compaction-verdicts <workflow-run-id>",
		Short: "Show what the shadow economic gate would have recommended for a run",
		Long: "AO computes, at every point where it decides to compact a session's conversation, whether doing so " +
			"would actually have paid for itself -- and then ignores the answer. This prints those answers.\n\n" +
			"A compaction is a purchase: an output-priced summary plus a cache-write-priced rewrite now, in " +
			"exchange for a cache-read-priced discount on every call after. Whether it pays depends on how many " +
			"calls come after, so each verdict carries the break-even call count it computed, the forecast it " +
			"compared against, and the provenance of every estimate behind both.\n\n" +
			"NOTHING ACTS ON THESE VERDICTS. wouldHaveActed is false on every row by construction. A COMPACT " +
			"verdict did not cause a compaction and a SKIP verdict did not prevent one.\n\n" +
			"Read-only: it opens AO's database without a writable connection and needs no running daemon.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			runID := strings.TrimSpace(args[0])
			if runID == "" {
				return usageError{fmt.Errorf("a workflow run id is required")}
			}
			return ctx.runUsageCompactionVerdicts(cmd, runID, jsonOut)
		},
	}
	cmd.Flags().BoolVar(&jsonOut, "json", false, "Output the cohort as JSON")
	return cmd
}

func (c *commandContext) runUsageCompactionVerdicts(cmd *cobra.Command, runID string, jsonOut bool) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	store, err := sqlite.OpenReadOnly(cmd.Context(), cfg.DataDir)
	if err != nil {
		return err
	}
	defer func() { _ = store.Close() }()

	checkpoints, err := store.ListWorkflowCheckpoints(cmd.Context(), runID)
	if err != nil {
		return err
	}
	var records []domain.CompactionEconomicsRecord
	undecodable := 0
	for _, cp := range checkpoints {
		if cp.DurablePhase != workflow.CompactionEconomicsDurablePhase {
			continue
		}
		rec, ok := workflow.DecodeCompactionEconomicsRecord(cp.RetryState)
		if !ok {
			// Counted, never dropped silently: a payload this build cannot read
			// is a gap in the cohort and the reader has to know its size.
			undecodable++
			continue
		}
		records = append(records, rec)
	}
	summary := domain.SummarizeCompactionEconomics(records)
	if jsonOut {
		return writeJSON(cmd.OutOrStdout(), map[string]any{
			"workflowRunId":      runID,
			"summary":            summary,
			"verdicts":           records,
			"undecodableRecords": undecodable,
			"enforcement":        false,
		})
	}
	printCompactionVerdicts(cmd, runID, records, summary, undecodable)
	return nil
}

func printCompactionVerdicts(
	cmd *cobra.Command,
	runID string,
	records []domain.CompactionEconomicsRecord,
	summary domain.CompactionEconomicsSummary,
	undecodable int,
) {
	out := cmd.OutOrStdout()
	_, _ = fmt.Fprintf(out, "shadow economic verdicts -- run %s\n\n", runID)
	if summary.Evaluations == 0 {
		_, _ = fmt.Fprintf(out, "  no evaluations recorded\n\n")
		_, _ = fmt.Fprintf(out, "  A verdict is written only where the lifecycle decision said COMPACT. A run that\n")
		_, _ = fmt.Fprintf(out, "  never reached that decision has nothing to shadow, which is not the same as a run\n")
		_, _ = fmt.Fprintf(out, "  the gate could not evaluate -- the latter records UNKNOWN and appears here.\n")
		if undecodable > 0 {
			_, _ = fmt.Fprintf(out, "\n  %d record(s) could not be decoded by this build.\n", undecodable)
		}
		return
	}

	_, _ = fmt.Fprintf(out, "  evaluations   %d\n", summary.Evaluations)
	_, _ = fmt.Fprintf(out, "    COMPACT     %d  (recommended; NOT acted on)\n", summary.Compact)
	_, _ = fmt.Fprintf(out, "    SKIP        %d\n", summary.Skip)
	_, _ = fmt.Fprintf(out, "    UNKNOWN     %d\n", summary.Unknown)
	if undecodable > 0 {
		_, _ = fmt.Fprintf(out, "    undecodable %d\n", undecodable)
	}

	_, _ = fmt.Fprintf(out, "\n  why\n")
	for _, line := range sortedCounts(reasonCounts(summary.ByReason)) {
		_, _ = fmt.Fprintf(out, "    %-26s %d\n", line.key, line.n)
	}

	_, _ = fmt.Fprintf(out, "\n  pricing\n")
	if coverage, ok := summary.PricingCoverage(); ok {
		_, _ = fmt.Fprintf(out, "    priceable                  %d of %d (%.1f%%)\n",
			summary.PricedEvaluations, summary.Evaluations, coverage)
	}
	for _, line := range sortedCounts(pricingCounts(summary.ByPricingStatus)) {
		_, _ = fmt.Fprintf(out, "    %-26s %d\n", line.key, line.n)
	}
	// The P7.2B1 honesty check, printed rather than implied: a Claude cohort
	// priced mostly at the five-minute cache-write rate is measured in the wrong
	// currency, because every write AO has ever metered was created at one hour.
	_, _ = fmt.Fprintf(out, "    cache write priced at 1h   %d\n", summary.CacheWriteLifetime1h)
	_, _ = fmt.Fprintf(out, "    cache write priced at 5m   %d\n", summary.CacheWriteLifetime5m)

	if summary.BreakEvenComputed > 0 {
		_, _ = fmt.Fprintf(out, "\n  break-even calls (N*), over the %d verdicts that reached the arithmetic\n",
			summary.BreakEvenComputed)
		_, _ = fmt.Fprintf(out, "    min %.1f   mean %.1f   max %.1f\n",
			summary.BreakEvenMin, summary.BreakEvenMean, summary.BreakEvenMax)
	}
	if summary.Compact > 0 {
		_, _ = fmt.Fprintf(out, "\n  estimated net saving across COMPACT verdicts  %s\n",
			formatMicros(summary.CompactNetSavingsMicros))
		_, _ = fmt.Fprintf(out, "    MODELLED, not realized: no compaction happened because of these.\n")
	}
	if summary.SkipNearMisses > 0 {
		_, _ = fmt.Fprintf(out, "\n  near-miss skips (margin above %.1f)  %d\n",
			domain.NearMissMarginFloor, summary.SkipNearMisses)
		_, _ = fmt.Fprintf(out, "    These are the skips the safety factor made rather than the arithmetic.\n")
	}
	if summary.TerminalCycleSkips > 0 {
		_, _ = fmt.Fprintf(out, "\n  terminal-cycle skips  %d  (structural; no estimate was needed)\n",
			summary.TerminalCycleSkips)
	}
	if len(summary.GateVersions) > 1 || len(summary.EstimatorVersions) > 1 {
		_, _ = fmt.Fprintf(out, "\n  WARNING: this cohort spans more than one gate or estimator version.\n")
		_, _ = fmt.Fprintf(out, "  Split it before computing any rate over it; two arithmetics do not pool.\n")
	}

	_, _ = fmt.Fprintf(out, "\n  per evaluation\n")
	for _, r := range records {
		_, _ = fmt.Fprintf(out, "    cycle %d  %s/%s\n", r.Cycle, r.Verdict, r.Reason)
		_, _ = fmt.Fprintf(out, "      context %d -> %d (est), threshold %d, prefix %d, prompt %d\n",
			r.ContextBeforeTokens, r.EstimatedContextAfterTokens, r.ContextThresholdTokens,
			r.StablePrefixTokens, r.PromptTokens)
		if r.BreakEvenCallsKnown {
			_, _ = fmt.Fprintf(out, "      break-even %.1f calls x safety %.2f = %.1f required; forecast %d (margin %.2f)\n",
				r.BreakEvenCalls, r.SafetyFactor, r.RequiredCalls, r.EstimatedRemainingCalls, r.Margin)
			_, _ = fmt.Fprintf(out, "      cost %s, future saving %s, net %s\n",
				formatMicros(r.EstimatedCompactionCostMicros),
				formatMicros(r.EstimatedFutureSavingsMicros),
				formatMicros(r.EstimatedNetSavingsMicros))
		}
		_, _ = fmt.Fprintf(out, "      estimates: A=%s(%d) S=%s(%d) N=%s(%d)%s\n",
			r.ContextAfterBasis, r.ContextAfterSamples,
			r.SummaryBasis, r.SummarySamples,
			r.RemainingCallsBasis, r.RemainingCallsSamples,
			capNote(r.RemainingCallsCapApplied))
		_, _ = fmt.Fprintf(out, "      gate %s, estimators %s, pricing %s\n",
			r.GateVersion, r.EstimatorVersion, pricingProvenance(r))
	}
	_, _ = fmt.Fprintf(out, "\n  Nothing above changed what this run did. The shadow gate governs nothing.\n")
	if summary.Skip > 0 {
		// Said out loud because the opposite is the easiest wrong conclusion to
		// draw from this screen. A SKIP on a compaction that then did not happen
		// has NO observed counterfactual: AO never learns what the post-compaction
		// context or the summary would have been, so the skip is predicted correct
		// and never measured correct. The margin beside each one is a bound on how
		// wrong it could be, not a measurement of how wrong it was.
		_, _ = fmt.Fprintf(out, "  A SKIP whose compaction did not happen has no observed counterfactual:\n")
		_, _ = fmt.Fprintf(out, "  it is predicted correct, never measured correct. Read the margin as a bound.\n")
	}
}

type countLine struct {
	key string
	n   int
}

func reasonCounts(in map[domain.CompactionEconomicReason]int) []countLine {
	out := make([]countLine, 0, len(in))
	for k, n := range in {
		out = append(out, countLine{key: string(k), n: n})
	}
	return out
}

func pricingCounts(in map[domain.CompactionPricingStatus]int) []countLine {
	out := make([]countLine, 0, len(in))
	for k, n := range in {
		out = append(out, countLine{key: string(k), n: n})
	}
	return out
}

// sortedCounts orders by descending count then by key, so two runs of the same
// cohort print identically and a diff of two outputs is meaningful.
func sortedCounts(in []countLine) []countLine {
	sort.SliceStable(in, func(i, j int) bool {
		if in[i].n != in[j].n {
			return in[i].n > in[j].n
		}
		return in[i].key < in[j].key
	})
	return in
}

func capNote(applied bool) string {
	if applied {
		return " [N capped]"
	}
	return ""
}

func pricingProvenance(r domain.CompactionEconomicsRecord) string {
	if r.PricingSource == "" {
		return string(r.PricingStatus)
	}
	return fmt.Sprintf("%s %s (%s, write@%s)", r.PricingSource, r.PricingVersion, r.Currency, r.CacheWriteLifetimePriced)
}

// formatMicros renders integer micros as currency. Micros are what the record
// stores -- a float in a durable payload makes two builds' records incomparable
// -- so the conversion happens here, at the edge, once.
func formatMicros(micros int64) string {
	sign := ""
	if micros < 0 {
		sign, micros = "-", -micros
	}
	return fmt.Sprintf("%s$%d.%06d", sign, micros/1_000_000, micros%1_000_000)
}
