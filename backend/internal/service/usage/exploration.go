package usage

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/store"
)

// exploration.go -- Frente 3 / 3C: the read model of what a run's agents
// looked at, what they spent, and how the run turned out.
//
// Every figure travels with its basis. The rule for choosing one:
//
//   - OBSERVED: the provider transcript reported it, or AO counted it exactly
//     from facts the transcript reported (a count of Read calls, a distinct
//     count of their paths, a sum of result lengths). No heuristic.
//   - DERIVED: an inference is involved and Method names it -- a shell
//     command classified by its program name, a prompt assumed to be AO's, a
//     time ordering across two sources.
//   - UNAVAILABLE: the harness does not expose it. The value is nil, never 0.
//
// Nothing here reads a transcript or a file. It folds rows the ingestor
// already wrote.

// explorationStore is the narrow read surface the reader needs.
type explorationStore interface {
	ListRunToolObservations(ctx context.Context, runID string) ([]store.RunToolObservation, error)
	ListRunExplorationCalls(ctx context.Context, runID string) ([]store.RunExplorationCall, error)
	ListRunToolCoverage(ctx context.Context, runID string) ([]store.RunToolCoverage, error)
	ListProjectMemoryContextManifestsForRun(ctx context.Context, projectID domain.ProjectID, runID string) ([]domain.MemoryContextManifest, error)
	GetWorkflowRun(ctx context.Context, id string) (domain.WorkflowRun, bool, error)
	ListWorkflowSteps(ctx context.Context, runID string) ([]domain.WorkflowStep, error)
	ListWorkflowAttempts(ctx context.Context, stepID string) ([]domain.WorkflowAttempt, error)
	ListWorkflowCheckpoints(ctx context.Context, runID string) ([]domain.WorkflowCheckpoint, error)
	ListProviderAttemptsForRun(ctx context.Context, runID string) ([]domain.ProviderAttempt, error)
	GetReviewRun(ctx context.Context, id string) (domain.ReviewRun, bool, error)
}

// ExplorationReader builds RunExploration read models.
type ExplorationReader struct {
	store explorationStore
}

// NewExplorationReader constructs a reader.
func NewExplorationReader(s explorationStore) *ExplorationReader {
	return &ExplorationReader{store: s}
}

// ErrExplorationRunNotFound is returned for a run id AO does not know.
var ErrExplorationRunNotFound = fmt.Errorf("workflow run not found")

// topFilesLimit bounds the per-agent most-read list.
const topFilesLimit = 10

// WorkflowRun returns the exploration read model of one run.
func (r *ExplorationReader) WorkflowRun(ctx context.Context, runID string) (domain.RunExploration, error) {
	run, ok, err := r.store.GetWorkflowRun(ctx, runID)
	if err != nil {
		return domain.RunExploration{}, err
	}
	if !ok {
		return domain.RunExploration{}, ErrExplorationRunNotFound
	}
	observations, err := r.store.ListRunToolObservations(ctx, runID)
	if err != nil {
		return domain.RunExploration{}, err
	}
	calls, err := r.store.ListRunExplorationCalls(ctx, runID)
	if err != nil {
		return domain.RunExploration{}, err
	}
	coverageRows, err := r.store.ListRunToolCoverage(ctx, runID)
	if err != nil {
		return domain.RunExploration{}, err
	}
	coverage := coverageBySubject(coverageRows)
	out := domain.RunExploration{
		RunID:          runID,
		ProjectID:      run.ProjectID,
		Recorded:       len(observations) > 0 || len(calls) > 0,
		ContextSources: contextSourcesOf(run.PolicySnapshot),
	}
	out.MemoryPacks, err = r.memoryPacks(ctx, run)
	if err != nil {
		return domain.RunExploration{}, err
	}
	groups := map[agentKey]*agentFold{}
	var order []agentKey
	fold := func(k agentKey) *agentFold {
		if g, ok := groups[k]; ok {
			return g
		}
		g := newAgentFold(k)
		g.coverage = coverage.of(k.subject)
		groups[k] = g
		order = append(order, k)
		return g
	}
	totals := newAgentFold(agentKey{})
	for _, c := range calls {
		k := agentKey{role: c.Role, cycle: c.Cycle, subject: c.Subject, harness: c.Harness}
		fold(k).addCall(c)
		totals.addCall(c)
	}
	for _, o := range observations {
		k := agentKey{role: o.Role, cycle: o.Cycle, subject: o.Subject, harness: o.Harness}
		fold(k).addObservation(o)
		totals.addObservation(o)
	}
	sort.SliceStable(order, func(i, j int) bool {
		a, b := groups[order[i]], groups[order[j]]
		if !a.firstSeen.Equal(b.firstSeen) {
			if a.firstSeen.IsZero() || b.firstSeen.IsZero() {
				return !a.firstSeen.IsZero()
			}
			return a.firstSeen.Before(b.firstSeen)
		}
		return order[i].less(order[j])
	})
	totals.coverage = coverage.all(order)
	for _, k := range order {
		out.Agents = append(out.Agents, groups[k].result())
	}
	// The totals apply the capabilities of the run's one harness; a run
	// mixing harnesses reports only what every harness can observe.
	totals.key.harness = totalsHarness(out.Agents)
	out.Totals = totals.result()
	if len(out.Agents) > 1 {
		// Sequence figures belong to ONE conversation. Summed across agents
		// they would describe an interleaving that never happened.
		const perAgent = "a per-agent sequence figure; see each agent"
		out.Totals.FirstCallInput = unavailable(perAgent)
		out.Totals.HarnessTokensFirstCall = unavailable(perAgent)
		out.Totals.OpsBeforeFirstEdit = unavailable(perAgent)
		out.Totals.CallsBeforeFirstEdit = unavailable(perAgent)
		if out.Totals.TurnMix.Basis != domain.ExplorationUnavailable && totals.key.harness == "mixed" {
			out.Totals.TurnMix = domain.ExplorationTurnMix{Basis: domain.ExplorationUnavailable, Method: "harnesses classify turns by different methods; see each agent"}
		}
	}
	out.Quality, err = r.quality(ctx, run)
	if err != nil {
		return domain.RunExploration{}, err
	}
	return out, nil
}

