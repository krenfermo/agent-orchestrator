// Command aoctxregress is the disabled-vs-enabled quality gate for AO's
// role-aware context router.
//
// It runs one fixture task through the real agent dispatch wrappers twice —
// first with the router absent, then with the router AO ships installed — and
// compares the two runs:
//
//   - It exits non-zero when the routed run's task, review, or verify outcome
//     differs from the unrouted run's, whatever context saving was measured. A
//     smaller payload that changes what the pipeline concludes is a
//     regression, and this command exists to make that a build failure rather
//     than a footnote under a savings number.
//   - It always reports the measured context-reduction percentage, next to the
//     tool-call and file-read counts of both runs, so the saving is read
//     alongside what it cost.
//
// No provider is called. See internal/observe/ctxregress for exactly which
// parts of the run are real (the wrappers, the router, the evidence sources,
// every byte count) and which part is a deterministic stand-in (the agent).
//
// Usage:
//
//	go run ./cmd/aoctxregress [-repo PATH] [-evidence-dir PATH]
//
// With no -repo it builds a self-contained fixture checkout in a temporary
// directory, so the comparison never depends on what happens to be uncommitted
// on the machine running it.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/aoagents/agent-orchestrator/backend/internal/observe/ctxregress"
	"github.com/aoagents/agent-orchestrator/backend/internal/observe/projectmemory"
)

func main() {
	code, err := run(os.Args[1:], os.Stdout)
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return
		}
		fmt.Fprintln(os.Stderr, "aoctxregress:", err)
		os.Exit(1)
	}
	os.Exit(code)
}

// run returns the process exit code alongside any harness failure. A harness
// that could not complete and a comparison that found a regression are
// different outcomes, and only the second one has a report worth reading.
func run(args []string, out io.Writer) (int, error) {
	fs := flag.NewFlagSet("aoctxregress", flag.ContinueOnError)
	fs.SetOutput(out)
	repo := fs.String("repo", "", "checkout to route against (default: a self-contained fixture checkout in a temp dir)")
	evidenceDir := fs.String("evidence-dir", "", "persist both runs' evidence records here (default: keep them in memory)")
	verbose := fs.Bool("v", false, "log the dispatch wrappers' own warnings")
	if err := fs.Parse(args); err != nil {
		return 1, err
	}

	fixture, cleanup, err := resolveFixture(*repo)
	if err != nil {
		return 1, err
	}
	defer cleanup()

	opts := ctxregress.Options{Fixture: fixture}
	if *verbose {
		opts.Log = slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))
	}
	if *evidenceDir != "" {
		abs, absErr := filepath.Abs(*evidenceDir)
		if absErr != nil {
			return 1, fmt.Errorf("resolve -evidence-dir: %w", absErr)
		}
		sink, sinkErr := projectmemory.NewDirSink(abs)
		if sinkErr != nil {
			return 1, sinkErr
		}
		opts.Sink = sink
		_, _ = fmt.Fprintf(out, "evidence: %s\n\n", sink.Root())
	}

	comparison, err := ctxregress.Run(context.Background(), opts)
	if err != nil {
		return 1, err
	}
	if err := comparison.Report(out); err != nil {
		return 1, err
	}
	if comparison.Regressed() {
		return 1, nil
	}
	return 0, nil
}

// resolveFixture picks the checkout the comparison runs against and returns
// the cleanup for anything it had to create.
func resolveFixture(repo string) (ctxregress.Fixture, func(), error) {
	if repo != "" {
		abs, err := filepath.Abs(repo)
		if err != nil {
			return ctxregress.Fixture{}, func() {}, fmt.Errorf("resolve -repo: %w", err)
		}
		if _, err := os.Stat(abs); err != nil {
			return ctxregress.Fixture{}, func() {}, fmt.Errorf("resolve -repo: %w", err)
		}
		return ctxregress.DefaultFixture(abs), func() {}, nil
	}
	dir, err := os.MkdirTemp("", "aoctxregress-")
	if err != nil {
		return ctxregress.Fixture{}, func() {}, fmt.Errorf("create fixture checkout: %w", err)
	}
	cleanup := func() { _ = os.RemoveAll(dir) }
	fixture, err := ctxregress.WriteFixtureRepo(dir)
	if err != nil {
		cleanup()
		return ctxregress.Fixture{}, func() {}, err
	}
	// Populate the stores the router reuses rather than reads, so the routed
	// run's reused-versus-newly-read split is exercised instead of being
	// structurally zero.
	if err := ctxregress.IndexFixtureEvidence(context.Background(), fixture); err != nil {
		cleanup()
		return ctxregress.Fixture{}, func() {}, err
	}
	return fixture, cleanup, nil
}
