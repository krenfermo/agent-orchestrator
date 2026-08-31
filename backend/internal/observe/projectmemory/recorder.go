package projectmemory

import (
	stdctx "context"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

// Capabilities declares which signals a given dispatch surface can actually
// report. It is the mechanism that keeps the baseline honest: a metric whose
// surface cannot expose it is recorded as unavailable with that reason, rather
// than as a measured zero that would read like "this agent inspected no
// files".
//
// A worker spawn, for instance, hands a prompt to a provider process that then
// reads files on its own; AO sees the prompt (ContextPayload) but not the
// reads (FileReads) or the tool calls (ToolCalls). A planner generation, by
// contrast, is handed a context document AO itself assembled, so its file
// reads are visible.
type Capabilities struct {
	// FileReads is true when the surface reports which files were read.
	FileReads bool
	// ToolCalls is true when the surface reports the agent's tool calls.
	ToolCalls bool
	// ContextPayload is true when the surface carries the payload AO sends, so
	// its size can be measured.
	ContextPayload bool
	// ProviderTokens is true when provider usage telemetry is delivered for
	// this dispatch.
	ProviderTokens bool
	// SourceScope is true when the dispatch's reachable source scope is
	// scanned for this record.
	SourceScope bool
}

// Dispatch identifies one agent dispatch about to be instrumented.
type Dispatch struct {
	Role           domain.WorkflowRole
	WorkflowRunID  string
	WorkflowStepID string
	TaskID         string
	ProjectID      string
	SessionID      string
	Harness        string
	Model          string
	// Observable declares what this surface can report; see Capabilities.
	Observable Capabilities
	// UnavailableReason overrides the reason stamped on metrics this surface
	// cannot report. The defaults describe a live dispatch surface ("this
	// dispatch surface does not report the agent's file reads"); a caller that
	// is not a live dispatch -- the baseline harness measuring assembled
	// context without calling a provider -- says so here instead, so the file
	// explains its own gaps rather than borrowing an explanation that does not
	// apply. Empty keeps the defaults.
	UnavailableReason string
}

// Recorder opens a Span per agent dispatch and writes the finished evidence
// record to its Sink. It is safe for concurrent use.
type Recorder struct {
	sink  Sink
	clock func() time.Time
	newID func() string
}

// Option customizes a Recorder. The defaults (wall clock, UUID ids) are what
// production uses; the options exist so tests can pin both.
type Option func(*Recorder)

// WithClock pins the recorder's clock.
func WithClock(clock func() time.Time) Option {
	return func(r *Recorder) {
		if clock != nil {
			r.clock = clock
		}
	}
}

// WithIDs pins the recorder's record-id source.
func WithIDs(newID func() string) Option {
	return func(r *Recorder) {
		if newID != nil {
			r.newID = newID
		}
	}
}

// NewRecorder builds a recorder writing to sink.
func NewRecorder(sink Sink, opts ...Option) *Recorder {
	r := &Recorder{
		sink:  sink,
		clock: func() time.Time { return time.Now().UTC() },
		newID: func() string { return "pmb-" + uuid.NewString() },
	}
	for _, opt := range opts {
		opt(r)
	}
	return r
}

// Begin opens a span for one dispatch. A nil recorder returns a nil span, and
// every span method tolerates a nil receiver, so an uninstrumented deployment
// costs each call site nothing more than a nil check it does not have to
// write.
func (r *Recorder) Begin(d Dispatch) *Span {
	if r == nil {
		return nil
	}
	return &Span{
		recorder:  r,
		dispatch:  d,
		recordID:  r.newID(),
		startedAt: r.clock(),
		files:     make(map[string]*fileAccumulator),
		tools:     make(map[string]int64),
	}
}

type fileAccumulator struct {
	reads int64
	bytes int64
}

// Span accumulates observations for one dispatch and, on Finish, turns them
// into an evidence record.
type Span struct {
	recorder  *Recorder
	dispatch  Dispatch
	recordID  string
	startedAt time.Time

	mu        sync.Mutex
	fileOrder []string
	files     map[string]*fileAccumulator
	tools     map[string]int64
	toolTotal int64

	contextBytes    int64
	contextObserved bool

	scan         SourceScan
	scanObserved bool

	routing *RoutingMetrics
	memory  *MemoryMetrics

	usage         domain.UsageMetricTotals
	usageObserved bool

	provider string
	model    string
	session  string

	reviewRunIDs   []string
	reviewVerdict  string
	verifyExitCode *int
	verifyPassed   *bool
	verifyDuration *int64

	notes []string
}

// RecordID is the id the finished evidence record will carry. It is available
// before Finish so a caller can correlate a log line with the file that will
// be written.
func (s *Span) RecordID() string {
	if s == nil {
		return ""
	}
	return s.recordID
}

// ObserveFileRead records that path was read, contributing bytes of context.
// Calling it twice for the same path is how a repeated read is counted — that
// repetition is a finding, not a duplicate to be collapsed.
func (s *Span) ObserveFileRead(path string, bytes int64) {
	if s == nil || path == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	acc, ok := s.files[path]
	if !ok {
		acc = &fileAccumulator{}
		s.files[path] = acc
		s.fileOrder = append(s.fileOrder, path)
	}
	acc.reads++
	if bytes > 0 {
		acc.bytes += bytes
	}
}

// ObserveToolCall records one tool invocation by name.
func (s *Span) ObserveToolCall(name string) {
	if s == nil || name == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.tools[name]++
	s.toolTotal++
}

// ObserveContextSent records a payload AO handed the provider — a prompt, a
// context document, a fix message. Multiple payloads for one dispatch add up,
// because they all cost the same context window.
func (s *Span) ObserveContextSent(payload string) {
	s.ObserveContextBytes(int64(len(payload)))
}

// ObserveContextBytes is ObserveContextSent for a payload whose size is known
// without holding it in memory.
func (s *Span) ObserveContextBytes(bytes int64) {
	if s == nil || bytes < 0 {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.contextBytes += bytes
	s.contextObserved = true
}

// ObserveContextRouting records what the role-aware context router decided for
// this dispatch. It is how the routing block reaches an evidence record, and
// it is additive: a dispatch that never calls it produces exactly the record
// it produced before routing existed, except for the disabled-routing block
// build derives from the payload size when there is one to derive it from.
//
// The last call wins. A dispatch surface routes its payload once, so a second
// call means a caller changed its mind about what it sent, and the record
// should describe what was sent rather than the first guess.
func (s *Span) ObserveContextRouting(routing RoutingMetrics) {
	if s == nil {
		return
	}
	normalized := routing.normalized()
	s.mu.Lock()
	defer s.mu.Unlock()
	s.routing = &normalized
}

// ObserveRoutingFromContext records the routing block a dispatch call carries
// in its context, if it carries one, and reports whether it did. It is the
// bridge between the two independent dispatch wrappers — see WithRouting.
func (s *Span) ObserveRoutingFromContext(ctx stdctx.Context) bool {
	routing, ok := RoutingFromContext(ctx)
	if !ok {
		return false
	}
	s.ObserveContextRouting(routing)
	return true
}

// ObserveMemory records what project memory decided for this dispatch. Like
// ObserveContextRouting it is last-write-wins: a wrapper that assembles the
// memory twice (a compact attempt then a wider one) should describe what was
// actually sent rather than its first attempt.
func (s *Span) ObserveMemory(metrics MemoryMetrics) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.memory = &metrics
}

// ObserveMemoryFromContext records the memory block a dispatch call carries in
// its context, if it carries one, and reports whether it did. It is the same
// bridge between independent wrappers that ObserveRoutingFromContext is — see
// WithMemory.
func (s *Span) ObserveMemoryFromContext(ctx stdctx.Context) bool {
	metrics, ok := MemoryFromContext(ctx)
	if !ok {
		return false
	}
	s.ObserveMemory(metrics)
	return true
}

// memoryMetrics resolves the record's memory block.
//
// A dispatch that memory touched reports what it decided. Every other dispatch
// gets NO block, and the field stays absent from the file exactly as it was
// before P2-B. There is deliberately no "memory disabled" block invented for
// dispatches memory never saw: an absent field is the honest statement that
// this surface has nothing to say, and a fabricated one would make an
// untouched dispatch look measured.
func (s *Span) memoryMetrics() *MemoryMetrics {
	if s.memory == nil {
		return nil
	}
	metrics := *s.memory
	return &metrics
}

// ObserveSourceScope records how much source the dispatch's declared scope
// made reachable.
func (s *Span) ObserveSourceScope(scan SourceScan) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.scan = scan
	s.scanObserved = true
}

