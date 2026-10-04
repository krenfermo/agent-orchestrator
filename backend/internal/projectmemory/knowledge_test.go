package projectmemory_test

import (
	"fmt"
	"strings"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/projectmemory"
)

// knowledge_test.go — the P2-C bar, one test per claim.
//
// The claims that matter are not "the code runs". They are the four sentences
// P2-C's completion bar is written in, and each of them is a test below:
//
//	A later task working in the same area RECEIVES what an earlier one learned.
//	A later task working elsewhere receives NONE of it.
//	An unintegrated branch can never write into the project's own knowledge.
//	Nothing is deleted to change its status, so the history stays readable.

// recordTask is the shorthand every test here builds its world with.
func recordTask(t *testing.T, f *fixture, svc *projectmemory.Service, root string, out projectmemory.TaskOutcome) {
	t.Helper()
	out.ProjectID = testProject
	out.RepoPath = root
	if err := svc.RecordTaskOutcome(f.ctx, out); err != nil {
		t.Fatalf("record %s: %v", out.TaskRef, err)
	}
}

// --- retrieval: related vs unrelated ---------------------------------------

// TestRelatedTaskReceivesPriorKnowledgeAndUnrelatedDoesNot is the P2-C headline
// and the measurement §19 asks for, in one test.
//
// Task A works in internal/store and records a decision and a risk. Task B
// touches the same file; Task C touches a different one. B must receive A's
// knowledge and C must receive none of it — and the byte counts must show it,
// because "C got fewer items" is not the same claim as "C paid nothing".
func TestRelatedTaskReceivesPriorKnowledgeAndUnrelatedDoesNot(t *testing.T) {
	f, svc, root, _ := packService(t)

	recordTask(t, f, svc, root, projectmemory.TaskOutcome{
		TaskRef: "task-a", Title: "harden the store",
		WhatChanged:  "made the store write through a queue",
		FilesChanged: []string{"internal/store/store.go"},
		Modules:      []string{"internal/store"},
		Integrated:   true, Commit: "c1",
		Decisions: []projectmemory.TaskDecision{{
			Statement: "the store queue is a table, not a JSON blob",
			Topic:     "queue-storage", Scope: domain.MemoryScopeModule, ScopeKey: "internal/store",
			Evidence: []string{"internal/store/store.go"},
		}},
		Risks: []projectmemory.TaskRisk{{
			Statement: "the queue is unbounded under load",
			Topic:     "queue-bounds", Scope: domain.MemoryScopeModule, ScopeKey: "internal/store",
			Evidence: []string{"internal/store/store.go"},
		}},
	})

	related := svc.Context(f.ctx, projectmemory.PackRequest{
		ProjectID: testProject, RepoPath: root, Role: projectmemory.RoleWorker,
		TaskRef: "task-b", ChangedPaths: []string{"internal/store/store.go"},
		Modules: []string{"internal/store"},
	})
	unrelated := svc.Context(f.ctx, projectmemory.PackRequest{
		ProjectID: testProject, RepoPath: root, Role: projectmemory.RoleWorker,
		TaskRef: "task-c", ChangedPaths: []string{"cmd/app/main.go"},
		Modules: []string{"cmd/app"},
	})

	body := related.Render()
	if !strings.Contains(body, "not a JSON blob") {
		t.Errorf("a task in the same module did not receive the prior decision:\n%s", body)
	}
	if !strings.Contains(body, "unbounded under load") {
		t.Errorf("a task in the same module did not receive the known risk:\n%s", body)
	}
	if related.Stats.SharedSelected == 0 {
		t.Error("the related pack reports no shared knowledge selected")
	}

	other := unrelated.Render()
	for _, leaked := range []string{"not a JSON blob", "unbounded under load", "harden the store"} {
		if strings.Contains(other, leaked) {
			t.Errorf("an unrelated task received %q:\n%s", leaked, other)
		}
	}
	if unrelated.Stats.SharedSelected != 0 {
		t.Errorf("the unrelated pack carries %d shared facts, want 0", unrelated.Stats.SharedSelected)
	}
	if unrelated.Stats.SharedIrrelevantExcluded == 0 {
		t.Error("the unrelated pack excluded nothing, so the gate never ran")
	}
	// The measurement §19 asks for, stated as a comparison rather than an
	// absolute: an unrelated task must not pay for another task's history.
	if unrelated.Stats.KnowledgeBytes != 0 {
		t.Errorf("an unrelated task paid %d bytes for prior task knowledge, want 0",
			unrelated.Stats.KnowledgeBytes)
	}
	if related.Stats.KnowledgeBytes <= 0 {
		t.Error("the related task paid nothing for knowledge it was supposed to receive")
	}
	if related.Stats.KnowledgeBytes > related.Stats.SelectedBytes {
		t.Errorf("knowledge bytes (%d) exceed the pack (%d): shared knowledge must compete inside the budget, not beside it",
			related.Stats.KnowledgeBytes, related.Stats.SelectedBytes)
	}
}

// TestExplicitDependencySelectsPriorKnowledgeWithoutPathOverlap covers the
// second admission route: a task that declares it depends on another may read
// that task's knowledge even when their file sets do not intersect.
func TestExplicitDependencySelectsPriorKnowledgeWithoutPathOverlap(t *testing.T) {
	f, svc, root, _ := packService(t)

	recordTask(t, f, svc, root, projectmemory.TaskOutcome{
		TaskRef: "task-a", Title: "define the wire format",
		WhatChanged:  "chose the wire format for the queue",
		FilesChanged: []string{"internal/store/store.go"},
		Integrated:   true, Commit: "c1",
	})

	// No path or module overlap at all — the dependency is the only signal.
	withDep := svc.Context(f.ctx, projectmemory.PackRequest{
		ProjectID: testProject, RepoPath: root, Role: projectmemory.RoleWorker,
		TaskRef: "task-b", ChangedPaths: []string{"cmd/app/main.go"},
		UpstreamTaskRefs: []string{"task-a"},
	})
	if !strings.Contains(withDep.Render(), "define the wire format") {
		t.Errorf("a declared dependency did not admit the prior task's outcome:\n%s", withDep.Render())
	}

	without := svc.Context(f.ctx, projectmemory.PackRequest{
		ProjectID: testProject, RepoPath: root, Role: projectmemory.RoleWorker,
		TaskRef: "task-b", ChangedPaths: []string{"cmd/app/main.go"},
	})
	if strings.Contains(without.Render(), "define the wire format") {
		t.Error("the same task received the outcome without declaring the dependency: " +
			"overlap must be declared, never inferred")
	}
}

// --- promotion authority ----------------------------------------------------

