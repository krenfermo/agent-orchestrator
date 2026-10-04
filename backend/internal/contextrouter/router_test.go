package contextrouter

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/codegraph"
	baseline "github.com/aoagents/agent-orchestrator/backend/internal/observe/projectmemory"
	memory "github.com/aoagents/agent-orchestrator/backend/internal/projectmemory"
)

type fakeDiff struct {
	diff codegraph.Diff
	err  error
	// calls counts how many times the source was consulted, so a test can
	// prove an expansion re-retrieved rather than reused.
	calls int
}

func (f *fakeDiff) Changes(context.Context, Project) (codegraph.Diff, error) {
	f.calls++
	return f.diff, f.err
}

type fakeGraph struct {
	byFile map[string]codegraph.QueryResult
	err    error
	calls  int
}

func (f *fakeGraph) Query(_ context.Context, req codegraph.QueryRequest) (codegraph.QueryResult, error) {
	f.calls++
	if f.err != nil {
		return codegraph.QueryResult{}, f.err
	}
	return f.byFile[req.File], nil
}

type fakeMemory struct {
	items []memory.MemoryItem
	err   error
}

func (f *fakeMemory) List(string) ([]memory.MemoryItem, error) { return f.items, f.err }

func testProject() Project {
	return Project{ID: "proj-1", Root: "/checkout/proj-1"}
}

func testTask() Task {
	return Task{ID: "task-1", Title: "wire the router", Objective: "Assemble a bounded, role-aware context payload."}
}

func newTestRouter(t *testing.T, opts Options) *Router {
	t.Helper()
	router, err := New(opts)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return router
}

func fullSources() Options {
	return Options{
		Diff: &fakeDiff{diff: codegraph.Diff{Changes: []codegraph.FileChange{
			{Status: codegraph.ChangeModified, Path: "backend/internal/contextrouter/router.go"},
			{Status: codegraph.ChangeAdded, Path: "backend/internal/contextrouter/budget.go"},
			{Status: codegraph.ChangeDeleted, Path: "backend/internal/legacy/old.go"},
			{Status: codegraph.ChangeRenamed, Path: "docs/router.md", OldPath: "docs/routing.md"},
		}}},
		Graph: &fakeGraph{byFile: map[string]codegraph.QueryResult{
			"backend/internal/contextrouter/router.go": {
				Symbols: []codegraph.Symbol{
					{ID: "router.go#function:Select", Name: "Select", Kind: codegraph.SymbolFunction, File: "backend/internal/contextrouter/router.go", Line: 42},
				},
				Outgoing: []codegraph.Edge{{Kind: codegraph.EdgeCall, From: "router.go#function:Select", To: "pack"}},
			},
			"backend/internal/contextrouter/budget.go": {
				Symbols: []codegraph.Symbol{
					{ID: "budget.go#type:Budget", Name: "Budget", Kind: codegraph.SymbolType, File: "backend/internal/contextrouter/budget.go", Line: 7},
				},
			},
		}},
		Memory: &fakeMemory{items: []memory.MemoryItem{
			{
				ID: "mem-near", Project: "proj-1", Type: memory.TypeNote, Confidence: 0.5,
				Content:   strings.Repeat("the router budget is measured in estimated tokens. ", 12),
				Source:    memory.Source{Kind: memory.SourceManual, Path: "backend/internal/contextrouter/router.go"},
				UpdatedAt: time.Unix(200, 0).UTC(),
			},
			{
				ID: "mem-far", Project: "proj-1", Type: memory.TypeNote, Confidence: 0.9,
				Content:   "an unrelated fact about the deploy pipeline",
				Source:    memory.Source{Kind: memory.SourceManual, Path: "deploy/pipeline.yaml"},
				UpdatedAt: time.Unix(100, 0).UTC(),
			},
			{
				ID: "mem-stale", Project: "proj-1", Type: memory.TypeNote, Confidence: 1,
				Content: "a fact whose commit is gone", Stale: true, StaleReason: "source commit unreachable",
				Source: memory.Source{Kind: memory.SourceManual, Path: "backend/internal/contextrouter/router.go"},
			},
		}},
	}
}