// ObserveProviderUsage records the provider's own token telemetry for this
// dispatch. Nil fields inside totals stay unavailable in the record; they are
// never read as zero.
func (s *Span) ObserveProviderUsage(totals domain.UsageMetricTotals) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.usage = totals
	s.usageObserved = true
}

// Identify fills in the provider-side identity a dispatch only learns after it
// returns (the session it created, the provider and model that answered).
// Empty arguments leave the existing value alone.
func (s *Span) Identify(sessionID, provider, model string) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if sessionID != "" {
		s.session = sessionID
	}
	if provider != "" {
		s.provider = provider
	}
	if model != "" {
		s.model = model
	}
}

// LinkReviewRun ties this dispatch to a review run of the same workflow run.
func (s *Span) LinkReviewRun(reviewRunID string) {
	if s == nil || reviewRunID == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reviewRunIDs = append(s.reviewRunIDs, reviewRunID)
}

// LinkReviewVerdict records the reviewer's verdict when one is known at
// dispatch time. An unknown verdict is left empty rather than defaulted.
func (s *Span) LinkReviewVerdict(verdict string) {
	if s == nil || verdict == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reviewVerdict = verdict
}

// LinkVerifyOutcome records the result of a verify command run.
func (s *Span) LinkVerifyOutcome(exitCode int, durationMS int64) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	passed := exitCode == 0
	s.verifyExitCode = &exitCode
	s.verifyPassed = &passed
	if durationMS >= 0 {
		s.verifyDuration = &durationMS
	}
}

