package skillrunner

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// staging.go — put exactly the in-scope files somewhere the container can
// actually see, and prove both halves of that sentence.
//
// Two problems are solved here, and they are different:
//
//  1. SCOPE. Mounting the project checkout would hand the container everything
//     in it and leave "only look at these paths" as an instruction in a prompt.
//     Staging copies only the in-scope files, so the scope is enforced by what
//     exists on the mount rather than by what the skill was told.
//
//  2. VISIBILITY. On macOS the container runtime is a VM that shares only
//     configured host paths, and a bind mount from any other path arrives as an
//     EMPTY directory with no error and exit 0. A security audit over an empty
//     mount reads nothing, finds nothing, and reports a clean result for a
//     project it never opened. So AO records a digest per staged file and the
//     run reports what it could see; a mismatch fails the run.
//
// Neither ~/.ao nor the user's home is used: the first is outside the VM's
// mount set on a normal colima install, and the second is not AO's to expose.

// ErrStagingUnusable means AO could not put inputs anywhere the container will
// see them. Like every other refusal here, the answer is not to run.
var ErrStagingUnusable = errors.New("skillrunner: no usable staging directory")

// StagedInput is one file AO placed on the mount.
type StagedInput struct {
	// RelPath is the path inside the container, relative to /work.
	RelPath string
	// SHA256 is the content digest AO wrote. The run reports what it read back;
	// a mismatch means the mount did not deliver what AO staged.
	SHA256 string
	Bytes  int64
}

// Staging is one prepared input set.
type Staging struct {
	// Dir is the host directory bind-mounted read-only at /work.
	Dir string
	// Inputs are the files AO placed there, sorted by path.
	Inputs []StagedInput
	// Root is the staging root Dir lives under, kept so cleanup can tell an
	// AO-owned directory from anything else.
	Root string
	// Skipped are files AO deliberately did not stage, with the reason. They
	// belong in the report's coverage: "we did not look" and "we looked and
	// found nothing" are different results.
	//
	// The list is CAPPED at maxRecordedSkips; SkippedCount is not. Read the
	// count for the arithmetic and the list for the detail.
	Skipped []SkippedFile
	// Discovered is every candidate file the walk encountered, counted once
	// per path, before any exclusion. It is the denominator: Discovered ==
	// len(Inputs) + SkippedCount is the invariant that makes a file
	// impossible to lose between the tree and the report.
	Discovered int
	// SkippedCount is the exact number of pre-staging exclusions, whether or
	// not each one is enumerated in Skipped.
	SkippedCount int
	// SkippedTruncated says Skipped holds fewer entries than SkippedCount.
	SkippedTruncated bool
}

// maxRecordedSkips bounds the ENUMERATION of skipped files, never the count.
// A repository whose file budget is exhausted can produce hundreds of
// thousands of exclusions; listing every one would make the report unusable
// and the response enormous. Losing the COUNT, on the other hand, is the
// defect this file exists to prevent, so the count is always exact.
const maxRecordedSkips = 500

// skip records one pre-staging exclusion.
//
// Every path out of the walk that does not stage a file goes through here.
// That is the point: the previous version had four such paths and only two of
// them recorded anything, so a file could leave the walk without leaving a
// trace, and the report then described a smaller project than the one on disk.
func (s *Staging) skip(rel, reason string, size, limit int64) {
	s.SkippedCount++
	if len(s.Skipped) >= maxRecordedSkips {
		s.SkippedTruncated = true
		return
	}
	s.Skipped = append(s.Skipped, SkippedFile{
		Path:   filepath.ToSlash(rel),
		Reason: reason,
		Stage:  SkipAtStaging,
		Bytes:  size,
		Limit:  limit,
	})
}

// skipSummary is a reason histogram for the refusal message when a project
// stages nothing. "Nothing to scan" and "everything was excluded, here is why"
// are different answers, and only the second one tells an operator what to do.
func skipSummary(skipped []SkippedFile, total int) string {
	if total == 0 {
		return "no candidate files"
	}
	counts := map[string]int{}
	order := []string{}
	for _, sk := range skipped {
		if _, seen := counts[sk.Reason]; !seen {
			order = append(order, sk.Reason)
		}
		counts[sk.Reason]++
	}
	sort.Strings(order)
	parts := make([]string, 0, len(order))
	for _, reason := range order {
		parts = append(parts, fmt.Sprintf("%s x%d", reason, counts[reason]))
	}
	return strings.Join(parts, ", ")
}

