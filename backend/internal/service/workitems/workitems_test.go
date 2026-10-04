package workitems_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	"github.com/aoagents/agent-orchestrator/backend/internal/service/workitems"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/sqlitetest"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/store"
)

// workitems_test.go — the service's behaviour against a real database and a
// fake provider (P4-E §17).
//
// The provider is faked rather than served over HTTP because what is under test
// here is the LIFECYCLE — the outbox, the retry policy, the dedupe, the
// isolation — and driving those through a socket would test the adapter again
// while making the interesting failures harder to induce. The adapter's own
// contract is tested against a real HTTP surface in its package.

const (
	projectA = domain.ProjectID("proj-a")
	projectB = domain.ProjectID("proj-b")
)

// fakeProvider is a ports.WorkItems whose every method is scriptable.
type fakeProvider struct {
	mu sync.Mutex

	item domain.WorkItem

	preflightErr  error
	resolveErr    error
	createErr     error
	transitionErr error
	commentErr    error

	// failFirstN makes the next N transition/comment calls fail with failWith,
	// which is how a transient outage is induced.
	failFirstN int
	failWith   error

	transitions []domain.WorkItemStateGroup
	comments    []string
	dedupeKeys  []string
	creates     []ports.WorkItemCreateRequest
}

func (f *fakeProvider) Preflight(context.Context) (ports.WorkItemsIdentity, error) {
	if f.preflightErr != nil {
		return ports.WorkItemsIdentity{}, f.preflightErr
	}
	return ports.WorkItemsIdentity{
		Provider: domain.WorkItemProviderPlane, Workspace: "acme", Projects: 2,
	}, nil
}

func (f *fakeProvider) ListProjects(context.Context) ([]domain.WorkItemProject, error) {
	return []domain.WorkItemProject{{ID: "plane-proj", Name: "Acme Web", Identifier: "ACME"}}, nil
}

func (f *fakeProvider) ListStates(context.Context, string) ([]domain.WorkItemState, error) {
	return []domain.WorkItemState{{ID: "st", Name: "In Progress", Group: domain.WorkItemStateStarted}}, nil
}

func (f *fakeProvider) Get(context.Context, domain.WorkItemRef) (domain.WorkItem, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.resolveErr != nil {
		return domain.WorkItem{}, f.resolveErr
	}
	return f.item, nil
}

func (f *fakeProvider) Resolve(context.Context, string) (domain.WorkItem, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.resolveErr != nil {
		return domain.WorkItem{}, f.resolveErr
	}
	return f.item, nil
}

func (f *fakeProvider) FindByExternalID(context.Context, string, string) (domain.WorkItem, bool, error) {
	return f.item, true, nil
}

func (f *fakeProvider) Create(_ context.Context, req ports.WorkItemCreateRequest) (domain.WorkItem, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.creates = append(f.creates, req)
	if f.createErr != nil {
		return domain.WorkItem{}, f.createErr
	}
	return f.item, nil
}

func (f *fakeProvider) Transition(_ context.Context, _ domain.WorkItemRef, group domain.WorkItemStateGroup) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failFirstN > 0 {
		f.failFirstN--
		return f.failWith
	}
	if f.transitionErr != nil {
		return f.transitionErr
	}
	f.transitions = append(f.transitions, group)
	return nil
}

func (f *fakeProvider) Comment(_ context.Context, _ domain.WorkItemRef, body, dedupeKey string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failFirstN > 0 {
		f.failFirstN--
		return f.failWith
	}
	if f.commentErr != nil {
		return f.commentErr
	}
	f.comments = append(f.comments, body)
	f.dedupeKeys = append(f.dedupeKeys, dedupeKey)
	return nil
}

func (f *fakeProvider) snapshot() (transitions []domain.WorkItemStateGroup, comments, keys []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]domain.WorkItemStateGroup(nil), f.transitions...),
		append([]string(nil), f.comments...),
		append([]string(nil), f.dedupeKeys...)
}

// fakeBox is a reversible "encryption" so a test can assert that the store
// holds ciphertext without needing a key file.
type fakeBox struct{}

func (fakeBox) Seal(p string) (string, error) { return "sealed:" + p, nil }
func (fakeBox) Open(c string) (string, error) {
	if !strings.HasPrefix(c, "sealed:") {
		return "", errors.New("not sealed by this box")
	}
	return strings.TrimPrefix(c, "sealed:"), nil
}

type fixture struct {
	store    *sqlite.Store
	svc      *workitems.Service
	provider *fakeProvider
	env      map[string]string
	ctx      context.Context
	now      time.Time
	ids      int
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	st := sqlitetest.MustOpen(t)
	ctx := t.Context()
	for _, id := range []domain.ProjectID{projectA, projectB} {
		if err := st.UpsertProject(ctx, domain.ProjectRecord{
			ID: string(id), Path: t.TempDir(), RegisteredAt: time.Now().UTC(),
		}); err != nil {
			t.Fatal(err)
		}
	}
	f := &fixture{
		store: st, ctx: ctx, env: map[string]string{},
		now:      time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC),
		provider: &fakeProvider{item: sampleItem()},
	}
	f.svc = workitems.New(workitems.Deps{
		Store:    st,
		Secrets:  fakeBox{},
		Provider: func(workitems.ResolvedConfig) (ports.WorkItems, error) { return f.provider, nil },
		Env:      func(k string) string { return f.env[k] },
		Now:      func() time.Time { return f.now },
		NewID:    f.nextID,
	})
	return f
}

