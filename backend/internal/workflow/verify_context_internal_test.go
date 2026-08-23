package workflow

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeTree materializes a worktree layout: keys are worktree-relative file
// paths, values their contents.
func writeTree(t *testing.T, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	for rel, content := range files {
		p := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func goCheck(dir string, args ...string) VerificationCommandCheck {
	return VerificationCommandCheck{Command: "go", Args: args, WorkingDirectory: dir, RetrySafe: true}
}

func TestResolveVerifyCommandContext(t *testing.T) {
	for _, tc := range []struct {
		name         string
		files        map[string]string
		check        VerificationCommandCheck
		wantDir      string
		wantResolved bool
		wantArgs     []string
		wantErr      error
	}{
		{
			// The real incident: repo root is not the module root.
			name:         "multi-part repo resolves to the module root",
			files:        map[string]string{"backend/go.mod": "module x\n", "frontend/package.json": "{}"},
			check:        goCheck("", "build", "./..."),
			wantDir:      "backend",
			wantResolved: true,
		},
		{
			name:    "root module keeps the configured root",
			files:   map[string]string{"go.mod": "module x\n", "internal/a/a.go": "package a"},
			check:   goCheck("", "build", "./..."),
			wantDir: "",
		},
		{
			name:    "explicit valid working directory is preserved",
			files:   map[string]string{"backend/go.mod": "module x\n", "backend/internal/a/a.go": "package a"},
			check:   goCheck("backend/internal/a", "test", "./..."),
			wantDir: "backend/internal/a",
		},
		{
			name:    "explicit module root is preserved",
			files:   map[string]string{"backend/go.mod": "module x\n"},
			check:   goCheck("backend", "vet", "./..."),
			wantDir: "backend",
		},
		{
			name:    "non-Go commands are never moved",
			files:   map[string]string{"backend/go.mod": "module x\n"},
			check:   VerificationCommandCheck{Command: "npm", Args: []string{"run", "build"}},
			wantDir: "",
		},
		{
			name:    "multiple module roots are ambiguous, never guessed",
			files:   map[string]string{"backend/go.mod": "module x\n", "tools/go.mod": "module y\n"},
			check:   goCheck("", "build", "./..."),
			wantErr: errAmbiguousModuleRoot,
		},
		{
			name:    "no module root at all",
			files:   map[string]string{"frontend/package.json": "{}"},
			check:   goCheck("", "build", "./..."),
			wantErr: errNoModuleRoot,
		},
		{
			name: "a go.work covering several modules is the module context",
			files: map[string]string{
				"go.work": "go 1.22\nuse ./backend\nuse ./tools\n",
				// no root go.mod: the workspace file alone is the context
				"backend/go.mod": "module x\n", "tools/go.mod": "module y\n",
			},
			check:   goCheck("", "test", "./..."),
			wantDir: "",
		},
		{
			name:  "vendor and testdata modules are never selected",
			files: map[string]string{"backend/go.mod": "module x\n", "backend/vendor/dep/go.mod": "module dep\n", "testdata/fixture/go.mod": "module f\n"},
			check: goCheck("", "build", "./..."),
			// backend is the only real candidate; vendor/testdata are skipped
			// entirely, so this is a unique resolution rather than ambiguity.
			wantDir:      "backend",
			wantResolved: true,
		},
		{
			// Scope narrowing produced a worktree-root-relative package path;
			// moving into the module root must rebase it or the command would
			// name a package that does not exist there.
			name:         "scope-narrowed package path is rebased onto the module root",
			files:        map[string]string{"backend/go.mod": "module x\n", "backend/internal/foo/foo.go": "package foo"},
			check:        goCheck("", "test", "./backend/internal/foo/..."),
			wantDir:      "backend",
			wantResolved: true,
			wantArgs:     []string{"test", "./internal/foo/..."},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := writeTree(t, tc.files)
			got, resolution, err := resolveVerifyCommandContext(root, tc.check, false)
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("err = %v, want %v", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected err: %v", err)
			}
			if got.WorkingDirectory != tc.wantDir {
				t.Fatalf("workingDirectory = %q, want %q", got.WorkingDirectory, tc.wantDir)
			}
			if (resolution != nil) != tc.wantResolved {
				t.Fatalf("resolution = %+v, wantResolved=%v", resolution, tc.wantResolved)
			}
			if tc.wantArgs != nil && strings.Join(got.Args, " ") != strings.Join(tc.wantArgs, " ") {
				t.Fatalf("args = %v, want %v", got.Args, tc.wantArgs)
			}
		})
	}
}

// TestResolveVerifyCommandContextForcedSearchExcludesTheFailingDirectory is the
// self-heal search: a directory that has a go.mod and still made the toolchain
// say "does not contain main module" has proved it is not the right root, so
// the forced search must look past it instead of returning it again.
func TestResolveVerifyCommandContextForcedSearchExcludesTheFailingDirectory(t *testing.T) {
	root := writeTree(t, map[string]string{
		"go.mod":         "module placeholder\n",
		"backend/go.mod": "module x\n",
	})
	check := goCheck("", "build", "./...")
	if _, resolution, err := resolveVerifyCommandContext(root, check, false); err != nil || resolution != nil {
		t.Fatalf("pre-flight moved a command already inside a module: %+v (%v)", resolution, err)
	}
	got, resolution, err := resolveVerifyCommandContext(root, check, true)
	if err != nil {
		t.Fatalf("forced search: %v", err)
	}
	if resolution == nil || got.WorkingDirectory != "backend" {
		t.Fatalf("forced search resolved to %q (%+v), want backend", got.WorkingDirectory, resolution)
	}
}

func TestClassifyVerifyExecutionFailure(t *testing.T) {
	check := goCheck("", "build", "./...")
	for _, tc := range []struct {
		name      string
		exec      VerifyCommandExecution
		runErr    error
		wantNil   bool
		wantKind  VerifyInfraKind
		transient bool
		repair    bool
	}{
		{
			name:     "wrong module root",
			exec:     VerifyCommandExecution{ExitCode: 1, StderrTail: "pattern ./...: directory prefix . does not contain main module or its selected dependencies"},
			wantKind: VerifyInfraWrongModuleRoot,
			repair:   true,
		},
		{
			name:     "go.mod not found",
			exec:     VerifyCommandExecution{ExitCode: 1, StderrTail: "go: go.mod file not found in current directory or any parent directory; see 'go help modules'"},
			wantKind: VerifyInfraWrongModuleRoot,
			repair:   true,
		},
		{
			name:     "binary missing is not a code failure",
			runErr:   errors.New(`exec: "go": executable file not found in $PATH`),
			wantKind: VerifyInfraToolUnavailable,
		},
		{
			name:     "invalid verifier configuration",
			exec:     VerifyCommandExecution{ExitCode: 2, StderrTail: "flag provided but not defined: -nonsense"},
			wantKind: VerifyInfraConfigInvalid,
		},
		{
			name:      "transient host failure",
			exec:      VerifyCommandExecution{ExitCode: 1, StderrTail: "fork/exec: resource temporarily unavailable"},
			wantKind:  VerifyInfraRuntimeFailure,
			transient: true,
		},
		{
			name:    "a genuinely failing test is a code failure",
			exec:    VerifyCommandExecution{ExitCode: 1, StdoutTail: "--- FAIL: TestThing (0.00s)\nFAIL\tgithub.com/x/y\t0.2s"},
			wantNil: true,
		},
		{
			// The mirror-image false positive: a failing test that prints an OS
			// error about its own fixture is still the code's problem.
			name:    "a test complaining about a missing fixture is a code failure",
			exec:    VerifyCommandExecution{ExitCode: 1, StdoutTail: "--- FAIL: TestFixture\n    open testdata/x.json: no such file or directory"},
			wantNil: true,
		},
		{
			name:    "a compile error is a code failure",
			exec:    VerifyCommandExecution{ExitCode: 1, StderrTail: "./main.go:7:2: undefined: doesNotExist"},
			wantNil: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := classifyVerifyExecutionFailure(check, ".", tc.exec, tc.runErr)
			if tc.wantNil {
				if got != nil {
					t.Fatalf("classified a code failure as infrastructure: %+v", got)
				}
				return
			}
			if got == nil {
				t.Fatal("infrastructure failure classified as a code failure")
			}
			if got.Kind != tc.wantKind {
				t.Fatalf("kind = %q, want %q", got.Kind, tc.wantKind)
			}
			if got.Transient != tc.transient || got.Repairable != tc.repair {
				t.Fatalf("transient=%v repairable=%v, want %v/%v", got.Transient, got.Repairable, tc.transient, tc.repair)
			}
			if got.Detail == "" {
				t.Fatal("classification carries no detail; the stop reason would be unactionable")
			}
		})
	}
}

// TestInfraFailuresAreNeverRepairableByAFixWorker locks the invariant the
// incident violated: no infrastructure kind may map onto a stop reason that a
// fix cycle would answer.
func TestInfraFailuresAreNeverRepairableByAFixWorker(t *testing.T) {
	for _, kind := range []VerifyInfraKind{
		VerifyInfraWrongModuleRoot, VerifyInfraToolUnavailable,
		VerifyInfraConfigInvalid, VerifyInfraRuntimeFailure,
	} {
		reason := infraAttentionReason(kind)
		disp, ok := attentionDispositions[reason]
		if !ok {
			t.Fatalf("%s maps to %q, which is not in the canonical attention vocabulary", kind, reason)
		}
		if disp.HumanAction == "" {
			t.Fatalf("%s maps to %q with no human action", kind, reason)
		}
		if reason == ReasonVerifyBudgetExhausted || reason == ReasonFixNoVerifiableChange {
			t.Fatalf("%s is reported as a fix-cycle outcome", kind)
		}
	}
}