// subjectCoverage is what the extractor parsed of each subject's transcripts.
type subjectCoverage map[domain.UsageSubject]domain.ExplorationCoverage

// coverageBySubject folds per-source coverage rows into one verdict per
// subject. A subject is covered when, for every one of its sources, no usage
// event was ingested without the extractor and the extractor has caught up
// with the cursor -- by a single extractor version. "Parsed from byte zero"
// would be the wrong test: the collector legitimately starts a new source row
// mid-file when it resumes a transcript it already knew.
func coverageBySubject(rows []store.RunToolCoverage) subjectCoverage {
	out := subjectCoverage{}
	versions := map[domain.UsageSubject]map[int64]bool{}
	for _, r := range rows {
		c, ok := out[r.Subject]
		if !ok {
			c = domain.ExplorationCoverage{Complete: true}
			versions[r.Subject] = map[int64]bool{}
		}
		switch {
		case r.EventsBeforeCoverage > 0:
			c.Complete = false
			c.Reason = fmt.Sprintf("%d provider call(s) of transcript source %d were ingested without the 3C extractor (before migration 0175 or by an older binary): the tool activity around them is unknown, not zero", r.EventsBeforeCoverage, r.SourceID)
		case r.CoveredFrom >= 0 && r.CoveredTo < r.ByteOffset:
			c.Complete = false
			c.Reason = fmt.Sprintf("the 3C extractor has parsed transcript source %d to byte %d of %d: the rest is unknown, not zero", r.SourceID, r.CoveredTo, r.ByteOffset)
		}
		if r.CoveredFrom >= 0 {
			versions[r.Subject][r.MinExtractor] = true
			versions[r.Subject][r.MaxExtractor] = true
		}
		out[r.Subject] = c
	}
	for subject, vs := range versions {
		c := out[subject]
		for v := range vs {
			c.ExtractorVersions = append(c.ExtractorVersions, v)
		}
		sort.Slice(c.ExtractorVersions, func(i, j int) bool { return c.ExtractorVersions[i] < c.ExtractorVersions[j] })
		if c.Complete && len(c.ExtractorVersions) > 1 {
			c.Complete = false
			c.Reason = fmt.Sprintf("observations were classified by extractor versions %v; mixed classifications are not one measurement", c.ExtractorVersions)
		}
		out[subject] = c
	}
	return out
}

// of is one subject's coverage. A subject with no source row at all has no
// transcript AO read, so there is nothing the extractor could have missed.
func (c subjectCoverage) of(subject domain.UsageSubject) domain.ExplorationCoverage {
	if v, ok := c[subject]; ok {
		return v
	}
	return domain.ExplorationCoverage{Complete: true}
}

// all is the coverage of a set of agents: complete only when each is.
func (c subjectCoverage) all(keys []agentKey) domain.ExplorationCoverage {
	out := domain.ExplorationCoverage{Complete: true}
	seen := map[int64]bool{}
	for _, k := range keys {
		v := c.of(k.subject)
		if !v.Complete && out.Complete {
			out.Complete, out.Reason = false, v.Reason
		}
		for _, x := range v.ExtractorVersions {
			if !seen[x] {
				seen[x] = true
				out.ExtractorVersions = append(out.ExtractorVersions, x)
			}
		}
	}
	sort.Slice(out.ExtractorVersions, func(i, j int) bool { return out.ExtractorVersions[i] < out.ExtractorVersions[j] })
	if out.Complete && len(out.ExtractorVersions) > 1 {
		out.Complete = false
		out.Reason = fmt.Sprintf("the run's agents were classified by extractor versions %v", out.ExtractorVersions)
	}
	return out
}

// contextSourcesOf reads the context-decorator state frozen into a run's
// policy_snapshot. An unreadable or pre-3C snapshot yields the zero value,
// which reads as "not recorded".
func contextSourcesOf(snapshot string) domain.ContextSourcesSnapshot {
	var policy struct {
		ContextSources domain.ContextSourcesSnapshot `json:"contextSources"`
	}
	if json.Unmarshal([]byte(snapshot), &policy) != nil {
		return domain.ContextSourcesSnapshot{}
	}
	return policy.ContextSources
}

// memoryPacks summarises the run's project-memory context manifests, oldest
// first: which pack (digest, indexed commit, generation) each dispatch got.
func (r *ExplorationReader) memoryPacks(ctx context.Context, run domain.WorkflowRun) ([]domain.RunMemoryPack, error) {
	manifests, err := r.store.ListProjectMemoryContextManifestsForRun(ctx, domain.ProjectID(run.ProjectID), run.ID)
	if err != nil {
		return nil, err
	}
	out := make([]domain.RunMemoryPack, 0, len(manifests))
	for _, m := range manifests {
		out = append(out, domain.RunMemoryPack{
			Role:            m.Role,
			TaskRef:         m.TaskRef,
			PackDigest:      m.PackDigest,
			PolicyVersion:   m.PolicyVersion,
			Generation:      m.Generation,
			IndexedCommit:   m.IndexedCommit,
			ItemCount:       len(m.ItemIDs),
			SelectedBytes:   m.SelectedBytes,
			EstimatedTokens: m.EstimatedTokens,
			CreatedAt:       m.CreatedAt,
		})
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].CreatedAt.Before(out[j].CreatedAt) })
	return out, nil
}

