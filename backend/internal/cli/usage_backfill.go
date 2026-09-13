package cli

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/aoagents/agent-orchestrator/backend/internal/config"
	"github.com/aoagents/agent-orchestrator/backend/internal/runfile"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite"
	"github.com/aoagents/agent-orchestrator/backend/internal/usagebackfill"
)

// usage_backfill.go -- the one maintenance command that opens the store
// directly, and the reasons it is allowed to.
//
// This package's rule is that commands talk to the running daemon's loopback
// API and never to SQLite. `ao import` is the standing exception and this is
// the second, for the same reason: the daemon is the sole writer of the
// database, so an operation that rewrites rows underneath it cannot be done
// through it. It refuses to run while a daemon is alive, exactly as `ao import`
// does.
//
// DRY RUN IS THE DEFAULT AND --apply IS THE ONLY WAY TO WRITE. A maintenance
// command whose default mutates is one typo away from an incident, and this one
// touches the ledger.

type usageBackfillOptions struct {
	apply bool
	json  bool
}

func newUsageCommand(ctx *commandContext) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "usage",
		Short: "Maintain AO's token usage ledger",
	}
	cmd.AddCommand(newUsageBackfillCacheTTLCommand(ctx))
	return cmd
}

func newUsageBackfillCacheTTLCommand(ctx *commandContext) *cobra.Command {
	var opts usageBackfillOptions
	cmd := &cobra.Command{
		Use:   "backfill-cache-ttl",
		Short: "Recover the cache-creation lifetime of historical usage events",
		Long: "Creating a cache entry that lives for an hour costs more than creating one that " +
			"lives five minutes, and the provider reports which it made on every call. Events " +
			"recorded before AO stored that distinction (migration 0170) carry no lifetime, so " +
			"every cost computed from them prices the dear kind at the cheap kind's rate.\n\n" +
			"This command reads it back from the transcripts AO already has, using AO's own " +
			"parser, matching each event by its exactly-once source_event_key, and verifying the " +
			"whole token vector before it writes. Nothing is estimated: an event whose lifetime " +
			"cannot be reconstructed exactly keeps its unknown, and a row that already has one is " +
			"never overwritten. Transcripts are read and never modified; the only thing written is " +
			"two integer columns.\n\n" +
			"Dry run by default. Pass --apply to write. The daemon must be stopped: it is the " +
			"sole writer of the database. Running it twice is safe -- the second run updates " +
			"nothing.",
		Args: noArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return ctx.runUsageBackfillCacheTTL(cmd, opts)
		},
	}
	cmd.Flags().BoolVar(&opts.apply, "apply", false, "Write the reconstructed lifetimes (default: dry run, no writes)")
	cmd.Flags().BoolVar(&opts.json, "json", false, "Output the report as JSON")
	return cmd
}

func (c *commandContext) runUsageBackfillCacheTTL(cmd *cobra.Command, opts usageBackfillOptions) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}

	// The daemon is the sole writer. Refuse to open the store underneath a live
	// one -- for the dry run too, because opening the database at all beside an
	// active ingest is the part that is unsafe, not the writing. A stale
	// run-file (dead PID) is treated as safe, exactly as `ao import` does.
	if live, err := runfile.CheckStale(cfg.RunFilePath); err != nil {
		return fmt.Errorf("inspect run-file: %w", err)
	} else if live != nil {
		return usageError{fmt.Errorf(
			"the AO daemon is running (pid %d); stop it first with `ao stop` before backfilling the usage ledger", live.PID)}
	}

	store, err := sqlite.Open(cfg.DataDir)
	if err != nil {
		return err
	}
	defer func() { _ = store.Close() }()

	report, err := usagebackfill.Run(cmd.Context(), store, usagebackfill.Options{Apply: opts.apply})
	if err != nil {
		return err
	}
	if opts.json {
		return writeJSON(cmd.OutOrStdout(), newUsageBackfillReportJSON(report))
	}
	_, err = fmt.Fprint(cmd.OutOrStdout(), report.String())
	if err != nil {
		return err
	}
	if report.DryRun && report.RowsUpdated > 0 {
		_, err = fmt.Fprintf(cmd.OutOrStdout(),
			"\nNothing was written. Re-run with --apply to record these %d lifetimes.\n", report.RowsUpdated)
	}
	return err
}

// usageBackfillReportJSON is the machine-readable shape. Counts and token
// totals only -- the same discipline the text report follows.
type usageBackfillReportJSON struct {
	DryRun              bool           `json:"dryRun"`
	SourcesConsidered   int            `json:"sourcesConsidered"`
	SourcesRead         int            `json:"sourcesRead"`
	SourcesUnsupported  int            `json:"sourcesUnsupported"`
	ParserErrors        int            `json:"parserErrors"`
	EventsReconstructed int            `json:"eventsReconstructed"`
	RowsMatched         int            `json:"rowsMatched"`
	RowsUpdated         int            `json:"rowsUpdated"`
	EventsSkipped       int            `json:"eventsSkipped"`
	SkippedByReason     map[string]int `json:"skippedByReason"`
	Recovered5mTokens   int64          `json:"recovered5mTokens"`
	Recovered1hTokens   int64          `json:"recovered1hTokens"`
	RecoveredTotal      int64          `json:"recoveredTotalTokens"`
}

func newUsageBackfillReportJSON(r *usagebackfill.Report) usageBackfillReportJSON {
	byReason := make(map[string]int, len(r.Skipped))
	for reason, n := range r.Skipped {
		byReason[string(reason)] = n
	}
	return usageBackfillReportJSON{
		DryRun:              r.DryRun,
		SourcesConsidered:   r.SourcesConsidered,
		SourcesRead:         r.SourcesRead,
		SourcesUnsupported:  r.SourcesUnsupported,
		ParserErrors:        r.ParserErrors,
		EventsReconstructed: r.EventsReconstructed,
		RowsMatched:         r.RowsMatched,
		RowsUpdated:         r.RowsUpdated,
		EventsSkipped:       r.SkippedTotal(),
		SkippedByReason:     byReason,
		Recovered5mTokens:   r.Recovered5m,
		Recovered1hTokens:   r.Recovered1h,
		RecoveredTotal:      r.RecoverableTotal,
	}
}
