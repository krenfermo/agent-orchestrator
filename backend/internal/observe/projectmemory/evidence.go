package projectmemory

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

// EvidenceSchemaVersion identifies the shape of an evidence record on disk.
// Later phases read baseline files written by this phase, so the version is
// part of the file rather than implied by where it sits.
const EvidenceSchemaVersion = "project-memory-baseline/v1"

// ErrEvidenceInvalid is the sentinel every record-level validation failure
// wraps.
var ErrEvidenceInvalid = errors.New("invalid evidence record")

// EvidenceRecord is everything the baseline harness knows about one agent
// dispatch. One record is one dispatch — a planner generation, a worker spawn,
// a reviewer launch, a fix prompt delivery, a verify command run — not one
// whole workflow run, because the context question this baseline exists to
// answer ("how much did this role actually get, and how much did it use") is a
// per-dispatch question.
//
// Every numeric field is a Metric, so each one says whether it was measured,
// estimated, or unavailable.
type EvidenceRecord struct {
	SchemaVersion string    `json:"schemaVersion"`
	RecordID      string    `json:"recordId"`
	GeneratedAt   time.Time `json:"generatedAt"`

	// Role is the functional role this dispatch played, in the vocabulary the
	// rest of the codebase already uses (domain.WorkflowRole).
	Role domain.WorkflowRole `json:"role"`
	// WorkflowRunID / WorkflowStepID / TaskID key the record back to the run
	// that produced it. Any of them may be empty when the dispatch surface
	// genuinely does not carry it (a reviewer launch, for instance, knows its
	// review run but not the workflow run), and an empty id is left empty
	// rather than back-filled with a guess.
	WorkflowRunID  string `json:"workflowRunId,omitempty"`
	WorkflowStepID string `json:"workflowStepId,omitempty"`
	TaskID         string `json:"taskId,omitempty"`
	ProjectID      string `json:"projectId,omitempty"`
	SessionID      string `json:"sessionId,omitempty"`

	// Harness / Provider / Model describe who ran it, when known. Provider is
	// the static harness->vendor mapping (domain.ProviderForHarness), never an
	// inference from a binary name.
	Harness  string `json:"harness,omitempty"`
	Provider string `json:"provider,omitempty"`
	Model    string `json:"model,omitempty"`

	Dispatch       DispatchOutcome `json:"dispatch"`
	Context        ContextMetrics  `json:"context"`
	ProviderTokens ProviderTokens  `json:"providerTokens"`
	Tools          ToolMetrics     `json:"tools"`
	Outcomes       OutcomeLinks    `json:"outcomes"`

	// Routing is what the role-aware context router decided for this
	// dispatch, when there is anything to say about it. It is an ADDITIVE
	// extension of this schema, not a change to it: the field is a pointer
	// with omitempty, so a record from a dispatch with no routing story is
	// byte-for-byte the record this schema always produced, and a consumer
	// that predates the router keeps reading every record unchanged. See
	// RoutingMetrics.
	Routing *RoutingMetrics `json:"routing,omitempty"`

	// Notes carries harness-level remarks about how this record was produced
	// (for example: which dispatch surfaces were exercised without a live
	// provider). Notes never contain metrics.
	Notes []string `json:"notes,omitempty"`
}

// DispatchOutcome is the timing and result of the wrapped dispatch call
// itself.
type DispatchOutcome struct {
	StartedAt   time.Time `json:"startedAt"`
	CompletedAt time.Time `json:"completedAt"`
	DurationMS  Metric    `json:"durationMs"`
	// Succeeded reports whether the wrapped call returned without an error. A
	// dispatch that failed is still recorded: a failed launch is a real datum
	// about the pipeline, not a reason to drop the evidence.
	Succeeded bool   `json:"succeeded"`
	Error     string `json:"error,omitempty"`
}

