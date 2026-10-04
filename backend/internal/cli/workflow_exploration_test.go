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
"contextSources":{"recorded":true,"memoryMode":"assisted","contextRouter":"off"},
"memoryPacks":[{"role":"worker","packDigest":"abcdef0123456789","indexedCommit":"6c33d0d17f48","itemCount":5,"selectedBytes":900,"estimatedTokens":225}],
"agents":[{"role":"worker","cycle":0,"subjectKind":"session","subjectId":"s1","harness":"claude-code","models":["claude-opus-5"],
 "fileReads":{"value":37,"basis":"observed","method":"m"},"uniqueFilesRead":{"value":19,"basis":"observed","method":"m"},
 "searches":{"value":12,"basis":"observed","method":"m","lowerBound":true},
 "explorationOpsAll":{"value":53,"basis":"derived","method":"m"},
 "toolCoverage":{"complete":true,"reason":"","extractorVersions":[1]},
 "turnMix":{"basis":"observed","counts":[{"class":"read","count":3},{"class":"edit","count":1}]},"exploreCommands":{"value":4,"basis":"derived","method":"m"},
 "modelCalls":{"value":4,"basis":"observed","method":"m"},"inputTokens":{"value":81234,"basis":"observed","method":"m"},
 "harnessContextRatio":{"value":0.25,"basis":"derived","method":"m"},
 "pathScopes":[{"scope":"secret","count":1}],"topFiles":[{"path":"src/app.go","reads":3}]}],
"totals":{"role":"","fileReads":{"value":null,"basis":"unavailable","method":"x"},
 "toolCoverage":{"complete":false,"reason":"transcript source 7 was ingested without the 3C extractor"}},
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
		">=12",                       // a lower bound is marked
		"~53",                        // M3
		"memory=assisted",            // the frozen arm
		"digest abcdef012345",        // the pack it got
		"read=3",                     // turn mix
		"Tool telemetry unavailable", // uncovered transcript is said, not zeroed
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
