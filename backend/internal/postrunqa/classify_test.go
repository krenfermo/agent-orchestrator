package postrunqa_test

import (
	"strings"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/branchlock"
	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	"github.com/aoagents/agent-orchestrator/backend/internal/postrunqa"
)

const (
	appRepo   = "/repos/app"
	otherRepo = "/repos/unrelated"
)

var classifiedAt = time.Date(2026, time.August, 22, 13, 0, 0, 0, time.UTC)

func appScope() postrunqa.ExecutionScope {
	return postrunqa.ExecutionScope{
		ExecutionID:   "exec-1",
		WorkflowRunID: "run-1",
		SessionIDs:    []string{"sess-1"},
		Repositories:  []string{appRepo},
		DataDir:       "/home/dev/.ao/data",
	}
}

// snapshot builds an evidence report in the shape the collector produces, so
// the classifier under test is fed the same struct the real gate feeds it.
func snapshot(ended bool, git []postrunqa.GitEvidence) postrunqa.EvidenceReport {
	return postrunqa.EvidenceReport{
		ExecutionID:    "exec-1",
		WorkflowRunID:  "run-1",
		CollectedAt:    classifiedAt,
		ExecutionEnded: ended,
		Git:            git,
	}
}

func dirtyRepo(repoPath string, changes ...ports.WorkspaceChange) postrunqa.GitEvidence {
	return postrunqa.GitEvidence{
		RepoPath:         repoPath,
		ConfiguredBranch: "main",
		CurrentBranch:    "main",
		Dirty:            len(changes) > 0,
		Changes:          changes,
		Anomaly:          "working tree has uncommitted change(s)",
	}
}

// findingFor returns the single finding whose Signal mentions want, failing the
// test when there is not exactly one.
func findingFor(t *testing.T, c postrunqa.Classification, want string) postrunqa.Finding {
	t.Helper()
	var hits []postrunqa.Finding
	for _, f := range c.Findings {
		if strings.Contains(f.Signal, want) {
			hits = append(hits, f)
		}
	}
	if len(hits) != 1 {
		t.Fatalf("want exactly one finding mentioning %q, got %d:\n%s", want, len(hits), dumpFindings(c.Findings))
	}
	return hits[0]
}

func dumpFindings(findings []postrunqa.Finding) string {
	var b strings.Builder
	for _, f := range findings {
		b.WriteString("  - [" + string(f.Attribution) + "/" + string(f.Scope) + "/" +
			string(f.Verification) + "/" + string(f.Safety) + "/" + string(f.Severity) + "] " + f.Signal + "\n")
	}
	if b.Len() == 0 {
		return "  (no findings)\n"
	}
	return b.String()
}

// A signal absent from the pre-run snapshot and present after the execution is
// this execution's doing, and blocks.
func TestClassify_SignalAbsentPreRunIsNewAndBlocking(t *testing.T) {
	pre := snapshot(false, []postrunqa.GitEvidence{dirtyRepo(appRepo)})
	post := snapshot(true, []postrunqa.GitEvidence{
		dirtyRepo(appRepo, ports.WorkspaceChange{Path: "internal/svc/handler.go", Status: "M"}),
	})

	got := postrunqa.Classify(postrunqa.ClassificationInput{Baseline: &pre, Post: post, Scope: appScope()})

	f := findingFor(t, got, "internal/svc/handler.go")
	if f.Attribution != postrunqa.AttributionNew {
		t.Fatalf("attribution = %q (%s), want new", f.Attribution, f.AttributionReason)
	}
	if !f.Blocking() {
		t.Fatalf("a new, in-scope, evidence-backed major finding must block: %+v", f)
	}
	if f.Verification != postrunqa.VerificationEvidence || !f.Reproducible {
		t.Fatalf("verification = %q reproducible = %v, want evidence-backed and reproducible", f.Verification, f.Reproducible)
	}
	if len(got.Blocking()) != 1 {
		t.Fatalf("blocking findings = %d, want 1:\n%s", len(got.Blocking()), dumpFindings(got.Findings))
	}
}