// tokenEstimateMatchesBaseline holds this package's sizing to the baseline
// harness's, so a routed payload and a measured one are directly comparable.
func TestTokenEstimateMatchesBaseline(t *testing.T) {
	for _, size := range []int{0, 1, 3, 4, 5, 4096} {
		text := strings.Repeat("a", size)
		if got, want := estimateTokens(text), int(baseline.EstimateTokensFromBytes(int64(size))); got != want {
			t.Fatalf("estimateTokens(%d bytes) = %d, baseline says %d", size, got, want)
		}
	}
	if bytesPerToken*1 != 4 {
		t.Fatalf("bytesPerToken drifted from the baseline heuristic")
	}
}

func TestSelectRejectsUnknownRoleAndEmptyTask(t *testing.T) {
	router := newTestRouter(t, Options{})
	if _, err := router.Select(context.Background(), Request{Role: "architect", Task: testTask(), Project: testProject()}); !errors.Is(err, ErrRequest) {
		t.Fatalf("unknown role: got %v, want ErrRequest", err)
	}
	if _, err := router.Select(context.Background(), Request{Role: RoleWorker, Project: testProject()}); !errors.Is(err, ErrRequest) {
		t.Fatalf("empty task: got %v, want ErrRequest", err)
	}
}

// A planner and a reviewer are budgeted larger than a fix or a verify
// dispatch. The assertion is on the defaults themselves and on what actually
// comes out of Select for identical input, because a budget table that is
// right on paper and ignored by the packer is not a budget.
func TestRoleBudgetsDifferByRole(t *testing.T) {
	budgets := DefaultBudgets()
	for _, big := range []Role{RolePlanner, RoleReviewer} {
		for _, small := range []Role{RoleFix, RoleVerify} {
			b, s := budgets.For(big), budgets.For(small)
			if b.CompactTokens <= s.CompactTokens || b.ExpandedTokens <= s.ExpandedTokens || b.HardCapTokens <= s.HardCapTokens {
				t.Fatalf("role %q (%+v) is not budgeted above role %q (%+v)", big, b, small, s)
			}
		}
	}

	opts := fullSources()
	router := newTestRouter(t, opts)
	// Enough document text that the smaller budgets genuinely bind: a payload
	// that fits every role's budget would prove nothing about the budgets.
	docs := make([]Document, 0, 8)
	for i := 0; i < 8; i++ {
		docs = append(docs, Document{
			Path:    fmt.Sprintf("docs/reference-%d.md", i),
			Content: strings.Repeat("repository conventions and hard rules. ", 400),
		})
	}

	sizes := map[Role]int{}
	for _, role := range Roles() {
		selection, err := router.Select(context.Background(), Request{Role: role, Task: testTask(), Project: testProject(), Documents: docs})
		if err != nil {
			t.Fatalf("Select(%s): %v", role, err)
		}
		if !selection.WithinBudget() {
			t.Fatalf("Select(%s) produced %d tokens above its cap %d", role, selection.EstimatedTokens, selection.Budget.HardCapTokens)
		}
		if selection.EstimatedTokens > selection.Limit {
			t.Fatalf("Select(%s) produced %d tokens above its compact limit %d", role, selection.EstimatedTokens, selection.Limit)
		}
		sizes[role] = selection.EstimatedTokens
	}
	if sizes[RolePlanner] <= sizes[RoleFix] || sizes[RolePlanner] <= sizes[RoleVerify] {
		t.Fatalf("planner payload (%d) is not larger than fix (%d) / verify (%d)", sizes[RolePlanner], sizes[RoleFix], sizes[RoleVerify])
	}
	if sizes[RoleReviewer] <= sizes[RoleFix] || sizes[RoleReviewer] <= sizes[RoleVerify] {
		t.Fatalf("reviewer payload (%d) is not larger than fix (%d) / verify (%d)", sizes[RoleReviewer], sizes[RoleFix], sizes[RoleVerify])
	}
}

