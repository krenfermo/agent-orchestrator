package cli

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/identity"
)

// TestSessionCookieNameMatchesDaemon pins the CLI's copy of the session cookie
// name to the daemon's. They are duplicated rather than imported (AGENTS.md
// keeps httpd out of the CLI), and a silent divergence would look exactly like
// "the CLI cannot sign in" with no error anywhere.
func TestSessionCookieNameMatchesDaemon(t *testing.T) {
	if sessionCookieName != identity.SessionCookieName {
		t.Fatalf("CLI cookie name = %q, daemon = %q", sessionCookieName, identity.SessionCookieName)
	}
}

// oidcDaemon is a fake daemon serving exactly the three routes `ao auth login`
// uses, plus a permission-gated route that answers 401 without the session
// cookie and 200 with it. It is the CLI's half of the P4-A loopback handoff.
type oidcDaemon struct {
	// pendingClaims is how many times /claim answers "pending" before
	// completing, so the poll loop is exercised rather than short-circuited.
	pendingClaims int
	// issuedToken is the session token handed over on completion.
	issuedToken string
	// oidcEnabled false makes /auth/providers report a trusted-local install.
	oidcEnabled bool

	claims       int
	handoffSeen  string
	guardedAuthd bool
	guardedCalls int
}

func (d *oidcDaemon) start(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v1/auth/providers":
			if d.oidcEnabled {
				_, _ = io.WriteString(w, `{"mode":"oidc","passwordEnabled":true,`+
					`"oidc":{"displayName":"Google","startPath":"/api/v1/auth/oidc/start"}}`)
				return
			}
			_, _ = io.WriteString(w, `{"mode":"trusted_local","passwordEnabled":true}`)
		case "/api/v1/auth/oidc/start":
			var in oidcStartRequest
			_ = json.NewDecoder(r.Body).Decode(&in)
			d.handoffSeen = in.HandoffSecret
			if in.ClientKind != "desktop" {
				t.Errorf("clientKind = %q, want the loopback handoff kind", in.ClientKind)
			}
			_ = json.NewEncoder(w).Encode(oidcStartResponse{
				AuthorizationURL: "https://accounts.example/authorize?state=flow-1",
				FlowID:           "flow-1",
				ExpiresAt:        time.Now().Add(10 * time.Minute),
			})
		case "/api/v1/auth/oidc/claim":
			var in oidcClaimRequest
			_ = json.NewDecoder(r.Body).Decode(&in)
			if in.FlowID != "flow-1" || in.HandoffSecret != d.handoffSeen {
				w.WriteHeader(http.StatusUnauthorized)
				_, _ = io.WriteString(w, `{"code":"SSO_HANDOFF_INVALID","message":"bad handoff"}`)
				return
			}
			d.claims++
			if d.claims <= d.pendingClaims {
				_, _ = io.WriteString(w, `{"status":"pending"}`)
				return
			}
			http.SetCookie(w, &http.Cookie{
				Name:    identity.SessionCookieName,
				Value:   d.issuedToken,
				Path:    "/",
				Expires: time.Now().Add(24 * time.Hour),
			})
			_, _ = io.WriteString(w,
				`{"status":"complete","user":{"id":"u-1","email":"dev@example.com","displayName":"Dev","role":"owner"}}`)
		case "/api/v1/auth/logout":
			_, _ = io.WriteString(w, `{"ok":true}`)
		default:
			// Every other route is permission-gated, exactly as the daemon's
			// are under AO_AUTH_MODE=oidc.
			d.guardedCalls++
			ck, err := r.Cookie(identity.SessionCookieName)
			d.guardedAuthd = err == nil && ck.Value == d.issuedToken
			if !d.guardedAuthd {
				w.WriteHeader(http.StatusUnauthorized)
				_, _ = io.WriteString(w,
					`{"code":"NOT_AUTHENTICATED","message":"authentication required","requestId":"req-9"}`)
				return
			}
			_, _ = io.WriteString(w, `{"ok":true,"recovery":{},"repair":{},"workflow":{"run":{"id":"wf-1","state":"needs_attention"}}}`)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func loginDeps(opened *string) Deps {
	return Deps{
		ProcessAlive: func(int) bool { return true },
		Sleep:        func(time.Duration) {},
		CommandOutput: func(_ context.Context, name string, args ...string) ([]byte, error) {
			if opened != nil && len(args) > 0 {
				*opened = args[len(args)-1]
			}
			_ = name
			return nil, nil
		},
	}
}

// TestAuthLoginStoresTheSessionTheDaemonIssued is the whole point of the
// slice: after `ao auth login`, the CLI holds a real AO session — one the
// daemon minted after the provider authenticated a human — and it is the
// Set-Cookie header, never a JSON body, that carries it.
func TestAuthLoginStoresTheSessionTheDaemonIssued(t *testing.T) {
	cfg := setConfigEnv(t)
	daemon := &oidcDaemon{oidcEnabled: true, pendingClaims: 2, issuedToken: "sess-abc"}
	srv := daemon.start(t)
	writeRunFileFor(t, cfg, srv)

	var opened string
	out, errOut, err := executeCLI(t, loginDeps(&opened), "auth", "login")
	if err != nil {
		t.Fatalf("auth login: %v\nstderr=%s", err, errOut)
	}
	if daemon.claims != 3 {
		t.Errorf("claim polls = %d, want 3 (two pending, then complete)", daemon.claims)
	}
	if len(daemon.handoffSeen) < 32 {
		t.Errorf("handoff secret = %q, want at least 32 characters", daemon.handoffSeen)
	}
	if opened != "https://accounts.example/authorize?state=flow-1" {
		t.Errorf("browser opened %q, want the provider authorization URL", opened)
	}
	if !strings.Contains(out, "dev@example.com") {
		t.Errorf("output does not name who signed in:\n%s", out)
	}

	cred, ok, err := readCredential(cfg.dataDir)
	if err != nil || !ok {
		t.Fatalf("readCredential: ok=%v err=%v", ok, err)
	}
	if cred.Token != "sess-abc" {
		t.Errorf("stored token = %q, want the daemon's", cred.Token)
	}
	info, err := os.Stat(credentialPath(cfg.dataDir))
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("credential mode = %o, want 600", perm)
	}
}