type agentKey struct {
	role    domain.WorkflowRole
	cycle   int64
	subject domain.UsageSubject
	harness string
}

func (k agentKey) less(o agentKey) bool {
	if k.role != o.role {
		return k.role < o.role
	}
	if k.cycle != o.cycle {
		return k.cycle < o.cycle
	}
	if k.subject.Kind != o.subject.Kind {
		return k.subject.Kind < o.subject.Kind
	}
	return k.subject.ID < o.subject.ID
}

// totalsHarness names the run's harness when every agent shares one, so the
// totals row can apply that harness's capabilities; "mixed" otherwise.
func totalsHarness(agents []domain.AgentExploration) string {
	h := ""
	for _, a := range agents {
		switch {
		case h == "":
			h = a.Harness
		case a.Harness != h:
			return "mixed"
		}
	}
	return h
}

// agentFold accumulates one agent's calls and observations. Observations are
// folded per subject in transcript order (the query orders them), which is
// what "before the first edit" is measured against.
type agentFold struct {
	key       agentKey
	models    map[string]int64
	firstSeen time.Time
	lastSeen  time.Time
	timed     int64

	calls, input, uncached, output, cached, cacheWrite int64
	firstCall                                          *time.Time
	firstCallInput                                     int64
	callTimes                                          []time.Time
	untimedCalls                                       int64

	toolCalls, reads, searches, listings, commands, exploreCommands int64
	edits, shellEdits, opsBeforeEdit, unattributed                  int64
	sawEdit, firstEditDerived, sawCodexItems                        bool
	firstPromptBytes                                                *int64
	firstPromptOrdinal                                              int64
	firstPromptAt                                                   *time.Time
	firstEditAt                                                     *time.Time
	untimedEdit                                                     bool
	sources                                                         map[int64]bool
	coverage                                                        domain.ExplorationCoverage
	turns                                                           map[domain.TurnClass]int64
	codexToolCalls                                                  []codexToolCall
	readsByPath                                                     map[string]int64
	editedPaths                                                     map[string]bool
	projectReads                                                    int64
	repoBytes, exploreBytes, aoBytes, harnessBytes                  int64
	unobservedResults                                               int64
	scopes                                                          map[domain.ToolPathScope]int64
	approximate                                                     int64
}

// codexSubObservation names the Codex observations that describe a call
// already counted rather than being calls themselves.
var codexSubObservation = map[string]bool{
	"codex_parsed_cmd":  true,
	"codex_file_change": true,
	"patch_apply_end":   true,
}

// codexToolCall is one Codex tool call's time and the turn class it implies,
// kept to derive the per-call turn mix the rollout does not state.
type codexToolCall struct {
	at    *time.Time
	class domain.TurnClass
}

func newAgentFold(k agentKey) *agentFold {
	return &agentFold{
		key:         k,
		models:      map[string]int64{},
		readsByPath: map[string]int64{},
		editedPaths: map[string]bool{},
		scopes:      map[domain.ToolPathScope]int64{},
		sources:     map[int64]bool{},
		turns:       map[domain.TurnClass]int64{},
		coverage:    domain.ExplorationCoverage{Complete: true},
	}
}

func (f *agentFold) seen(t *time.Time) {
	if t == nil {
		return
	}
	if f.firstSeen.IsZero() || t.Before(f.firstSeen) {
		f.firstSeen = *t
	}
	if t.After(f.lastSeen) {
		f.lastSeen = *t
	}
	f.timed++
}

func (f *agentFold) addCall(c store.RunExplorationCall) {
	f.calls++
	f.models[c.ModelID]++
	f.input += c.Tokens.InputTokens
	f.uncached += c.Tokens.UncachedInputTokens
	f.output += c.Tokens.OutputTokens
	f.cached += c.Tokens.CacheReadTokens
	f.cacheWrite += c.Tokens.CacheWriteTokens
	f.turns[c.TurnClass]++
	if c.ObservedAt == nil {
		f.untimedCalls++
		return
	}
	f.seen(c.ObservedAt)
	f.callTimes = append(f.callTimes, *c.ObservedAt)
	if f.firstCall == nil || c.ObservedAt.Before(*f.firstCall) {
		t := *c.ObservedAt
		f.firstCall = &t
		f.firstCallInput = c.Tokens.InputTokens
	}
}

