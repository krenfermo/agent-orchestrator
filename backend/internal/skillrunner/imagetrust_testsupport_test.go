package skillrunner

import (
	"context"
	"errors"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/skillimage"
)

// fakeAuthority is an ImageAuthority a test drives directly. It applies expiry
// and revocation at the moment of the call, exactly as the real one must, so a
// test that revokes between two calls sees what production would.
type fakeAuthority struct {
	mu sync.Mutex
	// byKey holds one approval per scope+tool, like the UNIQUE constraint.
	byKey map[string]skillimage.Approval
	// err, when set, is returned instead. It stands for a store that is down.
	err error
	// calls counts lookups, so a test can prove the re-check actually happened
	// rather than assuming the code path ran.
	calls int
	now   func() time.Time
}

func newFakeAuthority() *fakeAuthority {
	return &fakeAuthority{
		byKey: map[string]skillimage.Approval{},
		now:   func() time.Time { return time.Now().UTC() },
	}
}

func authorityKey(scope skillimage.Scope, tool string) string {
	return scope.String() + "|" + tool
}

func (f *fakeAuthority) put(a skillimage.Approval) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.byKey[authorityKey(a.Scope, a.Tool)] = a
}

func (f *fakeAuthority) revoke(scope skillimage.Scope, tool string, at time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()
	key := authorityKey(scope, tool)
	a, ok := f.byKey[key]
	if !ok {
		return
	}
	a.RevokedAt = &at
	f.byKey[key] = a
}

func (f *fakeAuthority) remove(scope skillimage.Scope, tool string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.byKey, authorityKey(scope, tool))
}

func (f *fakeAuthority) lookups() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

// ApprovedImage mirrors the real authority's contract: an approval that is not
// active is ErrNotApproved, not a usable row.
func (f *fakeAuthority) ApprovedImage(
	_ context.Context, scope skillimage.Scope, tool string,
) (skillimage.Approval, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.err != nil {
		return skillimage.Approval{}, f.err
	}
	a, ok := f.byKey[authorityKey(scope, tool)]
	if !ok {
		return skillimage.Approval{}, skillimage.ErrNotApproved
	}
	if reason := a.InactiveReason(f.now()); reason != "" {
		return skillimage.Approval{}, errors.New("skillimage: " + reason)
	}
	return a, nil
}

// testScope is one fully-specified scope. Every field is set: a partial one is
// refused before anything else happens, which is the property under test
// elsewhere and not the thing most of these tests are about.
func testScope() skillimage.Scope {
	return skillimage.Scope{
		TenantID: domain.TenantID("tenant-1"), ProjectID: domain.ProjectID("proj-1"),
		SkillID: "security-audit", Version: "1.2.0", ModeID: "static-code",
	}
}

func approvalFor(scope skillimage.Scope, digest string) skillimage.Approval {
	return skillimage.Approval{
		ID: "img-1", Scope: scope, Tool: string(ToolStaticScan),
		Reference: "alpine", Digest: digest,
		ApprovedBy: "ada", ApprovedAt: time.Now().UTC().Add(-time.Hour),
		Note: "inspected the base image and its provenance",
	}
}

// hostAlpineDigest is the digest of the alpine image actually on this host. The
// live tests approve THAT, because approving something else would test nothing:
// the point is that AO runs the bytes an administrator named.
func hostAlpineDigest(t *testing.T, r *Runner) string {
	t.Helper()
	out, err := exec.Command(r.runtime.Binary, "image", "inspect", "alpine:3.19",
		"--format", "{{.Id}}").Output()
	if err != nil {
		t.Skipf("alpine:3.19 is not present locally, and these tests do not pull: %v", err)
	}
	digest, parseErr := skillimage.ParseDigest(strings.TrimSpace(string(out)))
	if parseErr != nil {
		t.Skipf("this host reports %q for alpine:3.19: %v", out, parseErr)
	}
	return digest
}

// liveApproval wires a fake trust root that approves the host's own alpine for
// the test scope, which is what lets a live run happen at all.
func liveApproval(t *testing.T, r *Runner) (*fakeAuthority, skillimage.Scope) {
	t.Helper()
	scope := testScope()
	auth := newFakeAuthority()
	auth.put(approvalFor(scope, hostAlpineDigest(t, r)))
	return auth, scope
}