// The same signature on both sides of the execution was already true before it
// ran. It is recorded, and it does not block.
func TestClassify_UnchangedSignatureOnBothSidesIsBaselineAndNonBlocking(t *testing.T) {
	stale := ports.WorkspaceChange{Path: "docs/notes.md", Status: "??"}
	fresh := ports.WorkspaceChange{Path: "internal/svc/handler.go", Status: "M"}

	pre := snapshot(false, []postrunqa.GitEvidence{dirtyRepo(appRepo, stale)})
	post := snapshot(true, []postrunqa.GitEvidence{dirtyRepo(appRepo, stale, fresh)})

	got := postrunqa.Classify(postrunqa.ClassificationInput{Baseline: &pre, Post: post, Scope: appScope()})

	baseline := findingFor(t, got, "docs/notes.md")
	if baseline.Attribution != postrunqa.AttributionBaseline {
		t.Fatalf("attribution = %q (%s), want baseline", baseline.Attribution, baseline.AttributionReason)
	}
	if baseline.Blocking() {
		t.Fatalf("a baseline finding must never block: %+v", baseline)
	}
	if baseline.AutoFixEligible() {
		t.Fatalf("a baseline finding must never be auto-fixed: %+v", baseline)
	}
	// Recorded, not discarded: the baseline finding is the evidence for *not*
	// blocking, and dropping it makes that decision unauditable.
	if len(got.BaselineFindings()) != 1 {
		t.Fatalf("baseline findings = %d, want the one that was carried through:\n%s",
			len(got.BaselineFindings()), dumpFindings(got.Findings))
	}

	if n := findingFor(t, got, "internal/svc/handler.go"); n.Attribution != postrunqa.AttributionNew {
		t.Fatalf("the freshly dirty path = %q, want new", n.Attribution)
	}
	if len(got.Blocking()) != 1 {
		t.Fatalf("blocking findings = %d, want only the new one:\n%s", len(got.Blocking()), dumpFindings(got.Findings))
	}
}

// Defect language nothing structured corroborates is kept as report-only. It
// must not be discarded, and it must not block.
func TestClassify_UncorroboratedReportLanguageIsReportOnlyNotDiscarded(t *testing.T) {
	pre := snapshot(false, nil)
	post := snapshot(true, nil)
	post.FinalAgentReport = "Implemented the handler.\n" +
		"The staging deploy could not be reached, so I skipped the smoke check.\n" +
		"Everything else is green."

	got := postrunqa.Classify(postrunqa.ClassificationInput{Baseline: &pre, Post: post, Scope: appScope()})

	f := findingFor(t, got, "staging deploy")
	if f.Verification != postrunqa.VerificationReportOnly {
		t.Fatalf("verification = %q, want report_only", f.Verification)
	}
	if f.Severity != postrunqa.SeverityInfo {
		t.Fatalf("severity = %q, want info", f.Severity)
	}
	if f.Reproducible {
		t.Fatalf("a claim no source reported is not reproducible: %+v", f)
	}
	if f.Blocking() || f.AutoFixEligible() {
		t.Fatalf("an unverified report claim must neither block nor be auto-fixed: %+v", f)
	}
	if len(got.ReportOnly()) != 1 {
		t.Fatalf("report-only findings = %d, want 1:\n%s", len(got.ReportOnly()), dumpFindings(got.Findings))
	}
	if len(got.Blocking()) != 0 {
		t.Fatalf("blocking findings = %d, want none:\n%s", len(got.Blocking()), dumpFindings(got.Findings))
	}
}