// The order of sections, not only their size, is role-aware: a planner reads
// documents before the diff, a worker reads the diff first.
func TestSectionOrderIsRoleAware(t *testing.T) {
	router := newTestRouter(t, fullSources())
	docs := []Document{{Path: "AGENTS.md", Content: "conventions"}}

	firstEvidenceKind := func(role Role) SectionKind {
		t.Helper()
		selection, err := router.Select(context.Background(), Request{Role: role, Task: testTask(), Project: testProject(), Documents: docs})
		if err != nil {
			t.Fatalf("Select(%s): %v", role, err)
		}
		for _, section := range selection.Sections {
			if section.Kind != SectionTask {
				return section.Kind
			}
		}
		t.Fatalf("Select(%s) produced no section beyond the task", role)
		return ""
	}
	if got := firstEvidenceKind(RolePlanner); got != SectionDocument {
		t.Fatalf("planner leads with %q, want %q", got, SectionDocument)
	}
	if got := firstEvidenceKind(RoleWorker); got != SectionDiff {
		t.Fatalf("worker leads with %q, want %q", got, SectionDiff)
	}
}

// The task section is mandatory and leads every role's payload.
func TestTaskSectionAlwaysLeads(t *testing.T) {
	router := newTestRouter(t, fullSources())
	for _, role := range Roles() {
		selection, err := router.Select(context.Background(), Request{Role: role, Task: testTask(), Project: testProject()})
		if err != nil {
			t.Fatalf("Select(%s): %v", role, err)
		}
		if len(selection.Sections) == 0 || selection.Sections[0].Kind != SectionTask {
			t.Fatalf("Select(%s) did not lead with the task section: %+v", role, selection.Sections)
		}
		if !strings.Contains(selection.Sections[0].Content, "Assemble a bounded") {
			t.Fatalf("Select(%s) task section lost the objective: %q", role, selection.Sections[0].Content)
		}
	}
}

// Stale memory is withheld and said to be withheld; a fact about a file the
// change touches outranks an unrelated one with higher confidence.
func TestMemoryIsRankedAndStaleWithheld(t *testing.T) {
	router := newTestRouter(t, fullSources())
	selection, err := router.Select(context.Background(), Request{Role: RolePlanner, Task: testTask(), Project: testProject()})
	if err != nil {
		t.Fatalf("Select: %v", err)
	}
	var memoryContents []string
	for _, section := range selection.Sections {
		if section.Kind == SectionMemory {
			memoryContents = append(memoryContents, section.Content)
		}
	}
	if len(memoryContents) != 2 {
		t.Fatalf("got %d memory sections, want 2 (the stale one withheld): %v", len(memoryContents), memoryContents)
	}
	if !strings.Contains(memoryContents[0], "estimated tokens") {
		t.Fatalf("the touched-file memory item did not rank first: %q", memoryContents[0])
	}
	for _, content := range memoryContents {
		if strings.Contains(content, "commit is gone") {
			t.Fatalf("a stale memory item reached the payload: %q", content)
		}
	}
	if !strings.Contains(strings.Join(selection.Notes, " "), "1 stale memory item(s) withheld") {
		t.Fatalf("notes did not report the withheld stale item: %v", selection.Notes)
	}
}

// A source that fails costs its own evidence and nothing else: the selection
// is still produced, the failure is a note, and the selection is marked
// insufficient so a caller knows to expand.
func TestFailingSourceDegradesRatherThanFails(t *testing.T) {
	opts := fullSources()
	opts.Diff = &fakeDiff{err: errors.New("git exploded")}
	router := newTestRouter(t, opts)
	selection, err := router.Select(context.Background(), Request{Role: RoleWorker, Task: testTask(), Project: testProject()})
	if err != nil {
		t.Fatalf("Select: %v", err)
	}
	if selection.EvidenceSufficient {
		t.Fatal("a failed source left the selection marked sufficient")
	}
	if !strings.Contains(strings.Join(selection.Notes, " "), "git exploded") {
		t.Fatalf("notes did not report the source failure: %v", selection.Notes)
	}
	for _, section := range selection.Sections {
		if section.Kind == SectionDiff {
			t.Fatal("a failed diff source still produced a diff section")
		}
	}
}