// Note attaches a remark about how this record was produced. Notes never carry
// metrics.
func (s *Span) Note(note string) {
	if s == nil || note == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.notes = append(s.notes, note)
}

// Finish closes the span, builds the evidence record, and writes it. dispatchErr
// is the error the wrapped call returned, or nil; a failed dispatch is still
// recorded, because a failed launch is a real fact about the pipeline.
//
// The returned error is a recording failure only — the caller's own dispatch
// result is never affected by it.
func (s *Span) Finish(ctx stdctx.Context, dispatchErr error) (EvidenceRecord, string, error) {
	if s == nil {
		return EvidenceRecord{}, "", nil
	}
	record := s.build(dispatchErr)
	if s.recorder == nil || s.recorder.sink == nil {
		return record, "", nil
	}
	path, err := s.recorder.sink.Write(ctx, record)
	return record, path, err
}

// Build produces the evidence record without writing it. It exists for callers
// that want to inspect or aggregate a record before it is persisted.
func (s *Span) Build(dispatchErr error) EvidenceRecord {
	if s == nil {
		return EvidenceRecord{}
	}
	return s.build(dispatchErr)
}

func (s *Span) build(dispatchErr error) EvidenceRecord {
	s.mu.Lock()
	defer s.mu.Unlock()

	completedAt := s.recorder.clock()
	sessionID := s.dispatch.SessionID
	if s.session != "" {
		sessionID = s.session
	}
	provider := s.provider
	if provider == "" {
		provider = domain.ProviderForHarness(domain.AgentHarness(s.dispatch.Harness))
	}
	model := s.dispatch.Model
	if s.model != "" {
		model = s.model
	}

	record := EvidenceRecord{
		SchemaVersion:  EvidenceSchemaVersion,
		RecordID:       s.recordID,
		GeneratedAt:    completedAt,
		Role:           s.dispatch.Role,
		WorkflowRunID:  s.dispatch.WorkflowRunID,
		WorkflowStepID: s.dispatch.WorkflowStepID,
		TaskID:         s.dispatch.TaskID,
		ProjectID:      s.dispatch.ProjectID,
		SessionID:      sessionID,
		Harness:        s.dispatch.Harness,
		Provider:       provider,
		Model:          model,
		Dispatch: DispatchOutcome{
			StartedAt:   s.startedAt,
			CompletedAt: completedAt,
			DurationMS:  Measured(completedAt.Sub(s.startedAt).Milliseconds(), "wall clock around the wrapped dispatch call"),
			Succeeded:   dispatchErr == nil,
		},
		Context:        s.contextMetrics(),
		Routing:        s.routingMetrics(),
		Memory:         s.memoryMetrics(),
		ProviderTokens: s.providerTokens(),
		Tools:          s.toolMetrics(),
		Outcomes: OutcomeLinks{
			ReviewRunIDs:     s.reviewRunIDs,
			ReviewVerdict:    s.reviewVerdict,
			VerifyExitCode:   s.verifyExitCode,
			VerifyPassed:     s.verifyPassed,
			VerifyDurationMS: verifyDurationMetric(s.verifyDuration),
		},
		Notes: s.notes,
	}
	if dispatchErr != nil {
		record.Dispatch.Error = dispatchErr.Error()
	}
	return record
}