func (f *fixture) nextID() string {
	f.ids++
	return "id-" + string(rune('a'+f.ids%26)) + time.Duration(f.ids).String()
}

func sampleItem() domain.WorkItem {
	return domain.WorkItem{
		Ref: domain.WorkItemRef{
			Provider: domain.WorkItemProviderPlane, Workspace: "acme",
			Project: "plane-proj", ID: "item-1", Key: "ACME-7",
		},
		Title: "Fix the login redirect", StateGroup: domain.WorkItemStateUnstarted,
		State: domain.IssueOpen, URL: "https://app.plane.so/acme/projects/plane-proj/issues/item-1",
	}
}

// connect switches a project on with a stored credential.
func (f *fixture) connect(t *testing.T, id domain.ProjectID) {
	t.Helper()
	ws, proj, token, on := "acme", "plane-proj", "plane-token", true
	if _, err := f.svc.PutConfig(f.ctx, id, workitems.ConfigUpdate{
		Workspace: &ws, ExternalProjectID: &proj, APIToken: &token, Enabled: &on,
	}); err != nil {
		t.Fatalf("connect %s: %v", id, err)
	}
}

func (f *fixture) link(t *testing.T, id domain.ProjectID, scope domain.WorkItemLinkScope, scopeID string) workitems.LinkView {
	t.Helper()
	view, err := f.svc.Link(f.ctx, workitems.LinkRequest{
		ProjectID: id, Scope: scope, ScopeID: scopeID,
		Reference: "ACME-7", SyncEnabled: true, Actor: "ana@example.test",
	})
	if err != nil {
		t.Fatalf("link: %v", err)
	}
	return view
}

// --- configuration ---------------------------------------------------------

// Section 3: AO starts normally with no configuration, and every read answers
// honestly rather than failing.
func TestAnUnconfiguredProjectIsInertAndSaysSo(t *testing.T) {
	f := newFixture(t)

	view, err := f.svc.Config(f.ctx, projectA)
	if err != nil {
		t.Fatalf("Config on an unconfigured project: %v", err)
	}
	if view.Enabled || view.TokenConfigured {
		t.Errorf("an unconfigured project reported enabled=%v token=%v", view.Enabled, view.TokenConfigured)
	}

	health, err := f.svc.Health(f.ctx, projectA)
	if err != nil {
		t.Fatalf("Health: %v", err)
	}
	if health.Configured || health.Enabled || health.Degraded {
		t.Errorf("an unconfigured project is not degraded, it is absent: %+v", health)
	}

	// Enqueue is the one thing the execution path calls, and it must be a
	// no-op rather than an error.
	if err := f.svc.Enqueue(f.ctx, workitems.EnqueueRequest{
		ProjectID: projectA, Scope: domain.WorkItemScopeRun, ScopeID: "run-1",
		Event: domain.WorkItemSyncStarted,
	}); err != nil {
		t.Errorf("Enqueue on an unconfigured project returned %v; it must be a silent no-op", err)
	}
	if _, err := f.svc.TestConnection(f.ctx, projectA); err == nil {
		t.Error("testing a connection that does not exist should say so")
	}
}

// The credential is stored as ciphertext and never comes back out through the
// API.
func TestTheTokenIsSealedAndNeverReturned(t *testing.T) {
	f := newFixture(t)
	f.connect(t, projectA)

	stored, found, err := f.store.GetWorkItemConfig(f.ctx, projectA)
	if err != nil || !found {
		t.Fatalf("GetWorkItemConfig: %v found=%v", err, found)
	}
	if stored.APITokenEncrypted == "plane-token" {
		t.Fatal("the token was stored in plaintext")
	}
	if !strings.HasPrefix(stored.APITokenEncrypted, "sealed:") {
		t.Fatalf("the token was not sealed: %q", stored.APITokenEncrypted)
	}

	view, err := f.svc.Config(f.ctx, projectA)
	if err != nil {
		t.Fatal(err)
	}
	if !view.TokenConfigured {
		t.Error("a stored credential should be reported as configured")
	}
	// The view type has no token field at all, so the strongest assertion
	// available is that nothing in its rendering carries the secret.
	if strings.Contains(view.Workspace+view.BaseURL+view.ExternalProjectID+view.LastCheckError, "plane-token") {
		t.Error("the token leaked into a rendered field")
	}
}

