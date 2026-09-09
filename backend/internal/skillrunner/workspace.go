package skillrunner

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// workspace.go — letting a run write, without letting it write anything of
// yours.
//
// # The shape
//
//	/work        read-only  the staged, scope-limited copy of the checkout
//	/workspace   read-write  a tmpfs, size-capped, where the run may write
//	/out         read-write  a bind mount AO's own wrapper transfers into
//
// The operator's checkout is never mounted, read-only or otherwise. What the
// run sees is a copy AO made, and what it writes goes to a filesystem that
// exists only for the length of the container.
//
// # Why a tmpfs rather than a writable bind mount
//
// A bind mount has no size limit AO can enforce, so a runaway write fills the
// operator's disk — and `--memory` does not help, because pages written to a
// bind mount are page cache, not the cgroup's. A tmpfs IS charged to the memory
// cgroup and its `size=` is enforced by the kernel: measured here, a 4 MiB
// write into a 1 MiB tmpfs stops at exactly 1 MiB.
//
// The cost is that a tmpfs dies with the container, and `docker cp` cannot
// reach it afterwards (measured: the copy comes back empty). So AO's wrapper
// transfers /workspace to /out as its last act. What reaches the host is
// bounded by the tmpfs cap by construction, whatever the workload did.
//
// # Everything that arrives is quarantined, not trusted
//
// The transfer preserves file types, so a symlink the run created pointing at
// /etc/passwd arrives on the HOST pointing at the host's /etc/passwd, and a
// FIFO arrives as a FIFO. That is deliberate: the defense is the validator
// below, and a transfer that silently dropped those would leave the validator
// untested. Nothing here follows a link — every check uses Lstat.

// ErrWorkspace marks a workspace that could not be prepared, collected or
// cleaned up safely.
var ErrWorkspace = errors.New("skillrunner: workspace")

// workspaceDirName marks a directory as AO's writable-workspace root. Cleanup
// refuses any path without it.
const workspaceDirName = ".ao-skill-workspace"

// ownerMarkerName is the file that proves a directory is this run's. Cleanup
// without a matching marker fails closed and KEEPS the directory: an
// unremovable orphan is recoverable, and deleting somebody else's is not.
const ownerMarkerName = ".ao-owner.json"

// Container paths. They are constants rather than parameters so a manifest can
// never influence where a run may write.
const (
	ContainerWorkspacePath = "/workspace"
	ContainerOutPath       = "/out"
)

// WorkspaceLimits bound what a run may produce.
type WorkspaceLimits struct {
	// MaxBytes is the tmpfs size AND the ceiling on what may be collected.
	// The kernel enforces the first; the validator re-checks the second, so a
	// runtime that ignored the flag produces a refusal rather than a surprise.
	MaxBytes int64
	// MaxFiles bounds the artifact count.
	MaxFiles int
	// MaxPathLen bounds one path, so a deep tree cannot be used to exhaust
	// something downstream.
	MaxPathLen int
}

// DefaultWorkspaceLimits are deliberately small.
func DefaultWorkspaceLimits() WorkspaceLimits {
	return WorkspaceLimits{MaxBytes: 64 << 20, MaxFiles: 2000, MaxPathLen: 512}
}

// Validate rejects limits that would not bound anything.
func (l WorkspaceLimits) Validate() error {
	if l.MaxBytes < 1<<10 || l.MaxBytes > (1<<30) {
		return fmt.Errorf("%w: maxBytes %d is out of range (1KiB..1GiB)", ErrWorkspace, l.MaxBytes)
	}
	if l.MaxFiles < 1 || l.MaxFiles > 100000 {
		return fmt.Errorf("%w: maxFiles %d is out of range (1..100000)", ErrWorkspace, l.MaxFiles)
	}
	if l.MaxPathLen < 16 || l.MaxPathLen > 4096 {
		return fmt.Errorf("%w: maxPathLen %d is out of range (16..4096)", ErrWorkspace, l.MaxPathLen)
	}
	return nil
}