func (f *agentFold) addObservation(o store.RunToolObservation) {
	f.seen(o.ObservedAt)
	if o.SourceID > 0 {
		f.sources[o.SourceID] = true
	}
	if o.AttributionBasis == domain.AttributionApproximate {
		f.approximate++
	}
	switch o.Origin {
	case domain.OriginAOContext:
		f.aoBytes += deref(o.ResultBytes)
		if o.ResultBytes != nil && f.earlierPrompt(o) {
			b := *o.ResultBytes
			f.firstPromptBytes, f.firstPromptOrdinal = &b, o.Ordinal
			f.firstPromptAt = nil
			if o.ObservedAt != nil {
				t := *o.ObservedAt
				f.firstPromptAt = &t
			}
		}
		return
	case domain.OriginHarnessContext:
		f.harnessBytes += deref(o.ResultBytes)
		return
	case domain.OriginUnknown:
		return
	}
	if o.Op == domain.ToolOpEdit && o.PathScope == domain.ToolPathProject {
		f.editedPaths[o.Path] = true
	}
	subObservation := codexSubObservation[o.ToolName]
	if subObservation {
		// A Codex item names what a call already counted did: the files a
		// patch changed, or Codex's own parse of the command it ran. It is
		// not a tool call of its own, and it carries no result of its own.
		f.sawCodexItems = true
		if o.Op == domain.ToolOpEdit {
			f.markEdit(o)
			return
		}
	} else {
		f.toolCalls++
		if o.ResultBytes == nil {
			f.unobservedResults++
		}
		if f.isCodex() {
			f.codexToolCalls = append(f.codexToolCalls, codexToolCall{at: o.ObservedAt, class: codexTurnClass(o.Op)})
		}
	}
	if subObservation && o.ToolName == "codex_parsed_cmd" && o.Op == domain.ToolOpCommand {
		f.unattributed++
		return
	}
	if o.PathScope != domain.ToolPathNone {
		f.scopes[o.PathScope]++
	}
	switch o.Op {
	case domain.ToolOpRead:
		f.reads++
		if o.PathScope == domain.ToolPathProject {
			f.projectReads++
			f.readsByPath[o.Path]++
		}
	case domain.ToolOpSearch:
		f.searches++
	case domain.ToolOpList:
		f.listings++
	case domain.ToolOpCommandExplore:
		f.commands++
		f.exploreCommands++
	case domain.ToolOpCommand:
		f.commands++
	case domain.ToolOpCommandEdit:
		f.commands++
		f.shellEdits++
		if !f.sawEdit {
			f.firstEditDerived = true
		}
		f.markEdit(o)
	case domain.ToolOpEdit:
		f.edits++
		f.markEdit(o)
	}
	if !subObservation && f.key.harness != string(domain.HarnessCodex) &&
		(o.Op == domain.ToolOpCommand || o.Op == domain.ToolOpCommandExplore || o.Op == domain.ToolOpCommandEdit) {
		f.unattributed++
	}
	if subObservation && o.Op.IsExploration() {
		if !f.sawEdit {
			f.opsBeforeEdit++
		}
		return
	}
	if f.isCodex() {
		// A Codex call's exploration is counted from Codex's own parse of it
		// (the parsed_cmd sub-observations above). Counting the call too would
		// count one `cat` twice.
		return
	}
	if o.Op.IsExploration() {
		f.exploreBytes += deref(o.ResultBytes)
		if o.PathScope == domain.ToolPathProject {
			f.repoBytes += deref(o.ResultBytes)
		}
		if !f.sawEdit {
			f.opsBeforeEdit++
		}
	}
}

func (f *agentFold) isCodex() bool { return f.key.harness == string(domain.HarnessCodex) }

// earlierPrompt reports whether o precedes the first prompt seen so far. Time
// orders across transcripts; a byte ordinal only within one.
func (f *agentFold) earlierPrompt(o store.RunToolObservation) bool {
	switch {
	case f.firstPromptBytes == nil:
		return true
	case o.ObservedAt != nil && f.firstPromptAt != nil:
		return o.ObservedAt.Before(*f.firstPromptAt)
	case o.ObservedAt != nil:
		return true
	case f.firstPromptAt != nil:
		return false
	default:
		return o.Ordinal < f.firstPromptOrdinal
	}
}

// markEdit records an edit. The first edit in fold order ends the "before
// the first edit" count; the first edit in TIME is what calls are compared
// against, since a run's transcripts are folded one after another.
func (f *agentFold) markEdit(o store.RunToolObservation) {
	f.sawEdit = true
	if o.ObservedAt == nil {
		f.untimedEdit = true
		return
	}
	if f.firstEditAt == nil || o.ObservedAt.Before(*f.firstEditAt) {
		t := *o.ObservedAt
		f.firstEditAt = &t
	}
}

// codexTurnClass maps a Codex tool call's operation to the turn class the
// same tool would give a Claude call (turn_class.go classifies by tool, so a
// shell call is a command whatever it ran).
func codexTurnClass(op domain.ToolOp) domain.TurnClass {
	switch op {
	case domain.ToolOpCommand, domain.ToolOpCommandExplore, domain.ToolOpCommandEdit:
		return domain.TurnCommand
	case domain.ToolOpEdit:
		return domain.TurnEdit
	case domain.ToolOpRead, domain.ToolOpSearch, domain.ToolOpList, domain.ToolOpWeb:
		return domain.TurnRead
	case domain.ToolOpWait:
		return domain.TurnWait
	case domain.ToolOpDelegate:
		return domain.TurnSubagent
	case domain.ToolOpPlan:
		return domain.TurnPlan
	}
	return domain.TurnUnclassified
}

func deref(v *int64) int64 {
	if v == nil {
		return 0
	}
	return *v
}

func observed(v int64, method string) domain.ExplorationMetric {
	return domain.ExplorationMetric{Value: &v, Basis: domain.ExplorationObserved, Method: method}
}

func derived(v int64, method string) domain.ExplorationMetric {
	return domain.ExplorationMetric{Value: &v, Basis: domain.ExplorationDerived, Method: method}
}

func unavailable(reason string) domain.ExplorationMetric {
	return domain.ExplorationMetric{Basis: domain.ExplorationUnavailable, Method: reason}
}

func ratio(num, den int64, basis domain.ExplorationBasis, method string) domain.ExplorationRatio {
	if den <= 0 {
		return domain.ExplorationRatio{Basis: domain.ExplorationUnavailable, Method: "no context bytes observed"}
	}
	v := float64(num) / float64(den)
	return domain.ExplorationRatio{Value: &v, Basis: basis, Method: method}
}

