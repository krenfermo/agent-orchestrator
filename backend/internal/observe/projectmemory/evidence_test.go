package projectmemory

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

func validRecord() EvidenceRecord {
	return EvidenceRecord{
		RecordID:      "pmb-1",
		GeneratedAt:   time.Date(2026, 8, 24, 12, 0, 0, 0, time.UTC),
		Role:          domain.WorkflowRoleWorker,
		WorkflowRunID: "wf-1",
	}
}

// A record with nothing populated must still serialize to something valid:
// every metric becomes an explicit "not recorded", never a blank or a zero.
func TestNormalizeTurnsAbsenceIntoLabeledUnavailable(t *testing.T) {
	record := validRecord().normalized()
	if err := record.Validate(); err != nil {
		t.Fatalf("normalized record must validate: %v", err)
	}
	if record.SchemaVersion != EvidenceSchemaVersion {
		t.Fatalf("schemaVersion = %q", record.SchemaVersion)
	}
	for _, m := range record.metrics() {
		if m.metric.Basis != BasisUnavailable {
			t.Fatalf("%s: basis = %q, want unavailable", m.field, m.metric.Basis)
		}
		if m.metric.Value != nil {
			t.Fatalf("%s: unrecorded metric was given the value %d", m.field, *m.metric.Value)
		}
	}
}

func TestRecordValidateRejectsMissingIdentity(t *testing.T) {
	cases := map[string]func(*EvidenceRecord){
		"no record id":      func(r *EvidenceRecord) { r.RecordID = "" },
		"no role":           func(r *EvidenceRecord) { r.Role = "" },
		"no generated time": func(r *EvidenceRecord) { r.GeneratedAt = time.Time{} },
		"no schema version": func(r *EvidenceRecord) { r.SchemaVersion = "" },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			record := validRecord().normalized()
			mutate(&record)
			err := record.Validate()
			if !errors.Is(err, ErrEvidenceInvalid) {
				t.Fatalf("expected ErrEvidenceInvalid, got %v", err)
			}
		})
	}
}

// The labeling rule is enforced record-wide, not just on the metric type: a
// single dishonest metric anywhere must stop the whole record.
func TestRecordValidateRejectsADishonestMetricAnywhere(t *testing.T) {
	zero := int64(0)
	record := validRecord().normalized()
	record.ProviderTokens.CacheRead = Metric{Value: &zero, Basis: BasisUnavailable, Method: "provider sent none"}
	err := record.Validate()
	if !errors.Is(err, ErrMetricInvalid) {
		t.Fatalf("expected the metric rule to fail the record, got %v", err)
	}
	if !strings.Contains(err.Error(), "providerTokens.cacheRead") {
		t.Fatalf("error must name the offending field, got %v", err)
	}
}

func TestRecordValidateChecksNestedFileMetrics(t *testing.T) {
	record := validRecord().normalized()
	record.Context.Files = []FileInspection{{
		Path:            "main.go",
		Reads:           1,
		Bytes:           Metric{Basis: BasisMeasured, Method: "counted"},
		EstimatedTokens: EstimatedTokensFor(10),
	}}
	err := record.Validate()
	if !errors.Is(err, ErrMetricInvalid) {
		t.Fatalf("expected ErrMetricInvalid, got %v", err)
	}
	if !strings.Contains(err.Error(), "context.files[0].bytes") {
		t.Fatalf("error must name the offending file metric, got %v", err)
	}
}

// Serialization must preserve the null/measured distinction across a round
// trip: a later phase reading these files has to be able to tell them apart.
func TestSerializationPreservesNullVersusZero(t *testing.T) {
	record := validRecord().normalized()
	record.Context.FilesInspected = Measured(0, "distinct paths this dispatch read")
	raw, err := json.Marshal(record)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(raw), `"prompt":{"value":null,"basis":"unavailable"`) {
		t.Fatalf("an unavailable metric must serialize its value as null:\n%s", raw)
	}
	var back EvidenceRecord
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if back.ProviderTokens.Prompt.Value != nil {
		t.Fatal("unavailable prompt tokens came back with a value")
	}
	if back.Context.FilesInspected.Value == nil || *back.Context.FilesInspected.Value != 0 {
		t.Fatalf("measured zero did not survive the round trip: %+v", back.Context.FilesInspected)
	}
	if back.Context.FilesInspected.Basis != BasisMeasured {
		t.Fatalf("basis did not survive the round trip: %q", back.Context.FilesInspected.Basis)
	}
}

