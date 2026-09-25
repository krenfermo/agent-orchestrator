package cli

import (
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
)

const explorationFixture = `{"runId":"wf-1","projectId":"p1","recorded":true,
"agents":[{"role":"worker","cycle":0,"subjectKind":"session","subjectId":"s1","harness":"claude-code","models":["claude-opus-5"],
 "fileReads":{"value":37,"basis":"observed","method":"m"},"uniqueFilesRead":{"value":19,"basis":"observed","method":"m"},
 "searches":{"value":12,"basis":"observed","method":"m"},"exploreCommands":{"value":4,"basis":"derived","method":"m"},
 "modelCalls":{"value":4,"basis":"observed","method":"m"},"inputTokens":{"value":81234,"basis":"observed","method":"m"},
 "harnessContextRatio":{"value":0.25,"basis":"derived","method":"m"},
 "pathScopes":[{"scope":"secret","count":1}],"topFiles":[{"path":"src/app.go","reads":3}]}],
"totals":{"role":"","fileReads":{"value":null,"basis":"unavailable","method":"x"}},
"quality":{"finalState":"completed","verifyPassed":true,"finalReviewVerdict":"approved",
 "fixCycles":{"value":1,"basis":"observed","method":"m"}}}`

func TestWorkflowExplorationPrintsBasisMarkers(t *testing.T) {
	cfg := setConfigEnv(t)
	var requests []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		appendPrimaryRequest(&requests, r)
		if r.Method == http.MethodGet && r.URL.Path == "/api/v1/workflows/wf-1/exploration" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, explorationFixture)
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(srv.Close)
	writeRunFileFor(t, cfg, srv)

	out, errOut, err := executeCLI(t, Deps{ProcessAlive: func(int) bool { return true }}, "workflow", "exploration", "wf-1")
	if err != nil {
		t.Fatalf("exploration failed: %v stderr=%s", err, errOut)
	}
	for _, want := range []string{
		"Agent Exploration -- worker", "Files read", "37", "Unique files", "19", "Searches", "12",
		"~4",       // derived is marked
		"~25.0%",   // derived ratio is marked
		"n/a",      // unavailable is never 0
		"secret=1", // counted, never named
		"src/app.go",
		"Review verdict", "approved",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("output missing %q:\n%s", want, out)
		}
	}
	if !reflect.DeepEqual(requests, []string{"GET /api/v1/workflows/wf-1/exploration"}) {
		t.Fatalf("requests = %v", requests)
	}

	jsonOut, _, err := executeCLI(t, Deps{ProcessAlive: func(int) bool { return true }}, "workflow", "exploration", "wf-1", "--json")
	if err != nil || !strings.Contains(jsonOut, `"runId":"wf-1"`) {
		t.Fatalf("--json must print the daemon response verbatim: err=%v out=%s", err, jsonOut)
	}
}

func TestWorkflowExplorationRequiresAnID(t *testing.T) {
	_, _, err := executeCLI(t, Deps{}, "workflow", "exploration")
	if err == nil {
		t.Fatal("missing workflow id must be a usage error")
	}
}