// harnessCapabilities is what each harness's transcript lets AO observe. It
// is the capability matrix of docs/frente3/3c-exploration-observability.md in
// code form; a figure a harness cannot report is UNAVAILABLE for its agents.
type harnessCapabilities struct {
	structuredFileTools bool // Read/Grep/Glob with a path argument
	resultItems         bool
	harnessContext      bool // injected material fully visible
	toolCalls           bool
}

func capabilitiesOf(harness string) harnessCapabilities {
	switch domain.AgentHarness(harness) {
	case domain.HarnessClaudeCode:
		return harnessCapabilities{structuredFileTools: true, resultItems: true, harnessContext: true, toolCalls: true}
	case domain.HarnessCodex:
		return harnessCapabilities{toolCalls: true}
	}
	return harnessCapabilities{}
}

const (
	codexNoFileTools = "this Codex rollout carries no structured parsed_cmd/FileChange items (older Codex version); AO does not parse shell or code-mode text"
	noToolTelemetry  = "this harness exposes no tool telemetry AO can read"
)

func (f *agentFold) result() domain.AgentExploration {
	caps := capabilitiesOf(f.key.harness)
	if f.key.harness == string(domain.HarnessCodex) && f.sawCodexItems {
		// This rollout carries Codex's structured items: its own parse of
		// each command names the files it read, searched and listed.
		caps.structuredFileTools = true
	}
	if f.key.harness == "mixed" || f.key.harness == "" {
		// Totals across harnesses: count what was observed, but every
		// capability-dependent figure is only as good as the weakest harness.
		caps = harnessCapabilities{toolCalls: true}
	}
	a := domain.AgentExploration{
		Role:                   f.key.role,
		Cycle:                  f.key.cycle,
		Subject:                f.key.subject,
		Harness:                f.key.harness,
		Models:                 sortedModels(f.models),
		PathScopes:             f.scopes,
		ApproximateAttribution: f.approximate,
		Sources:                int64(len(f.sources)),
		ToolCoverage:           f.coverage,
	}
	if f.calls > 0 {
		a.ModelCalls = observed(f.calls, "billed provider messages in the transcript")
		a.InputTokens = observed(f.input, "provider-reported input incl. cache reads/writes")
		a.UncachedInputTokens = observed(f.uncached, "provider-reported input that was neither a cache read nor a cache write")
		a.FreshInputTokens = observed(f.input-f.cached, "input not served from the cache: input minus cache reads (uncached + cache writes)")
		a.OutputTokens = observed(f.output, "provider-reported output")
		a.CachedInputTokens = observed(f.cached, "provider-reported cache reads")
		a.CacheWriteTokens = observed(f.cacheWrite, "provider-reported cache writes")
	} else {
		for _, m := range []*domain.ExplorationMetric{&a.ModelCalls, &a.InputTokens, &a.UncachedInputTokens, &a.FreshInputTokens, &a.OutputTokens, &a.CachedInputTokens, &a.CacheWriteTokens} {
			*m = unavailable("no provider usage recorded for this agent")
		}
	}
	if f.firstCall != nil {
		a.FirstCallInput = observed(f.firstCallInput, "input tokens of the earliest timed call: AO prompt + harness context")
	} else {
		a.FirstCallInput = unavailable("no timed provider call")
	}
	switch {
	case f.firstCall == nil:
		a.HarnessTokensFirstCall = unavailable("no timed provider call")
	case f.firstPromptBytes == nil:
		a.HarnessTokensFirstCall = unavailable("no AO prompt observed to subtract")
	case f.firstPromptAt == nil && len(f.sources) > 1:
		a.HarnessTokensFirstCall = unavailable(multiSource(len(f.sources)))
	default:
		est := f.firstCallInput - (*f.firstPromptBytes+3)/4
		if est < 0 {
			est = 0
		}
		a.HarnessTokensFirstCall = derived(est, "first-call input minus the first AO prompt at ~4 bytes/token: harness system prompt, tool schemas and injected instructions (estimate)")
	}

	if !caps.toolCalls {
		for _, m := range []*domain.ExplorationMetric{&a.ToolCalls, &a.Commands, &a.Edits} {
			*m = unavailable(noToolTelemetry)
		}
	} else {
		a.ToolCalls = observed(f.toolCalls, "tool calls in the transcript")
		a.Commands = observed(f.commands, "shell/code execution calls")
		a.Edits = observed(f.edits, "structured edit tool calls")
	}
	a.ShellEdits = derived(f.shellEdits, "shell commands that write files (sed -i, output redirect, tee, patch, git apply); no path is extracted")
	a.ExploreCommands = derived(f.exploreCommands, "shell commands whose leading program is a read-only inspector (cat, rg, sed -n, git log ...); no path is extracted")
	if f.key.harness == string(domain.HarnessCodex) {
		a.ExploreCommands = derived(f.exploreCommands, "structured shell calls only; code-mode `exec` JavaScript is not parsed, so this is a lower bound")
	}
	readMethod, searchMethod, listMethod := "Read/NotebookRead tool calls", "Grep tool calls", "Glob/LS tool calls"
	if f.key.harness == string(domain.HarnessCodex) {
		readMethod = "Codex parsed_cmd `read` items (the provider's own parse of executed commands)"
		searchMethod = "Codex parsed_cmd `search` items"
		listMethod = "Codex parsed_cmd `list_files` items"
	}
	lowerBound := f.unattributed > 0
	if lowerBound {
		note := fmt.Sprintf("; LOWER BOUND: %d executed command(s) inspected files AO cannot name", f.unattributed)
		readMethod += note
		searchMethod += note
		listMethod += note
	}
	if caps.structuredFileTools {
		a.UnattributedCommands = observed(f.unattributed, "commands whose inspected files are not named by the harness")
		a.FileReads = floor(observed(f.reads, readMethod), lowerBound)
		a.UniqueFilesRead = floor(observed(int64(len(f.readsByPath)), "distinct project-relative paths read"), lowerBound)
		a.RepeatedReads = floor(observed(f.projectReads-int64(len(f.readsByPath)), "project reads of an already-read path"), lowerBound)
		a.Searches = floor(observed(f.searches, searchMethod), lowerBound)
		a.Listings = floor(observed(f.listings, listMethod), lowerBound)
		a.ExplorationOps = floor(observed(f.reads+f.searches+f.listings, "structured read + search + list calls (explore commands reported separately)"), lowerBound)
		a.ExplorationOpsAll = f.explorationOpsAll()
		switch {
		case !f.sawEdit:
			a.OpsBeforeFirstEdit = unavailable("no edit observed: the sequence is censored, and exploration before an edit that never happened is not a measurement")
		case len(f.sources) > 1:
			a.OpsBeforeFirstEdit = unavailable(multiSource(len(f.sources)))
		case f.firstEditDerived:
			a.OpsBeforeFirstEdit = floor(derived(f.opsBeforeEdit, "exploration calls before the first edit, which was a shell write classified by program name"), lowerBound)
		default:
			a.OpsBeforeFirstEdit = floor(observed(f.opsBeforeEdit, "exploration calls before the first structured edit, in transcript order"), lowerBound)
		}
		a.RepoBytesObserved = observed(f.repoBytes, "text length the harness returned for project-scoped read/search/list calls (includes its line-number formatting)")
		a.ExplorationResultBytes = observed(f.exploreBytes, "text length returned for all exploration calls")
		if f.key.harness == string(domain.HarnessCodex) {
			const perCall = "Codex returns one output per exec call; bytes per file read are not reported"
			a.RepoBytesObserved = unavailable(perCall)
			a.ExplorationResultBytes = unavailable(perCall)
		}
	} else {
		reason := noToolTelemetry
		if f.key.harness == string(domain.HarnessCodex) {
			reason = codexNoFileTools
		}
		for _, m := range []*domain.ExplorationMetric{&a.UnattributedCommands, &a.FileReads, &a.UniqueFilesRead, &a.RepeatedReads, &a.Searches, &a.Listings, &a.ExplorationOps, &a.ExplorationOpsAll, &a.OpsBeforeFirstEdit, &a.RepoBytesObserved, &a.ExplorationResultBytes} {
			*m = unavailable(reason)
		}
	}
	if len(f.editedPaths) > 0 || caps.structuredFileTools || f.key.harness == string(domain.HarnessCodex) {
		method := "distinct project-relative paths edited (Claude edit tools; Codex FileChange/patch_apply_end)"
		if f.shellEdits > 0 {
			method += fmt.Sprintf("; LOWER BOUND: %d shell write(s) name no path", f.shellEdits)
		}
		a.UniqueFilesEdited = floor(observed(int64(len(f.editedPaths)), method), f.shellEdits > 0)
	} else {
		a.UniqueFilesEdited = unavailable(noToolTelemetry)
	}
	a.CallsBeforeFirstEdit = f.callsBeforeFirstEdit()
	a.TurnMix = f.turnMix()
	a.UnobservedResults = observed(f.unobservedResults, "tool calls whose result was never observed")

	a.AOContextBytes = derived(f.aoBytes, "prompt text delivered into the conversation, assumed AO's (a human typing into the pane is indistinguishable)")
	if caps.harnessContext {
		a.HarnessContextBytes = derived(f.harnessBytes, "harness-injected records (attachments, meta, compaction summaries) as recorded in the transcript; recorded size, not tokenizer input")
	} else if f.key.harness == string(domain.HarnessCodex) {
		a.HarnessContextBytes = derived(f.harnessBytes, "base instructions + developer messages only: AGENTS.md/environment injected as user-role items are not separable and are excluded (lower bound)")
	} else {
		a.HarnessContextBytes = unavailable(noToolTelemetry)
	}
	if caps.structuredFileTools && caps.harnessContext {
		den := f.exploreBytes + f.aoBytes + f.harnessBytes
		a.ExplorationRatio = ratio(f.exploreBytes, den, domain.ExplorationDerived, "exploration result bytes / (exploration + AO + harness bytes); bytes, not tokens")
		a.AOContextRatio = ratio(f.aoBytes, den, domain.ExplorationDerived, "AO prompt bytes / same denominator")
		a.HarnessContextRatio = ratio(f.harnessBytes, den, domain.ExplorationDerived, "harness-injected bytes / same denominator")
	} else {
		reason := "requires structured exploration and full harness-context visibility"
		a.ExplorationRatio = domain.ExplorationRatio{Basis: domain.ExplorationUnavailable, Method: reason}
		a.AOContextRatio = domain.ExplorationRatio{Basis: domain.ExplorationUnavailable, Method: reason}
		a.HarnessContextRatio = domain.ExplorationRatio{Basis: domain.ExplorationUnavailable, Method: reason}
	}
	if f.timed >= 2 {
		a.ActiveSpanMS = derived(f.lastSeen.Sub(f.firstSeen).Milliseconds(), "first to last transcript timestamp of this agent (idle time included)")
	} else {
		a.ActiveSpanMS = unavailable("fewer than two timed transcript records")
	}
	a.TopFiles = topFiles(f.readsByPath)
	if !f.coverage.Complete {
		withoutToolTelemetry(&a, f.coverage.Reason)
	}
	return a
}

