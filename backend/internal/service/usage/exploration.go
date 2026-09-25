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
	out := domain.RunExploration{
		RunID:     runID,
		ProjectID: run.ProjectID,
		Recorded:  len(observations) > 0 || len(calls) > 0,
	}
	groups := map[agentKey]*agentFold{}
	var order []agentKey
	fold := func(k agentKey) *agentFold {
		if g, ok := groups[k]; ok {
			return g
		}
		g := newAgentFold(k)
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
	}
	out.Quality, err = r.quality(ctx, run)
	if err != nil {
		return domain.RunExploration{}, err
	}
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

	calls, input, output, cached, cacheWrite int64
	firstCall                                *time.Time
	firstCallInput                           int64
	callTimes                                []time.Time
	untimedCalls                             int64

	toolCalls, reads, searches, listings, commands, exploreCommands int64
	edits, shellEdits, opsBeforeEdit, unattributed                  int64
	sawEdit, firstEditDerived, sawCodexItems                        bool
	firstPromptBytes                                                *int64
	firstPromptOrdinal                                              int64
	firstEditAt                                                     *time.Time
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

func newAgentFold(k agentKey) *agentFold {
	return &agentFold{
		key:         k,
		models:      map[string]int64{},
		readsByPath: map[string]int64{},
		editedPaths: map[string]bool{},
		scopes:      map[domain.ToolPathScope]int64{},
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
	f.output += c.Tokens.OutputTokens
	f.cached += c.Tokens.CacheReadTokens
	f.cacheWrite += c.Tokens.CacheWriteTokens
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
	if o.AttributionBasis == domain.AttributionApproximate {
		f.approximate++
	}
	switch o.Origin {
	case domain.OriginAOContext:
		f.aoBytes += deref(o.ResultBytes)
		if o.ResultBytes != nil && (f.firstPromptBytes == nil || o.Ordinal < f.firstPromptOrdinal) {
			b := *o.ResultBytes
			f.firstPromptBytes, f.firstPromptOrdinal = &b, o.Ordinal
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

func (f *agentFold) markEdit(o store.RunToolObservation) {
	if f.sawEdit {
		return
	}
	f.sawEdit = true
	if o.ObservedAt != nil {
		t := *o.ObservedAt
		f.firstEditAt = &t
	}
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
	}
	if f.calls > 0 {
		a.ModelCalls = observed(f.calls, "billed provider messages in the transcript")
		a.InputTokens = observed(f.input, "provider-reported input incl. cache reads/writes")
		a.OutputTokens = observed(f.output, "provider-reported output")
		a.CachedInputTokens = observed(f.cached, "provider-reported cache reads")
		a.CacheWriteTokens = observed(f.cacheWrite, "provider-reported cache writes")
	} else {
		for _, m := range []*domain.ExplorationMetric{&a.ModelCalls, &a.InputTokens, &a.OutputTokens, &a.CachedInputTokens, &a.CacheWriteTokens} {
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
	if f.unattributed > 0 {
		lowerBound := fmt.Sprintf("; LOWER BOUND: %d executed command(s) inspected files AO cannot name", f.unattributed)
		readMethod += lowerBound
		searchMethod += lowerBound
		listMethod += lowerBound
	}
	if caps.structuredFileTools {
		a.UnattributedCommands = observed(f.unattributed, "commands whose inspected files are not named by the harness")
		a.FileReads = observed(f.reads, readMethod)
		a.UniqueFilesRead = observed(int64(len(f.readsByPath)), "distinct project-relative paths read")
		a.RepeatedReads = observed(f.projectReads-int64(len(f.readsByPath)), "project reads of an already-read path")
		a.Searches = observed(f.searches, searchMethod)
		a.Listings = observed(f.listings, listMethod)
		a.ExplorationOps = observed(f.reads+f.searches+f.listings, "structured read + search + list calls (explore commands reported separately)")
		switch {
		case !f.sawEdit:
			a.OpsBeforeFirstEdit = observed(f.opsBeforeEdit, "no edit observed: every exploration call is counted")
		case f.firstEditDerived:
			a.OpsBeforeFirstEdit = derived(f.opsBeforeEdit, "exploration calls before the first edit, which was a shell write classified by program name")
		default:
			a.OpsBeforeFirstEdit = observed(f.opsBeforeEdit, "exploration calls before the first structured edit, in transcript order")
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
		for _, m := range []*domain.ExplorationMetric{&a.UnattributedCommands, &a.FileReads, &a.UniqueFilesRead, &a.RepeatedReads, &a.Searches, &a.Listings, &a.ExplorationOps, &a.OpsBeforeFirstEdit, &a.RepoBytesObserved, &a.ExplorationResultBytes} {
			*m = unavailable(reason)
		}
	}
	if len(f.editedPaths) > 0 || caps.structuredFileTools || f.key.harness == string(domain.HarnessCodex) {
		a.UniqueFilesEdited = observed(int64(len(f.editedPaths)), "distinct project-relative paths edited (Claude edit tools; Codex patch_apply_end)")
	} else {
		a.UniqueFilesEdited = unavailable(noToolTelemetry)
	}
	a.CallsBeforeFirstEdit = f.callsBeforeFirstEdit()
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
	return a
}

// callsBeforeFirstEdit counts provider calls made before the first edit. It
// orders calls and observations by their transcript timestamps, two different
// record streams, so it is DERIVED.
func (f *agentFold) callsBeforeFirstEdit() domain.ExplorationMetric {
	if !f.sawEdit {
		if f.calls == 0 {
			return unavailable("no provider calls")
		}
		return derived(f.calls, "no edit observed: every call preceded it")
	}
	if f.firstEditAt == nil || f.untimedCalls > 0 {
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
