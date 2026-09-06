package projectmemory_test

import (
	"context"
	"strings"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/projectmemory"
)

// provision_external_test.go — P4-F at the dispatch boundary.
//
// The guarantees under test are the ones that make external context safe to
// turn on: it never fails a dispatch, it never enters the pack cache, it is
// bounded by AO rather than by the provider, and it is measured as its own
// source category so a dispatch's bytes stay attributable.

type recordingExternal struct {
	out    projectmemory.ExternalEvidence
	calls  int
	lastAt projectmemory.ExternalContextRequest
}

func (r *recordingExternal) ExternalContext(
	_ context.Context, req projectmemory.ExternalContextRequest,
) projectmemory.ExternalEvidence {
	r.calls++
	r.lastAt = req
	return r.out
}

func TestProvisionAttachesAndMeasuresExternalContext(t *testing.T) {
	f, prov, root := provFixture(t, projectmemory.ModeAssisted)
	ext := &recordingExternal{out: projectmemory.ExternalEvidence{
		Source: "github", Rendered: "Repository: o/r\nBranch: feat/thing\nFailing checks: build\n",
	}}
	prov = prov.WithExternal(ext)

	out := prov.Provision(f.ctx, projectmemory.ProvisionRequest{
		ProjectID: testProject, RepoPath: root, Role: projectmemory.RoleReviewer,
		HeadSHA: "abc123", IssueRef: "o/r#7",
	})

	if ext.calls != 1 {
		t.Fatalf("external provider called %d times; want 1", ext.calls)
	}
	// The provider is told what the dispatch is, so it can decide relevance.
	if ext.lastAt.Role != projectmemory.RoleReviewer || ext.lastAt.HeadSHA != "abc123" ||
		ext.lastAt.IssueRef != "o/r#7" || ext.lastAt.RepoPath != root {
		t.Fatalf("external request lost dispatch identity: %+v", ext.lastAt)
	}
	if !strings.Contains(out.Render(), "Failing checks: build") {
		t.Fatalf("external context not attached:\n%s", out.Render())
	}

	// Its own source category, never folded into the pack's bytes.
	m := out.Metrics
	if m.ExternalSource != "github" || m.ExternalBytes == 0 || m.EstimatedExternalTokens == 0 {
		t.Fatalf("external contribution unmeasured: %+v", m)
	}
	if m.ExternalBytes == m.PackBytes {
		t.Fatal("external bytes were counted as pack bytes")
	}
	if m.ContextBytes < m.ExternalBytes {
		t.Fatalf("ContextBytes %d does not include the %d external bytes", m.ContextBytes, m.ExternalBytes)
	}
}

// External state moves independently of the indexed commit the pack cache is
// keyed on. A cached pack that carried it would serve a stale review decision
// for the cache's whole lifetime.
func TestProvisionReReadsExternalContextOnEveryDispatch(t *testing.T) {
	f, prov, root := provFixture(t, projectmemory.ModeAssisted)
	ext := &recordingExternal{out: projectmemory.ExternalEvidence{Source: "github", Rendered: "Branch: feat/thing\n"}}
	prov = prov.WithExternal(ext)

	req := projectmemory.ProvisionRequest{
		ProjectID: testProject, RepoPath: root, Role: projectmemory.RoleWorker,
	}
	first := prov.Provision(f.ctx, req)
	ext.out = projectmemory.ExternalEvidence{Source: "github", Rendered: "Branch: feat/thing\nFailing checks: build\n"}
	second := prov.Provision(f.ctx, req)

	if ext.calls != 2 {
		t.Fatalf("external provider called %d times across two dispatches; want 2", ext.calls)
	}
	if strings.Contains(first.Render(), "Failing checks") {
		t.Fatal("the first dispatch saw state that had not happened yet")
	}
	if !strings.Contains(second.Render(), "Failing checks: build") {
		t.Fatalf("the second dispatch was served cached external state:\n%s", second.Render())
	}
}

// A provider that returns a wall of text is cut by AO, not trusted.
func TestProvisionBoundsExternalContext(t *testing.T) {
	f, prov, root := provFixture(t, projectmemory.ModeAssisted)
	var huge strings.Builder
	for i := 0; i < 5000; i++ {
		huge.WriteString("noise from a service AO does not control\n")
	}
	prov = prov.WithExternal(&recordingExternal{
		out: projectmemory.ExternalEvidence{Source: "github", Rendered: huge.String()},
	})

	out := prov.Provision(f.ctx, projectmemory.ProvisionRequest{
		ProjectID: testProject, RepoPath: root, Role: projectmemory.RoleWorker,
	})
	if !out.External.Truncated {
		t.Fatal("an oversized external contribution was accepted whole")
	}
	if len(out.External.Rendered) > projectmemory.MaxExternalContextBytes {
		t.Fatalf("attached %d external bytes; ceiling is %d",
			len(out.External.Rendered), projectmemory.MaxExternalContextBytes)
	}
}

// A degraded provider contributes nothing and says why, and the dispatch is
// otherwise exactly what it would have been.
func TestProvisionSurvivesADegradedExternalProvider(t *testing.T) {
	f, prov, root := provFixture(t, projectmemory.ModeAssisted)
	req := projectmemory.ProvisionRequest{
		ProjectID: testProject, RepoPath: root, Role: projectmemory.RoleWorker,
	}
	baseline := prov.Provision(f.ctx, req)

	degraded := prov.WithExternal(&recordingExternal{out: projectmemory.ExternalEvidence{
		Source: "github", Degraded: true, Reason: "AO could not reach GitHub.",
	}}).Provision(f.ctx, req)

	if degraded.Render() != baseline.Render() {
		t.Fatalf("a degraded external provider changed the dispatch:\n%q\nvs\n%q",
			degraded.Render(), baseline.Render())
	}
	if !degraded.Metrics.ExternalDegraded || degraded.Metrics.ExternalReason == "" {
		t.Fatalf("degradation not recorded: %+v", degraded.Metrics)
	}
	if degraded.Metrics.ExternalBytes != 0 {
		t.Fatalf("a degraded provider was charged %d bytes", degraded.Metrics.ExternalBytes)
	}
}

// No provider installed is byte-for-byte the pre-P4-F dispatch.
func TestProvisionWithoutAnExternalProviderIsUnchanged(t *testing.T) {
	f, prov, root := provFixture(t, projectmemory.ModeAssisted)
	out := prov.Provision(f.ctx, projectmemory.ProvisionRequest{
		ProjectID: testProject, RepoPath: root, Role: projectmemory.RoleWorker,
	})
	if !out.External.Empty() || out.Metrics.ExternalSource != "" || out.Metrics.ExternalBytes != 0 {
		t.Fatalf("external fields populated with no provider installed: %+v", out.Metrics)
	}
}
