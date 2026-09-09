package skillrunner

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/skillcatalog"
)

func wsRoot(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), workspaceDirName)
}

func prepared(t *testing.T, root, run, attempt string) Workspace {
	t.Helper()
	ws, err := PrepareWorkspace(root, run, attempt, DefaultWorkspaceLimits())
	if err != nil {
		t.Fatalf("PrepareWorkspace: %v", err)
	}
	return ws
}

// A workspace has an identity and an ownership marker, and the directory name
// carries both ids — which is what makes "a previous attempt cannot collect
// this attempt's output" true by construction rather than by a check.
func TestPrepareWorkspace_HasIdentityAndOwnership(t *testing.T) {
	root := wsRoot(t)
	ws := prepared(t, root, "run1", "attempt1")

	if filepath.Base(ws.QuarantineDir) != "run1.attempt1" {
		t.Fatalf("directory = %q", ws.QuarantineDir)
	}
	body, err := os.ReadFile(filepath.Join(ws.QuarantineDir, ownerMarkerName))
	if err != nil {
		t.Fatalf("read marker: %v", err)
	}
	var marker ownerMarker
	if err := json.Unmarshal(body, &marker); err != nil {
		t.Fatalf("decode marker: %v", err)
	}
	if marker.Owner != ownerName || marker.RunID != "run1" || marker.AttemptID != "attempt1" ||
		marker.Token == "" {
		t.Fatalf("marker = %+v", marker)
	}

	// Two attempts of the same run get different directories.
	other := prepared(t, root, "run1", "attempt2")
	if other.QuarantineDir == ws.QuarantineDir {
		t.Fatal("two attempts shared a workspace")
	}
	// And different projects, by virtue of a different root.
	otherRoot := wsRoot(t)
	third := prepared(t, otherRoot, "run1", "attempt1")
	if third.QuarantineDir == ws.QuarantineDir {
		t.Fatal("two projects shared a workspace")
	}
}

func TestPrepareWorkspace_Refusals(t *testing.T) {
	limits := DefaultWorkspaceLimits()
	cases := []struct {
		name             string
		root, run, atmpt string
		limits           WorkspaceLimits
		wantSub          string
	}{
		{"root outside AO's namespace", t.TempDir(), "r", "a", limits, "not an AO workspace root"},
		{"no run id", wsRoot(t), "", "a", limits, "one run AND one attempt"},
		{"no attempt id", wsRoot(t), "r", "", limits, "one run AND one attempt"},
		{"traversing run id", wsRoot(t), "../escape", "a", limits, "plain identifiers"},
		{"shell-hostile attempt id", wsRoot(t), "r", "a b;rm", limits, "plain identifiers"},
		{"dotted id", wsRoot(t), "r.x", "a", limits, "plain identifiers"},
		{"unbounded size", wsRoot(t), "r", "a",
			WorkspaceLimits{MaxBytes: 0, MaxFiles: 10, MaxPathLen: 100}, "maxBytes"},
		{"unbounded count", wsRoot(t), "r", "a",
			WorkspaceLimits{MaxBytes: 1 << 20, MaxFiles: 0, MaxPathLen: 100}, "maxFiles"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := PrepareWorkspace(tc.root, tc.run, tc.atmpt, tc.limits)
			if !errors.Is(err, ErrWorkspace) {
				t.Fatalf("err = %v, want ErrWorkspace", err)
			}
			if !strings.Contains(err.Error(), tc.wantSub) {
				t.Fatalf("err = %v, want %q", err, tc.wantSub)
			}
		})
	}
}