// The mirror image: defect language a structured source does corroborate is
// promoted to a blocking finding, and takes its attribution from the diff
// rather than from the agent's own wording.
func TestClassify_CorroboratedReportLanguageBlocksAndOverridesTheAgentsWording(t *testing.T) {
	exit := 1
	pre := snapshot(false, nil)
	post := snapshot(true, nil)
	post.Processes = []postrunqa.ProcessEvidence{{
		ProcessRecord: postrunqa.ProcessRecord{Label: "go vet ./...", ExitCode: &exit},
		Anomaly:       "exited with status 1",
	}}
	post.FinalAgentReport = "Note: `go vet ./...` failed here before my change - pre-existing failure."

	got := postrunqa.Classify(postrunqa.ClassificationInput{Baseline: &pre, Post: post, Scope: appScope()})

	claim := findingFor(t, got, "pre-existing failure")
	if claim.Verification != postrunqa.VerificationEvidence {
		t.Fatalf("verification = %q, want evidence-backed", claim.Verification)
	}
	if claim.CorroboratedBy == "" {
		t.Fatalf("a corroborated claim must name the signal backing it: %+v", claim)
	}
	// The agent called it pre-existing. The pre-run snapshot did not have it.
	// The snapshots win.
	if claim.Attribution != postrunqa.AttributionNew {
		t.Fatalf("attribution = %q (%s), want new despite the report calling it pre-existing",
			claim.Attribution, claim.AttributionReason)
	}
	if !claim.Blocking() {
		t.Fatalf("a corroborated defect claim must block: %+v", claim)
	}
	if claim.AutoFixEligible() {
		t.Fatalf("repairs are dispatched from the structured finding, never from the prose describing it: %+v", claim)
	}

	// And the structured finding it corroborates is itself auto-fixable.
	vet := findingFor(t, got, "exited with status 1")
	if !vet.AutoFixEligible() {
		t.Fatalf("a new, in-scope, mechanical failure should be auto-fixable: %+v", vet)
	}
}

// A finding about a repository this execution was never permitted to write is
// recorded, but it is not the execution's problem and no machine may repair it.
func TestClassify_OutOfScopeFindingIsRejectedFromAutoFixAndDoesNotBlock(t *testing.T) {
	pre := snapshot(false, nil)
	post := snapshot(true, []postrunqa.GitEvidence{
		dirtyRepo(otherRepo, ports.WorkspaceChange{Path: "src/main.rs", Status: "M"}),
	})
	// A lock that would otherwise be textbook auto-fix material: stale by the
	// manager's own reconciliation, and new this execution.
	post.BranchLocks = []postrunqa.BranchLockEvidence{{
		LockID:            "lock-9",
		LockKey:           "unrelated-project:release",
		RepoPath:          otherRepo,
		Branch:            "release",
		Owner:             "run other-run",
		State:             domain.BranchLockHeld,
		RetentionDecision: branchlock.RetentionRelease,
		RetentionReason:   "stale: workflow run no longer exists",
		Leaked:            true,
		Anomaly:           "reconciliation considers it stale",
	}}

	got := postrunqa.Classify(postrunqa.ClassificationInput{Baseline: &pre, Post: post, Scope: appScope()})

	change := findingFor(t, got, "src/main.rs")
	if change.Scope != postrunqa.ScopeOutOfScope {
		t.Fatalf("scope = %q (%s), want out_of_scope", change.Scope, change.ScopeReason)
	}
	if change.Blocking() || change.AutoFixEligible() {
		t.Fatalf("an out-of-scope finding must neither block nor be auto-fixed: %+v", change)
	}

	lock := findingFor(t, got, "is stale")
	if lock.Attribution != postrunqa.AttributionNew {
		t.Fatalf("attribution = %q, want new", lock.Attribution)
	}
	if lock.Safety != postrunqa.SafetyAutoFix {
		t.Fatalf("safety = %q, want safe_to_autofix -- scope, not safety, is what disqualifies it", lock.Safety)
	}
	if lock.Scope != postrunqa.ScopeOutOfScope {
		t.Fatalf("scope = %q (%s), want out_of_scope", lock.Scope, lock.ScopeReason)
	}
	if lock.AutoFixEligible() {
		t.Fatalf("scope must veto auto-fix even for an otherwise mechanical repair: %+v", lock)
	}
	if len(got.AutoFixable()) != 0 {
		t.Fatalf("auto-fixable findings = %d, want none:\n%s", len(got.AutoFixable()), dumpFindings(got.Findings))
	}
	if len(got.Blocking()) != 0 {
		t.Fatalf("blocking findings = %d, want none:\n%s", len(got.Blocking()), dumpFindings(got.Findings))
	}
}

