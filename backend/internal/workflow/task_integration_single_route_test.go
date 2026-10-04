package workflow

// The reviewer's second finding, guarded structurally rather than by example.
//
// "Direct branch also goes through the Coordinator" is not a behaviour one test
// can pin down, because the failure mode is the EXISTENCE of a second way in,
// not any particular thing it does. A parallel route can be added again in one
// afternoon and every behavioural test in this package would still pass — the
// old one did exactly that, and stayed correct-looking for a whole checkpoint
// while it drifted out of sync with the lane, the gate and the audit.
//
// So this reads the package's own source. It is a coarse instrument and
// deliberately so: it asserts the shape the architecture rule is about (one
// entry point, one place that constructs a Coordinator, no survivors of the
// route that was removed) and nothing finer.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// theOneIntegrationFile is where every integration must be initiated. Both
// modes live in it, next to each other, which is the point: two functions in
// one file share the lane, the gate, the outcome handling and the audit by
// construction rather than by anyone remembering to keep them in step.
const theOneIntegrationFile = "task_integration_route.go"

func workflowSources(t *testing.T) map[string]string {
	t.Helper()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]string{}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		body, err := os.ReadFile(filepath.Clean(name))
		if err != nil {
			t.Fatal(err)
		}
		out[name] = string(body)
	}
	return out
}

// Exactly one file may build an Integration Coordinator, and exactly one
// function may be the way in.
func TestThereIsOnlyOneIntegrationRoute(t *testing.T) {
	sources := workflowSources(t)

	for name, body := range sources {
		if name == theOneIntegrationFile {
			continue
		}
		if strings.Contains(body, "integration.New(") {
			t.Errorf("%s constructs an Integration Coordinator; every integration must be initiated from %s", name, theOneIntegrationFile)
		}
		if strings.Contains(body, ".Integrate(ctx") {
			t.Errorf("%s calls Integrate directly; every integration must be initiated from %s", name, theOneIntegrationFile)
		}
	}

	// And promotion reaches it through the single entry point rather than
	// forking on execution mode itself.
	promotion := sources["master_integration.go"]
	if !strings.Contains(promotion, "c.integrateReadyTask(") {
		t.Fatal("promoteTaskToIntegration no longer routes through integrateReadyTask")
	}
	if strings.Contains(promotion, "promoteDirectBranchTask") {
		t.Fatal("the legacy direct-branch promotion route is referenced again")
	}
}

// The route that was removed stays removed. MaterializeIntegrationCommit was
// the old isolated-worktree path's whole mechanism, and the direct-branch
// adapter refuses it by design — a caller that reaches for it again is either
// resurrecting the content-materialization route or is about to fail in
// direct-branch mode exactly as the original incident did.
func TestTheMaterializeRouteIsGone(t *testing.T) {
	for name, body := range workflowSources(t) {
		if strings.Contains(body, ".MaterializeIntegrationCommit(") {
			t.Errorf("%s still calls MaterializeIntegrationCommit; that integration route was removed", name)
		}
		if strings.Contains(body, "func (c *Coordinator) promoteDirectBranchTask") {
			t.Errorf("%s re-declares the legacy direct-branch promotion", name)
		}
	}
}