// A leftover directory from a previous attempt is KEPT, not reused: its
// contents are unknown and its owner may not be AO.
func TestPrepareWorkspace_RefusesToReuseALeftover(t *testing.T) {
	root := wsRoot(t)
	first := prepared(t, root, "run1", "attempt1")
	if err := os.WriteFile(filepath.Join(first.QuarantineDir, "evidence.txt"),
		[]byte("from a crashed run\n"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	_, err := PrepareWorkspace(root, "run1", "attempt1", DefaultWorkspaceLimits())
	if !errors.Is(err, ErrWorkspace) || !strings.Contains(err.Error(), "kept for recovery") {
		t.Fatalf("err = %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(first.QuarantineDir, "evidence.txt")); statErr != nil {
		t.Fatalf("the leftover was destroyed: %v", statErr)
	}
}

// Cleanup without provable ownership fails CLOSED and keeps the directory. An
// orphan somebody removes by hand is recoverable; deleting a directory that
// turned out to be somebody else's is not.
func TestWorkspaceCleanup_FailsClosedWithoutProvableOwnership(t *testing.T) {
	root := wsRoot(t)

	t.Run("owned by this run", func(t *testing.T) {
		ws := prepared(t, root, "runA", "attempt1")
		if err := ws.Cleanup(); err != nil {
			t.Fatalf("Cleanup: %v", err)
		}
		if _, err := os.Stat(ws.QuarantineDir); !os.IsNotExist(err) {
			t.Fatalf("the workspace survived its own cleanup: %v", err)
		}
	})

	t.Run("marker missing", func(t *testing.T) {
		ws := prepared(t, root, "runB", "attempt1")
		if err := os.Remove(filepath.Join(ws.QuarantineDir, ownerMarkerName)); err != nil {
			t.Fatalf("remove marker: %v", err)
		}
		err := ws.Cleanup()
		if !errors.Is(err, ErrWorkspace) || !strings.Contains(err.Error(), "kept for recovery") {
			t.Fatalf("err = %v", err)
		}
		if _, statErr := os.Stat(ws.QuarantineDir); statErr != nil {
			t.Fatalf("the directory was removed without proof of ownership: %v", statErr)
		}
	})

	t.Run("marker belongs to another run", func(t *testing.T) {
		ws := prepared(t, root, "runC", "attempt1")
		body, _ := json.Marshal(ownerMarker{
			Owner: ownerName, RunID: "somebody-else", AttemptID: "attempt9", Token: "other",
		})
		if err := os.WriteFile(filepath.Join(ws.QuarantineDir, ownerMarkerName), body, 0o600); err != nil {
			t.Fatalf("write: %v", err)
		}
		err := ws.Cleanup()
		if !errors.Is(err, ErrWorkspace) || !strings.Contains(err.Error(), "somebody-else") {
			t.Fatalf("err = %v", err)
		}
		if _, statErr := os.Stat(ws.QuarantineDir); statErr != nil {
			t.Fatalf("another run's directory was deleted: %v", statErr)
		}
	})

	t.Run("token from a different preparation", func(t *testing.T) {
		ws := prepared(t, root, "runD", "attempt1")
		impostor := ws
		impostor.token = "not-the-token"
		if err := impostor.Cleanup(); err == nil {
			t.Fatal("a mismatched token removed the directory")
		}
		if _, statErr := os.Stat(ws.QuarantineDir); statErr != nil {
			t.Fatalf("the directory was removed: %v", statErr)
		}
	})

	t.Run("path outside AO's namespace", func(t *testing.T) {
		victim := t.TempDir()
		if err := os.WriteFile(filepath.Join(victim, "keep.txt"), []byte("keep\n"), 0o600); err != nil {
			t.Fatalf("write: %v", err)
		}
		err := Workspace{QuarantineDir: victim}.Cleanup()
		if !errors.Is(err, ErrWorkspace) || !strings.Contains(err.Error(), "not an AO workspace") {
			t.Fatalf("err = %v", err)
		}
		if _, statErr := os.Stat(filepath.Join(victim, "keep.txt")); statErr != nil {
			t.Fatalf("a directory AO does not own was deleted: %v", statErr)
		}
	})
}

// Collect's validation, driven directly so every branch is covered without a
// container.
func TestCollect_ValidatesEveryPathAndReportsRefusals(t *testing.T) {
	root := wsRoot(t)
	ws := prepared(t, root, "run1", "attempt1")
	ws.Limits = WorkspaceLimits{MaxBytes: 1024, MaxFiles: 3, MaxPathLen: 40}

	write := func(rel, body string) {
		t.Helper()
		p := filepath.Join(ws.QuarantineDir, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatalf("write %s: %v", rel, err)
		}
	}
	write("keep.txt", "kept\n")
	write("nested/also.txt", "kept\n")
	write("toolarge.txt", strings.Repeat("x", 2048))
	write(strings.Repeat("d/", 30)+"deep.txt", "deep\n")
	if err := os.Symlink("/etc/passwd", filepath.Join(ws.QuarantineDir, "link")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	out, err := ws.Collect([]StagedInput{{RelPath: "keep.txt", SHA256: "different"}})
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	reasons := map[string]string{}
	for _, r := range out.Rejected {
		reasons[r.Path] = r.Reason
	}
	if reasons["link"] != "symlink" {
		t.Fatalf("symlink not refused: %+v", out.Rejected)
	}
	if reasons["toolarge.txt"] != "file_too_large" {
		t.Fatalf("oversize not refused: %+v", out.Rejected)
	}
	// The refusal lands on the DIRECTORY that first exceeds the cap and the
	// whole subtree is skipped, which is the right behaviour: refusing each
	// leaf of a pathologically deep tree would be the walk the cap exists to
	// avoid.
	deepRefused := false
	for _, reason := range reasons {
		if reason == "path_too_long" {
			deepRefused = true
		}
	}
	if !deepRefused {
		t.Fatalf("a path past the length cap was not refused: %+v", out.Rejected)
	}
	for _, a := range out.Artifacts {
		if strings.Contains(a.Path, "deep.txt") {
			t.Fatalf("a file under a refused directory was collected: %+v", a)
		}
	}
	// The marker is AO's, not an artifact.
	for _, a := range out.Artifacts {
		if a.Path == ownerMarkerName {
			t.Fatal("the ownership marker was collected as an artifact")
		}
	}
	// keep.txt existed with a different digest, so it reads as modified.
	for _, a := range out.Artifacts {
		if a.Path == "keep.txt" && a.Status != ArtifactModified {
			t.Fatalf("keep.txt = %+v, want modified", a)
		}
	}
	// Nothing is ever applied.
	if out.Applied {
		t.Fatal("the result claims it was applied")
	}
}

// The total-size ceiling is re-checked at collection, so a runtime that
// ignored the tmpfs flag produces a refusal rather than a surprise.
func TestCollect_EnforcesTheTotalSizeCeiling(t *testing.T) {
	ws := prepared(t, wsRoot(t), "run1", "attempt1")
	ws.Limits = WorkspaceLimits{MaxBytes: 300, MaxFiles: 100, MaxPathLen: 100}
	for _, name := range []string{"a.txt", "b.txt", "c.txt", "d.txt"} {
		if err := os.WriteFile(filepath.Join(ws.QuarantineDir, name),
			[]byte(strings.Repeat("x", 100)), 0o600); err != nil {
			t.Fatalf("write: %v", err)
		}
	}
	out, err := ws.Collect(nil)
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if out.TotalBytes > 300 {
		t.Fatalf("collected %d bytes past a 300 ceiling", out.TotalBytes)
	}
	exceeded := false
	for _, r := range out.Rejected {
		if r.Reason == "total_size_exceeded" {
			exceeded = true
		}
	}
	if !exceeded {
		t.Fatalf("nothing was refused for the total ceiling: %+v", out.Rejected)
	}
}

// The file-count budget.
func TestCollect_EnforcesTheFileBudget(t *testing.T) {
	ws := prepared(t, wsRoot(t), "run1", "attempt1")
	ws.Limits = WorkspaceLimits{MaxBytes: 1 << 20, MaxFiles: 2, MaxPathLen: 100}
	for _, name := range []string{"a.txt", "b.txt", "c.txt"} {
		if err := os.WriteFile(filepath.Join(ws.QuarantineDir, name), []byte("x\n"), 0o600); err != nil {
			t.Fatalf("write: %v", err)
		}
	}
	out, err := ws.Collect(nil)
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if len(out.Artifacts) != 2 {
		t.Fatalf("collected %d artifacts past a budget of 2", len(out.Artifacts))
	}
	budgeted := false
	for _, r := range out.Rejected {
		if r.Reason == "file_budget_exhausted" {
			budgeted = true
		}
	}
	if !budgeted {
		t.Fatalf("nothing was refused for the budget: %+v", out.Rejected)
	}
}

// The workspace root is a separate namespace from staging and secrets, and is
// never the home directory or AO's data dir.
func TestWorkspaceRootFor_IsItsOwnNamespace(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skipf("no home directory: %v", err)
	}
	if _, err := WorkspaceRootFor(filepath.Join(home, "proj"), ""); err == nil {
		t.Fatal("a workspace in the home directory was accepted")
	}

	root, err := WorkspaceRootFor("/Users/someone/code/proj", "")
	if err != nil {
		t.Fatalf("WorkspaceRootFor: %v", err)
	}
	staging, _ := StagingRootFor("/Users/someone/code/proj", "")
	secrets, _ := SecretsRootFor("/Users/someone/code/proj", "")
	if root == staging || root == secrets || staging == secrets {
		t.Fatalf("the three roots are not distinct: %q %q %q", root, staging, secrets)
	}
	if strings.Contains(root, "/.ao/") || strings.HasSuffix(root, "/.ao") {
		t.Fatalf("the workspace resolved into AO's data dir: %q", root)
	}
	if _, err := WorkspaceRootFor("relative", ""); err == nil {
		t.Fatal("a relative project path was accepted")
	}
}

// The mount pair: a capped tmpfs the run writes to, and the quarantine AO's
// wrapper transfers into. The checkout is not among them.
func TestWorkspaceMountArgs_CarryTheCapAndNothingElse(t *testing.T) {
	ws := prepared(t, wsRoot(t), "run1", "attempt1")
	ws.Limits = WorkspaceLimits{MaxBytes: 1 << 20, MaxFiles: 10, MaxPathLen: 100}
	args := strings.Join(ws.MountArgs(), " ")

	for _, want := range []string{
		"--tmpfs " + ContainerWorkspacePath + ":rw,size=1048576",
		// uid/gid are load-bearing: without them the tmpfs is root-owned 0700
		// and the non-root container user cannot write to it at all.
		"uid=65534,gid=65534",
		"noexec,nosuid,nodev",
		ws.QuarantineDir + ":" + ContainerOutPath,
	} {
		if !strings.Contains(args, want) {
			t.Fatalf("mount args missing %q:\n%s", want, args)
		}
	}
	if strings.Contains(args, ":ro") {
		t.Fatalf("the writable pair should not carry a read-only mount: %s", args)
	}
}

// The control is attested only when AO can actually prepare a workspace, and
// it unblocks repo.write and nothing else. Writing is not a reason to reach
// the network, run a chosen command, or read a secret.
func TestAttestation_WorkspaceControlUnblocksOnlyWriting(t *testing.T) {
	runtime := Runtime{Binary: "docker", ServerVersion: "29.2.1", CgroupVersion: "2", OSType: "linux"}

	without := (&Runner{runtime: runtime, runner: fakeCLI{}}).Attestation()
	if without.Provides(skillcatalog.ControlWritableWorkspace) {
		t.Fatal("a runtime with no usable workspace root attested writable_workspace")
	}

	with := (&Runner{runtime: runtime, runner: fakeCLI{}}).
		WithWritableWorkspace(true).Attestation()
	if !with.Provides(skillcatalog.ControlWritableWorkspace) {
		t.Fatal("a usable workspace root did not attest writable_workspace")
	}
	// repo.write becomes carriable.
	repoWrite, _ := skillcatalog.CapRepoWrite.Spec()
	if _, ok := firstMissingControlFor(repoWrite.RequiresControls, with); !ok {
		t.Fatal("repo.write was still refused with every control it names present")
	}
	// And nothing else does.
	for _, stillBlocked := range []skillcatalog.Capability{
		skillcatalog.CapProcessExec, skillcatalog.CapSecretsRead,
		skillcatalog.CapNetEgress, skillcatalog.CapNetActiveScan,
	} {
		spec, _ := stillBlocked.Spec()
		if _, ok := firstMissingControlFor(spec.RequiresControls, with); ok {
			t.Fatalf("%s became grantable when a writable workspace arrived", stillBlocked)
		}
	}

	// An unusable runtime attests nothing, whatever the workspace root says:
	// there is nowhere to confine the writing.
	unusable := (&Runner{probeErr: errors.New("no daemon"), runner: fakeCLI{}}).
		WithWritableWorkspace(true).Attestation()
	if len(unusable.Controls) != 0 {
		t.Fatalf("an unusable runtime attested %v", unusable.Controls)
	}
}

// The egress control unblocks net.egress and NOT net.active_scan. Being
// allowed to open a connection is not being allowed to probe what is on the
// other end, and the capability table keeps them apart by requiring one more
// control for the scan.
func TestAttestation_EgressUnblocksTrafficNotScanning(t *testing.T) {
	runtime := Runtime{Binary: "docker", ServerVersion: "29.2.1", CgroupVersion: "2", OSType: "linux"}

	without := (&Runner{runtime: runtime, runner: fakeCLI{}}).Attestation()
	if without.Provides(skillcatalog.ControlEgressAllowlist) {
		t.Fatal("a runtime with no proxy attested egress_allowlist")
	}
	// Deny-all is what a plain container gives, and it is NOT an allowlist.
	if !without.Provides(skillcatalog.ControlEgressDenyAll) {
		t.Fatal("a plain container should attest deny-all")
	}
	if without.EgressControlled() {
		t.Fatal("deny-all was mistaken for an allowlist")
	}

	// A MEASURED boundary: a verified proxy for this runtime's architecture and
	// an outbound connection observed to fail on an internal network. Nothing
	// short of both halves reaches Attestation — TestEgressBoundary_* below
	// covers each way the measurement can come back short.
	with := (&Runner{runtime: runtime, runner: fakeCLI{}}).
		WithEgressBoundary(EgressBoundary{
			ProxyPackaged: true, ProxyArch: "arm64", ProxyVersion: "1",
			ProxyDigest:     strings.Repeat("a", 64),
			InternalNetwork: true, OutboundBlocked: true,
		}).Attestation()
	if !with.EgressControlled() {
		t.Fatal("a wired proxy did not attest an allowlist")
	}

	netEgress, _ := skillcatalog.CapNetEgress.Spec()
	if _, ok := firstMissingControlFor(netEgress.RequiresControls, with); !ok {
		t.Fatal("net.egress was still refused with every control it names present")
	}
	// The scan is not unblocked: it needs arbitrary_process_execution too,
	// which nothing here provides.
	activeScan, _ := skillcatalog.CapNetActiveScan.Spec()
	missing, ok := firstMissingControlFor(activeScan.RequiresControls, with)
	if ok {
		t.Fatal("net.active_scan became grantable when traffic was allowed")
	}
	if missing != skillcatalog.ControlArbitraryProcessExecution {
		t.Fatalf("active scan is missing %q, want arbitrary_process_execution", missing)
	}
	// And nothing else moved either.
	for _, stillBlocked := range []skillcatalog.Capability{
		skillcatalog.CapProcessExec, skillcatalog.CapSecretsRead, skillcatalog.CapRepoWrite,
	} {
		spec, _ := stillBlocked.Spec()
		if _, ok := firstMissingControlFor(spec.RequiresControls, with); ok {
			t.Fatalf("%s became grantable when egress arrived", stillBlocked)
		}
	}
}