// TestUnintegratedWorktreeKnowledgeNeverReachesCanonical is the completion
// bar's explicit refusal condition, tested from both sides: the sibling cannot
// read it, and the project's canonical knowledge does not contain it.
func TestUnintegratedWorktreeKnowledgeNeverReachesCanonical(t *testing.T) {
	f, svc, root, _ := packService(t)

	// A verified but unintegrated isolated task. ShareWorkflow is the MOST
	// permissive scope such a task can be given, so testing at that scope
	// tests the ceiling rather than a convenient floor.
	recordTask(t, f, svc, root, projectmemory.TaskOutcome{
		TaskRef: "task-a", WorkflowRunID: "run-1", Share: domain.ShareWorkflow,
		Title: "switch to GraphQL", WhatChanged: "moved the public API to GraphQL",
		FilesChanged: []string{"internal/store/store.go"},
		Integrated:   false,
		Decisions: []projectmemory.TaskDecision{{
			Statement: "the public API speaks GraphQL", Topic: "api-protocol",
		}},
	})

	// A sibling in the same run that declared no dependency. Same run, same
	// files, no declared dependency — this is the sibling-safety case.
	sibling := svc.Context(f.ctx, projectmemory.PackRequest{
		ProjectID: testProject, RepoPath: root, Role: projectmemory.RoleWorker,
		TaskRef: "task-b", WorkflowRunID: "run-1",
		ChangedPaths: []string{"internal/store/store.go"},
	})
	if strings.Contains(sibling.Render(), "GraphQL") {
		t.Errorf("a sibling read an unintegrated task's decision:\n%s", sibling.Render())
	}

	// A task in a different run entirely, even naming the dependency.
	otherRun := svc.Context(f.ctx, projectmemory.PackRequest{
		ProjectID: testProject, RepoPath: root, Role: projectmemory.RoleWorker,
		TaskRef: "task-z", WorkflowRunID: "run-2", UpstreamTaskRefs: []string{"task-a"},
		ChangedPaths: []string{"internal/store/store.go"},
	})
	if strings.Contains(otherRun.Render(), "GraphQL") {
		t.Errorf("a later run read an unintegrated task's decision:\n%s", otherRun.Render())
	}

	// And it is not canonical in storage either — the claim the completion bar
	// actually makes, checked at the row rather than through a pack.
	entries, err := svc.Decisions(f.ctx, projectmemory.KnowledgeFilter{
		ProjectID: testProject, RepoPath: root,
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.Share == domain.ShareCanonical && strings.Contains(e.Item.Summary, "GraphQL") {
			t.Fatal("an unintegrated task's decision is stored as canonical project knowledge")
		}
	}
}

// TestDependentTaskInSameRunReadsVerifiedUpstreamKnowledge is the other half of
// §14: workflow-local sharing must actually WORK, or sibling safety has been
// bought by making the feature inert.
func TestDependentTaskInSameRunReadsVerifiedUpstreamKnowledge(t *testing.T) {
	f, svc, root, _ := packService(t)

	recordTask(t, f, svc, root, projectmemory.TaskOutcome{
		TaskRef: "task-1", WorkflowRunID: "run-1", Share: domain.ShareWorkflow,
		Title: "add the queue table", WhatChanged: "added the queue table",
		FilesChanged: []string{"internal/store/store.go"},
		Integrated:   false,
	})

	dependent := svc.Context(f.ctx, projectmemory.PackRequest{
		ProjectID: testProject, RepoPath: root, Role: projectmemory.RoleWorker,
		TaskRef: "task-2", WorkflowRunID: "run-1", UpstreamTaskRefs: []string{"task-1"},
		ChangedPaths: []string{"internal/store/store.go"},
	})
	if !strings.Contains(dependent.Render(), "queue table") {
		t.Errorf("a dependent task in the same run did not receive its upstream's verified outcome:\n%s",
			dependent.Render())
	}
	if dependent.Stats.WorkflowLocalSelected == 0 {
		t.Error("workflow-local knowledge was served but not counted as workflow-local")
	}
}

// TestTaskScopedKnowledgeReachesOnlyItsOwnTask covers the default: a task whose
// sharing was never decided shares with nobody.
func TestTaskScopedKnowledgeReachesOnlyItsOwnTask(t *testing.T) {
	f, svc, root, _ := packService(t)

	recordTask(t, f, svc, root, projectmemory.TaskOutcome{
		TaskRef: "task-a", WorkflowRunID: "run-1",
		Title: "an attempt", WhatChanged: "tried something",
		FilesChanged: []string{"internal/store/store.go"},
		Integrated:   false,
	})

	own := svc.Context(f.ctx, projectmemory.PackRequest{
		ProjectID: testProject, RepoPath: root, Role: projectmemory.RoleRepair,
		TaskRef: "task-a", ChangedPaths: []string{"internal/store/store.go"},
	})
	if !strings.Contains(own.Render(), "tried something") {
		t.Error("a task cannot see its own recorded outcome")
	}

	// Even a declared dependent, in the same run, gets nothing: the producer
	// never reached a sharing scope that permits it.
	dependent := svc.Context(f.ctx, projectmemory.PackRequest{
		ProjectID: testProject, RepoPath: root, Role: projectmemory.RoleWorker,
		TaskRef: "task-b", WorkflowRunID: "run-1", UpstreamTaskRefs: []string{"task-a"},
		ChangedPaths: []string{"internal/store/store.go"},
	})
	if strings.Contains(dependent.Render(), "tried something") {
		t.Error("task-scoped knowledge reached a task other than its own")
	}
}

// TestPromotionAfterIntegrationMakesKnowledgeCanonical is the isolated-worktree
// happy path, and it also pins idempotency: promoting twice must leave one
// canonical fact, which is what a duplicate completion callback produces.
func TestPromotionAfterIntegrationMakesKnowledgeCanonical(t *testing.T) {
	f, svc, root, _ := packService(t)

	recordTask(t, f, svc, root, projectmemory.TaskOutcome{
		TaskRef: "task-a", WorkflowRunID: "run-1", Share: domain.ShareWorkflow,
		Title: "switch to GraphQL", WhatChanged: "moved the public API to GraphQL",
		FilesChanged: []string{"internal/store/store.go"},
		Integrated:   false,
		Decisions: []projectmemory.TaskDecision{{
			Statement: "the public API speaks GraphQL", Topic: "api-protocol",
		}},
	})

	first, err := svc.PromoteTaskMemory(f.ctx, testProject, "task-a", provenPromotion("c2"))
	if err != nil {
		t.Fatal(err)
	}
	if first == 0 {
		t.Fatal("promotion moved nothing")
	}
	second, err := svc.PromoteTaskMemory(f.ctx, testProject, "task-a", provenPromotion("c2"))
	if err != nil {
		t.Fatal(err)
	}
	if second != 0 {
		t.Fatalf("a duplicate promotion moved %d more facts; promotion must be exactly-once", second)
	}

	// An unrelated third task, in no run and with no dependency, now sees it —
	// because it is the project's knowledge rather than a branch's.
	after := svc.Context(f.ctx, projectmemory.PackRequest{
		ProjectID: testProject, RepoPath: root, Role: projectmemory.RoleWorker,
		TaskRef: "task-z", ChangedPaths: []string{"internal/store/store.go"},
	})
	if !strings.Contains(after.Render(), "GraphQL") {
		t.Errorf("an integrated decision did not become canonical knowledge:\n%s", after.Render())
	}

	decisions, err := svc.Decisions(f.ctx, projectmemory.KnowledgeFilter{
		ProjectID: testProject, RepoPath: root,
	})
	if err != nil {
		t.Fatal(err)
	}
	graphql := 0
	for _, d := range decisions {
		if strings.Contains(d.Item.Summary, "GraphQL") {
			graphql++
			if d.Share != domain.ShareCanonical {
				t.Errorf("a promoted decision has share %q, want canonical", d.Share)
			}
			if d.SourceTask != "task-a" {
				t.Errorf("a promoted decision lost its provenance: source task %q", d.SourceTask)
			}
		}
	}
	if graphql != 1 {
		t.Fatalf("%d canonical GraphQL decisions after two promotions, want exactly 1", graphql)
	}
}

// TestDiscardedTaskLeavesNoKnowledge covers cancelled and failed work: nothing
// it believed survives.
func TestDiscardedTaskLeavesNoKnowledge(t *testing.T) {
	f, svc, root, _ := packService(t)

	recordTask(t, f, svc, root, projectmemory.TaskOutcome{
		TaskRef: "task-a", WorkflowRunID: "run-1", Share: domain.ShareWorkflow,
		Title: "an abandoned attempt", WhatChanged: "half-migrated the API",
		FilesChanged: []string{"internal/store/store.go"},
		Integrated:   false,
		Decisions: []projectmemory.TaskDecision{{
			Statement: "the public API speaks GraphQL", Topic: "api-protocol",
		}},
	})
	if _, err := svc.DiscardTaskMemory(f.ctx, testProject, "task-a"); err != nil {
		t.Fatal(err)
	}

	entries, err := svc.TaskKnowledge(f.ctx, testProject, "task-a")
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("a discarded task left %d knowledge entries behind", len(entries))
	}
}

