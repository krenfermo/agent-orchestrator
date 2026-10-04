package cli

import (
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
)

// memory_test.go — P2-A's operator commands, against a stub daemon.
//
// What these assert is the property the commands exist for: an operator can
// tell, from the output alone, whether AO's memory of a project can currently
// be relied on and why not. A `memory status` that printed counts without the
// phase and the failure reason would look fine and diagnose nothing.

func memoryServer(t *testing.T, requests *[]string, routes map[string]string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		appendPrimaryRequest(requests, r)
		w.Header().Set("Content-Type", "application/json")
		key := r.Method + " " + r.URL.Path
		if body, ok := routes[key]; ok {
			_, _ = io.WriteString(w, body)
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestMemoryStatusPrintsProvenanceAndCensus(t *testing.T) {
	cfg := setConfigEnv(t)
	var requests []string
	srv := memoryServer(t, &requests, map[string]string{
		"GET /api/v1/projects/p1/memory": `{"repositories":[{"repoId":"repo_abc","repoPath":"/checkout/app",` +
			`"phase":"idle","generation":7,"indexedCommit":"abc123","branch":"main","healthy":true,` +
			`"items":40,"valid":37,"stale":2,"invalidated":1,"rebuilding":0,"taskLocal":3,"relations":55,` +
			`"filesIndexed":12,"filesSkipped":300,"lastIndexedAt":"2026-08-31T10:00:00.000Z",` +
			`"lastUpdatedAt":"2026-08-31T10:00:00.000Z"}]}`,
	})
	writeRunFileFor(t, cfg, srv)

	out, errOut, err := executeCLI(t, Deps{ProcessAlive: func(int) bool { return true }}, "memory", "status", "p1")
	if err != nil {
		t.Fatalf("memory status failed: %v stderr=%s", err, errOut)
	}
	for _, want := range []string{
		"/checkout/app",
		"generation:  7 (usable)",
		"abc123 on main",
		"37 valid, 2 stale, 1 invalidated",
		"3 task-local",
		"12 files indexed, 300 unchanged and skipped",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output is missing %q:\n%s", want, out)
		}
	}
	if want := []string{"GET /api/v1/projects/p1/memory"}; !reflect.DeepEqual(requests, want) {
		t.Fatalf("requests=%#v want %#v", requests, want)
	}
}

// A failed pass must be legible as "this memory is not being served", not as a
// row of counts an operator reads as healthy.
func TestMemoryStatusExplainsAFailedPass(t *testing.T) {
	cfg := setConfigEnv(t)
	var requests []string
	srv := memoryServer(t, &requests, map[string]string{
		"GET /api/v1/projects/p1/memory": `{"repositories":[{"repoId":"repo_abc","repoPath":"/checkout/app",` +
			`"phase":"scanning","generation":3,"indexedCommit":"","resumeCursor":"internal/workflow/plan.go",` +
			`"lastError":"disk full","healthy":false,"items":0,"valid":0}]}`,
	})
	writeRunFileFor(t, cfg, srv)

	out, _, err := executeCLI(t, Deps{ProcessAlive: func(int) bool { return true }}, "memory", "status", "p1")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"not vouched for",
		"resume point: internal/workflow/plan.go",
		"disk full",
		"NOT being served to agents",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output is missing %q:\n%s", want, out)
		}
	}
}

func TestMemoryStatusOnAnUnindexedProjectSaysWhatToDo(t *testing.T) {
	cfg := setConfigEnv(t)
	var requests []string
	srv := memoryServer(t, &requests, map[string]string{
		"GET /api/v1/projects/p1/memory": `{"repositories":[]}`,
	})
	writeRunFileFor(t, cfg, srv)

	out, _, err := executeCLI(t, Deps{ProcessAlive: func(int) bool { return true }}, "memory", "status", "p1")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "has been indexed yet") || !strings.Contains(out, "ao memory rebuild p1") {
		t.Fatalf("output does not tell an operator what to do:\n%s", out)
	}
}

