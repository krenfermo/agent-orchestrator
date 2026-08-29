package workflow

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

// WorkspaceFingerprint computes a deterministic, content-aware identity for
// the current worktree state (Checkpoint 8D) — not just HeadSHA, because
// Checkpoint 8B/8C established that Codex workers are told never to commit,
// so most real corrections land as uncommitted/untracked changes with an
// unchanged HEAD.
//
// Path+status alone (from ports.WorkspaceObservation.Changes) is not enough:
// a fix cycle very commonly means editing the SAME file the work step just
// created, which keeps the identical untracked "??" status (or the identical
// "M"/"A" status for an already-modified tracked file) across both
// observations — path+status would report no change even though the
// content genuinely changed, making the fix cycle's completion detection
// blind to the single most common real correction. So each changed path's
// CURRENT file content is hashed too, read directly from the worktree (the
// same path ObserveWorkspace already reported, no new port/capability
// needed — reading, not writing, and only the paths git status already
// named, not an arbitrary tree walk). A path that no longer exists (deleted)
// hashes as an explicit "deleted" marker rather than being silently skipped,
// so a delete is also a real, detectable change.
//
// No timestamps, no secrets: only HeadSHA, the three boolean git-status
// summary flags, and each changed path's status + current content hash —
// all derived from `git status` plus the worktree's own current files, which
// already respect .gitignore (git status never lists ignored paths). Two
// observations of the same real state hash identically (Changes is sorted
// before hashing); any change to a touched path's content, status, or the
// set of touched paths changes the hash.
func WorkspaceFingerprint(obs ports.WorkspaceObservation) string {
	lines := make([]string, 0, len(obs.Changes)+4)
	lines = append(lines,
		"head_sha="+obs.HeadSHA,
		"dirty="+strconv.FormatBool(obs.Dirty),
		"staged="+strconv.FormatBool(obs.Staged),
		"untracked="+strconv.FormatBool(obs.Untracked),
	)

	changes := make([]string, 0, len(obs.Changes))
	for _, ch := range obs.Changes {
		if IsEphemeralArtifactPath(ch.Path) {
			continue
		}
		changes = append(changes, ch.Path+":"+ch.Status+":"+contentHash(obs.Path, ch.Path))
	}
	sort.Strings(changes)
	for _, c := range changes {
		lines = append(lines, "change="+c)
	}

	canonical := strings.Join(lines, "\n")
	sum := sha256.Sum256([]byte(canonical))
	return hex.EncodeToString(sum[:])
}

// ephemeralArtifactDirs and ephemeralArtifactFilePatterns are Checkpoint
// 8M.1's WorkspaceFingerprintPolicy: a minimal, explicit list of runtime
// cache/output paths that tooling (mainly Python) commonly leaves behind
// even in repos whose own .gitignore doesn't happen to cover them. git
// status already excludes anything .gitignore DOES cover (no --ignored flag
// is ever passed — see ObserveWorkspace), so this list only closes the gap
// git itself can't: cache artifacts that are untracked, not ignored, and
// carry no semantic meaning about the task's actual work. Deliberately
// short — see docs/plans and the 8M.1 checkpoint brief for the exact
// candidate list; do not grow this ad hoc.
var (
	ephemeralArtifactDirs = []string{
		"__pycache__", ".pytest_cache", "htmlcov", ".mypy_cache", ".ruff_cache", ".pyright",
	}
	ephemeralArtifactFileSuffixes = []string{".pyc", ".pyo"}
	ephemeralArtifactFileExact    = map[string]bool{".coverage": true}
	ephemeralArtifactFilePrefix   = ".coverage."
)

// IsEphemeralArtifactPath reports whether path (as reported by git status,
// forward-slash separated and relative to the worktree root) is a known
// ephemeral runtime artifact rather than a relevant source/config change.
// Matching is by path segment/basename, never a bare substring match, so a
// real file that merely contains one of these words in a longer name (e.g.
// "mypycache_report.py") is never mistaken for the cache directory itself.
func IsEphemeralArtifactPath(path string) bool {
	segments := strings.Split(strings.Trim(path, "/"), "/")
	for _, seg := range segments {
		for _, dir := range ephemeralArtifactDirs {
			if seg == dir {
				return true
			}
		}
	}
	base := segments[len(segments)-1]
	if ephemeralArtifactFileExact[base] {
		return true
	}
	if strings.HasPrefix(base, ephemeralArtifactFilePrefix) {
		return true
	}
	for _, suffix := range ephemeralArtifactFileSuffixes {
		if strings.HasSuffix(base, suffix) {
			return true
		}
	}
	return false
}

// EphemeralArtifactExcludePatterns returns the same ephemeral-artifact
// pattern list IsEphemeralArtifactPath matches against, as glob/segment
// patterns a caller outside this package (the gitworktree adapter's
// MaterializeIntegrationCommit) can apply directly — one policy list, two
// consumers, per Checkpoint 8M.1: fingerprinting here, and excluding these
// same artifacts from the internal integration commit.
func EphemeralArtifactExcludePatterns() []string {
	patterns := append([]string{}, ephemeralArtifactDirs...)
	for _, suffix := range ephemeralArtifactFileSuffixes {
		patterns = append(patterns, "*"+suffix)
	}
	for name := range ephemeralArtifactFileExact {
		patterns = append(patterns, name)
	}
	patterns = append(patterns, ephemeralArtifactFilePrefix+"*")
	return patterns
}

// contentHash reads path (relative to worktreePath, exactly as reported by
// git status) and returns a short content hash, or a stable "deleted"/
// "unreadable" marker when the file can't be read — never an error, since a
// fingerprint must always be computable from whatever git status just said.
func contentHash(worktreePath, relPath string) string {
	if worktreePath == "" || relPath == "" {
		return "unreadable"
	}
	full := filepath.Join(worktreePath, relPath)
	if !strings.HasPrefix(full, filepath.Clean(worktreePath)+string(filepath.Separator)) {
		return "unreadable" // defensive: never read outside the reported worktree
	}
	data, err := os.ReadFile(full)
	if err != nil {
		if os.IsNotExist(err) {
			return "deleted"
		}
		return "unreadable"
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:8]) // short: this is one component of a larger hash, not the identity itself
}