// Without a pre-run snapshot nothing can be attributed. That is its own
// durable answer, never a silent promotion to new or demotion to baseline.
func TestClassify_NoBaselineSnapshotMakesEveryAttributionAmbiguous(t *testing.T) {
	post := snapshot(true, []postrunqa.GitEvidence{
		dirtyRepo(appRepo, ports.WorkspaceChange{Path: "internal/svc/handler.go", Status: "M"}),
	})

	got := postrunqa.Classify(postrunqa.ClassificationInput{Post: post, Scope: appScope()})

	if got.HasBaseline {
		t.Fatalf("HasBaseline = true with no baseline supplied")
	}
	f := findingFor(t, got, "internal/svc/handler.go")
	if f.Attribution != postrunqa.AttributionAmbiguous {
		t.Fatalf("attribution = %q, want ambiguous", f.Attribution)
	}
	if f.AutoFixEligible() {
		t.Fatalf("an unattributable finding must not be auto-fixed: %+v", f)
	}
}

// A source that could not be read must not read as a source that reported
// nothing, on either side of the diff.
func TestClassify_UnreadableSourceIsRecordedAndBlocksAttribution(t *testing.T) {
	pre := snapshot(false, nil)
	pre.SourceErrors = []postrunqa.SourceError{{
		Source: postrunqa.SourceGit, Subject: appRepo, Message: "repository was locked",
	}}
	post := snapshot(true, []postrunqa.GitEvidence{
		dirtyRepo(appRepo, ports.WorkspaceChange{Path: "internal/svc/handler.go", Status: "M"}),
	})

	got := postrunqa.Classify(postrunqa.ClassificationInput{Baseline: &pre, Post: post, Scope: appScope()})

	f := findingFor(t, got, "internal/svc/handler.go")
	if f.Attribution != postrunqa.AttributionAmbiguous {
		t.Fatalf("attribution = %q (%s), want ambiguous: git was unreadable pre-run",
			f.Attribution, f.AttributionReason)
	}

	post.SourceErrors = pre.SourceErrors
	got = postrunqa.Classify(postrunqa.ClassificationInput{Baseline: &pre, Post: post, Scope: appScope()})
	if len(got.Findings) < 2 {
		t.Fatalf("a post-run source error must be its own finding:\n%s", dumpFindings(got.Findings))
	}
	unreadable := findingFor(t, got, "could not be read")
	if unreadable.Blocking() {
		t.Fatalf("an unreadable source informs, it does not block: %+v", unreadable)
	}
	if got.Findings[0].Reproducible {
		t.Fatalf("a finding from a source that failed mid-read is not reproducible: %+v", got.Findings[0])
	}
}

// Classifying the same pair of snapshots twice must give the same answer:
// signatures carry no clock, and the classifier reads none.
func TestClassify_IsDeterministicForTheSameSnapshots(t *testing.T) {
	pre := snapshot(false, []postrunqa.GitEvidence{dirtyRepo(appRepo, ports.WorkspaceChange{Path: "a.go", Status: "M"})})
	post := snapshot(true, []postrunqa.GitEvidence{
		dirtyRepo(appRepo, ports.WorkspaceChange{Path: "a.go", Status: "M"}, ports.WorkspaceChange{Path: "b.go", Status: "??"}),
	})
	in := postrunqa.ClassificationInput{Baseline: &pre, Post: post, Scope: appScope()}

	first, second := postrunqa.Classify(in), postrunqa.Classify(in)

	if first.ClassifiedAt != post.CollectedAt {
		t.Fatalf("ClassifiedAt = %v, want the post-run snapshot's collection time", first.ClassifiedAt)
	}
	if len(first.Findings) != len(second.Findings) {
		t.Fatalf("finding counts differ across runs: %d vs %d", len(first.Findings), len(second.Findings))
	}
	for i := range first.Findings {
		if first.Findings[i] != second.Findings[i] {
			t.Fatalf("finding %d differs across runs:\n%+v\n%+v", i, first.Findings[i], second.Findings[i])
		}
	}
}