// stagingDirName marks the root as AO's. Cleanup refuses to remove a directory
// whose path does not contain it, so a misconfigured root cannot turn teardown
// into a delete of somebody's work.
const stagingDirName = ".ao-skill-staging"

// StagingRootFor picks where to stage for one project.
//
// The choice is constrained by a fact AO does not control: the container
// runtime may be a VM that shares only certain host paths. The project's own
// parent directory is the one place AO can reason about — if the project is
// mountable at all, its parent is inside the shared tree — so that is the
// default. An operator can override it when their layout differs.
//
// It deliberately never returns a path under the user's home root or under
// AO's data dir.
func StagingRootFor(projectPath, override string) (string, error) {
	if trimmed := strings.TrimSpace(override); trimmed != "" {
		if !filepath.IsAbs(trimmed) {
			return "", fmt.Errorf("%w: override %q must be absolute", ErrStagingUnusable, trimmed)
		}
		return filepath.Join(trimmed, stagingDirName), nil
	}
	if strings.TrimSpace(projectPath) == "" || !filepath.IsAbs(projectPath) {
		return "", fmt.Errorf("%w: project path %q must be absolute", ErrStagingUnusable, projectPath)
	}
	parent := filepath.Dir(filepath.Clean(projectPath))
	if parent == "/" || parent == "." {
		return "", fmt.Errorf("%w: project path %q has no usable parent", ErrStagingUnusable, projectPath)
	}
	home, err := os.UserHomeDir()
	if err == nil && filepath.Clean(parent) == filepath.Clean(home) {
		return "", fmt.Errorf("%w: refusing to stage directly in the home directory %q; "+
			"set an explicit staging root", ErrStagingUnusable, home)
	}
	return filepath.Join(parent, stagingDirName), nil
}

// StageRequest describes what to stage.
type StageRequest struct {
	// SourceDir is the project checkout to copy from.
	SourceDir string
	// ScopePaths are repo-relative paths to include. Empty means the whole
	// checkout, minus the exclusions below.
	ScopePaths []string
	// Root is the staging root from StagingRootFor.
	Root string
	// RunID names this run's subdirectory.
	RunID string
	// MaxFiles and MaxFileBytes bound what is copied, so staging cannot fill
	// the disk before the container's limits ever apply.
	MaxFiles     int
	MaxFileBytes int64
}

// excludedFromStaging never reaches the container. `.git` is the important
// one: it holds every version of every file, so staging it would defeat a
// scope restriction and hand over deleted secrets besides.
var excludedFromStaging = map[string]bool{
	".git": true, "node_modules": true, "vendor": true, ".venv": true,
	"__pycache__": true, "dist": true, "build": true, "target": true,
	stagingDirName: true,
}