func TestRunKeyPrefersTheRunThenFallsBack(t *testing.T) {
	record := validRecord()
	if got := record.RunKey(); got != "wf-1" {
		t.Fatalf("RunKey() = %q, want wf-1", got)
	}
	record.WorkflowRunID = ""
	record.TaskID = "task/../escape"
	if got := record.RunKey(); got != "task--..-escape" && strings.ContainsAny(got, `/\`) {
		t.Fatalf("RunKey() must not contain a path separator, got %q", got)
	}
	record.TaskID = ""
	record.SessionID = "sess-9"
	if got := record.RunKey(); got != "sess-9" {
		t.Fatalf("RunKey() = %q, want sess-9", got)
	}
	record.SessionID = ""
	if got := record.RunKey(); got != "pmb-1" {
		t.Fatalf("RunKey() = %q, want the record id", got)
	}
}

func TestDirSinkWritesUnderTheEvidenceRoot(t *testing.T) {
	root := t.TempDir()
	sink, err := NewDirSink(root)
	if err != nil {
		t.Fatalf("NewDirSink: %v", err)
	}
	path, err := sink.Write(context.Background(), validRecord())
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	if want := filepath.Join(root, "wf-1", "pmb-1.json"); path != want {
		t.Fatalf("path = %q, want %q", path, want)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	var back EvidenceRecord
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatalf("written file is not valid JSON: %v", err)
	}
	if back.SchemaVersion != EvidenceSchemaVersion {
		t.Fatalf("schemaVersion = %q", back.SchemaVersion)
	}
	if left, _ := filepath.Glob(filepath.Join(root, "wf-1", "*.tmp")); len(left) > 0 {
		t.Fatalf("temp files left behind: %v", left)
	}
}

// A record that breaks the labeling rule must never reach disk: an evidence
// file that exists is one whose numbers are honestly labeled.
func TestDirSinkRefusesToWriteADishonestRecord(t *testing.T) {
	root := t.TempDir()
	sink, err := NewDirSink(root)
	if err != nil {
		t.Fatalf("NewDirSink: %v", err)
	}
	record := validRecord()
	record.Tools.Total = Metric{Basis: BasisMeasured, Method: "counted"}
	if _, err := sink.Write(context.Background(), record); !errors.Is(err, ErrMetricInvalid) {
		t.Fatalf("expected the write to be refused, got %v", err)
	}
	entries, _ := os.ReadDir(root)
	if len(entries) != 0 {
		t.Fatalf("a refused write left files behind: %v", entries)
	}
}

func TestValidateEvidenceDirRejectsUnsafeLocations(t *testing.T) {
	if err := ValidateEvidenceDir(""); !errors.Is(err, ErrEvidencePath) {
		t.Fatalf("empty dir: %v", err)
	}
	if err := ValidateEvidenceDir(filepath.Join("relative", "evidence")); !errors.Is(err, ErrEvidencePath) {
		t.Fatalf("a relative dir could resolve inside a repository checkout: %v", err)
	}
	osAppData := filepath.Join(string(filepath.Separator), "Users", "someone", "Library", "Application Support", "ao")
	if err := ValidateEvidenceDir(osAppData); !errors.Is(err, ErrEvidencePath) {
		t.Fatalf("the OS application-data dir must be refused: %v", err)
	}
	if err := ValidateEvidenceDir(t.TempDir()); err != nil {
		t.Fatalf("an absolute dir outside app-data must be accepted: %v", err)
	}
}

func TestEvidenceDirFollowsAODataDir(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("AO_DATA_DIR", dir)
	got, err := EvidenceDir()
	if err != nil {
		t.Fatalf("EvidenceDir: %v", err)
	}
	want := filepath.Join(dir, "project-memory", "baseline")
	if got != want {
		t.Fatalf("EvidenceDir() = %q, want %q", got, want)
	}
}

func TestEvidenceDirDefaultsUnderAOHomeNotOSAppData(t *testing.T) {
	home := t.TempDir()
	t.Setenv("AO_DATA_DIR", "")
	switch runtime.GOOS {
	case "windows":
		t.Setenv("USERPROFILE", home)
	default:
		t.Setenv("HOME", home)
	}
	got, err := EvidenceDir()
	if err != nil {
		t.Fatalf("EvidenceDir: %v", err)
	}
	want := filepath.Join(home, ".ao", "data", "project-memory", "baseline")
	if got != want {
		t.Fatalf("EvidenceDir() = %q, want %q", got, want)
	}
}
