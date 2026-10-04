package projectmemory

import (
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"math"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"

	baseline "github.com/aoagents/agent-orchestrator/backend/internal/observe/projectmemory"
)

// defaultMaxFilesPerRecord caps how many per-file items one evidence record
// contributes. A dispatch that read hundreds of files is a finding in its own
// right, but turning every read into a durable memory item would bury the
// facts worth keeping under a directory listing.
const defaultMaxFilesPerRecord = 25

// BaselineReader ingests the per-dispatch baseline evidence that
// internal/observe/projectmemory already records.
//
// It is strictly a reader. The evidence schema, the recorder, and the sink
// that writes those files are that package's; this one imports the schema
// rather than restating it, and never writes into the evidence tree. If the
// recording side changes what it measures, this reader sees the change without
// being edited, which is the only way two copies of the same knowledge cannot
// drift apart.
type BaselineReader struct {
	dir string
}

// NewBaselineReader returns a reader over an evidence directory. The directory
// is validated by the recording side's own rule (baseline.ValidateEvidenceDir),
// so a reader can never be pointed somewhere the writer would have refused.
func NewBaselineReader(dir string) (*BaselineReader, error) {
	if err := baseline.ValidateEvidenceDir(dir); err != nil {
		return nil, err
	}
	return &BaselineReader{dir: filepath.Clean(dir)}, nil
}

// NewDefaultBaselineReader returns a reader over the standard evidence
// location under AO's data dir.
func NewDefaultBaselineReader() (*BaselineReader, error) {
	dir, err := baseline.EvidenceDir()
	if err != nil {
		return nil, err
	}
	return NewBaselineReader(dir)
}

// Root is the evidence directory this reader reads.
func (r *BaselineReader) Root() string { return r.dir }

// Records reads every evidence record under the reader's root, sorted by file
// path so ingestion order is stable across runs. A root that does not exist
// yet yields no records and no error: a deployment that has never run the
// baseline harness has nothing to ingest, which is not a failure.
func (r *BaselineReader) Records(ctx context.Context) ([]baseline.EvidenceRecord, error) {
	var paths []string
	err := filepath.WalkDir(r.dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			if os.IsNotExist(err) {
				return nil
			}
			return err
		}
		if d.IsDir() || !strings.HasSuffix(d.Name(), ".json") {
			return nil
		}
		paths = append(paths, p)
		return nil
	})
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("walk baseline evidence: %w", err)
	}
	sort.Strings(paths)

	records := make([]baseline.EvidenceRecord, 0, len(paths))
	for _, p := range paths {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		raw, err := os.ReadFile(p) //nolint:gosec // p came from walking the validated evidence root
		if err != nil {
			return nil, fmt.Errorf("read baseline evidence %s: %w", p, err)
		}
		var record baseline.EvidenceRecord
		if err := json.Unmarshal(raw, &record); err != nil {
			return nil, fmt.Errorf("decode baseline evidence %s: %w", p, err)
		}
		if record.SchemaVersion != baseline.EvidenceSchemaVersion {
			// A record from a schema this build does not know is skipped
			// rather than guessed at: reading unknown fields as if they were
			// the current ones is how a wrong fact becomes durable.
			continue
		}
		records = append(records, record)
	}
	return records, nil
}

// IngestOptions parameterises turning evidence records into memory items.
type IngestOptions struct {
	// Project is the project id to file items under when a record does not
	// carry one of its own. A record's own ProjectID always wins.
	Project string
	// Repo is the absolute path of the checkout the evidence was produced
	// against. It is optional: without it, file paths are still recorded but
	// cannot be hashed, so those items carry no file-hash provenance and the
	// staleness check falls back to the commit alone.
	Repo string
	// SourceCommit is the commit to stamp on the items. When empty and Git is
	// available, HEAD of Repo is resolved instead.
	SourceCommit string
	// Git resolves HEAD when SourceCommit is empty. Optional.
	Git CommitResolver
	// Hash hashes source files. Defaults to HashFile.
	Hash FileHasher
	// MaxFilesPerRecord caps per-file items per record. Zero uses the default.
	MaxFilesPerRecord int
}

