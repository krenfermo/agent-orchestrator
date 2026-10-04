package providersetup_test

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/apierr"
	"github.com/aoagents/agent-orchestrator/backend/internal/runtimehome"
	authsvc "github.com/aoagents/agent-orchestrator/backend/internal/service/authsvc"
	"github.com/aoagents/agent-orchestrator/backend/internal/service/providerprofile"
	"github.com/aoagents/agent-orchestrator/backend/internal/service/providersetup"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/sqlitetest"
)

type fixedDataDir string

func (d fixedDataDir) DataDir() string { return string(d) }

type fakeProber struct {
	state domain.ProviderAuthState
	err   error
}

func (f fakeProber) Probe(ctx context.Context, harness domain.AgentHarness, env runtimehome.Environment) (domain.ProviderAuthState, error) {
	return f.state, f.err
}

// openCall records one OpenProviderSetupTerminal invocation, capturing
// exactly what would have been exec'd -- the thing that must always trace
// back to the profile owner's own isolated runtime-home, never anyone else's.
type openCall struct {
	workingDir string
	argv       []string
	env        map[string]string
	title      string
}

type fakeTerminals struct {
	opens       []openCall
	closed      []string
	nextHandle  int
	closeErr    error
	closeErrFor string
}

func (f *fakeTerminals) OpenProviderSetupTerminal(ctx context.Context, workingDir string, argv []string, env map[string]string, title string) (providersetup.ShellTerminal, error) {
	f.nextHandle++
	f.opens = append(f.opens, openCall{workingDir: workingDir, argv: argv, env: env, title: title})
	handleID := "handle-" + time.Now().Format("150405.000000") + "-" + string(rune('a'+f.nextHandle))
	return providersetup.ShellTerminal{HandleID: handleID}, nil
}

func (f *fakeTerminals) CloseShellTerminal(ctx context.Context, handleID string) error {
	f.closed = append(f.closed, handleID)
	if f.closeErrFor != "" && f.closeErrFor == handleID {
		return f.closeErr
	}
	return nil
}

type fakeLauncher struct {
	argv         []string
	instructions string
	err          error
}

func (f fakeLauncher) Launch(ctx context.Context, harness domain.AgentHarness) ([]string, string, error) {
	if f.err != nil {
		return nil, "", f.err
	}
	return f.argv, f.instructions, nil
}

// harness wires a real providerprofile.Service (the actual ownership/lookup
// logic Start/Stop depend on) with fake Terminals/Launcher, and hands back
// two distinct users so cross-user tests need no extra setup.
type harness struct {
	setup     *providersetup.Service
	terminals *fakeTerminals
	profiles  *providerprofile.Service
	dataDir   string
	userA     domain.UserID
	userB     domain.UserID
}

func newHarness(t *testing.T, probeState domain.ProviderAuthState) *harness {
	t.Helper()
	store := sqlitetest.MustOpen(t)
	authMgr := authsvc.New(store, func() time.Time { return time.Now().UTC() })
	a, err := authMgr.CreateUser(context.Background(), authsvc.CreateUserInput{
		Email: "alice@example.com", Username: "alice", Password: "correct-horse-a",
	})
	if err != nil {
		t.Fatalf("create user a: %v", err)
	}
	b, err := authMgr.CreateUser(context.Background(), authsvc.CreateUserInput{
		Email: "bob@example.com", Username: "bob", Password: "correct-horse-b",
	})
	if err != nil {
		t.Fatalf("create user b: %v", err)
	}
	dataDir := t.TempDir()
	profiles := &providerprofile.Service{
		Store:   store,
		Prober:  fakeProber{state: probeState},
		DataDir: fixedDataDir(dataDir),
		Clock:   func() time.Time { return time.Now().UTC() },
	}
	terminals := &fakeTerminals{}
	setup := &providersetup.Service{
		Profiles:  profiles,
		Terminals: terminals,
		Launcher:  fakeLauncher{argv: []string{"claude"}, instructions: "Run /login."},
		Prober:    fakeProber{state: probeState},
		DataDir:   fixedDataDir(dataDir),
	}
	return &harness{setup: setup, terminals: terminals, profiles: profiles, dataDir: dataDir, userA: a.ID, userB: b.ID}
}