// ContextMetrics is the heart of the baseline: what the task's scope made
// available versus what AO actually sent.
type ContextMetrics struct {
	// FilesInspected counts distinct paths this dispatch read.
	FilesInspected Metric `json:"filesInspected"`
	// FilesInspectedBytes is the total bytes read across those paths, counting
	// a re-read twice, because a re-read costs context twice.
	FilesInspectedBytes Metric `json:"filesInspectedBytes"`
	// RepeatedReads counts reads beyond the first for each path — the signal a
	// later phase would expect project memory to reduce.
	RepeatedReads Metric `json:"repeatedReads"`
	// SourceTokensAvailable is how much source the dispatch's declared scope
	// could have supplied. Always an estimate: it is derived from measured
	// bytes, since AO does not tokenize with the provider's tokenizer.
	SourceTokensAvailable Metric `json:"sourceTokensAvailable"`
	// SourceBytesAvailable is the measured byte total behind that estimate.
	SourceBytesAvailable Metric `json:"sourceBytesAvailable"`
	// ContextSentBytes is the size of the payload AO actually handed the
	// provider (a prompt, a planner context document, a fix message). Measured
	// where the dispatch surface carries the payload; unavailable where it does
	// not.
	ContextSentBytes Metric `json:"contextSentBytes"`
	// ContextSentTokens is that payload in tokens. Estimated from
	// ContextSentBytes unless provider telemetry reported a real prompt-token
	// count, in which case ObserveProviderUsage upgrades it to measured.
	ContextSentTokens Metric `json:"contextSentTokens"`
	// Files is the per-path detail behind the counters above, in first-read
	// order. Omitted when nothing was observed.
	Files []FileInspection `json:"files,omitempty"`
}

// FileInspection is one path this dispatch read, and how much of it.
type FileInspection struct {
	Path  string `json:"path"`
	Reads int64  `json:"reads"`
	// Bytes is the total across every read of this path.
	Bytes           Metric `json:"bytes"`
	EstimatedTokens Metric `json:"estimatedTokens"`
}

// ProviderTokens is what the provider's own telemetry reported. Every field is
// unavailable unless a real usage signal was observed for this dispatch — AO
// never derives these from its own byte counts, because a provider's cache
// accounting is not something a byte count can approximate.
type ProviderTokens struct {
	Prompt     Metric `json:"prompt"`
	Output     Metric `json:"output"`
	CacheRead  Metric `json:"cacheRead"`
	CacheWrite Metric `json:"cacheWrite"`
	Reasoning  Metric `json:"reasoning"`
	Total      Metric `json:"total"`
}

// ToolMetrics counts the tool calls the dispatch made, when the surface
// exposes them.
type ToolMetrics struct {
	Total Metric `json:"total"`
	// ByName is the per-tool breakdown behind Total. Omitted when no tool call
	// was observed.
	ByName map[string]int64 `json:"byName,omitempty"`
}

// OutcomeLinks ties this dispatch to the review and verify results of the same
// run, so a later phase can ask whether a context change moved outcomes rather
// than only token counts.
type OutcomeLinks struct {
	// ReviewRunIDs are review_runs rows produced or targeted by this dispatch.
	ReviewRunIDs []string `json:"reviewRunIds,omitempty"`
	// ReviewVerdict is the reviewer's verdict when one is known at record time.
	// Empty means no verdict was available yet, never "approved by default".
	ReviewVerdict string `json:"reviewVerdict,omitempty"`
	// VerifyExitCode is the exit status of a verify command run, or nil when
	// this dispatch ran none.
	VerifyExitCode *int `json:"verifyExitCode"`
	// VerifyPassed mirrors that exit status as a verdict, nil when unknown.
	VerifyPassed *bool `json:"verifyPassed"`
	// VerifyDurationMS is how long the verify command took.
	VerifyDurationMS Metric `json:"verifyDurationMs"`
}

// RunKey is the directory an evidence record is filed under: the workflow run
// it belongs to when it has one, then the task, then the session, and finally
// the record's own id. It never returns an empty string, so a record whose
// dispatch surface carried no run id is still filed somewhere findable instead
// of being dropped.
func (r EvidenceRecord) RunKey() string {
	for _, candidate := range []string{r.WorkflowRunID, r.TaskID, r.SessionID, r.RecordID} {
		if key := sanitizePathSegment(candidate); key != "" {
			return key
		}
	}
	return "unattributed"
}