// Stage copies the in-scope files into a fresh per-run directory.
//
// It refuses rather than stage a symlink: a link inside the checkout could
// point at a credential outside it, and a copy that follows it would carry the
// credential across the boundary that exists to stop exactly that.
func Stage(req StageRequest) (Staging, error) {
	if strings.TrimSpace(req.SourceDir) == "" || !filepath.IsAbs(req.SourceDir) {
		return Staging{}, fmt.Errorf("%w: source %q must be absolute", ErrStagingUnusable, req.SourceDir)
	}
	if strings.TrimSpace(req.Root) == "" || !strings.Contains(req.Root, stagingDirName) {
		return Staging{}, fmt.Errorf("%w: staging root %q is not an AO staging root",
			ErrStagingUnusable, req.Root)
	}
	if req.MaxFiles < 1 || req.MaxFileBytes < 1 {
		return Staging{}, fmt.Errorf("%w: staging limits are required", ErrStagingUnusable)
	}

	dir := filepath.Join(req.Root, req.RunID)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return Staging{}, fmt.Errorf("%w: create %q: %w", ErrStagingUnusable, dir, err)
	}
	staging := Staging{Dir: dir, Root: req.Root}

	roots, err := scopeRoots(req.SourceDir, req.ScopePaths)
	if err != nil {
		_ = os.RemoveAll(dir)
		return Staging{}, err
	}

	seen := map[string]bool{}
	for _, root := range roots {
		err := filepath.WalkDir(root, func(p string, d fs.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			rel, relErr := filepath.Rel(req.SourceDir, p)
			if relErr != nil {
				return relErr
			}
			if d.IsDir() {
				if excludedFromStaging[d.Name()] {
					return filepath.SkipDir
				}
				return nil
			}
			// The candidate is counted HERE, once per path, before any
			// exclusion can send it down one of the branches below. Counting
			// later -- as each branch's business -- is how a branch that
			// forgot to count made a file vanish.
			//
			// `seen` now means "already considered", not "already staged", so
			// a path reachable through two scope roots is one candidate.
			if seen[rel] {
				return nil
			}
			seen[rel] = true
			staging.Discovered++

			// A symlink is refused, not followed and not copied: following one
			// is how a credential outside the checkout ends up inside the
			// boundary. It is RECORDED, because "we refused to read this" and
			// "this was not here" are different facts about the scope.
			if d.Type()&os.ModeSymlink != 0 {
				staging.skip(rel, SkipReasonSymlink, 0, 0)
				return nil
			}
			if excludedFromStaging[d.Name()] {
				staging.skip(rel, SkipReasonExcludedName, 0, 0)
				return nil
			}
			if !d.Type().IsRegular() {
				staging.skip(rel, SkipReasonNotRegular, 0, 0)
				return nil
			}
			info, infoErr := d.Info()
			if infoErr != nil {
				return infoErr
			}
			// Oversize, recorded with the size AND the bound.
			//
			// This is the single source of truth for the size decision. The
			// scan tool carries its own MAX_BYTES guard against the same
			// bound, so nothing that reaches the mount can trip it -- see the
			// note in staticScanArgv. Keeping the decision here means the
			// oversized file is never copied into the container, and keeping
			// it in exactly one place means it cannot be counted twice.
			if info.Size() > req.MaxFileBytes {
				staging.skip(rel, SkipReasonTooLarge, info.Size(), req.MaxFileBytes)
				return nil
			}
			// A path the scan tool cannot address unambiguously is skipped
			// rather than staged. The tool passes its file list through a
			// shell, where a space or a quote would word-split the name and
			// the file would simply not be scanned -- and a file that was
			// never scanned but was counted as staged is a coverage lie, which
			// is the one failure this whole path exists to prevent.
			if hasShellHostileName(rel) {
				staging.skip(rel, SkipReasonUnaddressable, 0, 0)
				return nil
			}
			// Past the budget the walk CONTINUES, recording rather than
			// stopping. It used to return fs.SkipAll, which abandoned the walk
			// and left every remaining file uncounted: the report then had no
			// way to say how much of the project it had not looked at, on
			// exactly the repositories -- the large ones -- where that matters
			// most. Continuing costs a stat per file and no read or copy.
			if len(staging.Inputs) >= req.MaxFiles {
				staging.skip(rel, SkipReasonBudget, info.Size(), 0)
				return nil
			}
			body, readErr := os.ReadFile(p) //nolint:gosec // path from the walked project root.
			if readErr != nil {
				// An unreadable file is skipped, not fatal: a checkout with one
				// permission-odd file should still be auditable. It is RECORDED
				// rather than dropped, because a file nobody could read is a
				// gap in coverage, and a gap the report does not mention reads
				// as a clean result.
				staging.skip(rel, SkipReasonUnreadable, 0, 0)
				return nil //nolint:nilerr // deliberate: one unreadable file must not fail the scan.
			}
			target := filepath.Join(dir, rel)
			if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
				return err
			}
			if err := os.WriteFile(target, body, 0o600); err != nil {
				return err
			}
			sum := sha256.Sum256(body)
			staging.Inputs = append(staging.Inputs, StagedInput{
				RelPath: filepath.ToSlash(rel),
				SHA256:  hex.EncodeToString(sum[:]),
				Bytes:   info.Size(),
			})
			return nil
		})
		// No branch returns fs.SkipAll any more: the budget path records and
		// carries on, so any error here is a real one.
		if err != nil {
			_ = os.RemoveAll(dir)
			return Staging{}, fmt.Errorf("%w: stage %q: %w", ErrStagingUnusable, root, err)
		}
	}

	if len(staging.Inputs) == 0 {
		_ = os.RemoveAll(dir)
		// The reasons are named. A project of nothing but oversized files and
		// a project of nothing at all both stage zero inputs, and an operator
		// can act on the first one the moment the message distinguishes them.
		return Staging{}, fmt.Errorf("%w: nothing in scope to stage from %q: "+
			"%d candidate file(s), all excluded before staging (%s); "+
			"a run over no inputs would report a clean result for a project it never read",
			ErrStagingUnusable, req.SourceDir, staging.Discovered,
			skipSummary(staging.Skipped, staging.SkippedCount))
	}
	sort.Slice(staging.Inputs, func(i, j int) bool {
		return staging.Inputs[i].RelPath < staging.Inputs[j].RelPath
	})
	return staging, nil
}