// Everything the classifier writes has to survive a round trip through the
// store, so every enum it sets must be persistable.
func TestClassify_ProducesPersistableFindings(t *testing.T) {
	exit := 2
	pre := snapshot(false, nil)
	post := snapshot(true, []postrunqa.GitEvidence{
		dirtyRepo(appRepo, ports.WorkspaceChange{Path: "internal/svc/handler.go", Status: "M"}),
		dirtyRepo(otherRepo, ports.WorkspaceChange{Path: "src/main.rs", Status: "M"}),
	})
	post.Processes = []postrunqa.ProcessEvidence{{
		ProcessRecord: postrunqa.ProcessRecord{Label: "npm run lint", ExitCode: &exit},
	}}
	post.Sessions = []postrunqa.SessionEvidence{{
		SessionID: "sess-1", Found: true, Terminated: true, CleanupRecorded: true,
		WorkspaceDisposition: domain.DispositionFailed,
	}}
	post.FinalAgentReport = "npm run lint failed. The deploy webhook could not be reached."

	got := postrunqa.Classify(postrunqa.ClassificationInput{Baseline: &pre, Post: post, Scope: appScope()})

	if len(got.Findings) == 0 {
		t.Fatal("expected findings")
	}
	run := postrunqa.QARun{
		ID: "qa-1", SubjectKind: postrunqa.SubjectTask, SubjectID: "task-1",
		StartedAt: classifiedAt, Findings: got.Findings,
	}.WithDefaults()
	if err := run.Validate(); err != nil {
		t.Fatalf("classified findings are not persistable: %v\n%s", err, dumpFindings(got.Findings))
	}
}

// A finding written before classification existed loads with every new enum
// unset, and must stay persistable rather than failing validation on read.
func TestFinding_UnsetClassificationFieldsStayPersistable(t *testing.T) {
	legacy := postrunqa.Finding{
		Source:      "go vet ./...",
		Signal:      "vet failed",
		Attribution: postrunqa.AttributionNew,
		Severity:    postrunqa.SeverityBlocker,
	}
	if err := legacy.Validate(); err != nil {
		t.Fatalf("a pre-classification finding must still validate: %v", err)
	}
	if legacy.AutoFixEligible() {
		t.Fatalf("an unclassified finding must not be auto-fixed: %+v", legacy)
	}
	if !legacy.Blocking() {
		t.Fatalf("an unclassified blocker still blocks: %+v", legacy)
	}
}

// The hook-launcher probe's headline defect: a shim still pinned to a Go
// build-cache binary. Reinstalling it is mechanical, so a new one is
// auto-fixable -- and a shim that was already pinned before the execution ran
// is the previous install's problem, not this agent's.
func TestClassify_HookLauncherEphemeralTarget(t *testing.T) {
	broken := postrunqa.HookLauncherEvidence{
		Probed: true, Path: "/home/dev/.ao/data/hooks/ao-hook", Present: true, Executable: true,
		Target: "/tmp/go-build123/b001/exe/ao", TargetPresent: true, TargetExecutable: true, TargetEphemeral: true,
		Anomaly: "launcher targets an ephemeral Go build-cache binary",
	}

	pre := snapshot(false, nil)
	post := snapshot(true, nil)
	post.HookLauncher = broken

	got := postrunqa.Classify(postrunqa.ClassificationInput{Baseline: &pre, Post: post, Scope: appScope()})
	f := findingFor(t, got, "ephemeral Go build-cache binary")
	if f.Attribution != postrunqa.AttributionNew {
		t.Fatalf("attribution = %q, want new", f.Attribution)
	}
	if !f.AutoFixEligible() {
		t.Fatalf("reinstalling the shim is mechanical, so this should be auto-fixable: %+v", f)
	}

	// Same shim, same target, already broken pre-run.
	preBroken := snapshot(false, nil)
	preBroken.HookLauncher = broken
	got = postrunqa.Classify(postrunqa.ClassificationInput{Baseline: &preBroken, Post: post, Scope: appScope()})
	f = findingFor(t, got, "ephemeral Go build-cache binary")
	if f.Attribution != postrunqa.AttributionBaseline {
		t.Fatalf("attribution = %q (%s), want baseline", f.Attribution, f.AttributionReason)
	}
	if f.Blocking() || f.AutoFixEligible() {
		t.Fatalf("a baseline launcher defect neither blocks nor is auto-fixed: %+v", f)
	}

	// And an execution with no data dir does not own the launcher it probed.
	scope := appScope()
	scope.DataDir = ""
	got = postrunqa.Classify(postrunqa.ClassificationInput{Baseline: &pre, Post: post, Scope: scope})
	f = findingFor(t, got, "ephemeral Go build-cache binary")
	// Undecided, not exonerated: a probed launcher the scope says nothing
	// about is not thereby someone else's launcher.
	if f.Scope != postrunqa.ScopeUnknown {
		t.Fatalf("scope = %q (%s), want unknown", f.Scope, f.ScopeReason)
	}
	if f.AutoFixEligible() {
		t.Fatalf("unknown ownership must not be auto-fixed: %+v", f)
	}
	if !f.Blocking() {
		t.Fatalf("unknown ownership still blocks -- it may well be ours: %+v", f)
	}
}