// `memory inspect` is the surface that shows what AO can no longer vouch for.
// The stale marker and the reason are the whole point of it.
func TestMemoryInspectMarksAndExplainsNonValidFacts(t *testing.T) {
	cfg := setConfigEnv(t)
	var requests []string
	srv := memoryServer(t, &requests, map[string]string{
		"GET /api/v1/projects/p1/memory/items": `{"repoId":"repo_abc","items":[` +
			`{"type":"convention","scope":"repository","key":"AGENTS.md#hard-rules","origin":"canonical",` +
			`"summary":"AGENTS.md: Hard rules","state":"valid","confidence":0.95,"generation":4},` +
			`{"type":"module","scope":"module","key":"internal/workflow","origin":"canonical",` +
			`"summary":"internal/workflow — 40 files","state":"stale","stateReason":"source content moved",` +
			`"confidence":0.85,"generation":4},` +
			`{"type":"task_result","scope":"task","key":"t1","origin":"task_local","originRef":"t1",` +
			`"summary":"t1 — added the queue","state":"valid","confidence":0.95,"generation":0}` +
			`],"total":3,"truncated":false}`,
	})
	writeRunFileFor(t, cfg, srv)

	out, _, err := executeCLI(t, Deps{ProcessAlive: func(int) bool { return true }},
		"memory", "inspect", "p1", "--state", "stale", "--type", "module", "--path", "internal/", "--limit", "10")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "! module") {
		t.Errorf("a non-valid fact is not marked:\n%s", out)
	}
	if !strings.Contains(out, "reason: source content moved") {
		t.Errorf("the reason a fact is stale is not shown:\n%s", out)
	}
	if !strings.Contains(out, "task-local to t1") {
		t.Errorf("a task-local fact is not labelled as outside the canonical memory:\n%s", out)
	}
	if !strings.Contains(out, "3 facts") {
		t.Errorf("the total is not reported:\n%s", out)
	}

	// Every filter must reach the daemon: a flag that is accepted and dropped
	// would silently show the operator a different set than they asked for.
	if len(requests) != 1 {
		t.Fatalf("requests = %#v", requests)
	}
	for _, want := range []string{"state=stale", "type=module", "path=internal", "limit=10"} {
		if !strings.Contains(requests[0], want) {
			t.Errorf("request %q does not carry %q", requests[0], want)
		}
	}
}

func TestMemoryRebuildReportsWhatThePassDid(t *testing.T) {
	cfg := setConfigEnv(t)
	var requests []string
	srv := memoryServer(t, &requests, map[string]string{
		"POST /api/v1/projects/p1/memory/rebuild": `{"repoId":"repo_abc","generation":8,"skipped":false,` +
			`"filesIndexed":120,"filesSkipped":3000,"itemsWritten":9,"itemsReconfirmed":540,"itemsRetired":2,` +
			`"indexedCommit":"def456","truncated":true,"truncatedReason":"stopped at the 6000-file bound"}`,
	})
	writeRunFileFor(t, cfg, srv)

	out, _, err := executeCLI(t, Deps{ProcessAlive: func(int) bool { return true }},
		"memory", "rebuild", "p1", "--purge")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"generation 8 at def456",
		"120 files indexed, 3000 unchanged and skipped",
		"9 facts written, 540 reconfirmed unchanged, 2 retired",
		"bounded: stopped at the 6000-file bound",
		"covers less than the whole repository",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output is missing %q:\n%s", want, out)
		}
	}
}

// A rebuild the daemon declined is a normal outcome, and must be reported as
// one rather than as a silent success.
func TestMemoryRebuildReportsASkip(t *testing.T) {
	cfg := setConfigEnv(t)
	var requests []string
	srv := memoryServer(t, &requests, map[string]string{
		"POST /api/v1/projects/p1/memory/rebuild": `{"repoId":"repo_abc","skipped":true,` +
			`"skipReason":"another indexing pass holds this repository"}`,
	})
	writeRunFileFor(t, cfg, srv)

	out, _, err := executeCLI(t, Deps{ProcessAlive: func(int) bool { return true }}, "memory", "rebuild", "p1")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "Skipped: another indexing pass holds this repository") {
		t.Fatalf("a declined rebuild was not reported:\n%s", out)
	}
}

// With no --path the command runs drift detection, and the output distinguishes
// "checked and found nothing" from "did not check".
func TestMemoryInvalidateWithoutPathsReportsTheDriftCheck(t *testing.T) {
	cfg := setConfigEnv(t)
	var requests []string
	srv := memoryServer(t, &requests, map[string]string{
		"POST /api/v1/projects/p1/memory/invalidate": `{"repoId":"repo_abc","itemsInvalidated":2,` +
			`"driftChecked":37,"driftFound":2}`,
	})
	writeRunFileFor(t, cfg, srv)

	out, _, err := executeCLI(t, Deps{ProcessAlive: func(int) bool { return true }}, "memory", "invalidate", "p1")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "checked 37 facts against their sources; 2 had drifted") {
		t.Errorf("the drift check was not reported:\n%s", out)
	}
	if !strings.Contains(out, "2 facts are no longer served as authoritative") {
		t.Errorf("the outcome was not reported:\n%s", out)
	}
}