// A router with no sources configured still routes, and says which evidence it
// did not have rather than pretending there was none.
func TestMissingSourcesAreNotedNotFailed(t *testing.T) {
	router := newTestRouter(t, Options{})
	selection, err := router.Select(context.Background(), Request{Role: RoleWorker, Task: testTask(), Project: testProject()})
	if err != nil {
		t.Fatalf("Select: %v", err)
	}
	notes := strings.Join(selection.Notes, " | ")
	for _, want := range []string{"no diff source", "no code graph", "no project memory"} {
		if !strings.Contains(notes, want) {
			t.Fatalf("notes %q do not mention %q", notes, want)
		}
	}
	if !selection.EvidenceSufficient {
		t.Fatal("an absent source is a configuration, not a failure; the selection should be sufficient")
	}
	if selection.Expandable {
		t.Fatal("nothing to expand into, yet the selection advertised expansion")
	}
}

// Progressive expansion: the compact pass truncates, reports itself
// insufficient, and a bounded Expand call retrieves the detail.
func TestExpansionTriggersOnInsufficientEvidence(t *testing.T) {
	opts := fullSources()
	longFact := strings.Repeat("a durable fact about this module that does not fit a compact retrieval. ", 10)
	opts.Memory = &fakeMemory{items: []memory.MemoryItem{{
		ID: "mem-long", Project: "proj-1", Type: memory.TypeNote, Confidence: 0.8,
		Content:   longFact,
		Source:    memory.Source{Kind: memory.SourceManual, Path: "backend/internal/contextrouter/router.go"},
		UpdatedAt: time.Unix(10, 0).UTC(),
	}}}
	diff := opts.Diff.(*fakeDiff)
	router := newTestRouter(t, opts)
	req := Request{Role: RolePlanner, Task: testTask(), Project: testProject()}

	compact, err := router.Select(context.Background(), req)
	if err != nil {
		t.Fatalf("Select: %v", err)
	}
	if compact.Tier != TierCompact {
		t.Fatalf("Select produced tier %q, want %q", compact.Tier, TierCompact)
	}
	if compact.EvidenceSufficient || !compact.Expandable {
		t.Fatalf("a truncated compact pass should be insufficient and expandable: %+v", compact)
	}

	expanded, err := router.Expand(context.Background(), req, compact)
	if err != nil {
		t.Fatalf("Expand: %v", err)
	}
	if expanded.Tier != TierExpanded {
		t.Fatalf("Expand produced tier %q, want %q", expanded.Tier, TierExpanded)
	}
	if expanded.Limit <= compact.Limit {
		t.Fatalf("expanded limit %d is not above the compact limit %d", expanded.Limit, compact.Limit)
	}
	if expanded.EstimatedTokens <= compact.EstimatedTokens {
		t.Fatalf("expansion did not retrieve more: %d tokens vs %d", expanded.EstimatedTokens, compact.EstimatedTokens)
	}
	if !expanded.WithinBudget() {
		t.Fatalf("expansion exceeded the hard cap: %d > %d", expanded.EstimatedTokens, expanded.Budget.HardCapTokens)
	}
	if expanded.Expandable {
		t.Fatal("an expanded selection advertised further expansion")
	}
	var sawWholeFact bool
	for _, section := range expanded.Sections {
		if section.Kind == SectionMemory && strings.Contains(section.Content, strings.TrimSpace(longFact[len(longFact)-40:])) {
			sawWholeFact = true
		}
	}
	if !sawWholeFact {
		t.Fatal("expansion did not deliver the whole memory item")
	}
	if diff.calls != 2 {
		t.Fatalf("the diff source was consulted %d time(s); expansion must re-retrieve", diff.calls)
	}
}