func runtimeError(component, code, message string) postrunqa.RuntimeErrorEvidence {
	return postrunqa.RuntimeErrorEvidence{
		RuntimeErrorRecord: postrunqa.RuntimeErrorRecord{
			Component: component, Code: code, Message: message,
			Level: postrunqa.RuntimeLevelError, Count: 1,
		},
		Anomaly: component + " recorded an error: " + message,
	}
}

// A daemon/runtime error the pre-run snapshot did not have is this execution's,
// and blocks like any other structured signal.
func TestClassify_RuntimeErrorAbsentPreRunIsNew(t *testing.T) {
	pre := snapshot(false, nil)
	pre.RuntimeErrors = []postrunqa.RuntimeErrorEvidence{
		runtimeError("lifecycle.reaper", "REAP_TIMEOUT", "reap pass timed out"),
	}
	post := snapshot(true, nil)
	post.RuntimeErrors = append(append([]postrunqa.RuntimeErrorEvidence(nil), pre.RuntimeErrors...),
		runtimeError("runtime.tmux", "PANE_GONE", "pane disappeared mid-send"))

	got := postrunqa.Classify(postrunqa.ClassificationInput{Baseline: &pre, Post: post, Scope: appScope()})

	fresh := findingFor(t, got, "pane disappeared mid-send")
	if fresh.Attribution != postrunqa.AttributionNew {
		t.Fatalf("attribution = %q (%s), want new", fresh.Attribution, fresh.AttributionReason)
	}
	if fresh.Verification != postrunqa.VerificationEvidence || !fresh.Reproducible {
		t.Fatalf("a daemon error record is structured evidence: %+v", fresh)
	}
	if !fresh.Blocking() {
		t.Fatalf("a new in-scope runtime error must block: %+v", fresh)
	}
	if fresh.Safety != postrunqa.SafetyAmbiguous || fresh.AutoFixEligible() {
		t.Fatalf("the gate cannot mechanically repair a daemon error: %+v", fresh)
	}

	if old := findingFor(t, got, "reap pass timed out"); old.Attribution != postrunqa.AttributionBaseline {
		t.Fatalf("the error present on both sides = %q, want baseline", old.Attribution)
	}
}

// The same runtime error on both sides is the daemon's pre-existing problem,
// and stays that way however many more times it fired during the execution.
func TestClassify_RuntimeErrorOnBothSidesIsBaselineEvenWhenItFiredMoreOften(t *testing.T) {
	before := runtimeError("lifecycle.reaper", "REAP_TIMEOUT", "reap pass timed out after 3 attempts")
	before.Count = 3
	before.FirstSeenAt = classifiedAt.Add(-72 * time.Hour)
	after := runtimeError("lifecycle.reaper", "REAP_TIMEOUT", "reap pass timed out after 11 attempts")
	after.Count = 11
	after.LastSeenAt = classifiedAt

	pre := snapshot(false, nil)
	pre.RuntimeErrors = []postrunqa.RuntimeErrorEvidence{before}
	post := snapshot(true, nil)
	post.RuntimeErrors = []postrunqa.RuntimeErrorEvidence{after}

	got := postrunqa.Classify(postrunqa.ClassificationInput{Baseline: &pre, Post: post, Scope: appScope()})

	f := findingFor(t, got, "REAP_TIMEOUT")
	if f.Attribution != postrunqa.AttributionBaseline {
		t.Fatalf("attribution = %q (%s), want baseline: a rising counter is not a new error",
			f.Attribution, f.AttributionReason)
	}
	if f.Blocking() {
		t.Fatalf("a baseline runtime error must not block: %+v", f)
	}
	if len(got.BaselineFindings()) != 1 {
		t.Fatalf("baseline findings = %d, want the runtime error carried through:\n%s",
			len(got.BaselineFindings()), dumpFindings(got.Findings))
	}
}