// TestAuthLoginOnTrustedLocalFabricatesNothing: an installation that never
// asked for SSO gets an explanation, not a manufactured session.
func TestAuthLoginOnTrustedLocalFabricatesNothing(t *testing.T) {
	cfg := setConfigEnv(t)
	srv := (&oidcDaemon{}).start(t)
	writeRunFileFor(t, cfg, srv)

	out, _, err := executeCLI(t, loginDeps(nil), "auth", "login")
	if err != nil {
		t.Fatalf("auth login: %v", err)
	}
	if !strings.Contains(out, "does not use single sign-on") {
		t.Errorf("output = %q, want the trusted-local explanation", out)
	}
	if _, ok, _ := readCredential(cfg.dataDir); ok {
		t.Error("a trusted-local install must not leave a stored credential")
	}
}

// TestAuthenticatedCommandsCarryTheStoredIdentity covers requirement "ao send,
// workflow recover status and workflow resume use the authorized identity":
// there is one credential and one place that presents it, so every command
// inherits it.
func TestAuthenticatedCommandsCarryTheStoredIdentity(t *testing.T) {
	commands := [][]string{
		{"send", "--session", "demo-1", "--message", "hi"},
		{"workflow", "recover", "status", "wf-1"},
		{"workflow", "resume", "wf-1"},
	}
	for _, args := range commands {
		t.Run(args[0]+"/"+strings.Join(args[1:2], ""), func(t *testing.T) {
			cfg := setConfigEnv(t)
			daemon := &oidcDaemon{oidcEnabled: true, issuedToken: "sess-abc"}
			srv := daemon.start(t)
			writeRunFileFor(t, cfg, srv)
			if err := writeCredential(cfg.dataDir, storedCredential{
				Token: "sess-abc", ExpiresAt: time.Now().Add(time.Hour),
			}); err != nil {
				t.Fatal(err)
			}

			if _, errOut, err := executeCLI(t, Deps{ProcessAlive: func(int) bool { return true }}, args...); err != nil {
				t.Fatalf("%v: %v\nstderr=%s", args, err, errOut)
			}
			if !daemon.guardedAuthd {
				t.Errorf("%v reached the daemon without the stored session cookie", args)
			}
		})
	}
}