// IngestResult reports what an ingestion did.
type IngestResult struct {
	// Records is how many evidence records were read.
	Records int
	// Items are the memory items the records produced, before upsert.
	Items []MemoryItem
	// Upsert is what the store did with them.
	Upsert UpsertResult
}

// Ingest reads the evidence and upserts it into store.
//
// Running it twice over unchanged evidence at the same commit is a no-op: the
// items are content-addressed and identified by the evidence record they came
// from, so the second run reports every item unchanged and writes nothing.
func (r *BaselineReader) Ingest(ctx context.Context, store *Store, opts IngestOptions) (IngestResult, error) {
	if store == nil {
		return IngestResult{}, fmt.Errorf("%w: store is required", ErrItemInvalid)
	}
	records, err := r.Records(ctx)
	if err != nil {
		return IngestResult{}, err
	}
	items, err := r.itemsFor(ctx, records, opts)
	if err != nil {
		return IngestResult{}, err
	}
	result := IngestResult{Records: len(records), Items: items}
	if len(items) == 0 {
		return result, nil
	}
	upserted, err := store.Upsert(ctx, items...)
	if err != nil {
		return IngestResult{}, err
	}
	result.Upsert = upserted
	result.Items = upserted.Items
	return result, nil
}

// Items turns the evidence into memory items without storing them, for a
// caller that wants to inspect what an ingestion would record.
func (r *BaselineReader) Items(ctx context.Context, opts IngestOptions) ([]MemoryItem, error) {
	records, err := r.Records(ctx)
	if err != nil {
		return nil, err
	}
	return r.itemsFor(ctx, records, opts)
}

func (r *BaselineReader) itemsFor(ctx context.Context, records []baseline.EvidenceRecord, opts IngestOptions) ([]MemoryItem, error) {
	if len(records) == 0 {
		return nil, nil
	}
	commit, err := resolveIngestCommit(ctx, opts)
	if err != nil {
		return nil, err
	}
	hash := opts.Hash
	if hash == nil {
		hash = HashFile
	}
	limit := opts.MaxFilesPerRecord
	if limit <= 0 {
		limit = defaultMaxFilesPerRecord
	}
	repo := strings.TrimSpace(opts.Repo)
	if repo != "" {
		repo = filepath.Clean(repo)
	}

	items := make([]MemoryItem, 0, len(records)*2)
	for _, record := range records {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		project := strings.TrimSpace(record.ProjectID)
		if project == "" {
			project = strings.TrimSpace(opts.Project)
		}
		if project == "" {
			return nil, fmt.Errorf("%w: evidence record %s carries no project and none was supplied", ErrItemInvalid, record.RecordID)
		}
		items = append(items, dispatchItem(record, project, commit, limit))
		items = append(items, fileItems(record, project, commit, repo, hash, limit)...)
	}
	return items, nil
}

func resolveIngestCommit(ctx context.Context, opts IngestOptions) (string, error) {
	if commit := strings.TrimSpace(opts.SourceCommit); commit != "" {
		return commit, nil
	}
	repo := strings.TrimSpace(opts.Repo)
	if repo == "" || opts.Git == nil {
		// No commit provenance is recorded rather than a placeholder one. An
		// item with no source commit is simply not invalidated by history
		// moving, which is the honest consequence of not knowing where it came
		// from.
		return "", nil
	}
	head, err := opts.Git.ResolveCommit(ctx, filepath.Clean(repo), "HEAD")
	if err != nil {
		return "", fmt.Errorf("%w: resolve HEAD in %s: %w", ErrStaleCheck, repo, err)
	}
	return head, nil
}

