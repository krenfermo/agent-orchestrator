package cli

import (
	"reflect"
	"strings"
	"testing"
)

// memory_graph_test.go — the code graph's operator commands, against a stub
// daemon.
//
// What these assert is what the commands exist for. An operator has to be able
// to tell, from the output alone: which backend is serving this graph (and that
// AO is not claiming a third-party one it does not have), whether the graph can
// currently be relied on, and -- for a sync -- whether it did a file's work or a
// repository's.

func TestMemoryGraphStatusNamesTheBackendAndTheSize(t *testing.T) {
	cfg := setConfigEnv(t)
	var requests []string
	srv := memoryServer(t, &requests, map[string]string{
		"GET /api/v1/projects/p1/memory/graph": `{"repositories":[{"repoId":"repo_abc","repoPath":"/checkout/app",` +
			`"backend":"local","generation":4,"phase":"idle","indexedCommit":"abc123def456",` +
			`"files":812,"symbols":9134,"edges":21044,"lastSyncKind":"incremental",` +
			`"filesParsed":2,"filesReused":810,"filesRemoved":0,"lastMillis":41,"healthy":true,` +
			`"architecture":"Project structure (812 files, 9134 symbols)\n"}]}`,
	})
	writeRunFileFor(t, cfg, srv)

	out, errOut, err := executeCLI(t, Deps{ProcessAlive: func(int) bool { return true }},
		"memory", "graph", "status", "p1")
	if err != nil {
		t.Fatalf("memory graph status failed: %v stderr=%s", err, errOut)
	}
	for _, want := range []string{
		"/checkout/app",
		"backend:     local",
		"healthy, generation 4 at abc123def456",
		"812 files, 9134 symbols, 21044 relations",
		"incremental, 2 parsed / 810 reused / 0 removed, 41ms",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output is missing %q:\n%s", want, out)
		}
	}
	// The architecture summary is opt-in: it is several hundred bytes and an
	// operator checking health does not want it every time.
	if strings.Contains(out, "Project structure") {
		t.Errorf("the architecture summary was printed without --architecture:\n%s", out)
	}
	if want := []string{"GET /api/v1/projects/p1/memory/graph"}; !reflect.DeepEqual(requests, want) {
		t.Fatalf("requests=%#v want %#v", requests, want)
	}
}

// A drifted graph must read as "cannot be relied on", not as a row of healthy
// counts. The rows are intact; what is missing is the proof they describe the
// checkout.
func TestMemoryGraphStatusExplainsDrift(t *testing.T) {
	cfg := setConfigEnv(t)
	var requests []string
	srv := memoryServer(t, &requests, map[string]string{
		"GET /api/v1/projects/p1/memory/graph": `{"repositories":[{"repoId":"repo_abc","repoPath":"/checkout/app",` +
			`"backend":"local","generation":4,"phase":"idle","indexedCommit":"abc123",` +
			`"files":10,"symbols":90,"edges":100,"healthy":false,` +
			`"drift":"the checkout is at def456789012 and the graph was indexed at abc123; run a sync"}]}`,
	})
	writeRunFileFor(t, cfg, srv)

	out, _, err := executeCLI(t, Deps{ProcessAlive: func(int) bool { return true }},
		"memory", "graph", "status", "p1")
	if err != nil {
		t.Fatalf("memory graph status failed: %v", err)
	}
	for _, want := range []string{"not servable", "drift:       the checkout is at def456789012"} {
		if !strings.Contains(out, want) {
			t.Errorf("output is missing %q:\n%s", want, out)
		}
	}
}

func TestMemoryGraphStatusOnAProjectWithNoGraphSaysWhatToDo(t *testing.T) {
	cfg := setConfigEnv(t)
	var requests []string
	srv := memoryServer(t, &requests, map[string]string{
		"GET /api/v1/projects/p1/memory/graph": `{"repositories":[]}`,
	})
	writeRunFileFor(t, cfg, srv)

	out, _, err := executeCLI(t, Deps{ProcessAlive: func(int) bool { return true }},
		"memory", "graph", "status", "p1")
	if err != nil {
		t.Fatalf("memory graph status failed: %v", err)
	}
	if !strings.Contains(out, "ao memory graph sync") {
		t.Errorf("an empty status did not say how to build one:\n%s", out)
	}
}

func TestMemoryGraphSyncReportsParsedAgainstReused(t *testing.T) {
	cfg := setConfigEnv(t)
	var requests []string
	srv := memoryServer(t, &requests, map[string]string{
		"POST /api/v1/projects/p1/memory/graph/sync": `{"repoId":"repo_abc","repoPath":"/checkout/app",` +
			`"kind":"incremental","generation":4,"indexedCommit":"def456","filesScanned":2,"filesParsed":1,` +
			`"filesReused":1,"filesRemoved":0,"symbolsAdded":6,"symbolsRemoved":5,"edgesAdded":12,` +
			`"edgesRemoved":11,"files":812,"symbols":9135,"edges":21045,"millis":37}`,
	})
	writeRunFileFor(t, cfg, srv)

	out, errOut, err := executeCLI(t, Deps{ProcessAlive: func(int) bool { return true }},
		"memory", "graph", "sync", "p1")
	if err != nil {
		t.Fatalf("memory graph sync failed: %v stderr=%s", err, errOut)
	}
	for _, want := range []string{
		"sync:        incremental, generation 4 at def456 (37ms)",
		"files:       2 scanned, 1 parsed, 1 reused, 0 removed",
		"symbols:     +6 / -5    relations: +12 / -11",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output is missing %q:\n%s", want, out)
		}
	}
	if want := []string{"POST /api/v1/projects/p1/memory/graph/sync"}; !reflect.DeepEqual(requests, want) {
		t.Fatalf("requests=%#v want %#v", requests, want)
	}
}

