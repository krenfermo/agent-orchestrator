package practical3d

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const (
	testPrimaryModel = "claude-primary-test"
	testHelperModel  = "claude-helper-test"
	testOrg          = "org-test"
)

// claudeCodeManifest turns the technical fixture into a manifest whose cells
// describe real Claude Code requests observed at the proxy.
func claudeCodeManifest(t *testing.T) (Manifest, TechnicalArtifacts) {
	t.Helper()
	m, _, art := testManifest(t)
	m = deepCopyManifest(t, m)
	m.Provider.AccountRefSHA256 = sha256Hex([]byte(testOrg))
	classes := []CallClass{CallInitial, CallContinuation, CallToolResult, CallHelper, CallRetry}
	m.TreatmentMapping, m.InvocationConfigs = nil, nil
	present, absent := true, false
	for i := range m.Workflow.TaskRoles {
		m.Workflow.TaskRoles[i].RoleFlow[0].ReachableCallClasses = classes
		task := m.Workflow.TaskRoles[i].TaskID
		role := m.Workflow.TaskRoles[i].RoleFlow[0].Role
		for _, class := range classes {
			model := testPrimaryModel
			assisted := TreatmentArm{AttachmentPresent: &present, AttachmentSHA256: sha256Hex(art.Attachment), AttachmentArtifactRef: art.AttachmentRef, AttachmentVersion: "fixture-v1", ProvenanceSchemaVersion: "v1", ConstructionVersion: "v1", RenderVersion: "v1", IndexedCommit: m.AOCommit, SourceManifestSHA256: m.Tasks[0].OracleRef, FreshnessInputsSHA256: m.Tasks[0].OracleRef, Origin: "PROJECT_MEMORY"}
			if class == CallHelper {
				model = testHelperModel
				assisted = TreatmentArm{AttachmentPresent: &absent}
			}
			m.TreatmentMapping = append(m.TreatmentMapping, TreatmentCell{TaskID: task, Role: role, CallClass: class, OFF: TreatmentArm{AttachmentPresent: &absent}, ASSISTED: assisted})
			cfg := json.RawMessage(`{"claude_code_version":"2.1.285","model":"` + model + `","permission_mode":"bypassPermissions","stream":true,"tool_policy":"no_mcp_no_web"}`)
			c, _ := canonicalRaw(cfg, noDecimals)
			m.InvocationConfigs = append(m.InvocationConfigs, InvocationConfig{TaskID: task, Role: role, CallClass: class, ModelID: model, ModelVersion: model, EffectiveConfigSchema: EffectiveConfigSchemaClaudeCodeV1, EffectiveConfigSHA256: sha256Hex(c), EffectiveConfig: cfg})
		}
	}
	m.Randomization.Schedule, _ = GenerateSchedule(m.Randomization)
	if err := ValidateManifest(m); err != nil {
		t.Fatal(err)
	}
	return m, art
}

type staticRole struct {
	role Role
	err  error
}

func (s staticRole) ResolveRole(context.Context, string, time.Time) (Role, error) { return s.role, s.err }

type proxyRig struct {
	m        Manifest
	art      TechnicalArtifacts
	client   *ObservedClient
	proxy    *ProviderProxy
	base     string
	ledger   string
	upstream *httptest.Server
	hits     atomic.Int32
}