func TestMemoryInvalidateWithPathsSkipsTheDriftLine(t *testing.T) {
	cfg := setConfigEnv(t)
	var requests []string
	srv := memoryServer(t, &requests, map[string]string{
		"POST /api/v1/projects/p1/memory/invalidate": `{"repoId":"repo_abc","itemsInvalidated":1}`,
	})
	writeRunFileFor(t, cfg, srv)

	out, _, err := executeCLI(t, Deps{ProcessAlive: func(int) bool { return true }},
		"memory", "invalidate", "p1", "--path", "AGENTS.md", "--reason", "rebased")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, "checked") {
		t.Errorf("an explicit-path invalidation claimed to have run a drift check:\n%s", out)
	}
	if !strings.Contains(out, "1 facts are no longer served as authoritative") {
		t.Errorf("the outcome was not reported:\n%s", out)
	}
}

func TestMemoryCommandsRequireAProjectID(t *testing.T) {
	setConfigEnv(t)
	for _, sub := range []string{"status", "inspect", "rebuild", "invalidate"} {
		if _, _, err := executeCLI(t, Deps{}, "memory", sub); err == nil {
			t.Errorf("ao memory %s accepted no project id", sub)
		}
	}
}

// `ao memory report` is the P2-B operator answer. A warm project must read as
// warm, and the honesty caveat must be on screen rather than only in the docs.
func TestMemoryReportShowsWarmthAndPerRoleCost(t *testing.T) {
	cfg := setConfigEnv(t)
	var requests []string
	srv := memoryServer(t, &requests, map[string]string{
		"GET /api/v1/projects/p1/memory/report": `{"mode":"assisted","cacheEnabled":true,"syncTimeout":"20s",` +
			`"repoId":"repo_abc","repoPath":"/checkout/app","warm":true,"generation":7,"indexedCommit":"abc123",` +
			`"syncKind":"none","syncFilesRead":0,"syncMillis":2,"cacheHits":3,"cacheMisses":1,"roles":[` +
			`{"role":"planner","budgetBytes":24576,"budgetItems":40,"budgetDocuments":4,"packItems":40,` +
			`"packBytes":5981,"estimatedPackTokens":1496,"candidates":467,"rejectedByBudget":427},` +
			`{"role":"worker","budgetBytes":16384,"budgetItems":24,"budgetDocuments":2,"packItems":14,` +
			`"packBytes":15900,"estimatedPackTokens":3975,"candidates":546,"rejectedByBudget":532,` +
			`"reducedToSummary":1,"staleExcluded":2}]}`,
	})
	writeRunFileFor(t, cfg, srv)

	out, errOut, err := executeCLI(t, Deps{ProcessAlive: func(int) bool { return true }}, "memory", "report", "p1")
	if err != nil {
		t.Fatalf("memory report failed: %v stderr=%s", err, errOut)
	}
	for _, want := range []string{
		"mode:        assisted (cache on, sync timeout 20s)",
		"warm at abc123 (generation 7)",
		"last sync:   none, 0 files read",
		"pack cache:  3 hits, 1 misses",
		"planner",
		"40 items / 5981B / ~1496t",
		"427 of 467",
		"(+1 to summary)",
		"2 facts withheld",
		"AO-assembled context only",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output is missing %q:\n%s", want, out)
		}
	}
}

// With memory off the report says so and tells the operator how to turn it on,
// rather than printing an empty table that reads as "warm with nothing in it".
func TestMemoryReportExplainsAnOffMode(t *testing.T) {
	cfg := setConfigEnv(t)
	var requests []string
	srv := memoryServer(t, &requests, map[string]string{
		"GET /api/v1/projects/p1/memory/report": `{"mode":"off","syncKind":"skipped",` +
			`"syncReason":"project memory is switched off","roles":[]}`,
	})
	writeRunFileFor(t, cfg, srv)

	out, _, err := executeCLI(t, Deps{ProcessAlive: func(int) bool { return true }}, "memory", "report", "p1")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "switched off") || !strings.Contains(out, "AO_MEMORY_MODE=assisted") {
		t.Fatalf("the off mode was not explained:\n%s", out)
	}
}