// explorationOpsAll is M3: every exploration operation AO can classify, by
// one method per harness (see domain.AgentExploration.ExplorationOpsAll).
func (f *agentFold) explorationOpsAll() domain.ExplorationMetric {
	if f.isCodex() {
		m := observed(f.reads+f.searches+f.listings, "Codex parsed_cmd read + search + list_files items (the provider's own parse of each executed command); commands it could not parse are unattributedCommands")
		return floor(m, f.unattributed > 0)
	}
	return derived(f.reads+f.searches+f.listings+f.exploreCommands,
		"structured Read/Grep/Glob/LS calls + shell commands whose leading program only inspects (cat, rg, sed -n, git log ...); other shell commands are not counted")
}

// turnMix is how the agent's calls split by turn class. Claude states a
// class per billed message (turn_class, observed). A Codex rollout states
// none: each tool call is assigned to the first provider call timestamped at
// or after it, which is how a rollout orders a response's items before the
// token_count that bills it (derived).
func (f *agentFold) turnMix() domain.ExplorationTurnMix {
	if f.calls == 0 {
		return domain.ExplorationTurnMix{Basis: domain.ExplorationUnavailable, Method: "no provider calls"}
	}
	if !f.isCodex() {
		counts := make(map[domain.TurnClass]int64, len(f.turns))
		for c, n := range f.turns {
			counts[c] = n
		}
		return domain.ExplorationTurnMix{Basis: domain.ExplorationObserved, Method: "turn_class recorded per billed message (tool names only)", Counts: counts}
	}
	if f.untimedCalls > 0 {
		return domain.ExplorationTurnMix{Basis: domain.ExplorationUnavailable, Method: "some Codex calls carry no timestamp to order tool calls against"}
	}
	times := append([]time.Time(nil), f.callTimes...)
	sort.Slice(times, func(i, j int) bool { return times[i].Before(times[j]) })
	perCall := make([]map[domain.TurnClass]bool, len(times))
	for _, tc := range f.codexToolCalls {
		if tc.at == nil {
			return domain.ExplorationTurnMix{Basis: domain.ExplorationUnavailable, Method: "some Codex tool calls carry no timestamp"}
		}
		i := sort.Search(len(times), func(i int) bool { return !times[i].Before(*tc.at) })
		if i == len(times) || tc.class == domain.TurnUnclassified {
			continue
		}
		if perCall[i] == nil {
			perCall[i] = map[domain.TurnClass]bool{}
		}
		perCall[i][tc.class] = true
	}
	counts := map[domain.TurnClass]int64{}
	for _, classes := range perCall {
		switch len(classes) {
		case 0:
			counts[domain.TurnMessage]++
		case 1:
			for c := range classes {
				counts[c]++
			}
		default:
			counts[domain.TurnMixed]++
		}
	}
	return domain.ExplorationTurnMix{Basis: domain.ExplorationDerived, Method: "each Codex tool call assigned to the first provider call timestamped at or after it; a call with no tool call is a message", Counts: counts}
}