func verifyDurationMetric(ms *int64) Metric {
	if ms == nil {
		return Unavailable("this dispatch ran no verify command")
	}
	return Measured(*ms, "verify command wall clock reported by the runner")
}

// reason resolves the explanation stamped on a metric this dispatch could not
// obtain, honoring Dispatch.UnavailableReason when the caller set one.
func (s *Span) reason(defaultReason string) string {
	if s.dispatch.UnavailableReason != "" {
		return s.dispatch.UnavailableReason
	}
	return defaultReason
}

func (s *Span) contextMetrics() ContextMetrics {
	noReads := s.reason("this dispatch surface does not report the agent's file reads")
	noPayload := s.reason("this dispatch surface does not carry the payload AO sends")
	out := ContextMetrics{
		FilesInspected:        Unavailable(noReads),
		FilesInspectedBytes:   Unavailable(noReads),
		RepeatedReads:         Unavailable(noReads),
		SourceTokensAvailable: Unavailable("no source scope was scanned for this dispatch"),
		SourceBytesAvailable:  Unavailable("no source scope was scanned for this dispatch"),
		ContextSentBytes:      Unavailable(noPayload),
		ContextSentTokens:     Unavailable(noPayload),
	}
	if s.dispatch.Observable.FileReads {
		var totalBytes, repeated int64
		files := make([]FileInspection, 0, len(s.fileOrder))
		for _, path := range s.fileOrder {
			acc := s.files[path]
			totalBytes += acc.bytes
			repeated += acc.reads - 1
			files = append(files, FileInspection{
				Path:            path,
				Reads:           acc.reads,
				Bytes:           Measured(acc.bytes, "byte length of the content AO read for this path"),
				EstimatedTokens: EstimatedTokensFor(acc.bytes),
			})
		}
		out.FilesInspected = Measured(int64(len(s.fileOrder)), "distinct paths this dispatch read")
		out.FilesInspectedBytes = Measured(totalBytes, "bytes read across every read, re-reads counted again")
		out.RepeatedReads = Measured(repeated, "reads beyond the first for each path")
		out.Files = files
	}
	if s.dispatch.Observable.SourceScope && s.scanObserved {
		out.SourceBytesAvailable = Measured(s.scan.Bytes, "size on disk of the source files in this dispatch's declared scope")
		out.SourceTokensAvailable = EstimatedTokensFor(s.scan.Bytes)
	}
	if s.dispatch.Observable.ContextPayload && s.contextObserved {
		out.ContextSentBytes = Measured(s.contextBytes, "byte length of the payload AO handed the provider")
		out.ContextSentTokens = EstimatedTokensFor(s.contextBytes)
	}
	// A real prompt-token count from the provider beats AO's byte-derived
	// estimate for the same quantity, so it replaces it rather than sitting
	// beside it.
	if s.usageObserved && s.usage.InputTokens != nil {
		out.ContextSentTokens = Measured(*s.usage.InputTokens, "prompt tokens reported by provider telemetry")
	}
	return out
}

