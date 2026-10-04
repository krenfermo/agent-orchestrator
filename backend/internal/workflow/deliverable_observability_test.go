package workflow

import (
	stdctx "context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

// deliverable_observability_test.go pins the pre-dispatch check that refuses to
// spend a turn on work git could not have shown.
//
// The incident it exists for (MEDUSA, 2026-09-09) is recoverable only by a
// person, and only after the run has already stopped on
// `ambiguous_worker_state`. Every case below is therefore about one of two
// mistakes, and the second is the expensive one:
//
//   - failing to refuse a dispatch whose deliverable is genuinely hidden, or
//   - refusing a dispatch that was fine, which grounds working tasks on a
//     guess. Every "proceed" case here is a guard against the second.

// --- extraction -------------------------------------------------------------

func TestRequiredDeliverables(t *testing.T) {
	tests := []struct {
		name     string
		artifact PlanArtifact
		want     []RequiredDeliverable
	}{
		{
			name: "a verification file check that must exist is a required deliverable",
			artifact: PlanArtifact{
				Verification: VerificationPlan{Files: []VerificationFileCheck{
					{Path: "reports/audit.pdf", Exists: true},
				}},
			},
			want: []RequiredDeliverable{
				{Path: "reports/audit.pdf", Source: DeliverableFromVerification, Declaration: "reports/audit.pdf", Readings: []string{"reports/audit.pdf"}},
			},
		},
		{
			name: "a check asserting ABSENCE is not a deliverable",
			artifact: PlanArtifact{
				Verification: VerificationPlan{Files: []VerificationFileCheck{
					{Path: "dist/stale.js", Exists: false},
				}},
			},
			want: nil,
		},
		{
			name: "a code path named by a criterion is a required deliverable",
			artifact: PlanArtifact{
				AcceptanceCriteria: []string{"backend/internal/foo/bar.go compiles and is covered."},
			},
			want: []RequiredDeliverable{{
				Path:        "backend/internal/foo/bar.go",
				Source:      DeliverableFromCriterion,
				Declaration: "backend/internal/foo/bar.go compiles and is covered.",
				Readings:    []string{"backend/internal/foo/bar.go"},
			}},
		},
		{
			// The incident's own shape: the deliverable is a report, not code,
			// so the scope classifier's code-extension rule would never see it.
			name: "an artifact extension a plan can require is admitted",
			artifact: PlanArtifact{
				AcceptanceCriteria: []string{"The run writes postrunqa/report.csv with one row per finding."},
			},
			want: []RequiredDeliverable{{
				Path:        "postrunqa/report.csv",
				Source:      DeliverableFromCriterion,
				Declaration: "The run writes postrunqa/report.csv with one row per finding.",
				Readings:    []string{"postrunqa/report.csv"},
			}},
		},
		{
			name: "prose that is not a path stays out",
			artifact: PlanArtifact{
				AcceptanceCriteria: []string{
					"Existing tests are not knowingly broken.",
					"Read/write access is unchanged and the input/output contract holds.",
					"No unrelated files are modified.",
				},
			},
			want: nil,
		},
		{
			name: "urls, absolute paths and parent traversal are never repository paths",
			artifact: PlanArtifact{
				AcceptanceCriteria: []string{
					"Fetch https://example.com/report.pdf, copy /etc/hosts and ../../outside/thing.csv.",
				},
			},
			want: nil,
		},
		{
			name: "a verification check outranks a criterion naming the same path",
			artifact: PlanArtifact{
				AcceptanceCriteria: []string{"produces out/report.pdf"},
				Verification: VerificationPlan{Files: []VerificationFileCheck{
					{Path: "out/report.pdf", Exists: true},
				}},
			},
			want: []RequiredDeliverable{
				{Path: "out/report.pdf", Source: DeliverableFromVerification, Declaration: "out/report.pdf", Readings: []string{"out/report.pdf"}},
			},
		},
		{
			name: "the result is sorted and de-duplicated",
			artifact: PlanArtifact{
				AcceptanceCriteria: []string{
					"touch zeta/last.go",
					"touch alpha/first.go",
					"touch alpha/first.go again",
				},
			},
			want: []RequiredDeliverable{
				{Path: "alpha/first.go", Source: DeliverableFromCriterion, Declaration: "touch alpha/first.go", Readings: []string{"alpha/first.go"}},
				{Path: "zeta/last.go", Source: DeliverableFromCriterion, Declaration: "touch zeta/last.go", Readings: []string{"zeta/last.go"}},
			},
		},
		{
			// verifyFile reads a relative path in the namespace its spec's
			// commands run in, and falls back to the root reading. The preflight
			// must ask about both, or it could refuse on the reading verify
			// does not use.
			name: "a verification file check carries the namespace reading verify will use",
			artifact: PlanArtifact{
				Verification: VerificationPlan{
					Commands: []VerificationCommandCheck{{Command: "go test ./...", WorkingDirectory: "backend"}},
					Files:    []VerificationFileCheck{{Path: "out/report.pdf", Exists: true}},
				},
			},
			want: []RequiredDeliverable{{
				Path: "out/report.pdf", Source: DeliverableFromVerification, Declaration: "out/report.pdf",
				Readings: []string{"out/report.pdf", "backend/out/report.pdf"},
			}},
		},
		{
			name:     "the generic artifact every standalone objective gets names nothing",
			artifact: BuildPlanArtifact("proj", "do the thing", "v1"),
			want:     nil,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := RequiredDeliverables(tt.artifact)
			if len(got) == 0 && len(tt.want) == 0 {
				return
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("RequiredDeliverables:\n got %+v\nwant %+v", got, tt.want)
			}
		})
	}
}