// ownerMarker is written into the quarantine directory and checked before any
// removal.
type ownerMarker struct {
	Owner     string    `json:"owner"`
	RunID     string    `json:"runId"`
	AttemptID string    `json:"attemptId"`
	Token     string    `json:"token"`
	CreatedAt time.Time `json:"createdAt"`
}

// ownerName is what AO stamps as the owner of a workspace. A directory whose
// marker says anything else is not AO's to delete.
const ownerName = "ao.skillrunner"

// Workspace is one run's writable area and the quarantine it lands in.
type Workspace struct {
	// QuarantineDir is the host directory bind-mounted at ContainerOutPath.
	// It is where collected output lands, unvalidated, until Collect runs.
	QuarantineDir string
	// Root is the workspace root QuarantineDir lives under.
	Root      string
	RunID     string
	AttemptID string
	Limits    WorkspaceLimits
	// token proves this process created the directory.
	token string
}

// WorkspaceRootFor picks where one project's writable workspaces live.
//
// Same constraint as staging and secrets: the container runtime may be a VM
// that shares only certain host paths. It is a SEPARATE root from both, so a
// bug in one cannot expose the others, and it is never the home directory and
// never AO's data dir.
func WorkspaceRootFor(projectPath, override string) (string, error) {
	if trimmed := strings.TrimSpace(override); trimmed != "" {
		if !filepath.IsAbs(trimmed) {
			return "", fmt.Errorf("%w: override %q must be absolute", ErrWorkspace, trimmed)
		}
		return filepath.Join(trimmed, workspaceDirName), nil
	}
	if strings.TrimSpace(projectPath) == "" || !filepath.IsAbs(projectPath) {
		return "", fmt.Errorf("%w: project path %q must be absolute", ErrWorkspace, projectPath)
	}
	parent := filepath.Dir(filepath.Clean(projectPath))
	if parent == "/" || parent == "." {
		return "", fmt.Errorf("%w: project path %q has no usable parent", ErrWorkspace, projectPath)
	}
	if home, err := os.UserHomeDir(); err == nil && filepath.Clean(parent) == filepath.Clean(home) {
		return "", fmt.Errorf("%w: refusing to place a workspace directly in the home directory %q",
			ErrWorkspace, home)
	}
	return filepath.Join(parent, workspaceDirName), nil
}

// PrepareWorkspace creates the quarantine directory for one run/attempt and
// stamps it with an ownership marker.
//
// The directory name carries both ids, so two attempts of the same run never
// share one — which is what makes "a previous attempt cannot collect this
// attempt's output" true by construction rather than by a check.
func PrepareWorkspace(root, runID, attemptID string, limits WorkspaceLimits) (Workspace, error) {
	if strings.TrimSpace(root) == "" || !strings.Contains(root, workspaceDirName) {
		return Workspace{}, fmt.Errorf("%w: %q is not an AO workspace root", ErrWorkspace, root)
	}
	if strings.TrimSpace(runID) == "" || strings.TrimSpace(attemptID) == "" {
		return Workspace{}, fmt.Errorf("%w: a workspace binds to one run AND one attempt", ErrWorkspace)
	}
	if hasShellHostileName(runID) || hasShellHostileName(attemptID) ||
		strings.ContainsAny(runID+attemptID, "/\\.") {
		return Workspace{}, fmt.Errorf("%w: run and attempt ids must be plain identifiers", ErrWorkspace)
	}
	if err := limits.Validate(); err != nil {
		return Workspace{}, err
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		return Workspace{}, fmt.Errorf("%w: create root: %w", ErrWorkspace, err)
	}

	dir := filepath.Join(root, runID+"."+attemptID)
	// A leftover directory is NOT reused: its contents are unknown and its
	// owner may not be AO. It is left where it is, and this run refuses.
	if _, err := os.Lstat(dir); err == nil {
		return Workspace{}, fmt.Errorf("%w: %q already exists; a previous attempt's workspace is "+
			"kept for recovery rather than reused", ErrWorkspace, dir)
	}
	if err := os.Mkdir(dir, 0o700); err != nil {
		return Workspace{}, fmt.Errorf("%w: create %q: %w", ErrWorkspace, dir, err)
	}
	ws := Workspace{
		QuarantineDir: dir, Root: root, RunID: runID, AttemptID: attemptID,
		Limits: limits, token: randomToken(),
	}
	marker := ownerMarker{
		Owner: ownerName, RunID: runID, AttemptID: attemptID,
		Token: ws.token, CreatedAt: time.Now().UTC(),
	}
	body, err := json.Marshal(marker)
	if err != nil {
		_ = os.RemoveAll(dir)
		return Workspace{}, fmt.Errorf("%w: encode owner marker: %w", ErrWorkspace, err)
	}
	if err := os.WriteFile(filepath.Join(dir, ownerMarkerName), body, 0o600); err != nil {
		_ = os.RemoveAll(dir)
		return Workspace{}, fmt.Errorf("%w: write owner marker: %w", ErrWorkspace, err)
	}
	// The container writes here as a non-root user, so the directory has to be
	// traversable and writable by it. It holds nothing but this run's output.
	if err := os.Chmod(dir, 0o777); err != nil { //nolint:gosec // G302: the container's non-root user must write here; the directory holds only this run's output and is removed with it.
		_ = os.RemoveAll(dir)
		return Workspace{}, fmt.Errorf("%w: open %q for the container: %w", ErrWorkspace, dir, err)
	}
	return ws, nil
}

