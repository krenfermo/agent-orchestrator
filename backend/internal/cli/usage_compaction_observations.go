package cli

import (
	"fmt"
	"sort"

	"github.com/spf13/cobra"

	"github.com/aoagents/agent-orchestrator/backend/internal/config"
	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	usagepipeline "github.com/aoagents/agent-orchestrator/backend/internal/observe/usage"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite"
)

// usage_compaction_observations.go -- P7.2B2.1: is there evidence yet?
//
// The shadow economic gate's summary-cost prior is UNKNOWN until three
// independent sessions have compacted AND written their end-of-session rollup.
// That is a fact about the corpus, not about the arithmetic, and before this
// command the only way to learn it was to read a JSON column by hand.
//
// It answers three questions and refuses to flatter any of them: how many
// compactions AO has observed, how many of those sessions carry the rollup that
// makes a summary cost measurable, and how many INDEPENDENT sessions therefore
// contribute to the prior -- PER exact (harness, model) cohort, because the gate
// never mixes cohorts. Three boundaries of one session are one sample.
//
// Read-only, like the verdict readback beside it: sqlite.OpenReadOnly takes no
// writable connection, runs no migration and needs no daemon.

func newUsageCompactionObservationsCommand(ctx *commandContext) *cobra.Command {
	var jsonOut bool
	cmd := &cobra.Command{
		Use:   "compaction-observations",
		Short: "Show what compaction evidence AO has, and whether it is enough to price a summary",
		Long: "A compaction's cost is only measurable from two records the harness writes: the boundary that says " +
			"it replaced the conversation, and the once-per-session rollup that says what the session spent. " +
			"This prints how many of each AO holds.\n\n" +
			"The number that matters is the last one: INDEPENDENT SESSIONS. The shadow economic gate needs three " +
			"before it will estimate a summary cost at all, because one session is a point estimate wearing a " +
			"statistic's name and three boundaries of one session are still one sample.\n\n" +
			"Read-only: it opens AO's database without a writable connection and needs no running daemon.",
		Args: noArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return ctx.runUsageCompactionObservations(cmd, jsonOut)
		},
	}
	cmd.Flags().BoolVar(&jsonOut, "json", false, "Output the inventory as JSON")
	return cmd
}

// compactionObservationLine is one session's contribution to the evidence.
type compactionObservationLine struct {
	SessionID                string `json:"sessionId"`
	Harness                  string `json:"harness,omitempty"`
	ModelID                  string `json:"modelId,omitempty"`
	Compactions              int    `json:"compactions"`
	HarnessRollupObserved    bool   `json:"harnessRollupObserved"`
	UnattributedOutputTokens int64  `json:"unattributedOutputTokens"`
	SummaryTokensPerCompact  int64  `json:"summaryTokensPerCompaction"`
	Placeable                bool   `json:"placeableInTime"`
}

func (c *commandContext) runUsageCompactionObservations(cmd *cobra.Command, jsonOut bool) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	store, err := sqlite.OpenReadOnly(cmd.Context(), cfg.DataDir)
	if err != nil {
		return err
	}
	defer func() { _ = store.Close() }()

	reader := usagepipeline.NewCompactionReader(store, nil)
	observations, err := reader.CompactionSummaryObservations(cmd.Context(), "", "")
	if err != nil {
		return err
	}

	// The candidate list already excluded sessions with no compaction and no
	// rollup, so every row here is a COMPLETE summary-cost observation. The
	// boundary count is read separately, because a session can have compacted
	// while its rollup has not been written yet -- a session that is still
	// running is exactly that -- and reporting only the complete ones would hide
	// the gap between "it compacted" and "we can price it".
	lines := make([]compactionObservationLine, 0, len(observations))
	independent := map[string]struct{}{}
	var boundaries int
	for _, o := range observations {
		boundaries += o.Compactions
		independent[o.SessionID] = struct{}{}
		perCompaction := int64(0)
		if o.Compactions > 0 {
			perCompaction = o.UnattributedOutputTokens / int64(o.Compactions)
		}
		lines = append(lines, compactionObservationLine{
			SessionID:                o.SessionID,
			Harness:                  o.Harness,
			ModelID:                  o.ModelID,
			Compactions:              o.Compactions,
			HarnessRollupObserved:    true,
			UnattributedOutputTokens: o.UnattributedOutputTokens,
			SummaryTokensPerCompact:  perCompaction,
			Placeable:                o.ObservedAt != nil,
		})
	}
	sort.SliceStable(lines, func(i, j int) bool { return lines[i].SessionID < lines[j].SessionID })

	cohorts := summaryPriorCohorts(observations)

	if jsonOut {
		return writeJSON(cmd.OutOrStdout(), map[string]any{
			"compactionsObserved":         boundaries,
			"completeSummaryObservations": len(lines),
			"independentSessions":         len(independent),
			"minimumIndependentSessions":  domain.MinSummaryPriorSessions(),
			"cohorts":                     cohorts,
			"sessions":                    lines,
		})
	}
	printCompactionObservations(cmd, boundaries, lines, independent, cohorts)
	return nil
}

