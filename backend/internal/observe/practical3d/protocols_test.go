package practical3d

import (
	"encoding/json"
	"net/http"
	"testing"
)

func TestResponsesAccumulatorAccountingAndTools(t *testing.T) {
	t.Parallel()
	a := &responsesAccumulator{}
	for _, l := range []string{
		`data: {"type":"response.created","response":{"id":"resp_1","status":"in_progress"}}`,
		`data: {"type":"response.output_item.done","item":{"type":"function_call","call_id":"call_1","name":"shell","arguments":"{\"command\":[\"bash\",\"-lc\",\"ao review submit --verdict approve\"]}"}}`,
		`data: {"type":"response.output_item.done","item":{"type":"local_shell_call","call_id":"call_2","action":{"command":["rg","-n","Lockout"]}}}`,
		`data: {"type":"response.completed","response":{"id":"resp_1","status":"completed","usage":{"input_tokens":1000,"input_tokens_details":{"cached_tokens":800},"output_tokens":20}}}`,
	} {
		a.sseLine([]byte(l))
	}
	res := a.result(http.StatusOK, true, nil)
	if res.Outcome != OutcomeSuccess || *res.InputTokens != 1000 || *res.CachedInputTokens != 800 || *res.UncachedInputTokens != 200 {
		t.Fatalf("result=%+v", res)
	}
	tools := a.toolUses()
	if a.id() != "resp_1" || len(tools) != 2 || tools[0].Command != "bash -lc ao review submit --verdict approve" || tools[1].Name != "local_shell" {
		t.Fatalf("id=%s tools=%+v", a.id(), tools)
	}
}

func TestResponsesAccumulatorFailuresAndMissingUsage(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		status int
		lines  []string
		want   RequestOutcome
		zero   bool
		body   string
	}{
		// 3d-practical §accounting (Codex reviews R1/R3): a response without
		// usage is MISSING, never zero, error envelopes included; the outcome
		// is still classified.
		"rate limited":        {429, nil, OutcomeRateLimited, false, `{"error":{"type":"rate_limit_exceeded","code":"rate_limit_exceeded"}}`},
		"chatgpt detail":      {429, nil, OutcomeRateLimited, false, `{"detail":{"code":"usage_limit_reached"}}`},
		"server error":        {502, nil, OutcomeRetryable, false, `{"error":{"type":"server_error"}}`},
		"completed no cached": {200, []string{`data: {"type":"response.completed","response":{"id":"r","status":"completed","usage":{"input_tokens":100}}}`}, OutcomeSuccess, false, ""},
		"gateway page":        {502, nil, OutcomeRetryable, false, `<html>bad gateway</html>`},
		"stream cut":          {200, []string{`data: {"type":"response.created","response":{"id":"r"}}`}, OutcomeTerminalFailure, false, ""},
		"completed no usage":  {200, []string{`data: {"type":"response.completed","response":{"id":"r","status":"completed"}}`}, OutcomeSuccess, false, ""},
	} {
		a := &responsesAccumulator{}
		for _, l := range tc.lines {
			a.sseLine([]byte(l))
		}
		if tc.body != "" {
			a.jsonBody([]byte(tc.body))
		}
		res := a.result(tc.status, true, nil)
		if res.Outcome != tc.want {
			t.Errorf("%s outcome=%s", name, res.Outcome)
		}
		if tc.zero != (res.InputTokens != nil && *res.InputTokens == 0) {
			t.Errorf("%s accounting=%v", name, res.InputTokens)
		}
		if (name == "completed no usage" || name == "gateway page") && res.UncachedInputTokens != nil {
			t.Errorf("missing usage must stay MISSING")
		}
	}
}

func TestResponsesBaseClassAndTools(t *testing.T) {
	t.Parallel()
	m, _ := claudeCodeManifest(t)
	parse := func(body string) parsedRequest {
		p, err := openaiProtocol{}.parse([]byte(body))
		if err != nil {
			t.Fatal(err)
		}
		return p
	}
	cases := map[CallClass]string{
		CallInitial:      `{"model":"` + testPrimaryModel + `","stream":true,"input":[{"type":"message","role":"user","content":[]}]}`,
		CallToolResult:   `{"model":"` + testPrimaryModel + `","stream":true,"input":[{"type":"message","role":"user"},{"type":"function_call","name":"shell"},{"type":"function_call_output","call_id":"c"}]}`,
		CallContinuation: `{"model":"` + testPrimaryModel + `","stream":true,"input":[{"type":"message","role":"user"},{"type":"message","role":"assistant"},{"type":"message","role":"user"}]}`,
		CallHelper:       `{"model":"other-model","stream":true,"input":[]}`,
	}
	for want, body := range cases {
		if got := parse(body).baseClass(m, "A", RoleWorker); got != want {
			t.Errorf("want %s got %s", want, got)
		}
	}
	p := parse(`{"model":"x","tools":[{"type":"function","name":"shell"},{"type":"web_search"}]}`)
	if len(p.tools) != 2 || p.tools[0] != "shell" || p.tools[1] != "web_search" {
		t.Fatalf("tools=%v", p.tools)
	}
	if err := checkContextTools(m, p.tools); err == nil {
		t.Fatal("web_search offered while web is DISABLED accepted")
	}
	var h http.Header = map[string][]string{"Chatgpt-Account-Id": {"acct"}}
	if (openaiProtocol{}).accountRef(h, nil) != sha256Hex([]byte("acct")) {
		t.Fatal("codex account attestation")
	}
	_ = json.Valid
}

func TestAccountRefSetAttestation(t *testing.T) {
	t.Parallel()
	m, _ := claudeCodeManifest(t)
	refs := map[string]string{"anthropic": sha256Hex([]byte("org")), "openai": sha256Hex([]byte("acct"))}
	set, _ := AccountRefSet(refs)
	m.Provider.AccountRefSHA256 = set
	ok := func(ref, proto string, rs map[string]string) bool {
		raw, _ := json.Marshal(map[string]any{"account_ref_sha256": ref, "provider_protocol": proto, "account_refs": rs})
		return accountAttested(m, raw)
	}
	if !ok(refs["openai"], "openai", refs) || !ok(refs["anthropic"], "anthropic", refs) {
		t.Fatal("frozen per-provider accounts rejected")
	}
	if ok(sha256Hex([]byte("other")), "openai", refs) {
		t.Fatal("another ChatGPT account accepted")
	}
	forged := map[string]string{"anthropic": refs["anthropic"], "openai": sha256Hex([]byte("other"))}
	if ok(forged["openai"], "openai", forged) {
		t.Fatal("a forged account map was accepted")
	}
}
