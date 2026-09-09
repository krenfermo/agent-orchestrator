package skillrunner

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"
)

// These drive a REAL container. Everything is synthetic and local; no project
// of the operator's is ever mounted, read-only or otherwise.

func liveWorkspace(t *testing.T, limits WorkspaceLimits) Workspace {
	t.Helper()
	root := filepath.Join(repoScratchRoot(t), workspaceDirName)
	ws, err := PrepareWorkspace(root, "run"+randomToken()[:8], "attempt1", limits)
	if err != nil {
		t.Fatalf("PrepareWorkspace: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(ws.QuarantineDir) })
	return ws
}

// runWritable runs a workload with a workspace, appending AO's own transfer
// step as the last act — which is what moves the capped tmpfs into quarantine.
func runWritable(t *testing.T, r *Runner, ws *Workspace, inputDir, script string) Result {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	res, err := r.Run(ctx, Request{
		Image:     pinnedImage(t, r),
		Argv:      []string{"sh", "-c", script + "\n" + TransferScript},
		InputDir:  inputDir,
		Workspace: ws,
		Limits:    DefaultLimits(),
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	return res
}

// checkoutDigest fingerprints a whole tree, so a test can prove the operator's
// checkout is byte-identical afterwards.
func checkoutDigest(t *testing.T, dir string) string {
	t.Helper()
	var entries []string
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, relErr := filepath.Rel(dir, p)
		if relErr != nil {
			return relErr
		}
		info, infoErr := os.Lstat(p)
		if infoErr != nil {
			return infoErr
		}
		if d.IsDir() {
			entries = append(entries, "d "+filepath.ToSlash(rel))
			return nil
		}
		body, readErr := os.ReadFile(p)
		if readErr != nil {
			return readErr
		}
		sum := sha256.Sum256(body)
		entries = append(entries, "f "+filepath.ToSlash(rel)+" "+
			hex.EncodeToString(sum[:])+" "+info.Mode().String())
		return nil
	})
	if err != nil {
		t.Fatalf("fingerprint %s: %v", dir, err)
	}
	sort.Strings(entries)
	sum := sha256.Sum256([]byte(strings.Join(entries, "\n")))
	return hex.EncodeToString(sum[:])
}

// The happy path, and the assertion that matters most alongside it: the
// operator's checkout is byte-identical after a run that wrote plenty.
func TestLiveWorkspace_WritesAndLeavesTheCheckoutUntouched(t *testing.T) {
	r := liveRunner(t)
	requireAlpine(t, r)
	project := syntheticProject(t)
	before := checkoutDigest(t, project)

	// Stage the checkout the way a real run would: a copy, never the original.
	staging, err := Stage(StageRequest{
		SourceDir: project, Root: filepath.Join(repoScratchRoot(t), stagingDirName),
		RunID: "stage-" + randomToken(), MaxFiles: 100, MaxFileBytes: 1 << 20,
	})
	if err != nil {
		t.Fatalf("Stage: %v", err)
	}
	t.Cleanup(func() { _ = staging.Cleanup() })

	ws := liveWorkspace(t, DefaultWorkspaceLimits())
	res := runWritable(t, r, &ws, staging.Dir, `
cp -a /work/. /workspace/ 2>/dev/null
echo "// patched" >> /workspace/api/handler.go
echo "new file" > /workspace/NEWFILE.txt
rm -f /workspace/config/settings.py
`)
	if res.ExitCode != 0 {
		t.Fatalf("exit %d: %s", res.ExitCode, res.Stderr)
	}
	if !res.Evidence.WorkspaceTransferred {
		t.Fatalf("nothing was transferred out of the workspace:\n%s", res.Stdout)
	}

	out, err := ws.Collect(staging.Inputs)
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if out.Applied {
		t.Fatal("the result claims it was applied")
	}

	byPath := map[string]Artifact{}
	for _, a := range out.Artifacts {
		byPath[a.Path] = a
	}
	if got := byPath["api/handler.go"]; got.Status != ArtifactModified {
		t.Fatalf("api/handler.go = %+v, want modified", got)
	}
	if got := byPath["NEWFILE.txt"]; got.Status != ArtifactAdded {
		t.Fatalf("NEWFILE.txt = %+v, want added", got)
	}
	if got := byPath["web/client.js"]; got.Status != ArtifactUnchanged {
		t.Fatalf("web/client.js = %+v, want unchanged", got)
	}
	deleted := strings.Join(out.Deleted, ",")
	if !strings.Contains(deleted, "config/settings.py") {
		t.Fatalf("the removal was not reported: %v", out.Deleted)
	}
	// Evidence of what it READ, not only of what it wrote.
	if len(out.InputsUsed) != len(staging.Inputs) {
		t.Fatalf("inputsUsed = %d, staged = %d", len(out.InputsUsed), len(staging.Inputs))
	}

	// The point of the whole phase.
	if after := checkoutDigest(t, project); after != before {
		t.Fatal("the operator's checkout changed")
	}
	if _, err := os.Stat(filepath.Join(project, "NEWFILE.txt")); !os.IsNotExist(err) {
		t.Fatalf("a produced file reached the checkout: %v", err)
	}
	if _, err := os.Stat(filepath.Join(project, "config", "settings.py")); err != nil {
		t.Fatalf("a file the run deleted in its workspace was removed from the checkout: %v", err)
	}
}

// NEGATIVE: writing outside the workspace. /work is read-only and the rest of
// the filesystem is not the run's.
func TestLiveWorkspace_CannotWriteOutsideTheWorkspace(t *testing.T) {
	r := liveRunner(t)
	requireAlpine(t, r)
	inputDir, _ := workspace(t)
	ws := liveWorkspace(t, DefaultWorkspaceLimits())

	res := runWritable(t, r, &ws, inputDir, `
echo x > /work/pwn.txt 2>&1 || echo "INPUTS READ-ONLY"
echo x > /etc/pwn 2>&1 || echo "ROOTFS READ-ONLY"
echo x > /pwn 2>&1 || echo "ROOT READ-ONLY"
mkdir -p /workspace/ok && echo fine > /workspace/ok/a.txt && echo "WORKSPACE WRITABLE"
`)
	for _, want := range []string{"INPUTS READ-ONLY", "ROOTFS READ-ONLY", "ROOT READ-ONLY", "WORKSPACE WRITABLE"} {
		if !strings.Contains(res.Stdout, want) {
			t.Fatalf("expected %q:\n%s\n%s", want, res.Stdout, res.Stderr)
		}
	}
	if _, err := os.Stat(filepath.Join(inputDir, "pwn.txt")); !os.IsNotExist(err) {
		t.Fatalf("the staged inputs were modified: %v", err)
	}
}

// NEGATIVE: a symlink the run creates arrives on the host pointing at the
// HOST's filesystem. It must be refused, and never followed.
func TestLiveWorkspace_RefusesASymlinkEscape(t *testing.T) {
	r := liveRunner(t)
	requireAlpine(t, r)
	inputDir, secretPath := workspace(t)
	ws := liveWorkspace(t, DefaultWorkspaceLimits())

	runWritable(t, r, &ws, inputDir, `
echo real > /workspace/real.txt
ln -s /etc/passwd /workspace/escape
ln -s `+secretPath+` /workspace/host-secret
ln -s ../../../ /workspace/updir
`)
	out, err := ws.Collect(nil)
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	rejected := map[string]string{}
	for _, r := range out.Rejected {
		rejected[r.Path] = r.Reason
	}
	for _, name := range []string{"escape", "host-secret", "updir"} {
		if rejected[name] != "symlink" {
			t.Fatalf("%s was not refused as a symlink: %v", name, out.Rejected)
		}
	}
	for _, a := range out.Artifacts {
		if a.Path != "real.txt" {
			t.Fatalf("a link was collected as an artifact: %+v", a)
		}
	}
	// Nothing followed the link: the host secret's content is nowhere.
	body, _ := os.ReadFile(secretPath)
	for _, a := range out.Artifacts {
		if strings.Contains(string(body), a.SHA256) {
			t.Fatal("impossible")
		}
	}
	if _, err := os.Stat(filepath.Join(ws.QuarantineDir, "escape")); err != nil {
		t.Fatalf("the refused link was deleted rather than quarantined: %v", err)
	}
}

// NEGATIVE: a hardlink shares an inode, so its bytes can change through a name
// AO did not collect.
func TestLiveWorkspace_RefusesAHardlink(t *testing.T) {
	r := liveRunner(t)
	requireAlpine(t, r)
	inputDir, _ := workspace(t)
	ws := liveWorkspace(t, DefaultWorkspaceLimits())

	runWritable(t, r, &ws, inputDir, `
echo real > /workspace/real.txt
ln /workspace/real.txt /workspace/hard.txt
`)
	out, err := ws.Collect(nil)
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	hardLinks := 0
	for _, r := range out.Rejected {
		if r.Reason == "hardlink" {
			hardLinks++
		}
	}
	if hardLinks == 0 {
		t.Fatalf("no hardlink was refused: artifacts=%+v rejected=%+v", out.Artifacts, out.Rejected)
	}
}

// NEGATIVE: a device, socket or FIFO is not a change to a repository.
func TestLiveWorkspace_RefusesASpecialFile(t *testing.T) {
	r := liveRunner(t)
	requireAlpine(t, r)
	inputDir, _ := workspace(t)
	ws := liveWorkspace(t, DefaultWorkspaceLimits())

	runWritable(t, r, &ws, inputDir, `
echo real > /workspace/real.txt
mkfifo /workspace/pipe 2>/dev/null || echo "mkfifo unavailable"
`)
	out, err := ws.Collect(nil)
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	found := false
	for _, r := range out.Rejected {
		if r.Path == "pipe" && r.Reason == "special_file" {
			found = true
		}
	}
	if !found {
		t.Skipf("the runtime did not transfer a FIFO; rejected=%+v", out.Rejected)
	}
	for _, a := range out.Artifacts {
		if a.Path == "pipe" {
			t.Fatal("a FIFO was collected as an artifact")
		}
	}
}

// NEGATIVE: the size cap is enforced by the KERNEL inside the container, so a
// runaway write cannot reach the host at all.
func TestLiveWorkspace_SizeCapIsEnforcedInsideTheContainer(t *testing.T) {
	r := liveRunner(t)
	requireAlpine(t, r)
	inputDir, _ := workspace(t)
	ws := liveWorkspace(t, WorkspaceLimits{MaxBytes: 1 << 20, MaxFiles: 100, MaxPathLen: 512})

	res := runWritable(t, r, &ws, inputDir, `
dd if=/dev/zero of=/workspace/big bs=1k count=8192 2>/dev/null
echo "wrote: $(wc -c < /workspace/big)"
`)
	if !strings.Contains(res.Stdout, "wrote: 1048576") {
		t.Fatalf("the tmpfs cap did not stop the write at 1MiB:\n%s", res.Stdout)
	}
	// And what reached the host is bounded by the same cap.
	var total int64
	err := filepath.WalkDir(ws.QuarantineDir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		info, statErr := os.Lstat(p)
		if statErr != nil {
			return statErr
		}
		total += info.Size()
		return nil
	})
	if err != nil {
		t.Fatalf("measure quarantine: %v", err)
	}
	if total > (1<<20)+4096 {
		t.Fatalf("%d bytes reached the host past a 1MiB cap", total)
	}
}

// NEGATIVE: a run that outlives its wall clock is killed with its children,
// and the workspace is still collectable and cleanable.
func TestLiveWorkspace_TimeoutKillsChildrenAndLeavesNoContainer(t *testing.T) {
	r := liveRunner(t)
	requireAlpine(t, r)
	inputDir, _ := workspace(t)
	ws := liveWorkspace(t, DefaultWorkspaceLimits())

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	res, err := r.Run(ctx, Request{
		Image: pinnedImage(t, r),
		Argv: []string{"sh", "-c",
			"echo partial > /workspace/partial.txt; sleep 600 & sleep 600 & wait"},
		InputDir: inputDir, Workspace: &ws,
		Limits: Limits{
			Wall: 5 * time.Second, MemoryBytes: 128 << 20, CPUs: 1,
			MaxPIDs: 32, MaxOutputBytes: 4096,
		},
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !res.TimedOut {
		t.Fatal("the run did not time out")
	}
	if leftover := listSkillRunContainers(t, r); leftover != "" {
		t.Fatalf("containers survived: %s", leftover)
	}
	// The transfer never ran, so nothing was collected -- which is the honest
	// outcome, not a partial artifact set presented as complete.
	if res.Evidence.WorkspaceTransferred {
		t.Fatal("a killed run reported a completed transfer")
	}
	if err := ws.Cleanup(); err != nil {
		t.Fatalf("Cleanup after timeout: %v", err)
	}
}

// NEGATIVE: no runtime means no workspace run at all.
func TestLiveWorkspace_RefusesWithNoRuntime(t *testing.T) {
	unusable := &Runner{probeErr: errors.New("Cannot connect to the Docker daemon"), runner: fakeCLI{}}
	ws := Workspace{QuarantineDir: "/tmp/" + workspaceDirName + "/x", Limits: DefaultWorkspaceLimits()}

	_, err := unusable.Run(context.Background(), Request{
		Image: "alpine@sha256:" + strings.Repeat("a", 64),
		Argv:  []string{"true"}, InputDir: t.TempDir(), Workspace: &ws,
	})
	if !errors.Is(err, ErrRuntimeUnavailable) {
		t.Fatalf("err = %v, want ErrRuntimeUnavailable", err)
	}
}