// --- decisions --------------------------------------------------------------

// TestDecisionSupersessionKeepsTheHistory is §8: the old decision is retired,
// not deleted, and the two name each other.
func TestDecisionSupersessionKeepsTheHistory(t *testing.T) {
	f, svc, root, _ := packService(t)

	recordTask(t, f, svc, root, projectmemory.TaskOutcome{
		TaskRef: "task-a", Title: "pick REST", WhatChanged: "built the REST API",
		FilesChanged: []string{"internal/server/serve.go"}, Integrated: true, Commit: "c1",
		Decisions: []projectmemory.TaskDecision{{
			Statement: "the public API speaks REST", Topic: "api-protocol",
			Rationale: "it is what the client already spoke",
		}},
	})
	recordTask(t, f, svc, root, projectmemory.TaskOutcome{
		TaskRef: "task-b", Title: "migrate to GraphQL", WhatChanged: "migrated the API",
		FilesChanged: []string{"internal/server/serve.go"}, Integrated: true, Commit: "c2",
		Decisions: []projectmemory.TaskDecision{{
			Statement: "the public API speaks GraphQL", Topic: "api-protocol",
		}},
	})

	active, err := svc.Decisions(f.ctx, projectmemory.KnowledgeFilter{
		ProjectID: testProject, RepoPath: root,
	})
	if err != nil {
		t.Fatal(err)
	}
	var protocols []projectmemory.KnowledgeEntry
	for _, d := range active {
		if strings.Contains(d.Item.Summary, "public API speaks") {
			protocols = append(protocols, d)
		}
	}
	if len(protocols) != 1 {
		t.Fatalf("%d active protocol decisions, want exactly 1 — supersession did not happen", len(protocols))
	}
	if !strings.Contains(protocols[0].Item.Summary, "GraphQL") {
		t.Fatalf("the active decision is %q, want the newer one", protocols[0].Item.Summary)
	}

	// The old one still exists, says what replaced it, and is not served.
	superseded, err := svc.Decisions(f.ctx, projectmemory.KnowledgeFilter{
		ProjectID: testProject, RepoPath: root,
		Statuses: []domain.KnowledgeStatus{domain.KnowledgeSuperseded},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(superseded) != 1 {
		t.Fatalf("%d superseded decisions, want 1 — the history was deleted rather than retired", len(superseded))
	}
	if !strings.Contains(superseded[0].Item.Summary, "REST") {
		t.Fatalf("the superseded decision is %q, want the REST one", superseded[0].Item.Summary)
	}
	if superseded[0].SupersededBy != protocols[0].Item.ID {
		t.Errorf("the superseded decision names %q as its replacement, want %q",
			superseded[0].SupersededBy, protocols[0].Item.ID)
	}

	pack := svc.Context(f.ctx, projectmemory.PackRequest{
		ProjectID: testProject, RepoPath: root, Role: projectmemory.RoleWorker,
		TaskRef: "task-c", ChangedPaths: []string{"internal/server/serve.go"},
	})
	if strings.Contains(pack.Render(), "speaks REST") {
		t.Errorf("a superseded decision was served as current:\n%s", pack.Render())
	}
	if pack.Stats.SupersededExcluded == 0 {
		t.Error("the superseded decision was excluded but not counted")
	}
}

// TestUnintegratedDecisionConflictsRatherThanSupersedes is §13's unresolvable
// case: a branch that disagrees with the project may not silently win, and AO
// may not silently pick either side.
func TestUnintegratedDecisionConflictsRatherThanSupersedes(t *testing.T) {
	f, svc, root, _ := packService(t)

	recordTask(t, f, svc, root, projectmemory.TaskOutcome{
		TaskRef: "task-a", Title: "pick REST", WhatChanged: "built the REST API",
		FilesChanged: []string{"internal/server/serve.go"}, Integrated: true, Commit: "c1",
		Decisions: []projectmemory.TaskDecision{{
			Statement: "the public API speaks REST", Topic: "api-protocol",
		}},
	})
	// An unintegrated branch that disagrees.
	recordTask(t, f, svc, root, projectmemory.TaskOutcome{
		TaskRef: "task-b", WorkflowRunID: "run-1", Share: domain.ShareWorkflow,
		Title: "try GraphQL", WhatChanged: "prototyped GraphQL",
		FilesChanged: []string{"internal/server/serve.go"}, Integrated: false,
		Decisions: []projectmemory.TaskDecision{{
			Statement: "the public API speaks GraphQL", Topic: "api-protocol",
		}},
	})

	conflicting, err := svc.Decisions(f.ctx, projectmemory.KnowledgeFilter{
		ProjectID: testProject, RepoPath: root,
		Statuses: []domain.KnowledgeStatus{domain.KnowledgeConflicting},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(conflicting) != 2 {
		t.Fatalf("%d conflicting decisions, want 2 — AO must mark both sides, not pick one", len(conflicting))
	}
	for _, c := range conflicting {
		if c.ConflictsWith == "" {
			t.Errorf("a conflicting decision %q does not name its counterpart", c.Item.Summary)
		}
	}

	// A Worker is told neither. Choosing for it would be choosing silently.
	worker := svc.Context(f.ctx, projectmemory.PackRequest{
		ProjectID: testProject, RepoPath: root, Role: projectmemory.RoleWorker,
		TaskRef: "task-c", ChangedPaths: []string{"internal/server/serve.go"},
	})
	if strings.Contains(worker.Render(), "public API speaks") {
		t.Errorf("a worker was handed an unresolved contradiction as fact:\n%s", worker.Render())
	}
	if worker.Stats.ConflictingExcluded == 0 {
		t.Error("the contradiction was withheld from the worker but not counted")
	}

	// The Planner is told, and told that it IS a conflict.
	planner := svc.Context(f.ctx, projectmemory.PackRequest{
		ProjectID: testProject, RepoPath: root, Role: projectmemory.RolePlanner,
		ChangedPaths: []string{"internal/server/serve.go"},
	})
	if !strings.Contains(planner.Render(), "CONFLICTING") {
		t.Errorf("the planner was not told about the unresolved contradiction:\n%s", planner.Render())
	}
}

// TestPromotionResolvesTheConflictItCreated closes the loop: once the branch
// lands, the contradiction has an answer and supersession applies.
func TestPromotionResolvesTheConflictItCreated(t *testing.T) {
	f, svc, root, _ := packService(t)

	recordTask(t, f, svc, root, projectmemory.TaskOutcome{
		TaskRef: "task-a", Title: "pick REST", WhatChanged: "built the REST API",
		FilesChanged: []string{"internal/server/serve.go"}, Integrated: true, Commit: "c1",
		Decisions: []projectmemory.TaskDecision{{
			Statement: "the public API speaks REST", Topic: "api-protocol",
		}},
	})
	recordTask(t, f, svc, root, projectmemory.TaskOutcome{
		TaskRef: "task-b", WorkflowRunID: "run-1", Share: domain.ShareWorkflow,
		Title: "try GraphQL", WhatChanged: "prototyped GraphQL",
		FilesChanged: []string{"internal/server/serve.go"}, Integrated: false,
		Decisions: []projectmemory.TaskDecision{{
			Statement: "the public API speaks GraphQL", Topic: "api-protocol",
		}},
	})
	if _, err := svc.PromoteTaskMemory(f.ctx, testProject, "task-b", provenPromotion("c2")); err != nil {
		t.Fatal(err)
	}

	active, err := svc.Decisions(f.ctx, projectmemory.KnowledgeFilter{
		ProjectID: testProject, RepoPath: root,
	})
	if err != nil {
		t.Fatal(err)
	}
	var protocols []string
	for _, d := range active {
		if strings.Contains(d.Item.Summary, "public API speaks") {
			protocols = append(protocols, d.Item.Summary)
		}
	}
	if len(protocols) != 1 || !strings.Contains(protocols[0], "GraphQL") {
		t.Fatalf("after integration the active protocol decisions are %v, want exactly the GraphQL one", protocols)
	}
}

// --- risks ------------------------------------------------------------------

// TestRiskResolutionStopsServingItButKeepsWhoClosedIt is §9.
func TestRiskResolutionStopsServingItButKeepsWhoClosedIt(t *testing.T) {
	f, svc, root, _ := packService(t)

	recordTask(t, f, svc, root, projectmemory.TaskOutcome{
		TaskRef: "task-a", Title: "add the queue", WhatChanged: "added the queue",
		FilesChanged: []string{"internal/store/store.go"}, Integrated: true, Commit: "c1",
		Risks: []projectmemory.TaskRisk{{
			Statement: "the queue is unbounded under load", Topic: "queue-bounds",
			Modules: []string{"internal/store"},
		}},
	})

	open, err := svc.Risks(f.ctx, projectmemory.KnowledgeFilter{ProjectID: testProject, RepoPath: root})
	if err != nil {
		t.Fatal(err)
	}
	if len(open) != 1 {
		t.Fatalf("%d open risks, want 1", len(open))
	}
	subject := open[0].Subject

	recordTask(t, f, svc, root, projectmemory.TaskOutcome{
		TaskRef: "task-b", Title: "bound the queue", WhatChanged: "gave the queue a bound",
		FilesChanged: []string{"internal/store/store.go"}, Integrated: true, Commit: "c2",
		ResolvesRisks: []string{subject},
	})

	stillOpen, err := svc.Risks(f.ctx, projectmemory.KnowledgeFilter{ProjectID: testProject, RepoPath: root})
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range stillOpen {
		if r.Subject == subject {
			t.Fatal("a resolved risk is still carried as open")
		}
	}

	resolved, err := svc.Risks(f.ctx, projectmemory.KnowledgeFilter{
		ProjectID: testProject, RepoPath: root,
		Statuses: []domain.KnowledgeStatus{domain.KnowledgeResolved},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(resolved) != 1 {
		t.Fatalf("%d resolved risks, want 1 — resolution must retire, never delete", len(resolved))
	}
	if resolved[0].ResolvedBy != "task-b" {
		t.Errorf("the resolved risk says it was closed by %q, want task-b", resolved[0].ResolvedBy)
	}

	pack := svc.Context(f.ctx, projectmemory.PackRequest{
		ProjectID: testProject, RepoPath: root, Role: projectmemory.RoleWorker,
		TaskRef: "task-c", ChangedPaths: []string{"internal/store/store.go"},
	})
	if strings.Contains(pack.Render(), "unbounded under load") {
		t.Errorf("a resolved risk was served as current:\n%s", pack.Render())
	}
}

// --- lifecycle and hygiene ---------------------------------------------------

// TestRecordingTheSameOutcomeTwiceIsIdempotent is the duplicate-completion and
// crash-between-record-and-promote case.
func TestRecordingTheSameOutcomeTwiceIsIdempotent(t *testing.T) {
	f, svc, root, _ := packService(t)

	out := projectmemory.TaskOutcome{
		TaskRef: "task-a", Title: "add the queue", WhatChanged: "added the queue",
		FilesChanged: []string{"internal/store/store.go"}, Integrated: true, Commit: "c1",
		Decisions: []projectmemory.TaskDecision{{
			Statement: "the queue is a table", Topic: "queue-storage",
		}},
		Risks: []projectmemory.TaskRisk{{
			Statement: "the queue is unbounded", Topic: "queue-bounds",
		}},
	}
	recordTask(t, f, svc, root, out)
	recordTask(t, f, svc, root, out)

	entries, err := svc.TaskKnowledge(f.ctx, testProject, "task-a")
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 3 {
		summaries := make([]string, 0, len(entries))
		for _, e := range entries {
			summaries = append(summaries, e.Item.Summary+" ["+string(e.Status)+"]")
		}
		t.Fatalf("recording the same outcome twice produced %d facts, want 3: %v", len(entries), summaries)
	}
	for _, e := range entries {
		if e.Status != domain.KnowledgeActive {
			t.Errorf("re-recording an outcome retired its own fact %q as %q", e.Item.Summary, e.Status)
		}
	}
}

// TestNoTranscriptOrReasoningIsPersisted is the §3 refusal, checked at the
// storage layer rather than trusted to the callers.
func TestNoTranscriptOrReasoningIsPersisted(t *testing.T) {
	f, svc, root, _ := packService(t)

	transcript := strings.Repeat("assistant: let me think about this step by step...\n", 200)
	recordTask(t, f, svc, root, projectmemory.TaskOutcome{
		TaskRef: "task-a", Title: "a task",
		WhatChanged: "changed the store", Why: "the scheduler needed it",
		FilesChanged: []string{"internal/store/store.go"}, Integrated: true, Commit: "c1",
		Decisions: []projectmemory.TaskDecision{{
			Statement: "the queue is a table", Topic: "queue-storage",
			// A caller trying to smuggle a transcript in through the one free
			// prose field there is. It must be bounded, not stored whole.
			Rationale: transcript,
		}},
	})

	entries, err := svc.TaskKnowledge(f.ctx, testProject, "task-a")
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if len(e.Item.Content) > domain.MaxProjectMemoryContent {
			t.Errorf("fact %q stores %d bytes, over the %d cap",
				e.Item.Summary, len(e.Item.Content), domain.MaxProjectMemoryContent)
		}
		if len(e.Item.Content) > projectmemory.MaxDecisionRationale+domain.MaxProjectMemorySummary {
			t.Errorf("fact %q stored %d bytes; a rationale is bounded at %d and cannot become a transcript",
				e.Item.Summary, len(e.Item.Content), projectmemory.MaxDecisionRationale)
		}
		if strings.Contains(e.Item.Content, transcript) {
			t.Errorf("fact %q stored the transcript whole", e.Item.Summary)
		}
		if len(e.Item.Summary) > domain.MaxProjectMemorySummary {
			t.Errorf("fact %q has a summary of %d bytes, over the %d cap",
				e.Item.Summary, len(e.Item.Summary), domain.MaxProjectMemorySummary)
		}
	}
}

// TestCompactionRetiresOldOutcomesPerModuleAndKeepsProvenance is §12.
func TestCompactionRetiresOldOutcomesPerModuleAndKeepsProvenance(t *testing.T) {
	f, svc, root, _ := packService(t)

	const busy = projectmemory.MaxTaskResultsPerModule + 6
	for i := range busy {
		recordTask(t, f, svc, root, projectmemory.TaskOutcome{
			TaskRef: fmt.Sprintf("payments-%02d", i), Title: fmt.Sprintf("payments change %d", i),
			WhatChanged:  fmt.Sprintf("touched payments (%d)", i),
			FilesChanged: []string{"internal/store/store.go"},
			Modules:      []string{"internal/store"},
			Integrated:   true, Commit: "c1",
		})
	}
	// A different module, recorded last, must survive the busy one's churn.
	recordTask(t, f, svc, root, projectmemory.TaskOutcome{
		TaskRef: "server-1", Title: "server change", WhatChanged: "touched the server",
		FilesChanged: []string{"internal/server/serve.go"},
		Modules:      []string{"internal/server"},
		Integrated:   true, Commit: "c1",
	})

	active, err := svc.Knowledge(f.ctx, projectmemory.KnowledgeFilter{
		ProjectID: testProject, RepoPath: root,
		Types: []domain.ProjectMemoryType{domain.MemoryTypeTaskResult},
		Limit: 500,
	})
	if err != nil {
		t.Fatal(err)
	}
	payments, server := 0, 0
	for _, e := range active {
		switch {
		case strings.Contains(e.Item.Summary, "payments change"):
			payments++
		case strings.Contains(e.Item.Summary, "server change"):
			server++
		}
	}
	if payments > projectmemory.MaxTaskResultsPerModule {
		t.Errorf("%d active payments outcomes, over the per-module bound of %d",
			payments, projectmemory.MaxTaskResultsPerModule)
	}
	if server != 1 {
		t.Errorf("the quiet module kept %d outcomes, want 1 — compaction must be per scope, not global", server)
	}

	// The retired ones still exist, with their provenance intact.
	obsolete, err := svc.Knowledge(f.ctx, projectmemory.KnowledgeFilter{
		ProjectID: testProject, RepoPath: root,
		Types:    []domain.ProjectMemoryType{domain.MemoryTypeTaskResult},
		Statuses: []domain.KnowledgeStatus{domain.KnowledgeObsolete},
		Limit:    500,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(obsolete) == 0 {
		t.Fatal("compaction deleted the outcomes it retired")
	}
	for _, e := range obsolete {
		if e.SourceTask == "" || len(e.Item.SourcePaths) == 0 {
			t.Errorf("a compacted outcome %q lost its provenance", e.Item.Summary)
		}
	}
}

// TestKnowledgeStaysInsideTheRoleBudget is §19: shared knowledge competes
// inside the P2-B budget, never beside it.
func TestKnowledgeStaysInsideTheRoleBudget(t *testing.T) {
	f, svc, root, _ := packService(t)

	for i := range 30 {
		recordTask(t, f, svc, root, projectmemory.TaskOutcome{
			TaskRef: fmt.Sprintf("task-%02d", i), Title: fmt.Sprintf("change %d", i),
			WhatChanged:  strings.Repeat("a long account of what changed. ", 40),
			FilesChanged: []string{"internal/store/store.go"},
			Modules:      []string{"internal/store"},
			Integrated:   true, Commit: "c1",
		})
	}

	budget := projectmemory.PackBudget{MaxBytes: 4096, MaxItems: 12}
	pack := svc.Context(f.ctx, projectmemory.PackRequest{
		ProjectID: testProject, RepoPath: root, Role: projectmemory.RoleWorker,
		TaskRef: "task-z", ChangedPaths: []string{"internal/store/store.go"},
		Modules: []string{"internal/store"}, Budget: budget,
	})
	if pack.Stats.SelectedBytes > budget.MaxBytes {
		t.Errorf("the pack is %d bytes, over its %d budget", pack.Stats.SelectedBytes, budget.MaxBytes)
	}
	if pack.Stats.SelectedItems > budget.MaxItems {
		t.Errorf("the pack carries %d facts, over its %d bound", pack.Stats.SelectedItems, budget.MaxItems)
	}
	if pack.Stats.DroppedItems == 0 {
		t.Error("thirty task outcomes fitted a 4 KB budget, so the budget never bound")
	}
	if pack.Stats.KnowledgeBytes > pack.Stats.SelectedBytes {
		t.Error("shared knowledge was counted as an addition to the pack rather than a part of it")
	}
}

// TestSelectionIsDeterministicWithSharedKnowledge protects the frozen-context
// manifest: the same request must produce the same pack, or a manifest cannot
// be reproduced after a restart.
func TestSelectionIsDeterministicWithSharedKnowledge(t *testing.T) {
	f, svc, root, _ := packService(t)

	for i := range 6 {
		recordTask(t, f, svc, root, projectmemory.TaskOutcome{
			TaskRef: fmt.Sprintf("task-%02d", i), Title: fmt.Sprintf("change %d", i),
			WhatChanged:  fmt.Sprintf("changed the store (%d)", i),
			FilesChanged: []string{"internal/store/store.go"},
			Modules:      []string{"internal/store"}, Integrated: true, Commit: "c1",
			Decisions: []projectmemory.TaskDecision{{
				Statement: fmt.Sprintf("decision %d stands", i), Topic: fmt.Sprintf("topic-%d", i),
			}},
		})
	}
	req := projectmemory.PackRequest{
		ProjectID: testProject, RepoPath: root, Role: projectmemory.RoleWorker,
		TaskRef: "task-z", ChangedPaths: []string{"internal/store/store.go"},
		Modules: []string{"internal/store"},
	}
	first := svc.Context(f.ctx, req)
	second := svc.Context(f.ctx, req)
	if first.Digest != second.Digest {
		t.Fatalf("two identical requests produced different packs (%s vs %s); a frozen manifest could not be reproduced",
			first.Digest, second.Digest)
	}
}

// TestLegacyTaskMemoryWithoutLifecycleStillReads is §22: memory written before
// P2-C carries no status and must keep working.
func TestLegacyTaskMemoryWithoutLifecycleStillReads(t *testing.T) {
	f, svc, root, indexed := packService(t)
	repoID := indexed.RepoID

	// A P2-A-shaped row: canonical, no status, no subject, no share.
	legacy := domain.ProjectMemoryItem{
		Key: domain.ProjectMemoryKey{
			ProjectID: testProject, RepoID: repoID,
			Type: domain.MemoryTypeDecision, Scope: domain.MemoryScopeRepository,
			Key: "legacy-decision",
		},
		Origin:       domain.OriginCanonical,
		Summary:      "the daemon owns all state",
		Content:      "the daemon owns all state",
		SourcePaths:  []string{"internal/store/store.go"},
		SourceCommit: "c1", Confidence: 0.9, State: domain.MemoryStateValid,
	}.Normalized()
	if _, err := f.store.PutProjectMemoryItem(f.ctx, legacy, f.now()); err != nil {
		t.Fatal(err)
	}

	entries, err := svc.Decisions(f.ctx, projectmemory.KnowledgeFilter{
		ProjectID: testProject, RepoPath: root,
	})
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, e := range entries {
		if e.Item.ID == legacy.ID {
			found = true
			if e.Status != domain.KnowledgeActive {
				t.Errorf("a pre-P2-C decision reads as %q, want active", e.Status)
			}
		}
	}
	if !found {
		t.Fatal("a pre-P2-C decision is no longer readable")
	}

	pack := svc.Context(f.ctx, projectmemory.PackRequest{
		ProjectID: testProject, RepoPath: root, Role: projectmemory.RoleWorker,
		ChangedPaths: []string{"internal/store/store.go"},
	})
	if !strings.Contains(pack.Render(), "daemon owns all state") {
		t.Errorf("a pre-P2-C decision stopped being served:\n%s", pack.Render())
	}
}

// TestGraphRecordsTaskLineage is §20, over the LocalGraph that ships by
// default — the whole point being that none of this needs Grae or Graphify.
func TestGraphRecordsTaskLineage(t *testing.T) {
	f, svc, root, indexed := packService(t)

	recordTask(t, f, svc, root, projectmemory.TaskOutcome{
		TaskRef: "task-a", Title: "add the queue", WhatChanged: "added the queue",
		FilesChanged: []string{"internal/store/store.go"},
		Modules:      []string{"internal/store"}, Integrated: true, Commit: "c1",
		DependsOnTasks: []string{"task-0"},
		Decisions: []projectmemory.TaskDecision{{
			Statement: "the queue is a table", Topic: "queue-storage",
		}},
		Risks: []projectmemory.TaskRisk{{
			Statement: "the queue is unbounded", Topic: "queue-bounds",
			Modules: []string{"internal/store"},
		}},
	})

	rels, err := svc.Graph().Neighbors(f.ctx, projectmemory.GraphQuery{
		ProjectID: testProject, RepoID: indexed.RepoID,
		Node: domain.ProjectMemoryNode{Kind: domain.NodeTask, Key: "task-a"},
	})
	if err != nil {
		t.Fatal(err)
	}
	kinds := map[domain.ProjectMemoryRelationKind]int{}
	for _, r := range rels {
		kinds[r.Kind]++
	}
	for _, want := range []domain.ProjectMemoryRelationKind{
		domain.RelationProduced, domain.RelationChanged,
		domain.RelationAffects, domain.RelationDependsOn,
	} {
		if kinds[want] == 0 {
			t.Errorf("the graph records no %q edge from the task", want)
		}
	}
}

// TestGraphUnavailableDoesNotFailRecording is §21: a graph mirror that is down
// must not cost a task its knowledge. The items are the durable truth; the
// edges are an index a later pass can rebuild.
func TestGraphUnavailableDoesNotFailRecording(t *testing.T) {
	f, _, root, _ := packService(t)
	// The same store, read and written through a service whose graph backend
	// is unreachable. Indexing already happened over a healthy graph, so what
	// is under test is exactly the claim: an outage of the MIRROR must not
	// cost a task the knowledge itself.
	svc := projectmemory.NewService(f.store,
		projectmemory.WithGraph(projectmemory.UnavailableGraph{Backend: "grae"}))

	recordTask(t, f, svc, root, projectmemory.TaskOutcome{
		TaskRef: "task-a", Title: "add the queue", WhatChanged: "added the queue",
		FilesChanged: []string{"internal/store/store.go"}, Integrated: true, Commit: "c1",
		Decisions: []projectmemory.TaskDecision{{
			Statement: "the queue is a table", Topic: "queue-storage",
		}},
	})

	entries, err := svc.TaskKnowledge(f.ctx, testProject, "task-a")
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) < 2 {
		t.Fatalf("an unavailable graph cost the task its knowledge: %d facts recorded", len(entries))
	}
}

// TestFullReindexDoesNotRetirePromotedKnowledge is a regression test for the
// most dangerous shape of bug this subsystem can have.
//
// The generation sweep retires canonical facts a completed full pass did not
// re-derive, on the premise that a walk which did not produce a fact has
// shown its subject is gone. That premise is false for task knowledge: no walk
// ever produces a decision or a risk, so every one of them looked "not
// re-derived" and the first full re-index after a promotion silently retired
// the project's entire decision and risk memory — at the exact moment memory
// looked healthiest.
func TestFullReindexDoesNotRetirePromotedKnowledge(t *testing.T) {
	f, svc, root, _ := packService(t)

	recordTask(t, f, svc, root, projectmemory.TaskOutcome{
		TaskRef: "task-a", Title: "add the queue", WhatChanged: "added a durable queue",
		FilesChanged: []string{"internal/store/store.go"},
		Modules:      []string{"internal/store"}, Integrated: true, Commit: "c1",
		Decisions: []projectmemory.TaskDecision{{
			Statement: "the queue is a table, not a JSON blob", Topic: "queue-storage",
		}},
		Risks: []projectmemory.TaskRisk{{
			Statement: "the queue is unbounded under load", Topic: "queue-bounds",
		}},
	})

	// A second full pass, at a later generation, exactly as a rebuild or a
	// full lifecycle sync would run it.
	if _, err := svc.Index(f.ctx, projectmemory.IndexRequest{
		ProjectID: testProject, RepoPath: root, Commit: "c2", Branch: "main",
	}); err != nil {
		t.Fatal(err)
	}

	entries, err := svc.TaskKnowledge(f.ctx, testProject, "task-a")
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 3 {
		t.Fatalf("a full re-index left %d of 3 facts, want all of them", len(entries))
	}
	for _, e := range entries {
		if !e.Item.State.Authoritative() {
			t.Errorf("a full re-index retired %q as %q: a walk that never produces task knowledge "+
				"has no standing to retire it", e.Item.Summary, e.Item.State)
		}
		if e.Status != domain.KnowledgeActive {
			t.Errorf("a full re-index moved %q to status %q", e.Item.Summary, e.Status)
		}
	}

	pack := svc.Context(f.ctx, projectmemory.PackRequest{
		ProjectID: testProject, RepoPath: root, Role: projectmemory.RoleWorker,
		TaskRef: "task-b", ChangedPaths: []string{"internal/store/store.go"},
	})
	if !strings.Contains(pack.Render(), "not a JSON blob") {
		t.Errorf("the decision stopped being served after a full re-index:\n%s", pack.Render())
	}
}

// TestMeasureSharedKnowledgeTokenImpact is the P2-C §19 measurement, run as a
// test so the number is produced by the real selection path rather than by a
// script that approximates it.
//
// It is an assertion as well as a measurement: the related task must pay for
// the knowledge it receives, and the unrelated task must pay nothing. A build
// where both numbers drift toward each other has lost the property, whatever
// the absolute figures are.
func TestMeasureSharedKnowledgeTokenImpact(t *testing.T) {
	f, svc, root, _ := packService(t)

	// Twelve prior tasks in one module, the shape a real project reaches after
	// a few weeks: enough history that carrying all of it would be expensive.
	for i := range 12 {
		recordTask(t, f, svc, root, projectmemory.TaskOutcome{
			TaskRef: fmt.Sprintf("store-%02d", i),
			Title:   fmt.Sprintf("store change %d", i),
			WhatChanged: fmt.Sprintf("reworked the store queue, pass %d, "+
				"including the write path and its bounds", i),
			FilesChanged: []string{"internal/store/store.go", "internal/store/query.go"},
			Modules:      []string{"internal/store"}, Integrated: true, Commit: "c1",
			Decisions: []projectmemory.TaskDecision{{
				Statement: fmt.Sprintf("store rule %d: the queue write path stays synchronous", i),
				Topic:     fmt.Sprintf("store-rule-%d", i),
				Evidence:  []string{"internal/store/store.go"},
			}},
			Risks: []projectmemory.TaskRisk{{
				Statement: fmt.Sprintf("store risk %d: the queue is unbounded under load", i),
				Topic:     fmt.Sprintf("store-risk-%d", i),
				Evidence:  []string{"internal/store/store.go"},
			}},
		})
	}

	related := svc.Context(f.ctx, projectmemory.PackRequest{
		ProjectID: testProject, RepoPath: root, Role: projectmemory.RoleWorker,
		TaskRef: "task-b", ChangedPaths: []string{"internal/store/store.go"},
		Modules: []string{"internal/store"},
	})
	unrelated := svc.Context(f.ctx, projectmemory.PackRequest{
		ProjectID: testProject, RepoPath: root, Role: projectmemory.RoleWorker,
		TaskRef: "task-c", ChangedPaths: []string{"cmd/app/main.go"},
		Modules: []string{"cmd/app"},
	})

	t.Logf("related   task B: pack %d bytes (~%d tokens), %d facts, %d of them shared knowledge (%d bytes)",
		related.Stats.SelectedBytes, related.Stats.SelectedTokens, related.Stats.SelectedItems,
		related.Stats.SharedSelected, related.Stats.KnowledgeBytes)
	t.Logf("unrelated task C: pack %d bytes (~%d tokens), %d facts, %d of them shared knowledge (%d bytes); "+
		"%d shared facts were considered and excluded as irrelevant",
		unrelated.Stats.SelectedBytes, unrelated.Stats.SelectedTokens, unrelated.Stats.SelectedItems,
		unrelated.Stats.SharedSelected, unrelated.Stats.KnowledgeBytes,
		unrelated.Stats.SharedIrrelevantExcluded)

	if unrelated.Stats.KnowledgeBytes != 0 {
		t.Errorf("the unrelated task paid %d bytes for another task's history", unrelated.Stats.KnowledgeBytes)
	}
	if related.Stats.KnowledgeBytes == 0 {
		t.Error("the related task received nothing from twelve prior tasks in its own module")
	}
	// The gate must actually have had something to reject, or the measurement
	// is of an empty store rather than of the policy.
	if unrelated.Stats.SharedIrrelevantExcluded < 12 {
		t.Errorf("only %d shared facts were excluded from the unrelated task; "+
			"the gate did not see the history it was supposed to filter",
			unrelated.Stats.SharedIrrelevantExcluded)
	}
}

// --- derived knowledge (P2-C §15, closed) ------------------------------------
//
// The three tests below are about facts the workflow boundary DERIVED from
// durable rows — a reviewer's unresolved thread, a QA finding, a plan
// amendment — rather than from a caller who typed them in. Memory does not know
// or care where a fact came from, which is the point: the lifecycle that
// governs a hand-supplied decision governs a derived one identically.

// TestDerivedReviewerRiskReachesTheRelatedTaskOnly is the headline claim,
// re-stated for facts nobody supplied by hand: a risk derived from a reviewer
// thread on one file reaches the next task in that file and nothing else.
func TestDerivedReviewerRiskReachesTheRelatedTaskOnly(t *testing.T) {
	f, svc, root, _ := packService(t)

	recordTask(t, f, svc, root, projectmemory.TaskOutcome{
		TaskRef: "task-a", Title: "harden the store", WhatChanged: "made the store write through a queue",
		FilesChanged: []string{"internal/store/store.go"}, Modules: []string{"internal/store"},
		Integrated: true, Commit: "c1",
		Decisions: []projectmemory.TaskDecision{{
			Statement: "the acceptance criterion \"the tree stays dirty\" no longer applies to this work",
			Rationale: "the state it describes was committed in 70296042b (approved by a human)",
			Topic:     "acceptance-criterion:task-a:9f1c0c2b1a44",
			Evidence:  []string{"internal/store/store.go"},
		}},
		Risks: []projectmemory.TaskRisk{{
			Statement: "a reviewer thread on internal/store/store.go:42 is still unresolved",
			Topic:     "review-thread:th-1",
			Evidence:  []string{"internal/store/store.go"},
		}},
	})

	related := svc.Context(f.ctx, projectmemory.PackRequest{
		ProjectID: testProject, RepoPath: root, Role: projectmemory.RoleWorker,
		TaskRef: "task-b", ChangedPaths: []string{"internal/store/store.go"},
		Modules: []string{"internal/store"},
	}).Render()
	if !strings.Contains(related, "still unresolved") {
		t.Errorf("the next task in the same file did not receive the derived reviewer risk:\n%s", related)
	}
	if !strings.Contains(related, "no longer applies") {
		t.Errorf("the next task in the same file did not receive the derived decision:\n%s", related)
	}

	unrelated := svc.Context(f.ctx, projectmemory.PackRequest{
		ProjectID: testProject, RepoPath: root, Role: projectmemory.RoleWorker,
		TaskRef: "task-c", ChangedPaths: []string{"cmd/app/main.go"}, Modules: []string{"cmd/app"},
	}).Render()
	for _, leaked := range []string{"still unresolved", "no longer applies"} {
		if strings.Contains(unrelated, leaked) {
			t.Errorf("a task in another module received %q:\n%s", leaked, unrelated)
		}
	}
}

// TestRiskResolvesByTheTopicItsSourceRaisedItUnder is the mechanism that lets a
// derivation close what it opened.
//
// The boundary that derives a risk from a reviewer thread knows the thread's
// id, not memory's subject scheme. Naming the topic has to be enough — without
// it, a fixed finding could only ever fall silent, leaving the risk it raised
// open forever.
func TestRiskResolvesByTheTopicItsSourceRaisedItUnder(t *testing.T) {
	f, svc, root, _ := packService(t)

	recordTask(t, f, svc, root, projectmemory.TaskOutcome{
		TaskRef: "task-a", Title: "add the queue", WhatChanged: "added the queue",
		FilesChanged: []string{"internal/store/store.go"}, Integrated: true, Commit: "c1",
		Risks: []projectmemory.TaskRisk{{
			Statement: "a reviewer thread on internal/store/store.go:42 is still unresolved",
			Topic:     "review-thread:th-1",
		}},
	})

	// The later task names the SAME topic its source used, and nothing else.
	recordTask(t, f, svc, root, projectmemory.TaskOutcome{
		TaskRef: "task-b", Title: "answer the reviewer", WhatChanged: "addressed the thread",
		FilesChanged: []string{"internal/store/store.go"}, Integrated: true, Commit: "c2",
		ResolvesRisks: []string{"review-thread:th-1"},
	})

	open, err := svc.Risks(f.ctx, projectmemory.KnowledgeFilter{ProjectID: testProject, RepoPath: root})
	if err != nil {
		t.Fatal(err)
	}
	if len(open) != 0 {
		t.Fatalf("%d risks still open, want 0 — a topic must resolve the risk it raised: %+v", len(open), open)
	}
	resolved, err := svc.Risks(f.ctx, projectmemory.KnowledgeFilter{
		ProjectID: testProject, RepoPath: root,
		Statuses: []domain.KnowledgeStatus{domain.KnowledgeResolved},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(resolved) != 1 || resolved[0].ResolvedBy != "task-b" {
		t.Fatalf("resolution did not keep who closed it: %+v", resolved)
	}
}

// TestDerivedKnowledgeFromAnUnintegratedWorktreeStaysOutOfCanonical: deriving a
// fact from a durable row says nothing about whether the project HAS the work.
// An unintegrated task's derived risk is still its own, and a task in the same
// module that never declared a dependency on it must not see it.
func TestDerivedKnowledgeFromAnUnintegratedWorktreeStaysOutOfCanonical(t *testing.T) {
	f, svc, root, _ := packService(t)

	recordTask(t, f, svc, root, projectmemory.TaskOutcome{
		TaskRef: "task-a", WorkflowRunID: "wf-1", Title: "work in a worktree",
		WhatChanged:  "changed the store on an ao/* branch",
		FilesChanged: []string{"internal/store/store.go"}, Modules: []string{"internal/store"},
		Integrated: false, Share: domain.ShareWorkflow, Commit: "c1",
		Risks: []projectmemory.TaskRisk{{
			Statement: "a reviewer thread on internal/store/store.go:42 is still unresolved",
			Topic:     "review-thread:th-1", Evidence: []string{"internal/store/store.go"},
		}},
	})

	sibling := svc.Context(f.ctx, projectmemory.PackRequest{
		ProjectID: testProject, RepoPath: root, Role: projectmemory.RoleWorker,
		TaskRef: "task-sibling", ChangedPaths: []string{"internal/store/store.go"},
		Modules: []string{"internal/store"},
	}).Render()
	if strings.Contains(sibling, "still unresolved") {
		t.Errorf("a sibling read an unintegrated task's derived risk:\n%s", sibling)
	}
}

// provenPromotion is the proof a test's promotion stands on.
//
// Every existing promotion test predates P2-D and was written when promotion
// took a bare commit. They are asserting what promotion DOES with facts it is
// allowed to promote, so they get a proof that is provable — the refusal path
// has its own tests (see promotion_proof_test.go), and weakening these to go
// through it would silently stop them testing anything.
func provenPromotion(commit string) domain.MemoryPromotionProof {
	return domain.MemoryPromotionProof{
		Provable:             true,
		MutationProvenanceID: "wmp-test",
		VerifiedCommit:       commit,
		IntegratedCommit:     commit,
		RepoIdentity:         domain.RepoIdentity("root_test"),
		Placement:            domain.MutationPlacementDirectBranch,
		Method:               domain.IntegrationDirectCommit,
	}
}