// The extraction must be a function of the artifact alone: a check that
// sometimes finds a path and sometimes does not would refuse a dispatch
// non-reproducibly, which is worse than not checking.
func TestRequiredDeliverablesIsDeterministic(t *testing.T) {
	artifact := PlanArtifact{
		AcceptanceCriteria: []string{
			"writes out/report.pdf, docs/notes.md and backend/internal/a/b.go",
			"also touches out/report.pdf",
		},
		Verification: VerificationPlan{Files: []VerificationFileCheck{
			{Path: "docs/notes.md", Exists: true},
		}},
	}
	first := RequiredDeliverables(artifact)
	for i := 0; i < 20; i++ {
		if got := RequiredDeliverables(artifact); !reflect.DeepEqual(got, first) {
			t.Fatalf("run %d differs:\n got %+v\nwant %+v", i, got, first)
		}
	}
}

// --- the policy -------------------------------------------------------------

func TestEvaluateDeliverableObservability(t *testing.T) {
	req := func(paths ...string) []RequiredDeliverable {
		out := make([]RequiredDeliverable, 0, len(paths))
		for _, p := range paths {
			out = append(out, RequiredDeliverable{Path: p, Source: DeliverableFromCriterion, Declaration: "produces " + p})
		}
		return out
	}
	// vf builds contractual deliverables: Verification.Files with Exists true.
	vf := func(paths ...string) []RequiredDeliverable {
		out := make([]RequiredDeliverable, 0, len(paths))
		for _, p := range paths {
			out = append(out, RequiredDeliverable{Path: p, Source: DeliverableFromVerification, Declaration: p, Readings: []string{p}})
		}
		return out
	}
	both := func(sets ...[]RequiredDeliverable) []RequiredDeliverable {
		var out []RequiredDeliverable
		for _, s := range sets {
			out = append(out, s...)
		}
		return out
	}
	ign := func(path, pattern string) IgnoredDeliverable {
		return IgnoredDeliverable{Path: path, RuleSource: ".gitignore", RuleLine: 3, Pattern: pattern}
	}

	tests := []struct {
		name       string
		intent     domain.WorkflowWriteIntent
		required   []RequiredDeliverable
		ignored    []IgnoredDeliverable
		wantReady  bool
		wantHidden []string
	}{
		{
			name:      "an ordinary task whose paths git can see proceeds",
			intent:    domain.WorkflowWriteIntentMutating,
			required:  req("backend/internal/a/b.go"),
			ignored:   nil,
			wantReady: true,
		},
		{
			name:       "the incident: the only required deliverable is ignored",
			intent:     domain.WorkflowWriteIntentMutating,
			required:   req("postrunqa/report.csv"),
			ignored:    []IgnoredDeliverable{ign("postrunqa/report.csv", "postrunqa/")},
			wantReady:  false,
			wantHidden: []string{"postrunqa/report.csv"},
		},
		{
			// The audited BLOCKER. The task requires both files; only the
			// report is declared structurally. The worker's change to
			// src/main.go satisfies the completion classifier, verify reads
			// out/report.pdf from the worktree's filesystem and passes it, and
			// the integration commit (`git add -A`, no -f) drops it. AO would
			// report success with a required deliverable lost.
			name:       "a contractual deliverable ignored beside an observable one refuses",
			intent:     domain.WorkflowWriteIntentMutating,
			required:   both(vf("out/report.pdf"), req("src/main.go")),
			ignored:    []IgnoredDeliverable{ign("out/report.pdf", "out/")},
			wantReady:  false,
			wantHidden: []string{"out/report.pdf"},
		},
		{
			name:       "several contractual deliverables, one ignored, refuses naming only that one",
			intent:     domain.WorkflowWriteIntentMutating,
			required:   vf("docs/a.md", "out/report.pdf", "src/main.go"),
			ignored:    []IgnoredDeliverable{ign("out/report.pdf", "out/")},
			wantReady:  false,
			wantHidden: []string{"out/report.pdf"},
		},
		{
			name:      "several contractual deliverables, all observable, proceeds",
			intent:    domain.WorkflowWriteIntentMutating,
			required:  vf("docs/a.md", "out/report.pdf", "src/main.go"),
			ignored:   nil,
			wantReady: true,
		},
		{
			name:     "several contractual deliverables, all ignored, refuses naming them all",
			intent:   domain.WorkflowWriteIntentMutating,
			required: vf("out/b.pdf", "out/a.pdf"),
			ignored: []IgnoredDeliverable{
				ign("out/b.pdf", "out/"),
				ign("out/a.pdf", "out/"),
			},
			wantReady:  false,
			wantHidden: []string{"out/a.pdf", "out/b.pdf"},
		},
		{
			// A criterion that mentions a build output in passing is a common
			// sentence, not a defect: prose does not carry contractual force,
			// so an ignored path found ONLY in prose, beside an observable one,
			// does not refuse.
			name:      "a prose-only ignored path beside an observable one proceeds",
			intent:    domain.WorkflowWriteIntentMutating,
			required:  req("backend/internal/a/b.go", "dist/app.js"),
			ignored:   []IgnoredDeliverable{ign("dist/app.js", "dist/")},
			wantReady: true,
		},
		{
			name:      "contractual observable plus a prose-only ignored path proceeds",
			intent:    domain.WorkflowWriteIntentMutating,
			required:  both(vf("src/main.go"), req("dist/app.js")),
			ignored:   []IgnoredDeliverable{ign("dist/app.js", "dist/")},
			wantReady: true,
		},
		{
			// The refusal names the contractual path only: the prose mention
			// is observable and was never the problem.
			name:       "contractual ignored plus an observable prose path refuses on the contractual one",
			intent:     domain.WorkflowWriteIntentMutating,
			required:   both(vf("out/report.pdf"), req("docs/notes.md")),
			ignored:    []IgnoredDeliverable{ign("out/report.pdf", "out/")},
			wantReady:  false,
			wantHidden: []string{"out/report.pdf"},
		},
		{
			name:     "contractual and prose paths all ignored refuses naming every one",
			intent:   domain.WorkflowWriteIntentMutating,
			required: both(vf("out/report.pdf"), req("dist/app.js")),
			ignored: []IgnoredDeliverable{
				ign("out/report.pdf", "out/"),
				ign("dist/app.js", "dist/"),
			},
			wantReady:  false,
			wantHidden: []string{"dist/app.js", "out/report.pdf"},
		},
		{
			// verify may read either spelling; the file is only lost when
			// both are ignored.
			name:   "a contractual path with one observable reading proceeds",
			intent: domain.WorkflowWriteIntentMutating,
			required: []RequiredDeliverable{{
				Path: "out/report.pdf", Source: DeliverableFromVerification, Declaration: "out/report.pdf",
				Readings: []string{"out/report.pdf", "backend/out/report.pdf"},
			}, {Path: "src/main.go", Source: DeliverableFromCriterion, Readings: []string{"src/main.go"}}},
			ignored:   []IgnoredDeliverable{ign("out/report.pdf", "/out/")},
			wantReady: true,
		},
		{
			name:   "a contractual path with every reading ignored refuses under its declared name",
			intent: domain.WorkflowWriteIntentMutating,
			required: []RequiredDeliverable{{
				Path: "out/report.pdf", Source: DeliverableFromVerification, Declaration: "out/report.pdf",
				Readings: []string{"out/report.pdf", "backend/out/report.pdf"},
			}, {Path: "src/main.go", Source: DeliverableFromCriterion, Readings: []string{"src/main.go"}}},
			ignored: []IgnoredDeliverable{
				ign("out/report.pdf", "out/"),
				ign("backend/out/report.pdf", "out/"),
			},
			wantReady:  false,
			wantHidden: []string{"out/report.pdf"},
		},
		{
			name:      "a contractual read-only task is never refused",
			intent:    domain.WorkflowWriteIntentReadOnly,
			required:  both(vf("out/report.pdf"), req("src/main.go")),
			ignored:   []IgnoredDeliverable{ign("out/report.pdf", "out/")},
			wantReady: true,
		},
		{
			name:     "every required deliverable ignored refuses and reports them all, sorted",
			intent:   domain.WorkflowWriteIntentMutating,
			required: req("out/zeta.pdf", "out/alpha.pdf"),
			ignored: []IgnoredDeliverable{
				ign("out/zeta.pdf", "out/"),
				ign("out/alpha.pdf", "out/"),
			},
			wantReady:  false,
			wantHidden: []string{"out/alpha.pdf", "out/zeta.pdf"},
		},
		{
			// A read-only task's accepted outcome is an unchanged workspace, so
			// it has no deliverable to hide and must never be refused here.
			name:      "a plan-declared read-only task is never refused",
			intent:    domain.WorkflowWriteIntentReadOnly,
			required:  req("postrunqa/report.csv"),
			ignored:   []IgnoredDeliverable{ign("postrunqa/report.csv", "postrunqa/")},
			wantReady: true,
		},
		{
			// Unspecified is treated as mutating everywhere else in AO and must
			// be treated as mutating here too, or every legacy plan opts out.
			name:       "an undeclared write intent is treated as mutating",
			intent:     domain.WorkflowWriteIntentUnspecified,
			required:   req("postrunqa/report.csv"),
			ignored:    []IgnoredDeliverable{ign("postrunqa/report.csv", "postrunqa/")},
			wantReady:  false,
			wantHidden: []string{"postrunqa/report.csv"},
		},
		{
			name:      "a task that names no path is not suspicious",
			intent:    domain.WorkflowWriteIntentMutating,
			required:  nil,
			ignored:   []IgnoredDeliverable{ign("whatever/else.pdf", "whatever/")},
			wantReady: true,
		},
		{
			// A path that does not exist, or that git simply did not report,
			// is observable as far as this check is concerned. The probe is the
			// only authority on what is ignored; silence is never a refusal.
			name:      "a required path the probe did not report is observable",
			intent:    domain.WorkflowWriteIntentMutating,
			required:  req("reports/not-created-yet.pdf"),
			ignored:   nil,
			wantReady: true,
		},
		{
			// A probe that widened its answer beyond what was asked must not be
			// able to widen the refusal with it.
			name:      "an ignored path the task never required cannot cause a refusal",
			intent:    domain.WorkflowWriteIntentMutating,
			required:  req("backend/internal/a/b.go"),
			ignored:   []IgnoredDeliverable{ign("node_modules/x.js", "node_modules/")},
			wantReady: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := evaluateDeliverableObservability(tt.intent, tt.required, tt.ignored)
			if got.Ready != tt.wantReady {
				t.Fatalf("Ready = %v, want %v (detail %q)", got.Ready, tt.wantReady, got.Detail)
			}
			if tt.wantReady {
				if len(got.Ignored) != 0 || got.Detail != "" {
					t.Fatalf("a ready verdict carried a refusal: %+v", got)
				}
				return
			}
			var paths []string
			for _, ig := range got.Ignored {
				paths = append(paths, ig.Path)
			}
			if !reflect.DeepEqual(paths, tt.wantHidden) {
				t.Fatalf("hidden paths = %v, want %v", paths, tt.wantHidden)
			}
			// A refusal nobody can act on is the unreadable stop this check
			// replaces, so the detail must carry the path, the rule and a
			// remedy.
			for _, want := range append(append([]string{}, tt.wantHidden...), ".gitignore:3:", "git add -f") {
				if !strings.Contains(got.Detail, want) {
					t.Fatalf("detail is missing %q: %s", want, got.Detail)
				}
			}
		})
	}
}

