package ports_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// runtime_destroy_boundary_test.go — P1-E §Q: the Runtime.Destroy ABA boundary.
//
// P1-D documented that Runtime.Destroy(session name) is ABA-unsafe while
// production safety rests on DestroyInstance, and left the footgun open. The
// primitive itself cannot be made safe — a name is reusable, so the check and
// the destroy are always two moments — and removing it would be the broad
// runtime API redesign §Q rules out.
//
// What CAN be closed is the operational hazard: proving that no path which
// makes an OWNERSHIP decision reaches for the name-addressed call. Those paths
// are exactly the ones that look at a runtime AO did not just create and decide
// whether it is AO's to remove:
//
//	internal/runtimegc   the sweeper
//	internal/workflow    the capacity scheduler, recovery, and placement GC
//	internal/lifecycle   the reaper
//
// This test walks their source and fails on a call to `.Destroy(` against a
// runtime. It is a static check on purpose: the hazard is that somebody ADDS
// such a call, and a behavioural test only covers the paths it happens to
// drive.
//
// It deliberately does not police internal/session_manager or internal/review.
// Their destroys are teardown-then-recreate within AO's own handle namespace,
// where destroying a stranger that had taken the name is the same outcome as
// the Create that immediately follows — not an ownership decision. Widening the
// rule to them would be the redesign, and stating the boundary is the point.
func TestOwnershipSensitivePathsNeverUseNameOnlyDestroy(t *testing.T) {
	ownershipSensitive := []string{
		"../runtimegc",
		"../workflow",
		"../lifecycle",
	}

	var violations []string
	for _, pkg := range ownershipSensitive {
		root, err := filepath.Abs(pkg)
		if err != nil {
			t.Fatalf("resolve %s: %v", pkg, err)
		}
		if _, err := os.Stat(root); err != nil {
			t.Fatalf("ownership-sensitive package %s is not where this test expects it: %v", pkg, err)
		}
		violations = append(violations, nameOnlyDestroyCalls(t, root)...)
	}

	if len(violations) > 0 {
		t.Fatalf("ownership-sensitive code calls the ABA-unsafe name-addressed Destroy; "+
			"use SessionFactsReader.DestroyInstance, which names one incarnation:\n  %s",
			strings.Join(violations, "\n  "))
	}
}

// nameOnlyDestroyCalls reports every `x.Destroy(...)` selector call in a
// package's non-test source.
//
// It matches the SELECTOR rather than a resolved type, which is the right
// trade for a boundary guard: a receiver named anything at all still trips it,
// and the two legitimate non-runtime Destroys in the tree (workspace and
// workspace-project) live in packages this test does not walk. A false positive
// here costs one line of thought; a false negative costs a killed session
// somebody else owned.
func nameOnlyDestroyCalls(t *testing.T, root string) []string {
	t.Helper()
	var out []string
	fset := token.NewFileSet()
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		file, perr := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
		if perr != nil {
			return perr
		}
		ast.Inspect(file, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok || sel.Sel.Name != "Destroy" {
				return true
			}
			position := fset.Position(call.Pos())
			out = append(out, filepath.Base(filepath.Dir(path))+"/"+filepath.Base(path)+":"+
				strconv.Itoa(position.Line))
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}
	return out
}