// dispatchItem turns one evidence record into the memory item that summarises
// the dispatch.
func dispatchItem(record baseline.EvidenceRecord, project, commit string, fileLimit int) MemoryItem {
	var b strings.Builder
	fmt.Fprintf(&b, "Baseline dispatch evidence for role %q.\n", string(record.Role))
	if id := harnessLabel(record); id != "" {
		fmt.Fprintf(&b, "%s\n", id)
	}
	fmt.Fprintf(&b, "dispatch.succeeded: %t\n", record.Dispatch.Succeeded)
	if record.Dispatch.Error != "" {
		fmt.Fprintf(&b, "dispatch.error: %s\n", record.Dispatch.Error)
	}
	fmt.Fprintf(&b, "dispatch.durationMs: %s\n", formatMetric(record.Dispatch.DurationMS))
	fmt.Fprintf(&b, "context.filesInspected: %s\n", formatMetric(record.Context.FilesInspected))
	fmt.Fprintf(&b, "context.filesInspectedBytes: %s\n", formatMetric(record.Context.FilesInspectedBytes))
	fmt.Fprintf(&b, "context.repeatedReads: %s\n", formatMetric(record.Context.RepeatedReads))
	fmt.Fprintf(&b, "context.sourceBytesAvailable: %s\n", formatMetric(record.Context.SourceBytesAvailable))
	fmt.Fprintf(&b, "context.sourceTokensAvailable: %s\n", formatMetric(record.Context.SourceTokensAvailable))
	fmt.Fprintf(&b, "context.contextSentBytes: %s\n", formatMetric(record.Context.ContextSentBytes))
	fmt.Fprintf(&b, "context.contextSentTokens: %s\n", formatMetric(record.Context.ContextSentTokens))
	fmt.Fprintf(&b, "providerTokens.prompt: %s\n", formatMetric(record.ProviderTokens.Prompt))
	fmt.Fprintf(&b, "providerTokens.output: %s\n", formatMetric(record.ProviderTokens.Output))
	fmt.Fprintf(&b, "tools.total: %s\n", formatMetric(record.Tools.Total))
	if verdict := strings.TrimSpace(record.Outcomes.ReviewVerdict); verdict != "" {
		fmt.Fprintf(&b, "outcomes.reviewVerdict: %s\n", verdict)
	}
	if record.Outcomes.VerifyPassed != nil {
		fmt.Fprintf(&b, "outcomes.verifyPassed: %t\n", *record.Outcomes.VerifyPassed)
	}
	if len(record.Context.Files) > fileLimit {
		fmt.Fprintf(&b, "files.recorded: %d (memory kept the first %d in first-read order)\n", len(record.Context.Files), fileLimit)
	} else if len(record.Context.Files) > 0 {
		fmt.Fprintf(&b, "files.recorded: %d\n", len(record.Context.Files))
	}

	return MemoryItem{
		Project: project,
		Scope:   dispatchScope(record),
		Type:    TypeBaselineDispatch,
		Content: strings.TrimRight(b.String(), "\n"),
		Source: Source{
			Kind:   SourceBaselineEvidence,
			Ref:    record.RecordID,
			Detail: record.SchemaVersion,
		},
		SourceCommit: commit,
		Confidence:   dispatchConfidence(record),
	}
}

// harnessLabel names who ran the dispatch, skipping the parts the record does
// not carry. An unknown harness is left out rather than filled in with a
// plausible-looking default.
func harnessLabel(record baseline.EvidenceRecord) string {
	parts := make([]string, 0, 3)
	for _, kv := range [][2]string{
		{"harness", record.Harness},
		{"provider", record.Provider},
		{"model", record.Model},
	} {
		if value := strings.TrimSpace(kv[1]); value != "" {
			parts = append(parts, kv[0]+"="+value)
		}
	}
	return strings.Join(parts, " ")
}

// fileItems turns the per-file detail of one record into one memory item per
// file: which file a role actually read, and how much of it.
func fileItems(record baseline.EvidenceRecord, project, commit, repo string, hash FileHasher, limit int) []MemoryItem {
	files := record.Context.Files
	if len(files) > limit {
		files = files[:limit]
	}
	items := make([]MemoryItem, 0, len(files))
	for _, file := range files {
		rel, abs := resolveEvidencePath(file.Path, repo)
		if rel == "" {
			continue
		}
		content := fmt.Sprintf(
			"Role %q read %s during a baseline dispatch: %d read(s), bytes %s, estimated tokens %s.",
			string(record.Role), rel, file.Reads, formatMetric(file.Bytes), formatMetric(file.EstimatedTokens),
		)
		item := MemoryItem{
			Project: project,
			Scope:   fileScope(rel),
			Type:    TypeFileUsage,
			Content: content,
			Source: Source{
				Kind:   SourceBaselineEvidence,
				Ref:    record.RecordID + "#" + rel,
				Path:   rel,
				Detail: record.SchemaVersion,
			},
			SourceCommit: commit,
			Confidence:   metricConfidence(file.Bytes),
		}
		if abs != "" && hash != nil {
			if sum, err := hash(abs); err == nil {
				item.Source.FileHash = sum
			}
			// A file that cannot be hashed (deleted since the dispatch, or
			// outside the checkout) is recorded without a hash rather than
			// dropped. The read happened; only the ability to detect a later
			// edit is what is missing, and the commit still bounds it.
		}
		items = append(items, item)
	}
	return items
}