// --- the stop's vocabulary --------------------------------------------------

// The refusal must not be retried and must not be failed over to another
// provider: no amount of waiting or provider-hopping edits a .gitignore.
func TestDeliverableRefusalIsClassifiedAndNeverRetried(t *testing.T) {
	err := &ErrDeliverableNotObservable{Detail: "deliverable observability: hidden"}
	cls := classifyWorkerLaunchFailure(err)
	if cls.Class != WorkflowErrorDeliverableNotObservable {
		t.Fatalf("class = %q, want %q", cls.Class, WorkflowErrorDeliverableNotObservable)
	}
	if cls.Retryable {
		t.Fatal("a hidden deliverable was marked retryable")
	}
	if cls.Certainty != CertaintyActual {
		t.Fatalf("certainty = %v, want actual: the refusal comes from a sentinel AO returned itself", cls.Certainty)
	}
	if cls.Reason != ReasonDeliverableNotObservable {
		t.Fatalf("reason = %q, want %q", cls.Reason, ReasonDeliverableNotObservable)
	}
	var target *ErrDeliverableNotObservable
	if !errors.As(error(err), &target) {
		t.Fatal("the typed error does not unwrap to itself")
	}
}

// Every stop AO shows a person must name something to do. An error class with
// no registered human action reaches the Board as a dead end.
func TestDeliverableRefusalCarriesAHumanAction(t *testing.T) {
	disp, ok := attentionErrorClasses[WorkflowErrorDeliverableNotObservable]
	if !ok {
		t.Fatal("the class is not registered in attentionErrorClasses")
	}
	if strings.TrimSpace(disp.HumanAction) == "" {
		t.Fatal("the disposition carries no human action")
	}
	for _, want := range []string{".gitignore", "continue this run"} {
		if !strings.Contains(disp.HumanAction, want) {
			t.Fatalf("human action is missing %q: %s", want, disp.HumanAction)
		}
	}
}