// MountArgs are the runtime flags for the writable pair: a capped tmpfs the run
// writes to, and the quarantine the wrapper transfers into.
//
// The tmpfs carries uid/gid so a non-root container user can write to it at
// all — measured: without them the mount is root-owned 0700 and every write is
// denied.
func (w Workspace) MountArgs() []string {
	if w.QuarantineDir == "" {
		return nil
	}
	tmpfs := fmt.Sprintf("%s:rw,size=%d,mode=0700,uid=65534,gid=65534,noexec,nosuid,nodev",
		ContainerWorkspacePath, w.Limits.MaxBytes)
	return []string{
		"--tmpfs", tmpfs,
		"-v", w.QuarantineDir + ":" + ContainerOutPath,
	}
}

// TransferScript is AO's own last act inside the container: move what the run
// produced from the capped tmpfs into the quarantine mount.
//
// It preserves file types on purpose. A transfer that dropped symlinks and
// special files would leave the host-side validator untested, and the validator
// is the actual defense — this is a copy into quarantine, not a copy into
// trust.
const TransferScript = `
if [ -d ` + ContainerWorkspacePath + ` ]; then
  echo "ao_workspace_bytes=$(du -sk ` + ContainerWorkspacePath + ` 2>/dev/null | cut -f1)"
  echo "ao_workspace_files=$(find ` + ContainerWorkspacePath + ` -type f 2>/dev/null | wc -l | tr -d ' ')"
  tar -C ` + ContainerWorkspacePath + ` -cf - . 2>/dev/null | tar -C ` + ContainerOutPath + ` -xf - 2>/dev/null
  echo "ao_workspace_transferred=true"
fi
`

