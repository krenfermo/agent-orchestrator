package cli

import (
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/agentcred"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/identity"
)

// TestAgentTokenHeaderMatchesDaemon pins the duplicated header name to the
// daemon's, exactly as TestSessionCookieNameMatchesDaemon does for the cookie.
func TestAgentTokenHeaderMatchesDaemon(t *testing.T) {
	if agentTokenHeader != identity.AgentTokenHeader {
		t.Fatalf("agentTokenHeader = %q, daemon = %q", agentTokenHeader, identity.AgentTokenHeader)
	}
}

// credentialProbe records what identity each request actually presented.
type credentialProbe struct {
	sawAgentToken string
	sawCookie     string
	calls         int
	unauthorized  bool
}

func (p *credentialProbe) start(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.HasPrefix(r.URL.Path, "/api/v1/auth/") || strings.HasPrefix(r.URL.Path, "/internal/") {
			_, _ = io.WriteString(w, `{"ok":true}`)
			return
		}
		p.calls++
		p.sawAgentToken = r.Header.Get(identity.AgentTokenHeader)
		p.sawCookie = ""
		if ck, err := r.Cookie(identity.SessionCookieName); err == nil {
			p.sawCookie = ck.Value
		}
		if p.unauthorized {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = io.WriteString(w,
				`{"code":"NOT_AUTHENTICATED","message":"authentication required","requestId":"req-9"}`)
			return
		}
		_, _ = io.WriteString(w, `{"ok":true,"recovery":{},"repair":{},"workflow":{"run":{"id":"wf-1","state":"needs_attention"}}}`)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// envLookup builds a LookupEnv over a fixed map.
func envLookup(pairs map[string]string) func(string) (string, bool) {
	return func(key string) (string, bool) {
		v, ok := pairs[key]
		return v, ok
	}
}

// writeAgentCredential puts a credential file where an AO-launched pane would
// find it, and returns its path.
func writeAgentCredential(t *testing.T, dataDir, token string) string {
	t.Helper()
	path := filepath.Join(dataDir, "agent-credentials", "workflow-review-bf660d26.json")
	if err := agentcred.Write(path, agentcred.File{
		Token:       token,
		Role:        "reviewer",
		SessionID:   "agent-orchestrator-59",
		ReviewRunID: "bf660d26",
		ExpiresAt:   time.Now().Add(time.Hour),
	}); err != nil {
		t.Fatalf("write agent credential: %v", err)
	}
	return path
}

// The reviewer's own credential is what reaches the daemon. This is the fix for
// wf-98ab416c: before it, this request carried nothing and was refused 401 by
// the one route a reviewer exists to call.
func TestInsideAnAgentRuntimeTheAgentCredentialIsPresented(t *testing.T) {
	cfg := setConfigEnv(t)
	probe := &credentialProbe{}
	srv := probe.start(t)
	writeRunFileFor(t, cfg, srv)
	path := writeAgentCredential(t, cfg.dataDir, "agent-token-abc")

	_, errOut, err := executeCLI(t, Deps{
		ProcessAlive: func(int) bool { return true },
		LookupEnv: envLookup(map[string]string{
			agentcred.EnvSessionOwner:   "ao-reviewer:workflow-review-bf660d26",
			agentcred.EnvCredentialFile: path,
		}),
	}, "workflow", "recover", "status", "wf-1")
	if err != nil {
		t.Fatalf("recover status: %v\nstderr=%s", err, errOut)
	}
	if probe.sawAgentToken != "agent-token-abc" {
		t.Fatalf("agent token header = %q, want the reviewer's own credential", probe.sawAgentToken)
	}
	if probe.sawCookie != "" {
		t.Fatalf("an agent also presented a browser session cookie (%q)", probe.sawCookie)
	}
}

// THE ESCALATION THIS CLOSES. The operator's credential sits in a 0600 file
// under AO_DATA_DIR, readable by the same OS user the agent runs as. An agent
// that fell back onto it would act with the operator's authority over every
// project in every organization -- in order to record one verdict.
func TestAnAgentNeverBorrowsThePersonsCredential(t *testing.T) {
	cfg := setConfigEnv(t)
	probe := &credentialProbe{unauthorized: true}
	srv := probe.start(t)
	writeRunFileFor(t, cfg, srv)
	// The operator is signed in on this machine.
	if err := writeCredential(cfg.dataDir, storedCredential{
		Token: "the-operators-session", ExpiresAt: time.Now().Add(time.Hour),
	}); err != nil {
		t.Fatal(err)
	}
	// And this process is a reviewer pane whose own credential is missing --
	// revoked, expired, or never minted by an older build.
	_, errOut, err := executeCLI(t, Deps{
		ProcessAlive: func(int) bool { return true },
		LookupEnv: envLookup(map[string]string{
			agentcred.EnvSessionOwner: "ao-reviewer:workflow-review-bf660d26",
		}),
	}, "workflow", "recover", "status", "wf-1")
	if err == nil {
		t.Fatal("expected the daemon's 401")
	}
	if probe.sawCookie != "" {
		t.Fatalf("an agent borrowed the operator's session (%q)", probe.sawCookie)
	}
	if probe.sawAgentToken != "" {
		t.Fatalf("an agent invented a token (%q)", probe.sawAgentToken)
	}
	// And the advice it is given is advice it can actually act on: telling a
	// pane with no browser and no person to run `ao auth login` sends whoever
	// reads the transcript after the wrong problem.
	msg := err.Error() + errOut
	if strings.Contains(msg, "ao auth login") {
		t.Fatalf("an agent was told to sign in as a person:\n%s", msg)
	}
	if !strings.Contains(msg, "agent's own credential") {
		t.Fatalf("the 401 does not name the agent's own credential:\n%s", msg)
	}
}

// Outside an agent runtime nothing changes: a person's command presents the
// person's session, exactly as it did before.
func TestOutsideAnAgentRuntimeThePersonsCredentialIsUnchanged(t *testing.T) {
	cfg := setConfigEnv(t)
	probe := &credentialProbe{}
	srv := probe.start(t)
	writeRunFileFor(t, cfg, srv)
	if err := writeCredential(cfg.dataDir, storedCredential{
		Token: "the-operators-session", ExpiresAt: time.Now().Add(time.Hour),
	}); err != nil {
		t.Fatal(err)
	}
	// A credential file exists on disk, but this process is not in a pane.
	writeAgentCredential(t, cfg.dataDir, "agent-token-abc")

	if _, errOut, err := executeCLI(t, Deps{
		ProcessAlive: func(int) bool { return true },
		LookupEnv:    envLookup(map[string]string{}),
	}, "workflow", "recover", "status", "wf-1"); err != nil {
		t.Fatalf("recover status: %v\nstderr=%s", err, errOut)
	}
	if probe.sawCookie != "the-operators-session" {
		t.Fatalf("cookie = %q, want the operator's session", probe.sawCookie)
	}
	if probe.sawAgentToken != "" {
		t.Fatalf("a person's command presented an agent token (%q)", probe.sawAgentToken)
	}
}

// InsideAgentRuntime is the switch both halves hang on, so its answers are
// pinned directly.
func TestInsideAgentRuntimeDetection(t *testing.T) {
	for name, tc := range map[string]struct {
		env  map[string]string
		want bool
	}{
		"a person's shell":              {map[string]string{}, false},
		"credential file handed over":   {map[string]string{agentcred.EnvCredentialFile: "/x/y.json"}, true},
		"AO-owned session, no file yet": {map[string]string{agentcred.EnvSessionOwner: "ao-reviewer:x"}, true},
		"blank values are not a claim": {map[string]string{
			agentcred.EnvCredentialFile: "  ", agentcred.EnvSessionOwner: "",
		}, false},
	} {
		t.Run(name, func(t *testing.T) {
			if got := agentcred.InsideAgentRuntime(envLookup(tc.env)); got != tc.want {
				t.Fatalf("InsideAgentRuntime = %v, want %v", got, tc.want)
			}
		})
	}
}