// Expansion is not automatic: a sufficient compact selection is returned
// unchanged, with a note saying why, and no extra retrieval is paid for.
func TestExpandSkipsWhenEvidenceIsSufficient(t *testing.T) {
	opts := fullSources()
	opts.Memory = &fakeMemory{items: []memory.MemoryItem{{
		ID: "mem-small", Project: "proj-1", Type: memory.TypeNote, Confidence: 0.4,
		Content: "small fact", Source: memory.Source{Kind: memory.SourceManual},
	}}}
	diff := opts.Diff.(*fakeDiff)
	router := newTestRouter(t, opts)
	req := Request{Role: RolePlanner, Task: testTask(), Project: testProject()}

	compact, err := router.Select(context.Background(), req)
	if err != nil {
		t.Fatalf("Select: %v", err)
	}
	if !compact.EvidenceSufficient {
		t.Fatalf("expected a sufficient compact selection, got %+v", compact)
	}
	callsAfterSelect := diff.calls

	expanded, err := router.Expand(context.Background(), req, compact)
	if err != nil {
		t.Fatalf("Expand: %v", err)
	}
	if expanded.Tier != TierCompact {
		t.Fatalf("Expand escalated a sufficient selection to tier %q", expanded.Tier)
	}
	if expanded.EstimatedTokens != compact.EstimatedTokens {
		t.Fatalf("Expand changed a sufficient selection: %d vs %d tokens", expanded.EstimatedTokens, compact.EstimatedTokens)
	}
	if !strings.Contains(strings.Join(expanded.Notes, " "), "expansion skipped") {
		t.Fatalf("notes did not explain the skipped expansion: %v", expanded.Notes)
	}
	if diff.calls != callsAfterSelect {
		t.Fatalf("a skipped expansion still retrieved: %d calls vs %d", diff.calls, callsAfterSelect)
	}
}

// An already-expanded selection is never expanded again.
func TestExpandIsIdempotentOnAnExpandedSelection(t *testing.T) {
	router := newTestRouter(t, fullSources())
	req := Request{Role: RoleWorker, Task: testTask(), Project: testProject(), ForceExpand: true}
	first, err := router.Expand(context.Background(), req, Selection{Role: RoleWorker, Tier: TierCompact})
	if err != nil {
		t.Fatalf("Expand: %v", err)
	}
	second, err := router.Expand(context.Background(), req, first)
	if err != nil {
		t.Fatalf("Expand again: %v", err)
	}
	if second.Tier != TierExpanded || second.EstimatedTokens != first.EstimatedTokens {
		t.Fatalf("a second expansion changed the selection: %+v vs %+v", second, first)
	}
	if second.Expandable {
		t.Fatal("an expanded selection still advertises expansion")
	}
}

// Expand refuses a prior selection assembled for a different role rather than
// silently re-budgeting it.
func TestExpandRejectsRoleMismatch(t *testing.T) {
	router := newTestRouter(t, fullSources())
	_, err := router.Expand(context.Background(), Request{Role: RoleWorker, Task: testTask(), Project: testProject()}, Selection{Role: RolePlanner, Tier: TierCompact})
	if !errors.Is(err, ErrRequest) {
		t.Fatalf("got %v, want ErrRequest", err)
	}
}