// pathWithin reports whether p is root or sits inside it, lexically. It is a
// name check, not a link check: a symlink that points outside is caught by the
// type check in Collect, because WalkDir does not follow one.
func pathWithin(root, p string) bool {
	rel, err := filepath.Rel(root, p)
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// ArtifactStatus says how one collected file relates to the staged inputs.
type ArtifactStatus string

const (
	// ArtifactAdded is a path that did not exist in the inputs.
	ArtifactAdded ArtifactStatus = "added"
	// ArtifactModified is a path that existed and whose content changed.
	ArtifactModified ArtifactStatus = "modified"
	// ArtifactUnchanged is a path whose content is byte-identical to the
	// input. It is reported so a caller can tell "the run rewrote this
	// identically" from "the run never touched it".
	ArtifactUnchanged ArtifactStatus = "unchanged"
)

// Artifact is one validated file the run produced.
type Artifact struct {
	// Path is repo-relative and has been proven not to escape the workspace.
	Path   string         `json:"path"`
	SHA256 string         `json:"sha256"`
	Bytes  int64          `json:"bytes"`
	Status ArtifactStatus `json:"status"`
}

// RejectedArtifact is something the run produced that AO refused to collect.
// It is reported rather than silently dropped: a caller who sees three files
// and expected four needs to know which one was refused and why.
type RejectedArtifact struct {
	Path   string `json:"path"`
	Reason string `json:"reason"`
}

// WorkspaceResult is what a writable run produced, after validation.
type WorkspaceResult struct {
	RunID     string `json:"runId"`
	AttemptID string `json:"attemptId"`
	// InputsUsed are the staged inputs the run was given, by digest. Evidence
	// of what it read, not only of what it wrote.
	InputsUsed []StagedInput      `json:"inputsUsed"`
	Artifacts  []Artifact         `json:"artifacts"`
	Rejected   []RejectedArtifact `json:"rejected"`
	// Deleted are input paths the run removed from its workspace copy. They
	// are reported; nothing is deleted anywhere else.
	Deleted []string `json:"deleted"`
	// TotalBytes is the collected size, after validation.
	TotalBytes int64 `json:"totalBytes"`
	// Applied is always false. It is a field rather than an omission so a
	// consumer reading this struct sees the answer instead of assuming one.
	Applied bool `json:"applied"`
}

// Collect validates everything in the quarantine directory and turns it into
// artifacts and a diff against the staged inputs.
//
// It never follows a link, never opens a special file, and never writes
// anywhere outside the quarantine. Anything it refuses is reported, and the
// refused file is left in quarantine rather than being deleted, so a person can
// look at it.
func (w Workspace) Collect(inputs []StagedInput) (WorkspaceResult, error) {
	if w.QuarantineDir == "" {
		return WorkspaceResult{}, fmt.Errorf("%w: no workspace to collect", ErrWorkspace)
	}
	result := WorkspaceResult{
		RunID: w.RunID, AttemptID: w.AttemptID,
		InputsUsed: append([]StagedInput(nil), inputs...),
		Artifacts:  []Artifact{}, Rejected: []RejectedArtifact{}, Deleted: []string{},
	}
	inputDigests := make(map[string]string, len(inputs))
	for _, in := range inputs {
		inputDigests[in.RelPath] = in.SHA256
	}
	seen := map[string]bool{}

	root := filepath.Clean(w.QuarantineDir)
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, relErr := filepath.Rel(root, p)
		if relErr != nil {
			return relErr
		}
		if rel == "." {
			return nil
		}
		slashed := filepath.ToSlash(rel)
		// AO's own marker is not an artifact.
		if slashed == ownerMarkerName {
			return nil
		}
		reject := func(reason string) error {
			result.Rejected = append(result.Rejected, RejectedArtifact{Path: slashed, Reason: reason})
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}

		// The path must stay inside the quarantine. WalkDir does not follow
		// symlinks, so this catches a name that traverses rather than a link
		// that points out; the link case is caught by the type check below.
		if !pathWithin(root, p) || strings.Contains(slashed, "..") {
			return reject("path_escapes_workspace")
		}
		if len(slashed) > w.Limits.MaxPathLen {
			return reject("path_too_long")
		}
		if d.IsDir() {
			return nil
		}

		// Lstat, never Stat: following a link here is how a symlink to
		// /etc/passwd becomes a collected artifact.
		info, infoErr := os.Lstat(p)
		if infoErr != nil {
			return reject("unreadable")
		}
		mode := info.Mode()
		switch {
		case mode&os.ModeSymlink != 0:
			// A link created inside the workspace arrives pointing at the
			// HOST's filesystem. It is refused whatever it points at, because
			// deciding "this one is harmless" is a judgement AO should not be
			// making about a path a run chose.
			return reject("symlink")
		case !mode.IsRegular():
			// Devices, sockets, FIFOs. A run that produced one did not produce
			// a change to a repository.
			return reject("special_file")
		case info.Sys() != nil && hardLinkCount(info) > 1:
			// A hardlink shares an inode with something else. Inside a tar
			// transfer that is another collected file, but AO cannot prove
			// that in general, and an artifact whose bytes another name can
			// change is not an artifact.
			return reject("hardlink")
		}
		if info.Size() > w.Limits.MaxBytes {
			return reject("file_too_large")
		}
		if len(result.Artifacts) >= w.Limits.MaxFiles {
			return reject("file_budget_exhausted")
		}
		if result.TotalBytes+info.Size() > w.Limits.MaxBytes {
			return reject("total_size_exceeded")
		}

		body, readErr := os.ReadFile(p) //nolint:gosec // path from the walked quarantine root.
		if readErr != nil {
			return reject("unreadable")
		}
		sum := sha256.Sum256(body)
		digest := hex.EncodeToString(sum[:])
		status := ArtifactAdded
		if before, existed := inputDigests[slashed]; existed {
			status = ArtifactModified
			if before == digest {
				status = ArtifactUnchanged
			}
		}
		seen[slashed] = true
		result.TotalBytes += info.Size()
		result.Artifacts = append(result.Artifacts, Artifact{
			Path: slashed, SHA256: digest, Bytes: info.Size(), Status: status,
		})
		return nil
	})
	if err != nil {
		return WorkspaceResult{}, fmt.Errorf("%w: collect: %w", ErrWorkspace, err)
	}

	for _, in := range inputs {
		if !seen[in.RelPath] {
			result.Deleted = append(result.Deleted, in.RelPath)
		}
	}
	sort.Slice(result.Artifacts, func(i, j int) bool { return result.Artifacts[i].Path < result.Artifacts[j].Path })
	sort.Slice(result.Rejected, func(i, j int) bool { return result.Rejected[i].Path < result.Rejected[j].Path })
	sort.Strings(result.Deleted)
	return result, nil
}