func newProxyRig(t *testing.T, arm Arm, upstream http.HandlerFunc, resolver RoleResolver) *proxyRig {
	t.Helper()
	m, art := claudeCodeManifest(t)
	var p Position
	for _, x := range m.Randomization.Schedule {
		if x.TaskID == "A" && x.Arm == arm {
			p = x
			break
		}
	}
	dir := t.TempDir()
	if err := art.Write(filepath.Join(dir, "art")); err != nil {
		t.Fatal(err)
	}
	spans, err := preflightArtifacts(m, DirArtifactResolver{Root: filepath.Join(dir, "art")}, filepath.Join(dir, "frozen"))
	if err != nil {
		t.Fatal(err)
	}
	ledgerPath := filepath.Join(dir, "ledger.jsonl")
	l, _, err := CreateRunDirectory(filepath.Join(dir, "run"), m, EnvelopeMetadata{}, true)
	if err != nil {
		t.Fatal(err)
	}
	ledgerPath = l.Path()
	id, _ := ExperimentID(m)
	rig := &proxyRig{m: m, art: art, ledger: ledgerPath}
	rig.upstream = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rig.hits.Add(1)
		upstream(w, r)
	}))
	t.Cleanup(rig.upstream.Close)
	rig.client = newObservedClient(context.Background(), m, p, PositionWorkspace{}, id, l, nil, time.Now, sleepContext, spans)
	if resolver == nil {
		resolver = staticRole{role: RoleWorker}
	}
	px, err := NewProviderProxy(rig.upstream.URL, resolver, filepath.Join(dir, "evidence"))
	if err != nil {
		t.Fatal(err)
	}
	sockDir, err := os.MkdirTemp("/tmp", "p3d")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(sockDir) })
	port, err := px.Start(filepath.Join(sockDir, "ctl.sock"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = px.Close() })
	px.Bind(rig.client)
	rig.proxy = px
	tok, err := px.IssueToken("session:worker-1")
	if err != nil {
		t.Fatal(err)
	}
	rig.base = fmt.Sprintf("http://127.0.0.1:%d/t/%s", port, tok)
	return rig
}