// floor marks a metric as a lower bound when unattributed activity may add
// to it.
func floor(m domain.ExplorationMetric, lowerBound bool) domain.ExplorationMetric {
	if lowerBound && m.Basis != domain.ExplorationUnavailable {
		m.LowerBound = true
	}
	return m
}

func multiSource(n int) string {
	return fmt.Sprintf("the agent's observations span %d transcripts, whose byte orders are not comparable", n)
}

// withoutToolTelemetry blanks every figure that depends on the 3C extractor
// having parsed the agent's transcripts. Token and call counts come from the
// usage ledger and stand.
func withoutToolTelemetry(a *domain.AgentExploration, reason string) {
	for _, m := range []*domain.ExplorationMetric{
		&a.HarnessTokensFirstCall, &a.ToolCalls, &a.FileReads, &a.UniqueFilesRead, &a.RepeatedReads,
		&a.Searches, &a.Listings, &a.Commands, &a.ExploreCommands, &a.ExplorationOps, &a.ExplorationOpsAll,
		&a.UnattributedCommands, &a.Edits, &a.ShellEdits, &a.UniqueFilesEdited, &a.OpsBeforeFirstEdit,
		&a.CallsBeforeFirstEdit, &a.RepoBytesObserved, &a.ExplorationResultBytes, &a.AOContextBytes,
		&a.HarnessContextBytes, &a.UnobservedResults,
	} {
		*m = unavailable(reason)
	}
	for _, r := range []*domain.ExplorationRatio{&a.ExplorationRatio, &a.AOContextRatio, &a.HarnessContextRatio} {
		*r = domain.ExplorationRatio{Basis: domain.ExplorationUnavailable, Method: reason}
	}
	a.PathScopes = map[domain.ToolPathScope]int64{}
	a.TopFiles = nil
	if a.Harness == string(domain.HarnessCodex) {
		a.TurnMix = domain.ExplorationTurnMix{Basis: domain.ExplorationUnavailable, Method: reason}
	}
}

// callsBeforeFirstEdit counts provider calls made before the first edit. It
// orders calls and observations by their transcript timestamps, two different
// record streams, so it is DERIVED.
func (f *agentFold) callsBeforeFirstEdit() domain.ExplorationMetric {
	if !f.sawEdit {
		return unavailable("no edit observed: the sequence is censored, and calls before an edit that never happened is not a measurement")
	}
	if f.firstEditAt == nil || f.untimedEdit || f.untimedCalls > 0 {
		return unavailable("the first edit or some calls carry no timestamp")
	}
	var n int64
	for _, t := range f.callTimes {
		if t.Before(*f.firstEditAt) {
			n++
		}
	}
	return derived(n, "provider calls timestamped before the first edit's timestamp")
}