// With no component-supplied code the message is fingerprinted, and the ids
// and counters interpolated into it are exactly what the fingerprint drops.
func TestClassify_RuntimeErrorWithoutCodeMatchesOnMessageFingerprint(t *testing.T) {
	pre := snapshot(false, nil)
	pre.RuntimeErrors = []postrunqa.RuntimeErrorEvidence{
		runtimeError("runtime.tmux", "", "exec failed for pane 41 after 3 attempts"),
	}
	post := snapshot(true, nil)
	post.RuntimeErrors = []postrunqa.RuntimeErrorEvidence{
		runtimeError("runtime.tmux", "", "exec failed for pane 87 after 12 attempts"),
		runtimeError("runtime.tmux", "", "socket handshake was rejected"),
	}

	got := postrunqa.Classify(postrunqa.ClassificationInput{Baseline: &pre, Post: post, Scope: appScope()})

	same := findingFor(t, got, "exec failed for pane 87")
	if same.Attribution != postrunqa.AttributionBaseline {
		t.Fatalf("attribution = %q (%s), want baseline: only the ids moved",
			same.Attribution, same.AttributionReason)
	}
	fresh := findingFor(t, got, "socket handshake was rejected")
	if fresh.Attribution != postrunqa.AttributionNew {
		t.Fatalf("attribution = %q, want new: a genuinely different failure", fresh.Attribution)
	}
}

// A runtime error corroborates final-report defect language exactly as any
// other structured source does -- including overriding the agent's own
// classification of it.
func TestClassify_RuntimeErrorCorroboratesFinalReportLanguage(t *testing.T) {
	pre := snapshot(false, nil)
	post := snapshot(true, nil)
	post.RuntimeErrors = []postrunqa.RuntimeErrorEvidence{
		runtimeError("runtime.tmux", "PANE_GONE", "pane disappeared mid-send"),
	}
	post.FinalAgentReport = "Remaining issue: runtime.tmux kept erroring, but that is a pre-existing daemon problem.\n" +
		"The metrics exporter could not be reached either."

	got := postrunqa.Classify(postrunqa.ClassificationInput{Baseline: &pre, Post: post, Scope: appScope()})

	claim := findingFor(t, got, "runtime.tmux kept erroring")
	if claim.Verification != postrunqa.VerificationEvidence {
		t.Fatalf("verification = %q, want evidence-backed: a runtime error record corroborates it", claim.Verification)
	}
	if claim.CorroboratedBy == "" {
		t.Fatalf("a corroborated claim must name the signal backing it: %+v", claim)
	}
	if claim.Attribution != postrunqa.AttributionNew {
		t.Fatalf("attribution = %q (%s), want new despite the report calling it pre-existing",
			claim.Attribution, claim.AttributionReason)
	}
	if !claim.Blocking() {
		t.Fatalf("a corroborated defect claim must block: %+v", claim)
	}

	// And the claim no runtime error backs stays report-only.
	unverified := findingFor(t, got, "metrics exporter")
	if unverified.Verification != postrunqa.VerificationReportOnly {
		t.Fatalf("verification = %q, want report_only", unverified.Verification)
	}
	if unverified.Blocking() {
		t.Fatalf("an uncorroborated claim must not block: %+v", unverified)
	}
}

// A runtime error belonging to a session this execution never ran in is not
// its problem.
func TestClassify_RuntimeErrorForAnotherSessionIsOutOfScope(t *testing.T) {
	stray := runtimeError("runtime.tmux", "PANE_GONE", "pane disappeared mid-send")
	stray.SessionID = "sess-someone-else"

	pre := snapshot(false, nil)
	post := snapshot(true, nil)
	post.RuntimeErrors = []postrunqa.RuntimeErrorEvidence{stray}

	got := postrunqa.Classify(postrunqa.ClassificationInput{Baseline: &pre, Post: post, Scope: appScope()})

	f := findingFor(t, got, "PANE_GONE")
	if f.Scope != postrunqa.ScopeOutOfScope {
		t.Fatalf("scope = %q (%s), want out_of_scope", f.Scope, f.ScopeReason)
	}
	if f.Blocking() || f.AutoFixEligible() {
		t.Fatalf("another session's runtime error neither blocks nor is auto-fixed: %+v", f)
	}
}

