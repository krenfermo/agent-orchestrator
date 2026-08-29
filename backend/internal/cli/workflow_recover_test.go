package cli

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
)

func TestWorkflowRecoverReviewProvenancePostsToTheOperatorRoute(t *testing.T) {
	cfg := setConfigEnv(t)
	var requests []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		appendPrimaryRequest(&requests, r)
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodPost && r.URL.Path == "/api/v1/workflows/wf-1/recover/review-provenance" {
			_, _ = io.WriteString(w, `{"workflow":{"id":"wf-1","state":"waiting"}}`)
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(srv.Close)
	writeRunFileFor(t, cfg, srv)

	out, errOut, err := executeCLI(t, Deps{ProcessAlive: func(int) bool { return true }},
		"workflow", "recover", "review-provenance", "wf-1")
	if err != nil {
		t.Fatalf("recover review-provenance failed: %v stderr=%s", err, errOut)
	}
	if !strings.Contains(out, "wf-1") || !strings.Contains(out, "discarded") {
		t.Fatalf("output does not say what was given up:\n%s", out)
	}
	want := []string{"POST /api/v1/workflows/wf-1/recover/review-provenance"}
	if !reflect.DeepEqual(requests, want) {
		t.Fatalf("requests=%#v want %#v", requests, want)
	}
}

// The observed plan version is REQUIRED and is sent verbatim: it is the whole
// safety property of the CP7 reopen, and a client that omitted or reshaped it
// would be asking AO to reopen a state nobody looked at.
func TestWorkflowRecoverPlanSendsTheObservedVersionAndRefusesWithoutIt(t *testing.T) {
	cfg := setConfigEnv(t)
	var body map[string]string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodPost && r.URL.Path == "/api/v1/workflows/wf-2/plan/reopen" {
			_ = json.NewDecoder(r.Body).Decode(&body)
			_, _ = io.WriteString(w, `{"workflow":{"id":"wf-2","state":"pending","plan":{"status":"pending","commandStatus":"idle","updatedAt":"2026-08-28T12:00:00Z"}}}`)
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(srv.Close)
	writeRunFileFor(t, cfg, srv)

	const observed = "2026-08-28T11:59:00Z"
	out, errOut, err := executeCLI(t, Deps{ProcessAlive: func(int) bool { return true }},
		"workflow", "recover", "plan", "wf-2", "--observed-plan-updated-at", observed)
	if err != nil {
		t.Fatalf("recover plan failed: %v stderr=%s", err, errOut)
	}
	if body["observedPlanUpdatedAt"] != observed {
		t.Fatalf("observedPlanUpdatedAt = %q, want %q sent verbatim", body["observedPlanUpdatedAt"], observed)
	}
	if !strings.Contains(out, "nothing was adopted") {
		t.Fatalf("output does not say the interrupted planner was discarded:\n%s", out)
	}

	// Omitted, and malformed, are both usage errors rather than a defaulted
	// version — defaulting one would reopen a state nobody read.
	for _, args := range [][]string{
		{"workflow", "recover", "plan", "wf-2"},
		{"workflow", "recover", "plan", "wf-2", "--observed-plan-updated-at", "yesterday"},
	} {
		if _, _, err := executeCLI(t, Deps{ProcessAlive: func(int) bool { return true }}, args...); err == nil {
			t.Fatalf("%v: want a usage error", args)
		}
	}
}