// resolveEvidencePath turns an evidence path into the project-relative form
// memory stores, plus the absolute path to hash. An absolute path inside repo
// is relativised; a path outside repo keeps its absolute spelling, because
// rewriting it would claim a project relationship that does not hold.
func resolveEvidencePath(raw, repo string) (rel, abs string) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return "", ""
	}
	if filepath.IsAbs(trimmed) {
		clean := filepath.Clean(trimmed)
		if repo != "" {
			if r, err := filepath.Rel(repo, clean); err == nil && !strings.HasPrefix(r, "..") {
				return filepath.ToSlash(r), clean
			}
		}
		return filepath.ToSlash(clean), clean
	}
	slash := filepath.ToSlash(filepath.Clean(trimmed))
	if repo == "" {
		return slash, ""
	}
	return slash, filepath.Join(repo, filepath.FromSlash(slash))
}

// dispatchScope narrows a dispatch item to the role that produced it, so a
// later reader can ask what the planner (as opposed to the reviewer) has
// historically been given.
func dispatchScope(record baseline.EvidenceRecord) string {
	role := strings.TrimSpace(string(record.Role))
	if role == "" {
		return "dispatch"
	}
	return "dispatch/" + role
}

// fileScope is the module a file belongs to: its directory. A file at the
// project root scopes to ".", which is a real scope rather than the empty
// project-wide one.
func fileScope(rel string) string {
	dir := path.Dir(rel)
	if dir == "" {
		return "."
	}
	return dir
}

// formatMetric renders a baseline metric without ever letting an unavailable
// number read as a measured zero — the labelling rule the evidence schema
// exists to enforce, carried through into memory content.
func formatMetric(m baseline.Metric) string {
	if m.Value == nil {
		reason := strings.TrimSpace(m.Method)
		if reason == "" {
			reason = "no reason recorded"
		}
		return "unavailable (" + reason + ")"
	}
	basis := string(m.Basis)
	if basis == "" {
		basis = "unlabelled"
	}
	return fmt.Sprintf("%d (%s)", *m.Value, basis)
}

// dispatchConfidence is the share of the dispatch's headline metrics that were
// actually measured. It is computed, not assigned: an item built mostly from
// unavailable metrics says so through a low confidence instead of presenting
// the same weight as one built from observations.
func dispatchConfidence(record baseline.EvidenceRecord) float64 {
	metrics := []baseline.Metric{
		record.Dispatch.DurationMS,
		record.Context.FilesInspected,
		record.Context.FilesInspectedBytes,
		record.Context.SourceBytesAvailable,
		record.Context.ContextSentBytes,
		record.ProviderTokens.Prompt,
		record.Tools.Total,
	}
	total := 0.0
	for _, m := range metrics {
		total += basisWeight(m)
	}
	return roundConfidence(total / float64(len(metrics)))
}

func metricConfidence(m baseline.Metric) float64 {
	return roundConfidence(basisWeight(m))
}

func basisWeight(m baseline.Metric) float64 {
	switch m.Basis {
	case baseline.BasisMeasured:
		return 0.9
	case baseline.BasisEstimated:
		return 0.6
	default:
		return 0.2
	}
}

// roundConfidence pins the value to two decimals so the same evidence yields
// exactly the same number on every ingestion — float drift in a field the
// idempotence check compares would turn a no-op re-ingest into an update.
func roundConfidence(v float64) float64 {
	if v < 0 {
		v = 0
	}
	if v > 1 {
		v = 1
	}
	return math.Round(v*100) / 100
}