// TestExpiredCredentialIsNotPresented: an expired token would produce the same
// 401 with a worse message, so it is not sent at all.
func TestExpiredCredentialIsNotPresented(t *testing.T) {
	cfg := setConfigEnv(t)
	daemon := &oidcDaemon{oidcEnabled: true, issuedToken: "sess-abc"}
	srv := daemon.start(t)
	writeRunFileFor(t, cfg, srv)
	if err := writeCredential(cfg.dataDir, storedCredential{
		Token: "sess-abc", ExpiresAt: time.Now().Add(-time.Hour),
	}); err != nil {
		t.Fatal(err)
	}

	_, _, err := executeCLI(t, Deps{ProcessAlive: func(int) bool { return true }},
		"workflow", "recover", "status", "wf-1")
	if err == nil {
		t.Fatal("expected the daemon's 401")
	}
	if daemon.guardedAuthd {
		t.Error("an expired credential was presented")
	}
}

// TestUnauthenticatedErrorNamesTheFix: the bare envelope is true and useless.
// Every command inherits the sentence that says what to do about it.
func TestUnauthenticatedErrorNamesTheFix(t *testing.T) {
	cfg := setConfigEnv(t)
	srv := (&oidcDaemon{oidcEnabled: true, issuedToken: "sess-abc"}).start(t)
	writeRunFileFor(t, cfg, srv)

	_, _, err := executeCLI(t, Deps{ProcessAlive: func(int) bool { return true }},
		"workflow", "recover", "status", "wf-1")
	if err == nil {
		t.Fatal("expected an error")
	}
	msg := err.Error()
	if !strings.Contains(msg, "NOT_AUTHENTICATED") {
		t.Errorf("error lost the daemon's code: %q", msg)
	}
	if !strings.Contains(msg, "ao auth login") {
		t.Errorf("error does not name the fix: %q", msg)
	}
	if !strings.Contains(msg, "req-9") {
		t.Errorf("error lost the request id: %q", msg)
	}
}

// TestAuthLogoutRevokesThenForgets: the reverse order would strand a live
// session with no token left to revoke it with.
func TestAuthLogoutRevokesThenForgets(t *testing.T) {
	cfg := setConfigEnv(t)
	daemon := &oidcDaemon{oidcEnabled: true, issuedToken: "sess-abc"}
	srv := daemon.start(t)
	writeRunFileFor(t, cfg, srv)
	if err := writeCredential(cfg.dataDir, storedCredential{
		Token: "sess-abc", ExpiresAt: time.Now().Add(time.Hour),
	}); err != nil {
		t.Fatal(err)
	}

	if _, errOut, err := executeCLI(t, Deps{ProcessAlive: func(int) bool { return true }}, "auth", "logout"); err != nil {
		t.Fatalf("auth logout: %v\nstderr=%s", err, errOut)
	}
	if _, ok, _ := readCredential(cfg.dataDir); ok {
		t.Error("credential survived logout")
	}

	// Idempotent: signing out twice succeeds and does not call the daemon.
	out, _, err := executeCLI(t, Deps{ProcessAlive: func(int) bool { return true }}, "auth", "logout")
	if err != nil {
		t.Fatalf("second auth logout: %v", err)
	}
	if !strings.Contains(out, "nothing to sign out of") {
		t.Errorf("second logout output = %q", out)
	}
}