func TestMemoryGraphSyncPassesTheFullFlagThrough(t *testing.T) {
	cfg := setConfigEnv(t)
	var requests []string
	srv := memoryServer(t, &requests, map[string]string{
		"POST /api/v1/projects/p1/memory/graph/sync": `{"repoId":"r","kind":"full","generation":5}`,
	})
	writeRunFileFor(t, cfg, srv)

	if _, errOut, err := executeCLI(t, Deps{ProcessAlive: func(int) bool { return true }},
		"memory", "graph", "sync", "p1", "--full", "--repo", "/checkout/app"); err != nil {
		t.Fatalf("memory graph sync --full failed: %v stderr=%s", err, errOut)
	}
	if len(requests) != 1 || !strings.Contains(requests[0], "graph/sync") {
		t.Fatalf("requests=%#v", requests)
	}
}

func TestMemoryGraphQueryPrintsWhatADispatchWouldBeTold(t *testing.T) {
	cfg := setConfigEnv(t)
	var requests []string
	srv := memoryServer(t, &requests, map[string]string{
		"GET /api/v1/projects/p1/memory/graph/query": `{"repoId":"repo_abc","generation":4,` +
			`"indexedCommit":"abc123","symbols":[{"id":"internal/service/records.go#method:Records.MayExport",` +
			`"name":"Records.MayExport","kind":"method","path":"internal/service/records.go","line":22,` +
			`"signature":"(role Role) bool","summary":"method Records.MayExport(role Role) bool — MayExport decides whether a role may export records.",` +
			`"exported":true,"score":41.5,"reason":"name matches the objective"}],` +
			`"tests":[{"name":"TestRecordsMayExport","path":"internal/service/records_test.go","line":6}],` +
			`"endpoints":[{"name":"POST /api/records/export","path":"internal/api/routes.go","line":14}],` +
			`"callers":[{"kind":"call","from":"internal/service/records.go#method:Records.Export","to":"MayExport"}],` +
			`"callees":[],"tables":["record_exports"],"files":["internal/service/records.go"],` +
			`"consideredSymbols":312,"consideredEdges":90,"truncated":true}`,
	})
	writeRunFileFor(t, cfg, srv)

	out, errOut, err := executeCLI(t, Deps{ProcessAlive: func(int) bool { return true }},
		"memory", "graph", "query", "p1", "export", "permissions", "supervisor")
	if err != nil {
		t.Fatalf("memory graph query failed: %v stderr=%s", err, errOut)
	}
	for _, want := range []string{
		"internal/service/records.go:22",
		"MayExport decides whether a role may export records.",
		"selected because: name matches the objective",
		"covered by:",
		"TestRecordsMayExport",
		"reached from HTTP:",
		"POST /api/records/export",
		"tables touched: record_exports",
		"1 symbols selected from 312 considered (bounded; the graph holds more)",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output is missing %q:\n%s", want, out)
		}
	}
	if len(requests) != 1 || !strings.Contains(requests[0], "graph/query") {
		t.Fatalf("requests=%#v", requests)
	}
}

func TestMemoryGraphQueryWithoutAQuestionIsAUsageError(t *testing.T) {
	cfg := setConfigEnv(t)
	var requests []string
	srv := memoryServer(t, &requests, map[string]string{})
	writeRunFileFor(t, cfg, srv)

	_, _, err := executeCLI(t, Deps{ProcessAlive: func(int) bool { return true }},
		"memory", "graph", "query", "p1")
	if err == nil {
		t.Fatal("a query with no symbol, path or terms was accepted")
	}
	if ExitCode(err) != 2 {
		t.Fatalf("exit code = %d, want 2 for CLI misuse", ExitCode(err))
	}
	if len(requests) != 0 {
		t.Fatalf("a usage error still reached the daemon: %#v", requests)
	}
}

func TestMemoryGraphQueryOnAnUnbuiltGraphExplainsItself(t *testing.T) {
	cfg := setConfigEnv(t)
	var requests []string
	srv := memoryServer(t, &requests, map[string]string{
		"GET /api/v1/projects/p1/memory/graph/query": `{"repoId":"repo_abc","symbols":[],` +
			`"reason":"this repository's code graph has not been built yet; run ` + "`ao memory graph sync`" + `"}`,
	})
	writeRunFileFor(t, cfg, srv)

	out, _, err := executeCLI(t, Deps{ProcessAlive: func(int) bool { return true }},
		"memory", "graph", "query", "p1", "export")
	if err != nil {
		t.Fatalf("memory graph query failed: %v", err)
	}
	if !strings.Contains(out, "has not been built yet") {
		t.Errorf("an unbuilt graph did not explain itself:\n%s", out)
	}
}
