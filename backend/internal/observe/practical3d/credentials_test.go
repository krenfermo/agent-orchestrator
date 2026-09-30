package practical3d

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// An agent launch carries placeholders only, whatever credentials the
// daemon's environment held; Codex is configured as an external-auth
// provider (no ChatGPT login).
func TestShimLaunchCarriesNoProviderCredential(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	cfg := ShimConfig{RealClaude: "/bin/echo", RealCodex: "/bin/echo", Profile: filepath.Join(dir, "p.sb"),
		CodexArgs: []string{"-c", `model_providers.p3d.base_url="{BASE}"`, "-c", "model_providers.p3d.requires_openai_auth=false", "-c", `model_providers.p3d.env_key="` + CodexKeyEnv + `"`},
		Sandbox:   SandboxParams{AOHome: dir, RealHome: dir, AOSrc: dir, PrivateCtl: dir, ToolsRO: dir, OracleDir: dir, PosWork: dir, PosWorktrees: dir, PosHome: dir, PosTmp: dir, PosRunFile: dir, PosPrompts: dir, PosHookBin: dir, ProxyPort: 1, DaemonPort: 2}}
	environ := []string{"PATH=/usr/bin", "AO_SESSION_ID=s1", "ANTHROPIC_API_KEY=sk-secret", "CLAUDE_CODE_OAUTH_TOKEN=oauth-secret", "OPENAI_API_KEY=sk-openai", "CODEX_API_KEY=cx", "ANTHROPIC_AUTH_TOKEN=real-token"}
	token := func(string) (string, error) { return "tok", nil }
	for _, harness := range []string{"claude", "codex"} {
		argv, env, err := buildShimLaunch(harness, cfg, []string{"--", "task"}, environ, token)
		if err != nil {
			t.Fatal(err)
		}
		all := strings.Join(append(argv, env...), "\n")
		for _, secret := range []string{"sk-secret", "oauth-secret", "sk-openai", "=cx", "real-token", "auth.json"} {
			if strings.Contains(all, secret) {
				t.Errorf("%s launch carries %q", harness, secret)
			}
		}
		for _, want := range []string{"ANTHROPIC_AUTH_TOKEN=" + PlaceholderKey, CodexKeyEnv + "=" + PlaceholderKey} {
			if !strings.Contains(all, want) {
				t.Errorf("%s launch lacks %s", harness, want)
			}
		}
	}
}

// OperatorCredentials replaces whatever the client sent, attests the Codex
// account by hash, and fails closed on an unreadable login.
func TestOperatorCredentialsInjectAndFailClosed(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	auth := filepath.Join(dir, "auth.json")
	if err := os.WriteFile(auth, []byte(`{"tokens":{"access_token":"AT","account_id":"acct-1"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	o := &OperatorCredentials{CodexAuthFile: auth}
	h := http.Header{}
	h.Set("Authorization", "Bearer "+PlaceholderKey)
	h.Set("Chatgpt-Account-Id", "agent-chosen")
	ref, err := o.Inject("openai", h)
	if err != nil {
		t.Fatal(err)
	}
	if h.Get("Authorization") != "Bearer AT" || h.Get("Chatgpt-Account-Id") != "acct-1" || ref != sha256Hex([]byte("acct-1")) {
		t.Fatalf("injection: %v ref=%s", h, ref)
	}
	missing := &OperatorCredentials{CodexAuthFile: filepath.Join(dir, "none.json"), Now: func() time.Time { return time.Unix(0, 0) }}
	if _, err := missing.Inject("openai", http.Header{}); err == nil {
		t.Fatal("unreadable login did not fail closed")
	}
	if _, err := o.Inject("other", http.Header{}); err == nil {
		t.Fatal("unknown protocol accepted")
	}
}

type fakeCredentials struct{}

func (fakeCredentials) Inject(protocol string, h http.Header) (string, error) {
	h.Del("Authorization")
	h.Del("X-Api-Key")
	h.Set("Authorization", "Bearer operator-"+protocol)
	return "", nil
}

// The proxy forwards the operator's credential, never the agent's.
func TestProxyInjectsOperatorCredential(t *testing.T) {
	t.Parallel()
	var mu sync.Mutex
	var got []string
	rig := newProxyRig(t, ArmOff, func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		got = append(got, r.Header.Get("Authorization")+"|"+r.Header.Get("X-Api-Key"))
		mu.Unlock()
		sseSuccess(w, r)
	}, nil)
	rig.proxy.Credentials = fakeCredentials{}
	req, _ := http.NewRequest(http.MethodPost, rig.base+"/v1/messages", strings.NewReader(string(requestBody(testPrimaryModel, 1, ""))))
	req.Header.Set("Authorization", "Bearer "+PlaceholderKey)
	req.Header.Set("X-Api-Key", "agent-key")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	mu.Lock()
	defer mu.Unlock()
	if len(got) != 1 || got[0] != "Bearer operator-anthropic|" {
		t.Fatalf("upstream saw %q", got)
	}
}

// The Codex login a position gets is an API-key login holding only the
// placeholder, which `codex login status` reports as logged in.
func TestCodexPlaceholderLoginHoldsNoCredential(t *testing.T) {
	t.Parallel()
	var login map[string]string
	if err := json.Unmarshal(codexPlaceholderLogin(), &login); err != nil {
		t.Fatal(err)
	}
	if len(login) != 2 || login["auth_mode"] != "apikey" || login["OPENAI_API_KEY"] != PlaceholderKey {
		t.Fatalf("placeholder login: %v", login)
	}
}