// Omitting the token on a re-save keeps it. A form that cleared the credential
// every time somebody toggled a checkbox would be unusable.
func TestOmittingTheTokenKeepsIt(t *testing.T) {
	f := newFixture(t)
	f.connect(t, projectA)

	off := false
	if _, err := f.svc.PutConfig(f.ctx, projectA, workitems.ConfigUpdate{SyncComments: &off}); err != nil {
		t.Fatalf("PutConfig: %v", err)
	}
	stored, _, _ := f.store.GetWorkItemConfig(f.ctx, projectA)
	if stored.APITokenEncrypted == "" {
		t.Fatal("re-saving without the token field erased the stored credential")
	}
	if stored.SyncComments {
		t.Error("the field that WAS sent did not take effect")
	}

	// An explicit empty string clears it — that is the difference the pointer
	// exists to express. But not while the integration is switched ON: an
	// enabled connection with no credential is a promise AO cannot keep, and
	// the completeness rule refuses it rather than leaving a project quietly
	// failing every sync.
	empty := ""
	if _, err := f.svc.PutConfig(f.ctx, projectA, workitems.ConfigUpdate{APIToken: &empty}); err == nil {
		t.Error("the credential was cleared while the integration was still enabled")
	}

	disabled := false
	if _, err := f.svc.PutConfig(f.ctx, projectA, workitems.ConfigUpdate{
		Enabled: &disabled, APIToken: &empty,
	}); err != nil {
		t.Fatalf("clearing the token on a disabled connection: %v", err)
	}
	stored, _, _ = f.store.GetWorkItemConfig(f.ctx, projectA)
	if stored.APITokenEncrypted != "" {
		t.Error("an explicit empty token did not clear the credential")
	}
}

// Section 3: a partial configuration is savable; only enabling requires
// completeness.
func TestAPartialConfigurationSavesButCannotBeEnabled(t *testing.T) {
	f := newFixture(t)

	ws := "acme"
	if _, err := f.svc.PutConfig(f.ctx, projectA, workitems.ConfigUpdate{Workspace: &ws}); err != nil {
		t.Fatalf("saving a half-filled form failed: %v", err)
	}

	on := true
	_, err := f.svc.PutConfig(f.ctx, projectA, workitems.ConfigUpdate{Enabled: &on})
	if err == nil {
		t.Fatal("an incomplete configuration was switched on")
	}
	if !strings.Contains(err.Error(), "workspace") && !strings.Contains(err.Error(), "token") {
		t.Errorf("the refusal does not say what is missing: %v", err)
	}
}

// Section 3: environment variables supply defaults, and a stored value wins.
func TestEnvironmentSuppliesDefaultsAndStoredValuesWin(t *testing.T) {
	f := newFixture(t)
	f.env[workitems.EnvWorkspace] = "env-workspace"
	f.env[workitems.EnvProject] = "env-project"
	f.env[workitems.EnvAPIToken] = "env-token"

	cfg, err := f.svc.Resolve(f.ctx, projectA)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if cfg.Workspace != "env-workspace" || cfg.APIToken != "env-token" {
		t.Errorf("environment defaults were not applied: %+v", cfg.Workspace)
	}
	// But the integration is still OFF: "AO writes to your planning board" is
	// not a decision an environment variable makes on somebody's behalf.
	if cfg.Enabled {
		t.Error("an environment-only configuration enabled itself")
	}

	ws := "stored-workspace"
	if _, err := f.svc.PutConfig(f.ctx, projectA, workitems.ConfigUpdate{Workspace: &ws}); err != nil {
		t.Fatal(err)
	}
	cfg, _ = f.svc.Resolve(f.ctx, projectA)
	if cfg.Workspace != "stored-workspace" {
		t.Errorf("workspace = %q; a stored value must win over the environment", cfg.Workspace)
	}
	if cfg.APIToken != "env-token" {
		t.Errorf("the environment token should still fill in where nothing is stored")
	}
}

// A stored token that will not decrypt is a broken configuration, not a reason
// to quietly use a different credential.
func TestAnUndecryptableTokenIsReportedNotSubstituted(t *testing.T) {
	f := newFixture(t)
	f.env[workitems.EnvAPIToken] = "env-token"
	if err := f.store.PutWorkItemConfig(f.ctx, store.WorkItemConfig{
		ProjectID: projectA, Provider: domain.WorkItemProviderPlane,
		Workspace: "acme", ExternalProjectID: "plane-proj",
		APITokenEncrypted: "not-actually-sealed", Enabled: true,
	}, f.now); err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.Resolve(f.ctx, projectA); err == nil {
		t.Fatal("an undecryptable stored token silently fell back to the environment")
	}
}

// --- linking ---------------------------------------------------------------

// Section 5: a link stores identifiers, and re-linking replaces rather than
// accumulating.
func TestLinkingStoresIdentifiersAndReplaces(t *testing.T) {
	f := newFixture(t)
	f.connect(t, projectA)

	view := f.link(t, projectA, domain.WorkItemScopeRun, "run-1")
	if view.Ref.ID != "item-1" || view.Ref.Workspace != "acme" || view.Ref.Project != "plane-proj" {
		t.Errorf("the link did not store the full identifier triple: %+v", view.Ref)
	}
	if view.CreatedBy != "ana@example.test" {
		t.Errorf("createdBy = %q, want the actor", view.CreatedBy)
	}

	links, err := f.store.ListWorkItemLinks(f.ctx, projectA)
	if err != nil || len(links) != 1 {
		t.Fatalf("links = %d (%v), want 1", len(links), err)
	}
	firstCreated := links[0].CreatedAt

	// Re-linking the same run to a different item replaces the row.
	f.provider.item.Ref.ID = "item-2"
	f.now = f.now.Add(time.Hour)
	f.link(t, projectA, domain.WorkItemScopeRun, "run-1")
	links, _ = f.store.ListWorkItemLinks(f.ctx, projectA)
	if len(links) != 1 {
		t.Fatalf("re-linking produced %d links; one AO thing links to one item", len(links))
	}
	if links[0].Ref.ID != "item-2" {
		t.Errorf("the link still points at %q", links[0].Ref.ID)
	}
	if !links[0].CreatedAt.Equal(firstCreated) {
		t.Error(`"linked since" should survive a re-link`)
	}
}