func sortedModels(m map[string]int64) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Slice(out, func(i, j int) bool {
		if m[out[i]] != m[out[j]] {
			return m[out[i]] > m[out[j]]
		}
		return out[i] < out[j]
	})
	return out
}

func topFiles(m map[string]int64) []domain.ExplorationFileCount {
	out := make([]domain.ExplorationFileCount, 0, len(m))
	for p, n := range m {
		out = append(out, domain.ExplorationFileCount{Path: p, Reads: n})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Reads != out[j].Reads {
			return out[i].Reads > out[j].Reads
		}
		return out[i].Path < out[j].Path
	})
	if len(out) > topFilesLimit {
		out = out[:topFilesLimit]
	}
	return out
}

// quality reads the outcome facts of the run. Everything is read from durable
// state the workflow already keeps; the workflow Coordinator's GetRun is
// deliberately NOT used, because it observes and advances steps.
func (r *ExplorationReader) quality(ctx context.Context, run domain.WorkflowRun) (domain.RunQualitySignals, error) {
	q := domain.RunQualitySignals{FinalState: run.State, Completed: run.State == domain.WorkflowRunCompleted}
	if run.CompletedAt != nil {
		q.DurationMS = observed(run.CompletedAt.Sub(run.CreatedAt).Milliseconds(), "run created to completed (AO clock)")
	} else {
		q.DurationMS = unavailable("run has not completed")
	}
	steps, err := r.store.ListWorkflowSteps(ctx, run.ID)
	if err != nil {
		return q, err
	}
	var attempts, failed, retries int64
	var reviewSteps []domain.WorkflowStep
	for _, step := range steps {
		list, err := r.store.ListWorkflowAttempts(ctx, step.ID)
		if err != nil {
			return q, err
		}
		attempts += int64(len(list))
		if len(list) > 1 {
			retries += int64(len(list) - 1)
		}
		for _, a := range list {
			if a.Outcome == domain.WorkflowAttemptFailed {
				failed++
			}
		}
		if step.Kind == domain.WorkflowStepReview {
			reviewSteps = append(reviewSteps, step)
		}
	}
	q.Attempts = observed(attempts, "workflow attempts across all steps")
	q.FailedAttempts = observed(failed, "attempts with outcome failed")
	q.Retries = observed(retries, "attempts beyond the first, per step")

	providers, err := r.store.ListProviderAttemptsForRun(ctx, run.ID)
	if err != nil {
		return q, err
	}
	var hops int64
	for _, p := range providers {
		if p.Ordinal > 1 {
			hops++
		}
	}
	q.ProviderFailovers = observed(hops, "provider attempts beyond the preferred provider")

	checkpoints, err := r.store.ListWorkflowCheckpoints(ctx, run.ID)
	if err != nil {
		return q, err
	}
	var verifyRuns, checksPassed, checksFailed int64
	var latestVerify *domain.WorkflowCheckpoint
	fixCycles := map[string]bool{}
	reviewRuns := map[string]bool{}
	for i := range checkpoints {
		cp := checkpoints[i]
		if cp.ReviewRunID != nil && *cp.ReviewRunID != "" {
			reviewRuns[*cp.ReviewRunID] = true
		}
		switch cp.DurablePhase {
		case "verify_result":
			verifyRuns++
			if latestVerify == nil || !cp.CreatedAt.Before(latestVerify.CreatedAt) {
				latestVerify = &checkpoints[i]
			}
		case "fix_dispatched":
			var rec struct {
				CycleNumber int `json:"cycleNumber"`
			}
			step := ""
			if cp.WorkflowStepID != nil {
				step = *cp.WorkflowStepID
			}
			if json.Unmarshal([]byte(cp.RetryState), &rec) == nil && rec.CycleNumber > 0 {
				fixCycles[fmt.Sprintf("%s#%d", step, rec.CycleNumber)] = true
			} else {
				fixCycles[cp.ID] = true
			}
		}
	}
	q.VerifyRuns = observed(verifyRuns, "verify_result checkpoints")
	if latestVerify != nil {
		var res struct {
			Passed bool `json:"passed"`
			Checks []struct {
				Passed bool `json:"passed"`
			} `json:"checks"`
		}
		if json.Unmarshal([]byte(latestVerify.RetryState), &res) == nil {
			passed := res.Passed
			q.VerifyPassed = &passed
			for _, c := range res.Checks {
				if c.Passed {
					checksPassed++
				} else {
					checksFailed++
				}
			}
			q.ChecksPassed = observed(checksPassed, "checks passed in the latest verify result")
			q.ChecksFailed = observed(checksFailed, "checks failed in the latest verify result")
		}
	}
	if q.VerifyPassed == nil {
		q.ChecksPassed = unavailable("no readable verify result")
		q.ChecksFailed = unavailable("no readable verify result")
	}
	q.FixCycles = observed(int64(len(fixCycles)), "distinct fix cycles dispatched")
	for _, step := range reviewSteps {
		if step.ReviewRunID != nil && *step.ReviewRunID != "" {
			reviewRuns[*step.ReviewRunID] = true
		}
	}
	q.ReviewRuns = observed(int64(len(reviewRuns)), "distinct review runs linked to the run")
	var latest *domain.ReviewRun
	for id := range reviewRuns {
		rr, ok, err := r.store.GetReviewRun(ctx, id)
		if err != nil {
			return q, err
		}
		if ok && (latest == nil || rr.CreatedAt.After(latest.CreatedAt)) {
			rrCopy := rr
			latest = &rrCopy
		}
	}
	if latest != nil {
		q.FinalReviewVerdict = latest.Verdict
	}
	return q, nil
}