// The hard cap is never exceeded: not by a compact pass, not by an expansion,
// and not by a forced expansion over sources far larger than the budget.
func TestHardCapIsNeverExceeded(t *testing.T) {
	huge := strings.Repeat("x", 400_000)
	opts := fullSources()
	opts.Memory = &fakeMemory{items: []memory.MemoryItem{
		{ID: "m1", Project: "proj-1", Type: memory.TypeNote, Confidence: 1, Content: huge, Source: memory.Source{Kind: memory.SourceManual}},
		{ID: "m2", Project: "proj-1", Type: memory.TypeNote, Confidence: 1, Content: huge, Source: memory.Source{Kind: memory.SourceManual}},
	}}
	tight, err := DefaultBudgets().With(RoleFix, Budget{CompactTokens: 40, ExpandedTokens: 60, HardCapTokens: 60})
	if err != nil {
		t.Fatalf("With: %v", err)
	}
	opts.Budgets = tight
	router := newTestRouter(t, opts)

	req := Request{
		Role:      RoleFix,
		Task:      Task{ID: "task-1", Objective: strings.Repeat("fix the thing. ", 500)},
		Project:   testProject(),
		Documents: []Document{{Path: "AGENTS.md", Content: huge}, {Path: "README.md", Content: huge}},
	}

	compact, err := router.Select(context.Background(), req)
	if err != nil {
		t.Fatalf("Select: %v", err)
	}
	if compact.EstimatedTokens > compact.Limit {
		t.Fatalf("compact pass produced %d tokens over its limit %d", compact.EstimatedTokens, compact.Limit)
	}
	if !compact.WithinBudget() {
		t.Fatalf("compact pass exceeded the hard cap: %d > %d", compact.EstimatedTokens, compact.Budget.HardCapTokens)
	}
	if len(compact.Dropped) == 0 {
		t.Fatal("a payload this far over budget dropped nothing; the packer is not enforcing anything")
	}

	req.ForceExpand = true
	expanded, err := router.Expand(context.Background(), req, compact)
	if err != nil {
		t.Fatalf("Expand: %v", err)
	}
	if expanded.EstimatedTokens > 60 {
		t.Fatalf("forced expansion produced %d tokens over the hard cap of 60", expanded.EstimatedTokens)
	}
	if !expanded.WithinBudget() {
		t.Fatalf("forced expansion exceeded the hard cap: %d > %d", expanded.EstimatedTokens, expanded.Budget.HardCapTokens)
	}
	// Rendering is what a dispatch surface actually sends, so the cap has to
	// hold on the rendered text too, not only on the accounting.
	if got := estimateTokens(expanded.Render()); got > 60+len(expanded.Sections)*8 {
		t.Fatalf("the rendered payload (%d tokens) is not bounded by the cap", got)
	}
	// The task always survives, even when everything else is dropped.
	if len(expanded.Sections) == 0 || expanded.Sections[0].Kind != SectionTask {
		t.Fatalf("the mandatory task section was dropped under pressure: %+v", expanded.Sections)
	}
}

// Every dropped candidate is reported with the size it would have needed, so
// "it did not fit" is never silent.
func TestDroppedSectionsAreReported(t *testing.T) {
	opts := fullSources()
	tight, err := DefaultBudgets().With(RoleVerify, Budget{CompactTokens: 60, ExpandedTokens: 80, HardCapTokens: 80})
	if err != nil {
		t.Fatalf("With: %v", err)
	}
	opts.Budgets = tight
	router := newTestRouter(t, opts)
	selection, err := router.Select(context.Background(), Request{
		Role:      RoleVerify,
		Task:      testTask(),
		Project:   testProject(),
		Documents: []Document{{Path: "AGENTS.md", Content: strings.Repeat("conventions. ", 2000)}},
	})
	if err != nil {
		t.Fatalf("Select: %v", err)
	}
	if len(selection.Dropped) == 0 {
		t.Fatalf("nothing reported as dropped from a payload of %d tokens against a limit of %d", selection.EstimatedTokens, selection.Limit)
	}
	for _, dropped := range selection.Dropped {
		if dropped.Reason == "" || dropped.EstimatedTokens <= 0 {
			t.Fatalf("a dropped section was reported without its reason or size: %+v", dropped)
		}
	}
	if selection.EvidenceSufficient {
		t.Fatal("a selection that dropped evidence reported itself sufficient")
	}
}

// A deleted file has no symbols left to look up, so the graph is not asked
// about it; the diff still reports the deletion.
func TestDeletedFilesAreNotQueriedInTheGraph(t *testing.T) {
	opts := fullSources()
	graph := opts.Graph.(*fakeGraph)
	router := newTestRouter(t, opts)
	selection, err := router.Select(context.Background(), Request{Role: RoleWorker, Task: testTask(), Project: testProject()})
	if err != nil {
		t.Fatalf("Select: %v", err)
	}
	if graph.calls != 3 {
		t.Fatalf("graph consulted %d time(s), want 3 (the deleted path excluded)", graph.calls)
	}
	var diffContent string
	for _, section := range selection.Sections {
		if section.Kind == SectionDiff {
			diffContent = section.Content
		}
	}
	if !strings.Contains(diffContent, "backend/internal/legacy/old.go") {
		t.Fatalf("the deletion is missing from the diff section: %q", diffContent)
	}
	if !strings.Contains(diffContent, "docs/routing.md -> docs/router.md") {
		t.Fatalf("the rename lost its old path: %q", diffContent)
	}
}