func (h *harness) createClaudeProfile(t *testing.T, owner domain.UserID) domain.ProviderProfile {
	t.Helper()
	p, err := h.profiles.Create(context.Background(), owner, providerprofile.CreateInput{
		Provider: "anthropic",
		Harness:  domain.HarnessClaudeCode,
	})
	if err != nil {
		t.Fatalf("create profile: %v", err)
	}
	return p
}

// TestStart_LaunchesInsideOwnersIsolatedRuntimeHome covers the checkpoint's
// core isolation requirement: the working dir and env handed to the terminal
// must be exactly what runtimehome.Prepare computes for THAT profile's
// owner -- never a different user's, never the daemon host's real $HOME.
func TestStart_LaunchesInsideOwnersIsolatedRuntimeHome(t *testing.T) {
	h := newHarness(t, domain.ProviderAuthStateUnauthenticated)
	profile := h.createClaudeProfile(t, h.userA)

	sess, err := h.setup.Start(context.Background(), h.userA, profile.ID)
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	if sess.HandleID == "" {
		t.Fatal("expected a non-empty handle id")
	}
	if sess.Instructions == "" {
		t.Fatal("expected non-empty instructions")
	}
	if len(h.terminals.opens) != 1 {
		t.Fatalf("expected exactly one terminal opened, got %d", len(h.terminals.opens))
	}

	wantEnv, err := runtimehome.Prepare(h.dataDir, h.userA)
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	got := h.terminals.opens[0]
	if got.workingDir != wantEnv.RuntimeHome {
		t.Fatalf("workingDir = %q, want owner's runtime-home %q", got.workingDir, wantEnv.RuntimeHome)
	}
	if !reflect.DeepEqual(got.env, wantEnv.SubprocessEnv()) {
		t.Fatalf("env = %#v, want %#v", got.env, wantEnv.SubprocessEnv())
	}
	if got.env["HOME"] == "" || got.env["CLAUDE_CONFIG_DIR"] == "" {
		t.Fatal("expected HOME and CLAUDE_CONFIG_DIR to be set")
	}
}

// TestStart_CLINotInstalled covers Checkpoint 8P-E.8.4 Phase 5: a missing CLI
// must never open a dead-end terminal.
func TestStart_CLINotInstalled(t *testing.T) {
	h := newHarness(t, domain.ProviderAuthStateNotInstalled)
	profile := h.createClaudeProfile(t, h.userA)

	_, err := h.setup.Start(context.Background(), h.userA, profile.ID)
	if err == nil {
		t.Fatal("expected an error")
	}
	var apiErr *apierr.Error
	if !errors.As(err, &apiErr) {
		t.Fatalf("expected *apierr.Error, got %T: %v", err, err)
	}
	if apiErr.Code != "PROVIDER_CLI_NOT_INSTALLED" {
		t.Fatalf("code = %q, want PROVIDER_CLI_NOT_INSTALLED", apiErr.Code)
	}
	if len(h.terminals.opens) != 0 {
		t.Fatal("expected no terminal to be opened for a missing CLI")
	}
}

// TestStart_UnsupportedLauncher covers Phase 7's abstraction contract: a
// harness the Launcher doesn't know how to drive must fail cleanly, not
// silently open a terminal running nothing useful.
func TestStart_UnsupportedLauncher(t *testing.T) {
	h := newHarness(t, domain.ProviderAuthStateUnauthenticated)
	profile := h.createClaudeProfile(t, h.userA)
	h.setup.Launcher = fakeLauncher{err: providersetup.ErrProviderNotSupported}

	_, err := h.setup.Start(context.Background(), h.userA, profile.ID)
	var apiErr *apierr.Error
	if !errors.As(err, &apiErr) {
		t.Fatalf("expected *apierr.Error, got %T: %v", err, err)
	}
	if apiErr.Code != "PROVIDER_SETUP_UNSUPPORTED" {
		t.Fatalf("code = %q, want PROVIDER_SETUP_UNSUPPORTED", apiErr.Code)
	}
	if len(h.terminals.opens) != 0 {
		t.Fatal("expected no terminal to be opened")
	}
}