func (r *proxyRig) post(_ *testing.T, body []byte) (int, string) {
	req, _ := http.NewRequest(http.MethodPost, r.base+"/v1/messages?beta=true", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer secret")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, err.Error()
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func (r *proxyRig) events(t *testing.T) []Event {
	t.Helper()
	ev, err := ReadLedger(r.ledger)
	if err != nil {
		t.Fatal(err)
	}
	return ev
}

func requestBody(model string, messages int, extra string) []byte {
	msgs := []string{`{"role":"user","content":"do the task` + extra + `"}`}
	for i := 1; i < messages; i++ {
		msgs = append(msgs, `{"role":"assistant","content":"x"}`, `{"role":"user","content":[{"type":"tool_result","tool_use_id":"t","content":"r"}]}`)
	}
	return []byte(`{"model":"` + model + `","stream":true,"messages":[` + strings.Join(msgs, ",") + `],"tools":[{"name":"Read"},{"name":"Bash"}]}`)
}

func sseSuccess(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Anthropic-Organization-Id", testOrg)
	w.Header().Set("Content-Type", "text/event-stream")
	_, _ = io.WriteString(w, `event: message_start
data: {"type":"message_start","message":{"id":"msg_1","usage":{"input_tokens":10,"cache_creation_input_tokens":3,"cache_read_input_tokens":5}}}

event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"toolu_1","name":"Bash","input":{}}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"command\":\"ao review"}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":" submit --verdict approve\"}"}}

event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":7}}

event: message_stop
data: {"type":"message_stop"}

`)
}

func TestProxyRecordsEveryAttemptWithAccountingAndToolUses(t *testing.T) {
	t.Parallel()
	rig := newProxyRig(t, ArmOff, sseSuccess, nil)
	code, body := rig.post(t, requestBody(testPrimaryModel, 1, ""))
	if code != 200 || !strings.Contains(body, "message_stop") {
		t.Fatalf("status=%d body=%q", code, body)
	}
	ev := rig.events(t)
	var d, f *Event
	for i := range ev {
		switch ev[i].Type {
		case EventAttemptDispatched:
			d = &ev[i]
		case EventAttemptFinalized:
			f = &ev[i]
		}
	}
	if d == nil || f == nil || d.CallClass != CallInitial || d.Role != RoleWorker || d.CallIndex != 1 {
		t.Fatalf("dispatch/finalize missing or wrong: %+v %+v", d, f)
	}
	if *f.InputTokens != 18 || *f.CachedInputTokens != 5 || *f.UncachedInputTokens != 13 || f.RequestOutcome != OutcomeSuccess {
		t.Fatalf("accounting: in=%d cached=%d uncached=%d outcome=%s", *f.InputTokens, *f.CachedInputTokens, *f.UncachedInputTokens, f.RequestOutcome)
	}
	if !strings.Contains(string(f.ProviderMetadata), sha256Hex([]byte(testOrg))) {
		t.Fatal("account attestation missing from provider metadata")
	}
	obs := rig.proxy.Observations()
	if len(obs) != 1 || obs[0].MessageID != "msg_1" || len(obs[0].ToolUses) != 1 || obs[0].ToolUses[0].Command != "ao review submit --verdict approve" {
		t.Fatalf("observations: %+v", obs)
	}
	if m, f := rig.client.Outcome(); m != "" || f != "" {
		t.Fatalf("clean attempt marked the position: %q %q", m, f)
	}
}

func TestProxyRetryChainAndBudget(t *testing.T) {
	t.Parallel()
	rig := newProxyRig(t, ArmOff, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(529)
		_, _ = io.WriteString(w, `{"type":"error","error":{"type":"overloaded_error"}}`)
	}, nil)
	body := requestBody(testPrimaryModel, 2, "")
	for i := 0; i < 2; i++ {
		if code, _ := rig.post(t, body); code != 529 {
			t.Fatalf("attempt %d status=%d", i, code)
		}
	}
	// Budget is 1 retry: the third identical request is refused, not forwarded.
	if code, _ := rig.post(t, body); code == 529 {
		t.Fatal("retry beyond budget was forwarded")
	}
	if rig.hits.Load() != 2 {
		t.Fatalf("upstream hits=%d", rig.hits.Load())
	}
	var retry *Event
	ev := rig.events(t)
	for i := range ev {
		if ev[i].Type == EventAttemptDispatched && ev[i].CallClass == CallRetry {
			retry = &ev[i]
		}
	}
	if retry == nil || retry.RetryIndex != 1 || retry.RetryCause != OutcomeRetryable {
		t.Fatalf("retry dispatch: %+v", retry)
	}
	if _, f := rig.client.Outcome(); f != StateProviderRetryExhausted {
		t.Fatalf("failure=%q", f)
	}
}

func TestProxyPartialStreamIsTerminalAndCounted(t *testing.T) {
	t.Parallel()
	rig := newProxyRig(t, ArmOff, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_p\",\"usage\":{\"input_tokens\":40}}}\n\n")
		// connection ends before message_stop
	}, nil)
	rig.post(t, requestBody(testPrimaryModel, 1, ""))
	var f *Event
	ev := rig.events(t)
	for i := range ev {
		if ev[i].Type == EventAttemptFinalized {
			f = &ev[i]
		}
	}
	if f == nil || f.RequestOutcome != OutcomeTerminalFailure || f.UncachedInputTokens == nil || *f.UncachedInputTokens != 40 {
		t.Fatalf("partial stream: %+v", f)
	}
	if _, state := rig.client.Outcome(); state != StateProviderTerminalFailure {
		t.Fatalf("state=%q", state)
	}
}

func TestProxyMissingUsageIsMalformed(t *testing.T) {
	t.Parallel()
	rig := newProxyRig(t, ArmOff, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "data: {\"type\":\"message_start\",\"message\":{\"id\":\"m\"}}\n\ndata: {\"type\":\"message_stop\"}\n\n")
	}, nil)
	rig.post(t, requestBody(testPrimaryModel, 1, ""))
	if m, _ := rig.client.Outcome(); !strings.Contains(m, "MISSING") {
		t.Fatalf("malformed=%q", m)
	}
}