// A project with no code graph yet is not an error, and does not make the
// selection look like a failed retrieval.
func TestUnindexedProjectIsNoted(t *testing.T) {
	opts := fullSources()
	opts.Graph = &fakeGraph{err: fmt.Errorf("wrapped: %w", codegraph.ErrNotIndexed)}
	router := newTestRouter(t, opts)
	selection, err := router.Select(context.Background(), Request{Role: RoleWorker, Task: testTask(), Project: testProject()})
	if err != nil {
		t.Fatalf("Select: %v", err)
	}
	if !strings.Contains(strings.Join(selection.Notes, " "), "has not been indexed") {
		t.Fatalf("notes did not report the unindexed project: %v", selection.Notes)
	}
	for _, section := range selection.Sections {
		if section.Kind == SectionGraph {
			t.Fatal("an unindexed project produced graph sections")
		}
	}
}

func TestTruncateToTokensStaysWithinBudget(t *testing.T) {
	long := strings.Repeat("abcdef ", 500)
	for _, tokens := range []int{1, 5, 32, 100, 1000} {
		got, cut := truncateToTokens(long, tokens)
		if estimateTokens(got) > tokens {
			t.Fatalf("truncateToTokens(%d) returned %d tokens", tokens, estimateTokens(got))
		}
		if !cut && tokens < estimateTokens(long) {
			t.Fatalf("truncateToTokens(%d) did not report the cut", tokens)
		}
	}
	if got, cut := truncateToTokens("short", 1000); got != "short" || cut {
		t.Fatalf("a fitting string was altered: %q cut=%v", got, cut)
	}
	if got, _ := truncateToTokens("día", 0); got != "" {
		t.Fatalf("a zero budget returned %q", got)
	}
}

func TestCanceledContextIsRefused(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	router := newTestRouter(t, fullSources())
	if _, err := router.Select(ctx, Request{Role: RoleWorker, Task: testTask(), Project: testProject()}); !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v, want context.Canceled", err)
	}
}

// A request with no checkout root cannot carry diff or graph evidence: both
// sources are keyed by the root, and the production diff source refuses an
// empty one. That is reported as unavailable evidence, not as a retrieval
// failure — marking it a failure would make the selection insufficient and buy
// an expansion that cannot possibly help.
func TestMissingProjectRootIsNotedNotFailed(t *testing.T) {
	opts := fullSources()
	opts.Memory = &fakeMemory{items: []memory.MemoryItem{{
		ID: "mem-small", Project: "proj-1", Type: memory.TypeNote, Confidence: 0.4,
		Content: "small fact", Source: memory.Source{Kind: memory.SourceManual},
	}}}
	diff := opts.Diff.(*fakeDiff)
	graph := opts.Graph.(*fakeGraph)
	router := newTestRouter(t, opts)

	selection, err := router.Select(context.Background(), Request{
		Role:    RoleWorker,
		Task:    testTask(),
		Project: Project{ID: "proj-1"},
	})
	if err != nil {
		t.Fatalf("Select: %v", err)
	}
	if diff.calls != 0 {
		t.Fatalf("the diff source was consulted without a root: %d call(s)", diff.calls)
	}
	if graph.calls != 0 {
		t.Fatalf("the code graph was consulted without a root: %d call(s)", graph.calls)
	}
	notes := strings.Join(selection.Notes, " | ")
	if !strings.Contains(notes, "diff evidence unavailable: the request carries no project root") {
		t.Fatalf("notes %q do not report the missing root for the diff", notes)
	}
	if !strings.Contains(notes, "graph evidence unavailable: the request carries no project root") {
		t.Fatalf("notes %q do not report the missing root for the graph", notes)
	}
	if !selection.EvidenceSufficient {
		t.Fatal("a missing root is a request that cannot carry that evidence, not a failed retrieval")
	}
	if selection.Expandable {
		t.Fatal("a selection with no root to retrieve from advertised an expansion that cannot help")
	}
	for _, section := range selection.Sections {
		if section.Kind == SectionDiff || section.Kind == SectionGraph {
			t.Fatalf("evidence was produced without a checkout root: %+v", section)
		}
	}
}