// --- the gate ---------------------------------------------------------------

type stubIgnoreProbe struct {
	calls  int
	result []IgnoredDeliverable
	err    error
}

func (s *stubIgnoreProbe) IgnoredPaths(stdctx.Context, string, []string) ([]IgnoredDeliverable, error) {
	s.calls++
	return s.result, s.err
}

// The two "never a refusal" properties, which are the reason this check can be
// wired on by default: a coordinator with no probe behaves exactly as it did
// before the check existed, and a coordinator that cannot resolve the
// repository asks nothing rather than assuming the worst.
func TestDeliverablePreflightIsNeverARefusalWithoutAnAnswer(t *testing.T) {
	run := domain.WorkflowRun{ID: "run-1", ProjectID: "proj-1"}

	t.Run("no probe wired", func(t *testing.T) {
		c := New(Deps{})
		if err := c.preflightDeliverableObservability(stdctx.Background(), run); err != nil {
			t.Fatalf("a coordinator with no probe refused a dispatch: %v", err)
		}
	})

	t.Run("no repository path", func(t *testing.T) {
		probe := &stubIgnoreProbe{result: []IgnoredDeliverable{{Path: "x", Pattern: "x"}}}
		// Deps.Projects is nil, so projectPathFor cannot resolve a path.
		c := New(Deps{DeliverableIgnores: probe})
		if err := c.preflightDeliverableObservability(stdctx.Background(), run); err != nil {
			t.Fatalf("an unresolvable repository refused a dispatch: %v", err)
		}
		if probe.calls != 0 {
			t.Fatalf("the probe was asked %d times with no repository to ask about", probe.calls)
		}
	})
}