func TestProxyRefusesUnobservableOrOutOfMappingRequests(t *testing.T) {
	t.Parallel()
	cases := map[string]struct {
		arm      Arm
		resolver RoleResolver
		send     func(r *proxyRig) (int, string)
	}{
		"no token": {ArmOff, nil, func(r *proxyRig) (int, string) {
			resp, err := http.Post(strings.Replace(r.base, "/t/", "/t/bogus", 1)+"/v1/messages", "application/json", bytes.NewReader(requestBody(testPrimaryModel, 1, "")))
			if err != nil {
				return 0, err.Error()
			}
			_ = resp.Body.Close()
			return resp.StatusCode, ""
		}},
		"count_tokens endpoint": {ArmOff, nil, func(r *proxyRig) (int, string) {
			resp, err := http.Post(r.base+"/v1/messages/count_tokens", "application/json", bytes.NewReader(requestBody(testPrimaryModel, 1, "")))
			if err != nil {
				return 0, err.Error()
			}
			_ = resp.Body.Close()
			return resp.StatusCode, ""
		}},
		"OFF with attachment bytes": {ArmOff, nil, nil},
		"ASSISTED without attachment": {ArmAssisted, nil, func(r *proxyRig) (int, string) {
			return r.post(nil, requestBody(testPrimaryModel, 1, ""))
		}},
		"MCP tool offered": {ArmOff, nil, func(r *proxyRig) (int, string) {
			return r.post(nil, []byte(`{"model":"`+testPrimaryModel+`","stream":true,"messages":[{"role":"user","content":"x"}],"tools":[{"name":"mcp__github__search"}]}`))
		}},
		"unfrozen model": {ArmOff, nil, func(r *proxyRig) (int, string) {
			return r.post(nil, requestBody("claude-other", 1, ""))
		}},
		"role unresolvable": {ArmOff, staticRole{err: errors.New("no window")}, func(r *proxyRig) (int, string) {
			return r.post(nil, requestBody(testPrimaryModel, 1, ""))
		}},
		"role outside the closed set": {ArmOff, staticRole{role: RoleHelper}, func(r *proxyRig) (int, string) {
			return r.post(nil, requestBody(testPrimaryModel, 1, ""))
		}},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			rig := newProxyRig(t, tc.arm, sseSuccess, tc.resolver)
			send := tc.send
			if send == nil {
				send = func(r *proxyRig) (int, string) {
					span := string(canonicalStringBody(string(r.art.Attachment)))
					return r.post(nil, requestBody(testPrimaryModel, 1, " "+span))
				}
			}
			code, _ := send(rig)
			if code == 200 {
				t.Fatal("request was forwarded")
			}
			if rig.hits.Load() != 0 {
				t.Fatal("upstream reached")
			}
			if m, _ := rig.client.Outcome(); m == "" {
				t.Fatal("position not marked malformed")
			}
		})
	}
}

func TestProxyAssistedWithAttachmentIsTracedAsProjectMemory(t *testing.T) {
	t.Parallel()
	rig := newProxyRig(t, ArmAssisted, sseSuccess, nil)
	span := string(canonicalStringBody(string(rig.art.Attachment)))
	if code, _ := rig.post(t, requestBody(testPrimaryModel, 1, " "+span)); code != 200 {
		t.Fatalf("status=%d", code)
	}
	for _, e := range rig.events(t) {
		if e.Type == EventAttemptDispatched && (e.AttachmentPresent == nil || !*e.AttachmentPresent || e.AttachmentOrigin != "PROJECT_MEMORY") {
			t.Fatalf("assisted trace: %+v", e)
		}
	}
	// A helper-model request in the same position has no attachment cell.
	if code, _ := rig.post(t, requestBody(testHelperModel, 1, "")); code != 200 {
		t.Fatalf("helper status=%d", code)
	}
	if m, _ := rig.client.Outcome(); m != "" {
		t.Fatalf("malformed=%q", m)
	}
}