// routingMetrics resolves the record's routing block.
//
// A dispatch the router shaped reports what it decided. A dispatch it did not
// shape reports a disabled block sized by the payload that went out anyway --
// but only when this surface actually carries that payload, because a routing
// block whose sizes were invented would be worse than no routing block at all.
// Every other dispatch gets no block, and the field stays absent from the
// file exactly as it was before routing existed.
func (s *Span) routingMetrics() *RoutingMetrics {
	if s.routing != nil {
		routing := *s.routing
		return &routing
	}
	if !s.dispatch.Observable.ContextPayload || !s.contextObserved {
		return nil
	}
	routing := RoutingDisabled(s.contextBytes, "no context router selection reached this dispatch; the full assembled payload was sent")
	return &routing
}

func (s *Span) providerTokens() ProviderTokens {
	reason := s.reason("this dispatch surface delivers no provider token telemetry")
	if s.dispatch.Observable.ProviderTokens {
		reason = "provider telemetry reported no value for this counter"
		if !s.usageObserved {
			reason = "no provider telemetry was observed for this dispatch"
		}
	}
	const method = "provider usage telemetry"
	return ProviderTokens{
		Prompt:     MeasuredOrUnavailable(s.usage.InputTokens, method, reason),
		Output:     MeasuredOrUnavailable(s.usage.OutputTokens, method, reason),
		CacheRead:  MeasuredOrUnavailable(s.usage.CacheReadTokens, method, reason),
		CacheWrite: MeasuredOrUnavailable(s.usage.CacheWriteTokens, method, reason),
		Reasoning:  MeasuredOrUnavailable(s.usage.ReasoningTokens, method, reason),
		Total:      totalProviderTokens(s.usage, reason),
	}
}

// totalProviderTokens sums only the counters the provider actually reported.
// If it reported none, the total is unavailable — a sum of nothing is not
// zero tokens, it is an unknown number of tokens.
func totalProviderTokens(totals domain.UsageMetricTotals, reason string) Metric {
	var sum int64
	var reported bool
	for _, counter := range []*int64{totals.InputTokens, totals.OutputTokens, totals.CacheReadTokens, totals.CacheWriteTokens, totals.ReasoningTokens} {
		if counter == nil {
			continue
		}
		reported = true
		sum += *counter
	}
	if !reported {
		return Unavailable(reason)
	}
	return Measured(sum, "sum of the token counters provider telemetry reported")
}

func (s *Span) toolMetrics() ToolMetrics {
	if !s.dispatch.Observable.ToolCalls {
		return ToolMetrics{Total: Unavailable(s.reason("this dispatch surface does not report the agent's tool calls"))}
	}
	out := ToolMetrics{Total: Measured(s.toolTotal, "tool invocations observed during this dispatch")}
	if len(s.tools) > 0 {
		byName := make(map[string]int64, len(s.tools))
		for name, count := range s.tools {
			byName[name] = count
		}
		out.ByName = byName
	}
	return out
}
