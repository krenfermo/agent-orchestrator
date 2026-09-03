package cli

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// hooks_subject_test.go — a runtime pane reporting its own token spend.
//
// The routing is the substance: a reviewer pane carries AO_REVIEW_SESSION_ID, a
// resolver pane carries neither session env var, and BOTH must reach the usage
// subject route. The resolver case is the one that would silently do nothing
// under the pre-P3-E code, because it returns before reading stdin.

type recordedSubjectHook struct {
	mu    sync.Mutex
	calls []map[string]any
	paths []string
}

func (r *recordedSubjectHook) record(path string, body map[string]any) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.paths = append(r.paths, path)
	r.calls = append(r.calls, body)
}

func (r *recordedSubjectHook) subjectCalls() []map[string]any {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []map[string]any
	for i, p := range r.paths {
		if strings.HasSuffix(p, "/usage/subject-hook") {
			out = append(out, r.calls[i])
		}
	}
	return out
}

func newHookRecorder(t *testing.T) *recordedSubjectHook {
	t.Helper()
	rec := &recordedSubjectHook{}
	cfg := setConfigEnv(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(req.Body).Decode(&body)
		rec.record(req.URL.Path, body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	t.Cleanup(srv.Close)
	writeRunFileFor(t, cfg, srv)
	return rec
}

func runPaneHook(t *testing.T, payload string) {
	t.Helper()
	_, errOut, err := executeCLI(t, Deps{
		In:           strings.NewReader(payload),
		ProcessAlive: func(int) bool { return true },
	}, "hooks", "claude-code", "post-tool-use")
	if err != nil {
		t.Fatalf("hooks: %v\nstderr=%s", err, errOut)
	}
}

const claudeHookPayload = `{"session_id":"11111111-2222-3333-4444-555555555555","transcript_path":"/tmp/t.jsonl","model":"claude-opus-5"}`

func TestRunHook_ResolverPaneReportsItsOwnUsage(t *testing.T) {
	// A decision-resolver pane has no AO_SESSION_ID by design — it is not a
	// session. Before P3-E that meant it returned immediately and its tokens
	// were never seen at all.
	t.Setenv("AO_SESSION_ID", "")
	t.Setenv("AO_REVIEW_SESSION_ID", "")
	t.Setenv(usageSubjectEnv, "runtime_pane:wqr-1")
	rec := newHookRecorder(t)
	runPaneHook(t, claudeHookPayload)
	calls := rec.subjectCalls()
	if len(calls) != 1 {
		t.Fatalf("subject-hook calls = %d, want 1 — a resolver pane's spend must reach the ledger", len(calls))
	}
	if calls[0]["subject"] != "runtime_pane:wqr-1" {
		t.Fatalf("subject = %v", calls[0]["subject"])
	}
	if calls[0]["nativeSessionId"] != "11111111-2222-3333-4444-555555555555" {
		t.Fatalf("the pane's own conversation id must be forwarded, got %v", calls[0]["nativeSessionId"])
	}
	if calls[0]["harness"] != "claude-code" {
		t.Fatalf("harness = %v, want the pane's own", calls[0]["harness"])
	}
}

func TestRunHook_ReviewerPaneReportsUsageAlongsideItsActivity(t *testing.T) {
	t.Setenv("AO_SESSION_ID", "")
	t.Setenv("AO_REVIEW_SESSION_ID", "review-1")
	t.Setenv(usageSubjectEnv, "runtime_pane:rr-1")
	rec := newHookRecorder(t)
	runPaneHook(t, claudeHookPayload)
	if calls := rec.subjectCalls(); len(calls) != 1 || calls[0]["subject"] != "runtime_pane:rr-1" {
		t.Fatalf("reviewer subject calls = %+v, want one against its review run", calls)
	}
}

func TestRunHook_WithoutASubjectNothingIsReported(t *testing.T) {
	// A pane that carries no subject reports no usage. It must not fall back to
	// guessing one, and it must not post an empty subject the daemon would have
	// to reject on every hook.
	t.Setenv("AO_SESSION_ID", "")
	t.Setenv("AO_REVIEW_SESSION_ID", "")
	t.Setenv(usageSubjectEnv, "")
	rec := newHookRecorder(t)
	runPaneHook(t, claudeHookPayload)
	if calls := rec.subjectCalls(); len(calls) != 0 {
		t.Fatalf("subject calls = %+v, want none", calls)
	}
}

func TestRunHook_WorkerSessionDoesNotReportAgainstASubject(t *testing.T) {
	// A worker session meters itself through the session activity route, which
	// validates a launch id and an activity state. It must not ALSO report
	// through the pane door, which would be a second write of the same tokens.
	t.Setenv("AO_SESSION_ID", "proj-1")
	t.Setenv("AO_REVIEW_SESSION_ID", "")
	t.Setenv(usageSubjectEnv, "")
	rec := newHookRecorder(t)
	runPaneHook(t, claudeHookPayload)
	if calls := rec.subjectCalls(); len(calls) != 0 {
		t.Fatalf("a worker session must not use the pane route, got %+v", calls)
	}
}