// Section 6: AO creates an item only when explicitly asked, and stamps its own
// external id so the create is idempotent.
func TestCreatingAnItemIsExplicitAndCarriesAnExternalID(t *testing.T) {
	f := newFixture(t)
	f.connect(t, projectA)

	_, err := f.svc.Link(f.ctx, workitems.LinkRequest{
		ProjectID: projectA, Scope: domain.WorkItemScopeTask, ScopeID: "task-9",
		Create: true, Title: "Fix the login redirect", SyncEnabled: true,
	})
	if err != nil {
		t.Fatalf("Link with create: %v", err)
	}
	if len(f.provider.creates) != 1 {
		t.Fatalf("made %d creates, want 1", len(f.provider.creates))
	}
	if got := f.provider.creates[0].ExternalID; got != "task:task-9" {
		t.Errorf("external id = %q, want the derived scope key", got)
	}

	// And a link without create makes none.
	f.link(t, projectA, domain.WorkItemScopeRun, "run-1")
	if len(f.provider.creates) != 1 {
		t.Errorf("linking an existing item created one anyway")
	}
}

// An item in a different provider project than the one this AO project maps to
// is refused: AO would otherwise be writing into a project nobody mapped.
func TestLinkingRefusesAnItemOutsideTheMappedProject(t *testing.T) {
	f := newFixture(t)
	f.connect(t, projectA)
	f.provider.item.Ref.Project = "some-other-plane-project"

	_, err := f.svc.Link(f.ctx, workitems.LinkRequest{
		ProjectID: projectA, Scope: domain.WorkItemScopeRun, ScopeID: "run-1", Reference: "OTHER-1",
	})
	if err == nil {
		t.Fatal("linked an item from an unmapped project")
	}
}

// Unlinking forgets the association and leaves the external item alone.
func TestUnlinkingDoesNotTouchTheExternalItem(t *testing.T) {
	f := newFixture(t)
	f.connect(t, projectA)
	view := f.link(t, projectA, domain.WorkItemScopeRun, "run-1")

	if err := f.svc.Unlink(f.ctx, projectA, view.ID); err != nil {
		t.Fatalf("Unlink: %v", err)
	}
	links, _ := f.store.ListWorkItemLinks(f.ctx, projectA)
	if len(links) != 0 {
		t.Errorf("links = %d after unlink", len(links))
	}
	transitions, comments, _ := f.provider.snapshot()
	if len(transitions) != 0 || len(comments) != 0 {
		t.Error("unlinking wrote to the provider")
	}
}

// --- sync ------------------------------------------------------------------

// Section 7/8: an AO state change becomes a state move and a comment, in that
// order, with the mapping applied explicitly.
func TestARunStateChangeSyncsStateAndComment(t *testing.T) {
	f := newFixture(t)
	f.connect(t, projectA)
	f.link(t, projectA, domain.WorkItemScopeRun, "run-1")

	if err := f.svc.EnqueueRunState(f.ctx, projectA, "run-1", domain.WorkflowRunRunning, "starting the plan"); err != nil {
		t.Fatalf("EnqueueRunState: %v", err)
	}
	out, err := f.svc.SyncOnce(f.ctx, 10)
	if err != nil {
		t.Fatalf("SyncOnce: %v", err)
	}
	if out.Delivered != 1 {
		t.Fatalf("delivered %d of %d claimed: %+v", out.Delivered, out.Claimed, out)
	}
	transitions, comments, keys := f.provider.snapshot()
	if len(transitions) != 1 || transitions[0] != domain.WorkItemStateStarted {
		t.Errorf("transitions = %v, want [started]", transitions)
	}
	if len(comments) != 1 || !strings.Contains(comments[0], "starting the plan") {
		t.Errorf("comments = %v, want one carrying the detail", comments)
	}
	if len(keys) != 1 || keys[0] != "run:run-1:started" {
		t.Errorf("dedupe keys = %v", keys)
	}
}