// normalized fills in the record's schema version and turns every
// never-populated metric into an explicit "not recorded". It fills in
// absences, never numbers.
func (r EvidenceRecord) normalized() EvidenceRecord {
	if r.SchemaVersion == "" {
		r.SchemaVersion = EvidenceSchemaVersion
	}
	r.Dispatch.DurationMS = r.Dispatch.DurationMS.normalized()
	r.Context.FilesInspected = r.Context.FilesInspected.normalized()
	r.Context.FilesInspectedBytes = r.Context.FilesInspectedBytes.normalized()
	r.Context.RepeatedReads = r.Context.RepeatedReads.normalized()
	r.Context.SourceTokensAvailable = r.Context.SourceTokensAvailable.normalized()
	r.Context.SourceBytesAvailable = r.Context.SourceBytesAvailable.normalized()
	r.Context.ContextSentBytes = r.Context.ContextSentBytes.normalized()
	r.Context.ContextSentTokens = r.Context.ContextSentTokens.normalized()
	for i := range r.Context.Files {
		r.Context.Files[i].Bytes = r.Context.Files[i].Bytes.normalized()
		r.Context.Files[i].EstimatedTokens = r.Context.Files[i].EstimatedTokens.normalized()
	}
	r.ProviderTokens.Prompt = r.ProviderTokens.Prompt.normalized()
	r.ProviderTokens.Output = r.ProviderTokens.Output.normalized()
	r.ProviderTokens.CacheRead = r.ProviderTokens.CacheRead.normalized()
	r.ProviderTokens.CacheWrite = r.ProviderTokens.CacheWrite.normalized()
	r.ProviderTokens.Reasoning = r.ProviderTokens.Reasoning.normalized()
	r.ProviderTokens.Total = r.ProviderTokens.Total.normalized()
	r.Tools.Total = r.Tools.Total.normalized()
	r.Outcomes.VerifyDurationMS = r.Outcomes.VerifyDurationMS.normalized()
	if r.Routing != nil {
		routing := r.Routing.normalized()
		r.Routing = &routing
	}
	return r
}

// Validate checks the record's own required fields and then every metric it
// carries. It is called before any write, so an evidence file that exists is
// an evidence file whose labeling rule held.
func (r EvidenceRecord) Validate() error {
	if strings.TrimSpace(r.RecordID) == "" {
		return fmt.Errorf("%w: recordId is required", ErrEvidenceInvalid)
	}
	if r.SchemaVersion == "" {
		return fmt.Errorf("%w: schemaVersion is required", ErrEvidenceInvalid)
	}
	if r.Role == "" {
		return fmt.Errorf("%w: role is required", ErrEvidenceInvalid)
	}
	if r.GeneratedAt.IsZero() {
		return fmt.Errorf("%w: generatedAt is required", ErrEvidenceInvalid)
	}
	if r.Routing != nil {
		if err := r.Routing.Validate(); err != nil {
			return err
		}
	}
	for _, m := range r.metrics() {
		if err := m.metric.Validate(); err != nil {
			return fmt.Errorf("%w: %s: %w", ErrEvidenceInvalid, m.field, err)
		}
	}
	return nil
}

type namedMetric struct {
	field  string
	metric Metric
}

func (r EvidenceRecord) metrics() []namedMetric {
	out := make([]namedMetric, 0, 16+2*len(r.Context.Files))
	out = append(out,
		namedMetric{"dispatch.durationMs", r.Dispatch.DurationMS},
		namedMetric{"context.filesInspected", r.Context.FilesInspected},
		namedMetric{"context.filesInspectedBytes", r.Context.FilesInspectedBytes},
		namedMetric{"context.repeatedReads", r.Context.RepeatedReads},
		namedMetric{"context.sourceTokensAvailable", r.Context.SourceTokensAvailable},
		namedMetric{"context.sourceBytesAvailable", r.Context.SourceBytesAvailable},
		namedMetric{"context.contextSentBytes", r.Context.ContextSentBytes},
		namedMetric{"context.contextSentTokens", r.Context.ContextSentTokens},
		namedMetric{"providerTokens.prompt", r.ProviderTokens.Prompt},
		namedMetric{"providerTokens.output", r.ProviderTokens.Output},
		namedMetric{"providerTokens.cacheRead", r.ProviderTokens.CacheRead},
		namedMetric{"providerTokens.cacheWrite", r.ProviderTokens.CacheWrite},
		namedMetric{"providerTokens.reasoning", r.ProviderTokens.Reasoning},
		namedMetric{"providerTokens.total", r.ProviderTokens.Total},
		namedMetric{"tools.total", r.Tools.Total},
		namedMetric{"outcomes.verifyDurationMs", r.Outcomes.VerifyDurationMS},
	)
	for i, f := range r.Context.Files {
		out = append(out,
			namedMetric{fmt.Sprintf("context.files[%d].bytes", i), f.Bytes},
			namedMetric{fmt.Sprintf("context.files[%d].estimatedTokens", i), f.EstimatedTokens},
		)
	}
	if r.Routing != nil {
		out = append(out, r.Routing.metrics()...)
	}
	return out
}