// TestStart_ReplacesRatherThanLeaksExistingSession covers the no-PTY-leak
// requirement: a second Start for the same profile must close the first
// terminal, not accumulate an orphaned one.
func TestStart_ReplacesRatherThanLeaksExistingSession(t *testing.T) {
	h := newHarness(t, domain.ProviderAuthStateUnauthenticated)
	profile := h.createClaudeProfile(t, h.userA)

	first, err := h.setup.Start(context.Background(), h.userA, profile.ID)
	if err != nil {
		t.Fatalf("first start: %v", err)
	}
	second, err := h.setup.Start(context.Background(), h.userA, profile.ID)
	if err != nil {
		t.Fatalf("second start: %v", err)
	}
	if first.HandleID == second.HandleID {
		t.Fatal("expected a fresh handle on the second start")
	}
	if len(h.terminals.closed) != 1 || h.terminals.closed[0] != first.HandleID {
		t.Fatalf("expected the first handle to be closed, closed=%v", h.terminals.closed)
	}
}

// TestStop_ClosesTrackedTerminal covers explicit Cancel and the frontend's
// auto-close-on-success path, which both just call Stop.
func TestStop_ClosesTrackedTerminal(t *testing.T) {
	h := newHarness(t, domain.ProviderAuthStateUnauthenticated)
	profile := h.createClaudeProfile(t, h.userA)

	sess, err := h.setup.Start(context.Background(), h.userA, profile.ID)
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	if err := h.setup.Stop(context.Background(), h.userA, profile.ID); err != nil {
		t.Fatalf("stop: %v", err)
	}
	if len(h.terminals.closed) != 1 || h.terminals.closed[0] != sess.HandleID {
		t.Fatalf("expected %q closed, closed=%v", sess.HandleID, h.terminals.closed)
	}
}

// TestStop_NoLiveSessionIsANoOp covers restart persistence's flip side: a
// fresh Service (as a daemon restart would construct) has no in-memory
// session for any profile, and Stop against it must not error or panic --
// it is not evidence of a bug, just nothing to do.
func TestStop_NoLiveSessionIsANoOp(t *testing.T) {
	h := newHarness(t, domain.ProviderAuthStateUnauthenticated)
	profile := h.createClaudeProfile(t, h.userA)

	fresh := &providersetup.Service{
		Profiles:  h.profiles,
		Terminals: h.terminals,
		Launcher:  fakeLauncher{argv: []string{"claude"}, instructions: "Run /login."},
		Prober:    fakeProber{state: domain.ProviderAuthStateUnauthenticated},
		DataDir:   fixedDataDir(h.dataDir),
	}
	if err := fresh.Stop(context.Background(), h.userA, profile.ID); err != nil {
		t.Fatalf("stop on fresh service: %v", err)
	}
	if len(h.terminals.closed) != 0 {
		t.Fatalf("expected nothing closed, closed=%v", h.terminals.closed)
	}
}

// TestStart_CrossUserIsolation and TestStop_CrossUserIsolation cover
// Checkpoint 8P-E.8.4 Phase 8/9: a profile belongs only to its owner, and
// naming another user's profile id must be indistinguishable from naming one
// that doesn't exist (404, never 403 -- see providerprofile.Service.Get's own
// doc comment for why).
func TestStart_CrossUserIsolation(t *testing.T) {
	h := newHarness(t, domain.ProviderAuthStateUnauthenticated)
	profile := h.createClaudeProfile(t, h.userA)

	_, err := h.setup.Start(context.Background(), h.userB, profile.ID)
	var apiErr *apierr.Error
	if !errors.As(err, &apiErr) || apiErr.Kind != apierr.KindNotFound {
		t.Fatalf("expected KindNotFound, got %v", err)
	}
	if len(h.terminals.opens) != 0 {
		t.Fatal("expected no terminal opened for another user's profile")
	}
}

func TestStop_CrossUserIsolation(t *testing.T) {
	h := newHarness(t, domain.ProviderAuthStateUnauthenticated)
	profile := h.createClaudeProfile(t, h.userA)
	if _, err := h.setup.Start(context.Background(), h.userA, profile.ID); err != nil {
		t.Fatalf("start: %v", err)
	}
	h.terminals.closed = nil

	err := h.setup.Stop(context.Background(), h.userB, profile.ID)
	var apiErr *apierr.Error
	if !errors.As(err, &apiErr) || apiErr.Kind != apierr.KindNotFound {
		t.Fatalf("expected KindNotFound, got %v", err)
	}
	if len(h.terminals.closed) != 0 {
		t.Fatal("expected user A's live terminal to remain untouched by user B's Stop")
	}
}
