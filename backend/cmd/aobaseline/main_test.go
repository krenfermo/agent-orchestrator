package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/observe/projectmemory"
)

// fakeRepo builds a small repository-shaped tree so the harness exercises the
// real builders against real files without depending on the checkout it
// happens to run from.
func fakeRepo(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	files := map[string]string{
		"AGENTS.md":    "# Agents\nrules go here\n",
		"README.md":    "# Project\n",
		"package.json": `{"name":"fake"}`,
		filepath.Join("backend", "internal", "workflow", "workflow.go"): "package workflow\n",
		filepath.Join("backend", "internal", "observe", "observe.go"):   "package observe\n",
		filepath.Join("backend", "internal", "cli", "cli.go"):           "package cli\n",
	}
	for rel, body := range files {
		path := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func TestHarnessWritesOneEvidenceFilePerTask(t *testing.T) {
	repo := fakeRepo(t)
	evidence := t.TempDir()
	var out bytes.Buffer
	if err := run([]string{"-repo", repo, "-evidence-dir", evidence, "-run-id", "baseline-test"}, &out); err != nil {
		t.Fatalf("run: %v\n%s", err, out.String())
	}

	dir := filepath.Join(evidence, "baseline-test")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read evidence dir: %v", err)
	}
	if len(entries) != len(tasks()) {
		t.Fatalf("wrote %d evidence files, want one per task (%d)", len(entries), len(tasks()))
	}
	if len(tasks()) < 3 {
		t.Fatalf("the baseline must cover at least 3 representative tasks, got %d", len(tasks()))
	}

	roles := map[string]bool{}
	taskIDs := map[string]bool{}
	for _, entry := range entries {
		raw, err := os.ReadFile(filepath.Join(dir, entry.Name()))
		if err != nil {
			t.Fatalf("read %s: %v", entry.Name(), err)
		}
		var record projectmemory.EvidenceRecord
		if err := json.Unmarshal(raw, &record); err != nil {
			t.Fatalf("%s is not a valid evidence record: %v", entry.Name(), err)
		}
		if err := record.Validate(); err != nil {
			t.Fatalf("%s violates the evidence schema: %v", entry.Name(), err)
		}
		if record.SchemaVersion != projectmemory.EvidenceSchemaVersion {
			t.Fatalf("%s: schemaVersion = %q", entry.Name(), record.SchemaVersion)
		}
		if record.WorkflowRunID != "baseline-test" {
			t.Fatalf("%s: not keyed to the baseline run: %q", entry.Name(), record.WorkflowRunID)
		}
		if !record.Dispatch.Succeeded {
			t.Fatalf("%s: task failed: %s", entry.Name(), record.Dispatch.Error)
		}
		// The supply side is what this harness can measure, and it must be
		// measured rather than estimated for every task.
		if record.Context.ContextSentBytes.Basis != projectmemory.BasisMeasured {
			t.Fatalf("%s: contextSentBytes = %+v, want a measured payload", entry.Name(), record.Context.ContextSentBytes)
		}
		if record.Context.SourceBytesAvailable.Basis != projectmemory.BasisMeasured {
			t.Fatalf("%s: sourceBytesAvailable = %+v, want a measured scan", entry.Name(), record.Context.SourceBytesAvailable)
		}
		if record.Context.SourceTokensAvailable.Basis != projectmemory.BasisEstimated {
			t.Fatalf("%s: a byte-derived token count must be labeled estimated, got %+v", entry.Name(), record.Context.SourceTokensAvailable)
		}
		// The consumption side is not observable here and must say so.
		for name, metric := range map[string]projectmemory.Metric{
			"providerTokens.prompt": record.ProviderTokens.Prompt,
			"providerTokens.output": record.ProviderTokens.Output,
			"providerTokens.total":  record.ProviderTokens.Total,
			"tools.total":           record.Tools.Total,
		} {
			if metric.Value != nil {
				t.Fatalf("%s: %s reported %d without a provider call", entry.Name(), name, *metric.Value)
			}
			if metric.Basis != projectmemory.BasisUnavailable || metric.Method == "" {
				t.Fatalf("%s: %s must be labeled unavailable with a reason, got %+v", entry.Name(), name, metric)
			}
		}
		roles[string(record.Role)] = true
		taskIDs[record.TaskID] = true
	}
	for _, want := range []string{"planner", "worker", "reviewer", "fix_worker"} {
		if !roles[want] {
			t.Fatalf("no baseline evidence for role %q (got %v)", want, roles)
		}
	}
	for _, task := range tasks() {
		if !taskIDs[task.id] {
			t.Fatalf("no evidence record for task %q", task.id)
		}
	}
}

// The planner task is the one surface where AO assembles the context itself,
// so its file reads must come out measured rather than unavailable.
func TestHarnessMeasuresPlannerFileReads(t *testing.T) {
	repo := fakeRepo(t)
	evidence := t.TempDir()
	var out bytes.Buffer
	if err := run([]string{"-repo", repo, "-evidence-dir", evidence, "-run-id", "baseline-test"}, &out); err != nil {
		t.Fatalf("run: %v\n%s", err, out.String())
	}
	entries, err := os.ReadDir(filepath.Join(evidence, "baseline-test"))
	if err != nil {
		t.Fatal(err)
	}
	var planner projectmemory.EvidenceRecord
	for _, entry := range entries {
		raw, err := os.ReadFile(filepath.Join(evidence, "baseline-test", entry.Name()))
		if err != nil {
			t.Fatal(err)
		}
		var record projectmemory.EvidenceRecord
		if err := json.Unmarshal(raw, &record); err != nil {
			t.Fatal(err)
		}
		if record.TaskID == "planner-context" {
			planner = record
		}
	}
	if planner.RecordID == "" {
		t.Fatal("no planner-context record was written")
	}
	if planner.Context.FilesInspected.Basis != projectmemory.BasisMeasured {
		t.Fatalf("filesInspected = %+v, want measured", planner.Context.FilesInspected)
	}
	if planner.Context.FilesInspected.Value == nil || *planner.Context.FilesInspected.Value == 0 {
		t.Fatalf("the planner context builder read no files: %+v", planner.Context.FilesInspected)
	}
	if len(planner.Context.Files) == 0 {
		t.Fatal("per-file detail is missing from the planner record")
	}
}

// Evidence must land under AO's data dir, and a relative directory (which
// would resolve inside whatever checkout the harness was run from) is refused.
func TestHarnessRefusesARepoLocalEvidenceDir(t *testing.T) {
	var out bytes.Buffer
	err := run([]string{"-repo", fakeRepo(t), "-evidence-dir", "evidence"}, &out)
	if err == nil {
		t.Fatal("a relative evidence dir must be refused")
	}
}

func TestHarnessDefaultsToTheAODataDir(t *testing.T) {
	data := t.TempDir()
	t.Setenv("AO_DATA_DIR", data)
	var out bytes.Buffer
	if err := run([]string{"-repo", fakeRepo(t), "-run-id", "baseline-default"}, &out); err != nil {
		t.Fatalf("run: %v\n%s", err, out.String())
	}
	dir := filepath.Join(data, "project-memory", "baseline", "baseline-default")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("evidence did not land under AO_DATA_DIR: %v", err)
	}
	if len(entries) != len(tasks()) {
		t.Fatalf("wrote %d evidence files under the data dir, want %d", len(entries), len(tasks()))
	}
}
