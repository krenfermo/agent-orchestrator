package providersetup

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/apierr"
	"github.com/aoagents/agent-orchestrator/backend/internal/runtimehome"
	"github.com/aoagents/agent-orchestrator/backend/internal/service/providerprofile"
)

// ProfileGetter is the narrow provider-profile lookup this service needs --
// ownership-scoped, so Start/Stop inherit the same 404-not-403 IDOR-safe
// behavior as every other provider-profile call (see
// providerprofile.Service.Get's doc comment).
type ProfileGetter interface {
	Get(ctx context.Context, userID domain.UserID, id domain.ProviderProfileID) (domain.ProviderProfile, error)
}

// Terminals is the narrow shellterm surface this service needs: open a PTY
// with server-decided argv/env, and close it by handle. Backed by
// *shellterm.Service in production.
type Terminals interface {
	OpenProviderSetupTerminal(ctx context.Context, workingDir string, argv []string, env map[string]string, title string) (ShellTerminal, error)
	CloseShellTerminal(ctx context.Context, handleID string) error
}

// ShellTerminal mirrors the subset of shellterm.ShellTerminal this package
// reads, so it depends on shellterm only through the narrow Terminals
// interface above rather than importing the package's full surface.
type ShellTerminal struct {
	HandleID string
}

// DataDirer supplies AO_DATA_DIR so this service can prepare a user's
// runtime-home on demand -- mirrors providerprofile.DataDirer.
type DataDirer interface {
	DataDir() string
}

// Session is one live provider setup terminal.
type Session struct {
	HandleID     string `json:"handleId"`
	Instructions string `json:"instructions"`
}

// Manager is the controller-facing contract for the
// /api/v1/provider-profiles/{id}/setup surface.
type Manager interface {
	Start(ctx context.Context, userID domain.UserID, id domain.ProviderProfileID) (Session, error)
	Stop(ctx context.Context, userID domain.UserID, id domain.ProviderProfileID) error
}

// Service starts and stops provider setup terminals. It is the
// implementation behind Checkpoint 8P-E.8.4's "Connect Claude Code" button:
// Start resolves the profile owner's isolated runtime-home, refuses to open a
// terminal for a CLI that isn't installed, and launches the provider's own
// login flow inside it; Stop tears the terminal down (explicit Cancel, or
// auto-close once the frontend's Test-Connection poll observes
// authenticated).
type Service struct {
	Profiles  ProfileGetter
	Terminals Terminals
	Launcher  Launcher
	Prober    providerprofile.Prober
	DataDir   DataDirer

	mu       sync.Mutex
	sessions map[domain.ProviderProfileID]Session
}

var _ Manager = (*Service)(nil)

// Start resolves userID's profile, prepares its isolated runtime-home,
// refuses to proceed for a not-installed CLI, and opens a setup terminal
// running the harness's own login flow inside it. A profile with an already
// live setup session has that session closed and replaced rather than reused
// -- reusing a cached handle risks handing back one whose PTY already died
// (the user typed `exit`, or closed the pane by hand) with no way to detect
// that here, so replacing is the only way to guarantee the returned handle is
// live.
func (s *Service) Start(ctx context.Context, userID domain.UserID, id domain.ProviderProfileID) (Session, error) {
	profile, err := s.Profiles.Get(ctx, userID, id)
	if err != nil {
		return Session{}, err
	}
	if s.DataDir == nil {
		return Session{}, apierr.Internal("PROVIDER_SETUP_NO_DATA_DIR", "provider setup is unavailable")
	}
	env, err := runtimehome.Prepare(s.DataDir.DataDir(), userID)
	if err != nil {
		return Session{}, fmt.Errorf("providersetup: prepare runtime-home: %w", err)
	}
	if s.Prober != nil {
		if state, probeErr := s.Prober.Probe(ctx, profile.Harness, env); probeErr == nil && state == domain.ProviderAuthStateNotInstalled {
			return Session{}, apierr.Invalid("PROVIDER_CLI_NOT_INSTALLED",
				profile.DisplayName+" is not installed on this AO instance.", nil)
		}
	}
	argv, instructions, err := s.Launcher.Launch(ctx, profile.Harness)
	if err != nil {
		if errors.Is(err, ErrProviderNotSupported) {
			return Session{}, apierr.Invalid("PROVIDER_SETUP_UNSUPPORTED",
				profile.DisplayName+" doesn't support guided setup yet. Run its CLI login manually.", nil)
		}
		return Session{}, fmt.Errorf("providersetup: launch: %w", err)
	}

	s.closeExisting(ctx, id)

	term, err := s.Terminals.OpenProviderSetupTerminal(ctx, env.RuntimeHome, argv, env.SubprocessEnv(), "Connect "+profile.DisplayName)
	if err != nil {
		return Session{}, fmt.Errorf("providersetup: open terminal: %w", err)
	}
	sess := Session{HandleID: term.HandleID, Instructions: instructions}
	s.track(id, sess)
	return sess, nil
}

// Stop closes id's live setup terminal, if any. It is idempotent: calling it
// with no live session (already stopped, or never started) is a no-op, not
// an error -- both explicit Cancel and the frontend's auto-close-on-success
// path call this without knowing which case they're in.
func (s *Service) Stop(ctx context.Context, userID domain.UserID, id domain.ProviderProfileID) error {
	if _, err := s.Profiles.Get(ctx, userID, id); err != nil {
		return err
	}
	s.closeExisting(ctx, id)
	return nil
}

func (s *Service) track(id domain.ProviderProfileID, sess Session) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.sessions == nil {
		s.sessions = map[domain.ProviderProfileID]Session{}
	}
	s.sessions[id] = sess
}

// closeExisting best-effort closes and forgets id's tracked session, if any.
// A close failure (including "already gone") is logged nowhere and swallowed
// deliberately: the caller's own next step (opening a fresh terminal, or just
// returning after Stop) does not depend on this having succeeded, and a PTY
// that outlives its tracking entry is still reaped by shellterm's own
// per-app-run mechanisms.
func (s *Service) closeExisting(ctx context.Context, id domain.ProviderProfileID) {
	s.mu.Lock()
	sess, ok := s.sessions[id]
	if ok {
		delete(s.sessions, id)
	}
	s.mu.Unlock()
	if !ok {
		return
	}
	_ = s.Terminals.CloseShellTerminal(ctx, sess.HandleID)
}