// Cleanup removes this run's quarantine — and only when AO can prove it owns
// it.
//
// A missing, unreadable or mismatched marker fails closed and KEEPS the
// directory. An orphan somebody has to remove by hand is recoverable; deleting
// a directory that turned out to be somebody else's is not.
func (w Workspace) Cleanup() error {
	if w.QuarantineDir == "" {
		return nil
	}
	if !strings.Contains(w.QuarantineDir, workspaceDirName) {
		return fmt.Errorf("%w: refusing to remove %q: not an AO workspace directory",
			ErrWorkspace, w.QuarantineDir)
	}
	body, err := os.ReadFile(filepath.Join(w.QuarantineDir, ownerMarkerName)) //nolint:gosec // path built from the workspace root.
	if err != nil {
		return fmt.Errorf("%w: refusing to remove %q: its ownership marker is unreadable (%v); "+
			"the directory is kept for recovery", ErrWorkspace, w.QuarantineDir, err)
	}
	var marker ownerMarker
	if err := json.Unmarshal(body, &marker); err != nil {
		return fmt.Errorf("%w: refusing to remove %q: its ownership marker is unreadable; "+
			"the directory is kept for recovery", ErrWorkspace, w.QuarantineDir)
	}
	if marker.Owner != ownerName || marker.Token != w.token ||
		marker.RunID != w.RunID || marker.AttemptID != w.AttemptID {
		return fmt.Errorf("%w: refusing to remove %q: it belongs to %s/%s, not to this run; "+
			"the directory is kept for recovery",
			ErrWorkspace, w.QuarantineDir, marker.RunID, marker.AttemptID)
	}
	if err := os.RemoveAll(w.QuarantineDir); err != nil {
		return fmt.Errorf("%w: remove %q: %w", ErrWorkspace, w.QuarantineDir, err)
	}
	return nil
}