// summaryPriorCohort is the estimator's answer for one exact (harness, model).
type summaryPriorCohort struct {
	Harness string `json:"harness"`
	ModelID string `json:"modelId"`
	Known   bool   `json:"summaryPriorKnown"`
	Tokens  int64  `json:"summaryPriorTokens"`
	Samples int    `json:"summaryPriorSamples"`
}

// summaryPriorCohorts asks the estimator once per exact cohort present, rather
// than reimplementing it, so this screen can never disagree with the gate. An
// observation with no identity belongs to no cohort and is reported under an
// empty one, which the estimator always answers UNKNOWN.
func summaryPriorCohorts(observations []domain.CompactionSummarySessionObservation) []summaryPriorCohort {
	type key struct{ harness, model string }
	seen := map[key]struct{}{}
	var keys []key
	for _, o := range observations {
		k := key{o.Harness, o.ModelID}
		if _, ok := seen[k]; ok {
			continue
		}
		seen[k] = struct{}{}
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].harness != keys[j].harness {
			return keys[i].harness < keys[j].harness
		}
		return keys[i].model < keys[j].model
	})
	out := make([]summaryPriorCohort, 0, len(keys))
	for _, k := range keys {
		prior := domain.EstimateSummaryTokens(domain.CompactionSummaryEstimatorInput{
			Harness: k.harness, ModelID: k.model, Observations: observations,
		})
		out = append(out, summaryPriorCohort{
			Harness: k.harness, ModelID: k.model,
			Known: prior.Known, Tokens: prior.Value, Samples: prior.Samples,
		})
	}
	return out
}

func printCompactionObservations(
	cmd *cobra.Command,
	boundaries int,
	lines []compactionObservationLine,
	independent map[string]struct{},
	cohorts []summaryPriorCohort,
) {
	out := cmd.OutOrStdout()
	minimum := domain.MinSummaryPriorSessions()

	_, _ = fmt.Fprintf(out, "compaction evidence\n\n")
	_, _ = fmt.Fprintf(out, "  compactions observed            %d\n", boundaries)
	_, _ = fmt.Fprintf(out, "  complete summary observations   %d  (a boundary AND the session's rollup)\n", len(lines))
	_, _ = fmt.Fprintf(out, "  independent sessions            %d of %d needed\n", len(independent), minimum)

	_, _ = fmt.Fprintf(out, "\n  summary cost upper bound (S), per exact harness + model cohort\n")
	if len(cohorts) == 0 {
		_, _ = fmt.Fprintf(out, "    UNKNOWN  no cohort has any observation, %d independent sessions needed\n", minimum)
	}
	for _, co := range cohorts {
		name := co.Harness + " / " + co.ModelID
		if co.Harness == "" || co.ModelID == "" {
			name = "(no single harness + model: never used)"
		}
		if co.Known {
			_, _ = fmt.Fprintf(out, "    %s\n      KNOWN    %d tokens per compaction, from %d independent sessions\n",
				name, co.Tokens, co.Samples)
		} else {
			_, _ = fmt.Fprintf(out, "    %s\n      UNKNOWN  %d independent session(s) observed, %d needed\n",
				name, co.Samples, minimum)
		}
	}
	_, _ = fmt.Fprintf(out, "    A KNOWN figure is the conservative MAXIMUM across that cohort's sessions. An\n")
	_, _ = fmt.Fprintf(out, "    UNKNOWN cohort makes every verdict for it read summary_cost_unknown: the\n")
	_, _ = fmt.Fprintf(out, "    fail-closed answer, and NOT a reason to lower the minimum or mix cohorts.\n")

	if len(lines) == 0 {
		_, _ = fmt.Fprintf(out, "\n  No session has both compacted and written a rollup yet.\n")
		_, _ = fmt.Fprintf(out, "  New sessions accumulate this automatically: the parser records both records\n")
		_, _ = fmt.Fprintf(out, "  as it reads them. Sessions whose transcript was already consumed past those\n")
		_, _ = fmt.Fprintf(out, "  records before the parser knew to look will never carry them.\n")
		return
	}

	_, _ = fmt.Fprintf(out, "\n  per session\n")
	for _, l := range lines {
		placeable := ""
		if !l.Placeable {
			// Said out loud: an observation that cannot be ordered against a
			// decision is dropped by the estimator's look-ahead filter, so it
			// counts here and not there.
			placeable = "  [NOT placeable in time: the estimator will drop it]"
		}
		_, _ = fmt.Fprintf(out, "    %s  (%s / %s)\n", l.SessionID, l.Harness, l.ModelID)
		_, _ = fmt.Fprintf(out, "      compactions %d, unattributed output %d, summary/compaction %d%s\n",
			l.Compactions, l.UnattributedOutputTokens, l.SummaryTokensPerCompact, placeable)
	}
	_, _ = fmt.Fprintf(out, "\n  Unattributed output is the harness's own output total minus what AO's ledger\n")
	_, _ = fmt.Fprintf(out, "  holds an event for. On a compacting session that residual IS the summarization,\n")
	_, _ = fmt.Fprintf(out, "  reasoning included -- and it is an upper bound on it, never a measurement.\n")
}