// hasShellHostileName reports whether a path cannot be passed through the scan
// tool's shell word-splitting intact.
func hasShellHostileName(rel string) bool {
	return strings.ContainsAny(rel, " \t\n\r'\"\\$`*?[]{}();&|<>!#~")
}

// scopeRoots resolves the requested scope to absolute directories inside the
// checkout, refusing anything that escapes it.
func scopeRoots(sourceDir string, scopePaths []string) ([]string, error) {
	if len(scopePaths) == 0 {
		return []string{sourceDir}, nil
	}
	clean := filepath.Clean(sourceDir)
	out := make([]string, 0, len(scopePaths))
	for _, raw := range scopePaths {
		trimmed := strings.TrimSpace(raw)
		if trimmed == "" {
			continue
		}
		if filepath.IsAbs(trimmed) {
			return nil, fmt.Errorf("%w: scope path %q must be repo-relative", ErrStagingUnusable, trimmed)
		}
		joined := filepath.Join(clean, trimmed)
		rel, err := filepath.Rel(clean, joined)
		if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return nil, fmt.Errorf("%w: scope path %q escapes the project", ErrStagingUnusable, trimmed)
		}
		if _, err := os.Stat(joined); err != nil {
			return nil, fmt.Errorf("%w: scope path %q does not exist", ErrStagingUnusable, trimmed)
		}
		out = append(out, joined)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("%w: no usable scope paths", ErrStagingUnusable)
	}
	return out, nil
}

// Cleanup removes this run's staged inputs. It refuses any path that is not
// under an AO staging root, so a bug in root selection cannot turn teardown
// into a delete of somebody's project.
func (s Staging) Cleanup() error {
	if s.Dir == "" {
		return nil
	}
	if !strings.Contains(s.Dir, stagingDirName) {
		return fmt.Errorf("skillrunner: refusing to remove %q: not an AO staging directory", s.Dir)
	}
	if err := os.RemoveAll(s.Dir); err != nil {
		return fmt.Errorf("skillrunner: remove staging %q: %w", s.Dir, err)
	}
	// The per-run directory goes; the ROOT stays, deliberately. Removing and
	// recreating it churns a path the container runtime's shared mount is
	// caching, and on virtiofs the next run's mount source then fails to
	// resolve ("no such file or directory") for a directory that demonstrably
	// exists on the host. An empty marker directory is a much smaller cost
	// than an intermittently unrunnable scan.
	return nil
}

// VerifyVisible checks that the container saw what AO staged. `visible` is the
// file count the run reported from inside the mount.
//
// A count below what AO wrote is the silent-empty-mount failure, or a mount
// that delivered only part of the tree. Either way the report that would come
// out of it describes a project the scan did not read.
func (s Staging) VerifyVisible(visible int) error {
	switch {
	case visible == 0:
		return fmt.Errorf("%w: the container saw an empty /work while AO staged %d files; "+
			"the host path is almost certainly outside the container runtime's shared mounts",
			ErrStagingUnusable, len(s.Inputs))
	case visible < len(s.Inputs):
		return fmt.Errorf("%w: the container saw %d of %d staged files, so the scan did not read "+
			"the whole scope", ErrStagingUnusable, visible, len(s.Inputs))
	}
	return nil
}
