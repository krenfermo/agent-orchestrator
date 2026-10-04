package projectmemory_test

import (
	"strings"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/projectmemory"
)

// manifest_test.go — context freezing (P2-C §16).
//
// The claim under test is narrow and checkable: after a dispatch, AO can say
// which facts that dispatch was given, by identity, and can say it again after
// a restart with the same answer. Everything else about a manifest — that it
// holds no prompt, that it survives its facts being superseded — falls out of
// storing identities instead of content, and both are tested here because both
// are properties a future change could quietly lose.

func TestContextManifestRecordsWhatAnExecutionWasTold(t *testing.T) {
	f, prov, root := provFixture(t, projectmemory.ModeAssisted)

	out := prov.Provision(f.ctx, projectmemory.ProvisionRequest{
		ProjectID: testProject, RepoPath: root, Role: projectmemory.RoleWorker,
		TaskRef: "task-a", WorkflowRunID: "run-1",
		ChangedPaths: []string{"internal/store/store.go"},
	})
	if !out.Attached() {
		t.Fatalf("nothing was attached, so there is no manifest to check: %s", out.Metrics.FallbackReason)
	}

	svc := projectmemory.NewService(f.store)
	manifests, err := svc.ContextManifests(f.ctx, testProject, "task-a", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(manifests) != 1 {
		t.Fatalf("%d manifests recorded, want 1", len(manifests))
	}
	m := manifests[0]
	switch {
	case m.Role != string(projectmemory.RoleWorker):
		t.Errorf("manifest role = %q, want worker", m.Role)
	case m.WorkflowRunID != "run-1":
		t.Errorf("manifest run = %q, want run-1", m.WorkflowRunID)
	case m.PackDigest != out.Pack.Digest:
		t.Errorf("manifest digest %q does not match the pack it describes (%q)", m.PackDigest, out.Pack.Digest)
	case m.PolicyVersion != projectmemory.PackPolicyVersion:
		t.Errorf("manifest policy version = %d, want %d", m.PolicyVersion, projectmemory.PackPolicyVersion)
	case len(m.ItemIDs) != out.Pack.Stats.SelectedItems:
		t.Errorf("manifest names %d facts, the pack carried %d", len(m.ItemIDs), out.Pack.Stats.SelectedItems)
	case m.SelectedBytes != out.Pack.Stats.SelectedBytes:
		t.Errorf("manifest reports %d bytes, the pack was %d", m.SelectedBytes, out.Pack.Stats.SelectedBytes)
	}

	// It stores identities, not text. A manifest that had copied the facts
	// would go out of date the moment one of them was superseded.
	for _, id := range m.ItemIDs {
		if strings.Contains(id, " ") {
			t.Errorf("manifest entry %q looks like content rather than an identity", id)
		}
	}
}

func TestReprovisioningTheSameContextIsOneManifest(t *testing.T) {
	f, prov, root := provFixture(t, projectmemory.ModeAssisted)
	req := projectmemory.ProvisionRequest{
		ProjectID: testProject, RepoPath: root, Role: projectmemory.RoleWorker,
		TaskRef: "task-a", WorkflowRunID: "run-1",
		ChangedPaths: []string{"internal/store/store.go"},
	}
	first := prov.Provision(f.ctx, req)
	second := prov.Provision(f.ctx, req)
	if first.Pack.Digest != second.Pack.Digest {
		t.Fatalf("the same request produced two different packs; a manifest could not be reproduced")
	}

	svc := projectmemory.NewService(f.store)
	manifests, err := svc.ContextManifests(f.ctx, testProject, "task-a", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(manifests) != 1 {
		t.Fatalf("%d manifests after re-provisioning the same context, want 1: "+
			"a restart that reproduces the same answer must not look like a second observation", len(manifests))
	}
}

func TestManifestExpandsAndNamesFactsThatAreGone(t *testing.T) {
	f, prov, root := provFixture(t, projectmemory.ModeAssisted)
	out := prov.Provision(f.ctx, projectmemory.ProvisionRequest{
		ProjectID: testProject, RepoPath: root, Role: projectmemory.RoleWorker,
		TaskRef: "task-a", ChangedPaths: []string{"internal/store/store.go"},
	})
	if !out.Attached() {
		t.Fatalf("nothing attached: %s", out.Metrics.FallbackReason)
	}

	svc := projectmemory.NewService(f.store)
	manifests, err := svc.ContextManifests(f.ctx, testProject, "task-a", "")
	if err != nil {
		t.Fatal(err)
	}
	m := manifests[0]

	// Manifests name identities, so one that has since been forgotten is
	// reported as missing rather than silently dropped — "the Worker was told
	// something AO has since discarded" is the most interesting thing a
	// manifest can say, and a shorter list would hide it.
	m.ItemIDs = append(append([]string(nil), m.ItemIDs...), "0000000000000000000000000000dead")

	items, missing, err := svc.ManifestItems(f.ctx, m)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != len(m.ItemIDs)-1 {
		t.Errorf("expanded %d facts from %d ids", len(items), len(m.ItemIDs))
	}
	if len(missing) != 1 || missing[0] != "0000000000000000000000000000dead" {
		t.Errorf("missing = %v, want the one id that no longer exists", missing)
	}
}

func TestManifestSurvivesItsFactsBeingSuperseded(t *testing.T) {
	f, prov, root := provFixture(t, projectmemory.ModeAssisted)
	svc := projectmemory.NewService(f.store)

	recordTask(t, f, svc, root, projectmemory.TaskOutcome{
		ProjectID: testProject, RepoPath: root,
		TaskRef: "task-a", Title: "pick REST", WhatChanged: "built the REST API",
		FilesChanged: []string{"internal/store/store.go"}, Integrated: true, Commit: "c1",
		Decisions: []projectmemory.TaskDecision{{
			Statement: "the public API speaks REST", Topic: "api-protocol",
		}},
	})
	out := prov.Provision(f.ctx, projectmemory.ProvisionRequest{
		ProjectID: testProject, RepoPath: root, Role: projectmemory.RoleWorker,
		TaskRef: "task-b", ChangedPaths: []string{"internal/store/store.go"},
	})
	if !strings.Contains(out.Render(), "speaks REST") {
		t.Fatalf("the decision under test was never in the pack:\n%s", out.Render())
	}

	// A later task changes the project's mind.
	recordTask(t, f, svc, root, projectmemory.TaskOutcome{
		ProjectID: testProject, RepoPath: root,
		TaskRef: "task-c", Title: "migrate", WhatChanged: "migrated the API",
		FilesChanged: []string{"internal/store/store.go"}, Integrated: true, Commit: "c2",
		Decisions: []projectmemory.TaskDecision{{
			Statement: "the public API speaks GraphQL", Topic: "api-protocol",
		}},
	})

	manifests, err := svc.ContextManifests(f.ctx, testProject, "task-b", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(manifests) == 0 {
		t.Fatal("the manifest is gone")
	}
	items, _, err := svc.ManifestItems(f.ctx, manifests[0])
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, it := range items {
		if strings.Contains(it.Item.Summary, "speaks REST") {
			found = true
			if it.Status != domain.KnowledgeSuperseded {
				t.Errorf("the manifest's decision reads as %q, want superseded", it.Status)
			}
		}
	}
	if !found {
		t.Fatal("a manifest stopped naming a fact once that fact was superseded; " +
			"it must still say what the execution was told")
	}
}

func TestDispatchWithNoExecutionIdentityRecordsNoManifest(t *testing.T) {
	f, prov, root := provFixture(t, projectmemory.ModeAssisted)
	out := prov.Provision(f.ctx, projectmemory.ProvisionRequest{
		ProjectID: testProject, RepoPath: root, Role: projectmemory.RoleWorker,
	})
	if !out.Attached() {
		t.Skip("nothing was attached, so there is nothing to have recorded")
	}
	svc := projectmemory.NewService(f.store)
	manifests, err := svc.ContextManifests(f.ctx, testProject, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(manifests) != 0 {
		t.Errorf("%d manifests recorded for a dispatch that named no execution; "+
			"such a row could never be looked up", len(manifests))
	}
}
