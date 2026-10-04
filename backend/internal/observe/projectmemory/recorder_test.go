package projectmemory

import (
	stdctx "context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

type captureSink struct {
	records []EvidenceRecord
	err     error
}

func (c *captureSink) Write(_ stdctx.Context, record EvidenceRecord) (string, error) {
	if c.err != nil {
		return "", c.err
	}
	record = record.normalized()
	if err := record.Validate(); err != nil {
		return "", err
	}
	c.records = append(c.records, record)
	return "/dev/null/" + record.RecordID, nil
}

func testRecorder(sink Sink) *Recorder {
	now := time.Date(2026, 8, 24, 12, 0, 0, 0, time.UTC)
	var ticks int
	return NewRecorder(sink,
		WithClock(func() time.Time {
			t := now.Add(time.Duration(ticks) * 250 * time.Millisecond)
			ticks++
			return t
		}),
		WithIDs(func() string { return "pmb-test" }),
	)
}

func TestSpanRecordsWhatTheSurfaceCanReport(t *testing.T) {
	sink := &captureSink{}
	span := testRecorder(sink).Begin(Dispatch{
		Role:          domain.WorkflowRolePlanner,
		WorkflowRunID: "wf-1",
		Harness:       "claude-code",
		Observable:    Capabilities{FileReads: true, ContextPayload: true, SourceScope: true},
	})
	span.ObserveFileRead("AGENTS.md", 400)
	span.ObserveFileRead("README.md", 200)
	span.ObserveFileRead("AGENTS.md", 400)
	span.ObserveContextSent("0123456789")
	span.ObserveSourceScope(SourceScan{Files: 3, Bytes: 4000})
	record, path, err := span.Finish(stdctx.Background(), nil)
	if err != nil {
		t.Fatalf("Finish: %v", err)
	}
	if path == "" {
		t.Fatal("Finish returned no path")
	}
	if got := *record.Context.FilesInspected.Value; got != 2 {
		t.Fatalf("filesInspected = %d, want 2 distinct paths", got)
	}
	if got := *record.Context.RepeatedReads.Value; got != 1 {
		t.Fatalf("repeatedReads = %d, want 1", got)
	}
	if got := *record.Context.FilesInspectedBytes.Value; got != 1000 {
		t.Fatalf("filesInspectedBytes = %d, want 1000 (a re-read costs context twice)", got)
	}
	if got := *record.Context.ContextSentBytes.Value; got != 10 {
		t.Fatalf("contextSentBytes = %d, want 10", got)
	}
	if record.Context.ContextSentTokens.Basis != BasisEstimated {
		t.Fatalf("without provider telemetry, sent tokens must be estimated, got %q", record.Context.ContextSentTokens.Basis)
	}
	if record.Context.SourceTokensAvailable.Basis != BasisEstimated || *record.Context.SourceTokensAvailable.Value != 1000 {
		t.Fatalf("sourceTokensAvailable = %+v", record.Context.SourceTokensAvailable)
	}
	if record.Provider != "anthropic" {
		t.Fatalf("provider = %q, want the static harness mapping", record.Provider)
	}
	if record.Dispatch.DurationMS.Basis != BasisMeasured {
		t.Fatalf("duration must be measured, got %q", record.Dispatch.DurationMS.Basis)
	}
	if !record.Dispatch.Succeeded {
		t.Fatal("a dispatch that returned no error must be recorded as succeeded")
	}
	if len(sink.records) != 1 {
		t.Fatalf("sink received %d records", len(sink.records))
	}
}

// A surface that cannot report a signal must say so, rather than reporting the
// zero it happens to have accumulated.
func TestSpanReportsUnobservableSignalsAsUnavailable(t *testing.T) {
	span := testRecorder(&captureSink{}).Begin(Dispatch{
		Role:       domain.WorkflowRoleWorker,
		Observable: Capabilities{ContextPayload: true},
	})
	span.ObserveContextSent("hello")
	record := span.Build(nil)
	for name, metric := range map[string]Metric{
		"filesInspected":        record.Context.FilesInspected,
		"repeatedReads":         record.Context.RepeatedReads,
		"sourceTokensAvailable": record.Context.SourceTokensAvailable,
		"tools.total":           record.Tools.Total,
		"providerTokens.prompt": record.ProviderTokens.Prompt,
		"providerTokens.total":  record.ProviderTokens.Total,
	} {
		if metric.Basis != BasisUnavailable || metric.Value != nil {
			t.Fatalf("%s must be unavailable for this surface, got %+v", name, metric)
		}
		if metric.Method == "" {
			t.Fatalf("%s must state why it is unavailable", name)
		}
	}
	if record.Context.ContextSentBytes.Basis != BasisMeasured {
		t.Fatalf("the payload this surface does carry must be measured, got %q", record.Context.ContextSentBytes.Basis)
	}
}

// A tool-call-capable surface that observed no calls reports a measured zero:
// that IS an observation, and it must not be confused with the unavailable
// case above.
func TestObservedZeroIsMeasuredNotUnavailable(t *testing.T) {
	span := testRecorder(&captureSink{}).Begin(Dispatch{
		Role:       domain.WorkflowRoleVerify,
		Observable: Capabilities{ToolCalls: true},
	})
	record := span.Build(nil)
	if record.Tools.Total.Basis != BasisMeasured || record.Tools.Total.Value == nil || *record.Tools.Total.Value != 0 {
		t.Fatalf("tools.total = %+v, want a measured zero", record.Tools.Total)
	}
}

func TestProviderTelemetryUpgradesSentTokensToMeasured(t *testing.T) {
	prompt := int64(9000)
	output := int64(150)
	span := testRecorder(&captureSink{}).Begin(Dispatch{
		Role:       domain.WorkflowRoleWorker,
		Observable: Capabilities{ContextPayload: true, ProviderTokens: true},
	})
	span.ObserveContextSent("0123456789")
	span.ObserveProviderUsage(domain.UsageMetricTotals{InputTokens: &prompt, OutputTokens: &output})
	record := span.Build(nil)
	if record.Context.ContextSentTokens.Basis != BasisMeasured || *record.Context.ContextSentTokens.Value != 9000 {
		t.Fatalf("a real prompt-token count must replace the byte estimate, got %+v", record.Context.ContextSentTokens)
	}
	if record.ProviderTokens.CacheRead.Basis != BasisUnavailable {
		t.Fatalf("a counter the provider did not report must stay unavailable, got %+v", record.ProviderTokens.CacheRead)
	}
	if record.ProviderTokens.Total.Basis != BasisMeasured || *record.ProviderTokens.Total.Value != 9150 {
		t.Fatalf("total must sum only the reported counters, got %+v", record.ProviderTokens.Total)
	}
}

// A failed dispatch is still evidence: the record is written, and it says the
// dispatch failed rather than being silently dropped.
func TestFailedDispatchIsStillRecorded(t *testing.T) {
	sink := &captureSink{}
	span := testRecorder(sink).Begin(Dispatch{Role: domain.WorkflowRoleReviewer})
	record, _, err := span.Finish(stdctx.Background(), errors.New("reviewer binary not found"))
	if err != nil {
		t.Fatalf("Finish: %v", err)
	}
	if record.Dispatch.Succeeded {
		t.Fatal("a failed dispatch must not be recorded as succeeded")
	}
	if record.Dispatch.Error != "reviewer binary not found" {
		t.Fatalf("error = %q", record.Dispatch.Error)
	}
	if len(sink.records) != 1 {
		t.Fatalf("sink received %d records, want the failure to be persisted", len(sink.records))
	}
}

func TestNilRecorderAndNilSpanAreSafe(t *testing.T) {
	var recorder *Recorder
	span := recorder.Begin(Dispatch{Role: domain.WorkflowRoleWorker})
	if span != nil {
		t.Fatal("a nil recorder must produce no span")
	}
	span.ObserveFileRead("a.go", 1)
	span.ObserveToolCall("Read")
	span.ObserveContextSent("x")
	span.ObserveSourceScope(SourceScan{})
	span.ObserveProviderUsage(domain.UsageMetricTotals{})
	span.Identify("s", "p", "m")
	span.LinkReviewRun("rr-1")
	span.LinkReviewVerdict("approved")
	span.LinkVerifyOutcome(0, 1)
	span.Note("n")
	if got := span.RecordID(); got != "" {
		t.Fatalf("RecordID() = %q", got)
	}
	if _, path, err := span.Finish(stdctx.Background(), nil); err != nil || path != "" {
		t.Fatalf("Finish on a nil span: path=%q err=%v", path, err)
	}
}

func TestUnavailableReasonOverrideExplainsTheHarnessGap(t *testing.T) {
	const reason = "the baseline harness dispatches no provider call"
	span := testRecorder(&captureSink{}).Begin(Dispatch{
		Role:              domain.WorkflowRoleWorker,
		Observable:        Capabilities{ContextPayload: true},
		UnavailableReason: reason,
	})
	record := span.Build(nil)
	if record.ProviderTokens.Prompt.Method != reason {
		t.Fatalf("provider token reason = %q, want the override", record.ProviderTokens.Prompt.Method)
	}
	if record.Context.FilesInspected.Method != reason {
		t.Fatalf("file-read reason = %q, want the override", record.Context.FilesInspected.Method)
	}
}

func TestVerifyOutcomeIsLinked(t *testing.T) {
	span := testRecorder(&captureSink{}).Begin(Dispatch{Role: domain.WorkflowRoleVerify, Observable: Capabilities{ToolCalls: true}})
	span.ObserveToolCall("go")
	span.LinkVerifyOutcome(1, 4200)
	record := span.Build(nil)
	if record.Outcomes.VerifyExitCode == nil || *record.Outcomes.VerifyExitCode != 1 {
		t.Fatalf("verifyExitCode = %v", record.Outcomes.VerifyExitCode)
	}
	if record.Outcomes.VerifyPassed == nil || *record.Outcomes.VerifyPassed {
		t.Fatalf("a non-zero exit must record as not passed, got %v", record.Outcomes.VerifyPassed)
	}
	if record.Outcomes.VerifyDurationMS.Basis != BasisMeasured || *record.Outcomes.VerifyDurationMS.Value != 4200 {
		t.Fatalf("verifyDurationMs = %+v", record.Outcomes.VerifyDurationMS)
	}
	if record.Tools.ByName["go"] != 1 {
		t.Fatalf("tools.byName = %v", record.Tools.ByName)
	}
}

func TestScanSourceMeasuresOnlySourceOutsideExcludedTrees(t *testing.T) {
	root := t.TempDir()
	write := func(rel, body string) {
		path := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("main.go", "package main")           // 12 bytes, counted
	write("docs/readme.md", "hello")           // 5 bytes, counted
	write("node_modules/dep/index.js", "junk") // excluded tree
	write(".git/objects/blob", "junk")         // excluded tree
	write("logo.png", "binary-ish")            // not a source extension

	scan := ScanSource(root, []string{"."})
	if scan.Files != 2 {
		t.Fatalf("files = %d, want 2", scan.Files)
	}
	if scan.Bytes != 17 {
		t.Fatalf("bytes = %d, want 17", scan.Bytes)
	}
	if scan.Skipped == 0 {
		t.Fatal("a skipped non-source file must be counted, not silently dropped")
	}
}

// A scope that cannot be walked must not fail the scan, and must not silently
// look like an empty repository either.
func TestScanSourceCountsAnUnreadableRootAsSkipped(t *testing.T) {
	scan := ScanSource(t.TempDir(), []string{"does-not-exist"})
	if scan.Files != 0 || scan.Bytes != 0 {
		t.Fatalf("scan = %+v, want nothing measured", scan)
	}
	if scan.Skipped == 0 {
		t.Fatal("a missing root must be reported as skipped")
	}
}