// Section 7: needs_attention moves NO state — there is no external group that
// means "a human must decide", and inventing one would be a lie about progress
// or about intent. It still comments, which is the honest way to say it.
func TestNeedsAttentionCommentsWithoutMovingState(t *testing.T) {
	f := newFixture(t)
	f.connect(t, projectA)
	f.link(t, projectA, domain.WorkItemScopeRun, "run-1")

	if err := f.svc.EnqueueRunState(f.ctx, projectA, "run-1",
		domain.WorkflowRunNeedsAttention, "the fix budget is exhausted"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.SyncOnce(f.ctx, 10); err != nil {
		t.Fatal(err)
	}
	transitions, comments, _ := f.provider.snapshot()
	if len(transitions) != 0 {
		t.Errorf("needs_attention moved the state to %v", transitions)
	}
	if len(comments) != 1 || !strings.Contains(comments[0], "needs a decision") {
		t.Errorf("comments = %v", comments)
	}
}

// A failed run does not cancel the planned work: the work is still wanted, and
// deleting it from somebody's active plan is authority AO does not have.
func TestAFailedRunDoesNotCancelTheExternalItem(t *testing.T) {
	f := newFixture(t)
	f.connect(t, projectA)
	f.link(t, projectA, domain.WorkItemScopeRun, "run-1")

	if err := f.svc.EnqueueRunState(f.ctx, projectA, "run-1", domain.WorkflowRunFailed, "tests did not pass"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.SyncOnce(f.ctx, 10); err != nil {
		t.Fatal(err)
	}
	transitions, comments, _ := f.provider.snapshot()
	if len(transitions) != 0 {
		t.Errorf("a failed AO run moved the item to %v", transitions)
	}
	if len(comments) != 1 {
		t.Errorf("a failed run should still explain itself: %v", comments)
	}
}

// Queued and waiting are not news. Announcing "in progress" while nothing is
// happening is how a board stops being trusted.
func TestNonEventStatesEnqueueNothing(t *testing.T) {
	f := newFixture(t)
	f.connect(t, projectA)
	f.link(t, projectA, domain.WorkItemScopeRun, "run-1")

	for _, state := range []domain.WorkflowRunState{domain.WorkflowRunPending, domain.WorkflowRunWaiting} {
		if err := f.svc.EnqueueRunState(f.ctx, projectA, "run-1", state, ""); err != nil {
			t.Fatal(err)
		}
	}
	rows, err := f.store.ListWorkItemSyncs(f.ctx, projectA, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 0 {
		t.Errorf("queued %d rows for states that are not news", len(rows))
	}
}

// Section 8/9: the same real-world moment enqueues once, however many times the
// lifecycle observes it.
func TestEnqueueIsIdempotentPerRealWorldMoment(t *testing.T) {
	f := newFixture(t)
	f.connect(t, projectA)
	f.link(t, projectA, domain.WorkItemScopeRun, "run-1")

	for range 5 {
		if err := f.svc.EnqueueRunState(f.ctx, projectA, "run-1", domain.WorkflowRunCompleted, "done"); err != nil {
			t.Fatal(err)
		}
	}
	rows, _ := f.store.ListWorkItemSyncs(f.ctx, projectA, 10)
	if len(rows) != 1 {
		t.Fatalf("five observations produced %d rows, want 1", len(rows))
	}

	if _, err := f.svc.SyncOnce(f.ctx, 10); err != nil {
		t.Fatal(err)
	}
	transitions, comments, _ := f.provider.snapshot()
	if len(transitions) != 1 || len(comments) != 1 {
		t.Errorf("delivered %d transitions and %d comments, want one of each", len(transitions), len(comments))
	}

	// A second drain delivers nothing: the row is settled.
	out, _ := f.svc.SyncOnce(f.ctx, 10)
	if out.Claimed != 0 {
		t.Errorf("a settled row was claimed again: %+v", out)
	}
}

// Section 13: a provider outage defers, and AO carries on. Nothing about the
// run changes, and the row is still there when the provider comes back.
func TestATransientOutageDefersAndLaterSucceeds(t *testing.T) {
	f := newFixture(t)
	f.connect(t, projectA)
	f.link(t, projectA, domain.WorkItemScopeRun, "run-1")

	f.provider.failFirstN = 1
	f.provider.failWith = &ports.WorkItemsError{
		Op: "transition", Kind: ports.WorkItemsErrUnavailable, Message: "Plane could not be reached",
	}
	if err := f.svc.EnqueueRunState(f.ctx, projectA, "run-1", domain.WorkflowRunRunning, "go"); err != nil {
		t.Fatal(err)
	}

	out, err := f.svc.SyncOnce(f.ctx, 10)
	if err != nil {
		t.Fatalf("a provider outage must not fail the drain: %v", err)
	}
	if out.Deferred != 1 {
		t.Fatalf("outcome = %+v, want one deferred", out)
	}
	rows, _ := f.store.ListWorkItemSyncs(f.ctx, projectA, 10)
	if len(rows) != 1 || rows[0].Status != store.WorkItemSyncPending {
		t.Fatalf("the row did not stay pending: %+v", rows)
	}
	if rows[0].Attempts != 1 || rows[0].LastError == "" {
		t.Errorf("the attempt was not recorded: attempts=%d err=%q", rows[0].Attempts, rows[0].LastError)
	}

	// Nothing is due yet — the backoff is a durable timestamp, not a sleep.
	if out, _ = f.svc.SyncOnce(f.ctx, 10); out.Claimed != 0 {
		t.Errorf("a backed-off row was claimed early: %+v", out)
	}

	// Time passes; the provider recovers.
	f.now = f.now.Add(2 * time.Hour)
	out, err = f.svc.SyncOnce(f.ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if out.Delivered != 1 {
		t.Fatalf("the recovered row was not delivered: %+v", out)
	}
}

// A terminal failure — a rejected credential — is not retried. Retrying a bad
// token is how an account gets locked.
func TestATerminalFailureIsNotRetried(t *testing.T) {
	f := newFixture(t)
	f.connect(t, projectA)
	f.link(t, projectA, domain.WorkItemScopeRun, "run-1")

	f.provider.transitionErr = &ports.WorkItemsError{
		Op: "transition", Kind: ports.WorkItemsErrAuth, Message: "Plane rejected the API token",
	}
	if err := f.svc.EnqueueRunState(f.ctx, projectA, "run-1", domain.WorkflowRunRunning, "go"); err != nil {
		t.Fatal(err)
	}
	out, err := f.svc.SyncOnce(f.ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if out.Failed != 1 {
		t.Fatalf("outcome = %+v, want one permanent failure", out)
	}
	rows, _ := f.store.ListWorkItemSyncs(f.ctx, projectA, 10)
	if rows[0].Status != store.WorkItemSyncFailed {
		t.Errorf("status = %q, want failed", rows[0].Status)
	}
	// The reason survives, so an operator asking "why did this never reach
	// Plane" gets an answer rather than an absence.
	if !strings.Contains(rows[0].LastError, "token") {
		t.Errorf("last error = %q", rows[0].LastError)
	}
}

// Section 9: the queue IS the checkpoint. A daemon that dies mid-delivery comes
// back to a pending row and delivers it.
func TestAPendingRowSurvivesARestart(t *testing.T) {
	f := newFixture(t)
	f.connect(t, projectA)
	f.link(t, projectA, domain.WorkItemScopeRun, "run-1")

	f.provider.failFirstN = 1
	f.provider.failWith = &ports.WorkItemsError{Kind: ports.WorkItemsErrUnavailable, Message: "down"}
	if err := f.svc.EnqueueRunState(f.ctx, projectA, "run-1", domain.WorkflowRunCompleted, "done"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.SyncOnce(f.ctx, 10); err != nil {
		t.Fatal(err)
	}

	// A second service over the SAME database is what a restart looks like.
	restarted := workitems.New(workitems.Deps{
		Store: f.store, Secrets: fakeBox{},
		Provider: func(workitems.ResolvedConfig) (ports.WorkItems, error) { return f.provider, nil },
		Env:      func(k string) string { return f.env[k] },
		Now:      func() time.Time { return f.now.Add(2 * time.Hour) },
		NewID:    f.nextID,
	})
	out, err := restarted.SyncOnce(f.ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if out.Delivered != 1 {
		t.Fatalf("a restart did not resume the pending row: %+v", out)
	}

	// And the link survived too.
	links, _ := f.store.ListWorkItemLinks(f.ctx, projectA)
	if len(links) != 1 || links[0].Ref.ID != "item-1" {
		t.Errorf("the link did not survive the restart: %+v", links)
	}
}

// Unlinking withdraws the queued intent with it. A "completed" comment landing
// on an item somebody just detached is exactly the surprise unlinking is meant
// to prevent, and the outbox row's foreign key onto the link is what makes that
// automatic rather than a sweep somebody has to remember.
func TestUnlinkingWithdrawsQueuedSyncs(t *testing.T) {
	f := newFixture(t)
	f.connect(t, projectA)
	view := f.link(t, projectA, domain.WorkItemScopeRun, "run-1")

	if err := f.svc.EnqueueRunState(f.ctx, projectA, "run-1", domain.WorkflowRunCompleted, "done"); err != nil {
		t.Fatal(err)
	}
	if rows, _ := f.store.ListWorkItemSyncs(f.ctx, projectA, 10); len(rows) != 1 {
		t.Fatalf("the event did not queue: %d rows", len(rows))
	}

	if err := f.svc.Unlink(f.ctx, projectA, view.ID); err != nil {
		t.Fatal(err)
	}
	if rows, _ := f.store.ListWorkItemSyncs(f.ctx, projectA, 10); len(rows) != 0 {
		t.Errorf("unlinking left %d queued syncs behind", len(rows))
	}
	out, err := f.svc.SyncOnce(f.ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if out.Claimed != 0 {
		t.Errorf("outcome = %+v, want nothing to deliver", out)
	}
	transitions, comments, _ := f.provider.snapshot()
	if len(transitions) != 0 || len(comments) != 0 {
		t.Error("an unlinked item was still written to")
	}
}

// Muting a link AFTER an event was queued settles the row as skipped, not
// failed: a person's decision taking effect is not an error, and leaving it
// pending would retry it forever.
func TestMutingALinkSkipsItsQueuedSyncs(t *testing.T) {
	f := newFixture(t)
	f.connect(t, projectA)
	view := f.link(t, projectA, domain.WorkItemScopeRun, "run-1")

	if err := f.svc.EnqueueRunState(f.ctx, projectA, "run-1", domain.WorkflowRunCompleted, "done"); err != nil {
		t.Fatal(err)
	}
	if err := f.svc.SetLinkSync(f.ctx, projectA, view.ID, false); err != nil {
		t.Fatal(err)
	}
	out, err := f.svc.SyncOnce(f.ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if out.Skipped != 1 {
		t.Fatalf("outcome = %+v, want one skipped", out)
	}
	transitions, comments, _ := f.provider.snapshot()
	if len(transitions) != 0 || len(comments) != 0 {
		t.Error("a muted link still wrote to the provider")
	}
	// And it is settled, not left to retry forever.
	if out, _ = f.svc.SyncOnce(f.ctx, 10); out.Claimed != 0 {
		t.Errorf("the skipped row was claimed again: %+v", out)
	}
}

// A link with sync switched off records nothing. "Show me the link" and "let AO
// edit my board" are different consents.
func TestAMutedLinkEnqueuesNothing(t *testing.T) {
	f := newFixture(t)
	f.connect(t, projectA)
	view, err := f.svc.Link(f.ctx, workitems.LinkRequest{
		ProjectID: projectA, Scope: domain.WorkItemScopeRun, ScopeID: "run-1",
		Reference: "ACME-7", SyncEnabled: false,
	})
	if err != nil {
		t.Fatal(err)
	}
	if view.SyncEnabled {
		t.Fatal("the link was created with sync on")
	}
	if err := f.svc.EnqueueRunState(f.ctx, projectA, "run-1", domain.WorkflowRunRunning, "go"); err != nil {
		t.Fatal(err)
	}
	rows, _ := f.store.ListWorkItemSyncs(f.ctx, projectA, 10)
	if len(rows) != 0 {
		t.Errorf("a muted link queued %d rows", len(rows))
	}
}

// Section 8: a comment carries one line, never a transcript.
func TestCommentsAreBoundedToOneLine(t *testing.T) {
	f := newFixture(t)
	f.connect(t, projectA)
	f.link(t, projectA, domain.WorkItemScopeRun, "run-1")

	transcript := "the real reason\n$ go test ./...\nFAIL\n" + strings.Repeat("noise ", 500)
	if err := f.svc.EnqueueRunState(f.ctx, projectA, "run-1", domain.WorkflowRunFailed, transcript); err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.SyncOnce(f.ctx, 10); err != nil {
		t.Fatal(err)
	}
	_, comments, _ := f.provider.snapshot()
	if len(comments) != 1 {
		t.Fatalf("comments = %v", comments)
	}
	if strings.Contains(comments[0], "go test") || strings.Contains(comments[0], "noise") {
		t.Errorf("terminal output reached the provider: %q", comments[0])
	}
	if !strings.Contains(comments[0], "the real reason") {
		t.Errorf("the first line was dropped: %q", comments[0])
	}
}

// --- isolation -------------------------------------------------------------

// Section 10: two projects' configurations and links never mix. Tenancy is the
// project's tenancy, so this is the boundary that carries it.
func TestProjectsAreIsolatedFromEachOther(t *testing.T) {
	f := newFixture(t)
	f.connect(t, projectA)
	f.link(t, projectA, domain.WorkItemScopeRun, "run-1")

	// B is not configured, and reading it tells nothing about A.
	viewB, err := f.svc.Config(f.ctx, projectB)
	if err != nil {
		t.Fatal(err)
	}
	if viewB.TokenConfigured || viewB.Enabled || viewB.Workspace != "" {
		t.Errorf("project B saw project A's connection: %+v", viewB)
	}
	linksB, err := f.store.ListWorkItemLinks(f.ctx, projectB)
	if err != nil {
		t.Fatal(err)
	}
	if len(linksB) != 0 {
		t.Errorf("project B sees %d of project A's links", len(linksB))
	}

	// B cannot unlink A's link even knowing its id — the delete is scoped by
	// project in the predicate, not merely checked in Go.
	linksA, _ := f.store.ListWorkItemLinks(f.ctx, projectA)
	if err := f.svc.Unlink(f.ctx, projectB, linksA[0].ID); err == nil {
		t.Error("project B unlinked project A's link")
	}
	if err := f.svc.SetLinkSync(f.ctx, projectB, linksA[0].ID, false); err == nil {
		t.Error("project B muted project A's link")
	}
	still, _ := f.store.ListWorkItemLinks(f.ctx, projectA)
	if len(still) != 1 || !still[0].SyncEnabled {
		t.Errorf("project A's link was modified from project B: %+v", still)
	}
}

// A guessed project id reaches nothing. At the service layer it resolves to an
// unconfigured project; the HTTP layer answers 404 before this is even reached.
func TestGuessedProjectIDsReachNothing(t *testing.T) {
	f := newFixture(t)
	f.connect(t, projectA)
	f.link(t, projectA, domain.WorkItemScopeRun, "run-1")

	for _, guess := range []domain.ProjectID{"proj-c", "PROJ-A", "proj-a ", "../proj-a", ""} {
		view, err := f.svc.Config(f.ctx, guess)
		if err == nil && (view.TokenConfigured && !view.TokenFromEnv) {
			t.Errorf("guessed id %q saw a stored credential", guess)
		}
		links, err := f.store.ListWorkItemLinks(f.ctx, guess)
		if err == nil && len(links) > 0 {
			t.Errorf("guessed id %q listed %d links", guess, len(links))
		}
		if _, err := f.svc.TestConnection(f.ctx, guess); err == nil {
			t.Errorf("guessed id %q tested a connection", guess)
		}
	}
}

// --- observability ---------------------------------------------------------

// Section 14: operations are recorded, and the credential is not.
func TestOperationsAreAuditedWithoutTheCredential(t *testing.T) {
	f := newFixture(t)
	f.connect(t, projectA)
	f.link(t, projectA, domain.WorkItemScopeRun, "run-1")
	if err := f.svc.EnqueueRunState(f.ctx, projectA, "run-1", domain.WorkflowRunCompleted, "done"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.SyncOnce(f.ctx, 10); err != nil {
		t.Fatal(err)
	}

	rows, err := f.svc.Audit(f.ctx, projectA, 50)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) == 0 {
		t.Fatal("nothing was recorded")
	}
	var sawDelivery bool
	for _, row := range rows {
		if row.Outcome == store.WorkItemAuditOK && row.ExternalItemID == "item-1" {
			sawDelivery = true
		}
		for _, field := range []string{row.Detail, row.Operation, row.ExternalItemID, row.ExternalItemKey} {
			if strings.Contains(field, "plane-token") || strings.Contains(field, "sealed:") {
				t.Errorf("a credential reached the audit trail: %q", field)
			}
		}
	}
	if !sawDelivery {
		t.Error("the successful delivery was not recorded")
	}
}

// Section 13/14: a queue with permanent failures reports degraded even when the
// credential itself is fine. "The token works" and "AO is delivering" are
// different claims.
func TestHealthReportsDegradedOnPermanentFailures(t *testing.T) {
	f := newFixture(t)
	f.connect(t, projectA)
	f.link(t, projectA, domain.WorkItemScopeRun, "run-1")
	if _, err := f.svc.TestConnection(f.ctx, projectA); err != nil {
		t.Fatal(err)
	}

	health, _ := f.svc.Health(f.ctx, projectA)
	if !health.Connected || health.Degraded {
		t.Fatalf("a working connection reported %+v", health)
	}

	f.provider.transitionErr = &ports.WorkItemsError{Kind: ports.WorkItemsErrInvalid, Message: "refused"}
	if err := f.svc.EnqueueRunState(f.ctx, projectA, "run-1", domain.WorkflowRunRunning, "go"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.SyncOnce(f.ctx, 10); err != nil {
		t.Fatal(err)
	}

	health, _ = f.svc.Health(f.ctx, projectA)
	if !health.Degraded || health.Failed != 1 {
		t.Errorf("health = %+v, want degraded with one failure", health)
	}
}

// Section 13: with the provider unreachable, links still render from the cache
// and say they are stale rather than vanishing.
func TestLinksDegradeToTheCacheWhenTheProviderIsDown(t *testing.T) {
	f := newFixture(t)
	f.connect(t, projectA)
	f.link(t, projectA, domain.WorkItemScopeRun, "run-1")

	f.provider.resolveErr = &ports.WorkItemsError{
		Kind: ports.WorkItemsErrUnavailable, Message: "Plane could not be reached",
	}
	views, err := f.svc.Links(f.ctx, projectA, true)
	if err != nil {
		t.Fatalf("listing links must not fail because the provider is down: %v", err)
	}
	if len(views) != 1 {
		t.Fatalf("links = %d", len(views))
	}
	if views[0].Live != nil {
		t.Error("a live item was reported while the provider was down")
	}
	if views[0].LastSeenTitle != "Fix the login redirect" {
		t.Errorf("the cached title was lost: %q", views[0].LastSeenTitle)
	}
	if views[0].LiveError == "" {
		t.Error("the reason the item could not be refreshed was not reported")
	}
}

// Section 7: external state is advisory. It is reported as readiness and never
// becomes an AO state — a property demonstrated here by there being no API that
// could do it, and by the readiness value itself being the only output.
func TestExternalStateIsAdvisoryOnly(t *testing.T) {
	f := newFixture(t)
	f.connect(t, projectA)
	f.provider.item.StateGroup = domain.WorkItemStateBacklog
	view := f.link(t, projectA, domain.WorkItemScopeRun, "run-1")

	if view.Readiness != domain.PlanningDeferred {
		t.Errorf("readiness = %q, want deferred for a backlog item", view.Readiness)
	}
	// Enqueueing still works: a deferred plan does not stop AO reporting what
	// it actually did.
	if err := f.svc.EnqueueRunState(f.ctx, projectA, "run-1", domain.WorkflowRunRunning, "go"); err != nil {
		t.Fatal(err)
	}
	rows, _ := f.store.ListWorkItemSyncs(f.ctx, projectA, 10)
	if len(rows) != 1 {
		t.Error("an external backlog state suppressed AO's own report of what it is doing")
	}
}