// Missing scope metadata is undecided, never an affirmative "yes, ours". The
// findings still block -- they may well be this execution's -- but nothing
// with unestablished ownership is ever handed to an automated repair.
func TestClassify_IncompleteScopeMetadataIsUnknownAndNeverAutoFixed(t *testing.T) {
	exit := 1
	pre := snapshot(false, nil)
	post := snapshot(true, []postrunqa.GitEvidence{
		dirtyRepo(appRepo, ports.WorkspaceChange{Path: "internal/svc/handler.go", Status: "M"}),
	})
	post.Processes = []postrunqa.ProcessEvidence{{
		ProcessRecord: postrunqa.ProcessRecord{Label: "go build ./...", ExitCode: &exit},
	}}
	post.Sessions = []postrunqa.SessionEvidence{{
		SessionID: "sess-1", Found: true, Terminated: true, CleanupRecorded: true,
		WorkspaceDisposition: domain.DispositionFailed,
	}}
	post.RuntimeErrors = []postrunqa.RuntimeErrorEvidence{
		runtimeError("lifecycle.reaper", "REAP_TIMEOUT", "reap pass timed out"),
	}

	tests := []struct {
		name  string
		strip func(*postrunqa.ExecutionScope)
		want  string
	}{
		{"no repository allowlist", func(s *postrunqa.ExecutionScope) { s.Repositories = nil }, "internal/svc/handler.go"},
		{"no execution id", func(s *postrunqa.ExecutionScope) { s.ExecutionID = "" }, "go build ./..."},
		{"no session ids", func(s *postrunqa.ExecutionScope) { s.SessionIDs = nil }, "workspace teardown failed"},
		{"no execution id for a daemon-wide error", func(s *postrunqa.ExecutionScope) { s.ExecutionID = "" }, "REAP_TIMEOUT"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			scope := appScope()
			tc.strip(&scope)

			got := postrunqa.Classify(postrunqa.ClassificationInput{Baseline: &pre, Post: post, Scope: scope})

			f := findingFor(t, got, tc.want)
			if f.Scope != postrunqa.ScopeUnknown {
				t.Fatalf("scope = %q (%s), want unknown", f.Scope, f.ScopeReason)
			}
			if f.ScopeReason == "" {
				t.Fatalf("an unknown scope must say why: %+v", f)
			}
			if f.Attribution != postrunqa.AttributionNew {
				t.Fatalf("attribution = %q, want new -- scope is what is undecided, not attribution", f.Attribution)
			}
			if f.AutoFixEligible() {
				t.Fatalf("state whose ownership was never established must not be auto-fixed: %+v", f)
			}
			if !f.Blocking() {
				t.Fatalf("unknown ownership is undecided, not exonerated, so it still blocks: %+v", f)
			}
			for _, other := range got.AutoFixable() {
				if other.Scope != postrunqa.ScopeInScope {
					t.Fatalf("auto-fixable finding with %q scope: %+v", other.Scope, other)
				}
			}
		})
	}
}

// The full-scope control for the case above: with the same evidence and a
// complete scope, the mechanical failure IS auto-fixable. Without this, the
// test above would pass just as well if nothing were ever auto-fixable.
func TestClassify_CompleteScopeMetadataStillAllowsAutoFix(t *testing.T) {
	exit := 1
	pre := snapshot(false, nil)
	post := snapshot(true, nil)
	post.Processes = []postrunqa.ProcessEvidence{{
		ProcessRecord: postrunqa.ProcessRecord{Label: "go build ./...", ExitCode: &exit},
	}}

	got := postrunqa.Classify(postrunqa.ClassificationInput{Baseline: &pre, Post: post, Scope: appScope()})

	f := findingFor(t, got, "go build ./...")
	if f.Scope != postrunqa.ScopeInScope {
		t.Fatalf("scope = %q (%s), want in_scope", f.Scope, f.ScopeReason)
	}
	if !f.AutoFixEligible() {
		t.Fatalf("a new, in-scope, mechanical failure must stay auto-fixable: %+v", f)
	}
}
