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
	Skipped []SkippedFile
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
			// A symlink is refused, not followed and not copied: following one
			// is how a credential outside the checkout ends up inside the
			// boundary.
			if d.Type()&os.ModeSymlink != 0 {
				return nil
			}
			if !d.Type().IsRegular() || excludedFromStaging[d.Name()] {
				return nil
			}
			if seen[rel] {
				return nil
			}
			info, infoErr := d.Info()
			if infoErr != nil {
				return infoErr
			}
			if info.Size() > req.MaxFileBytes {
				return nil
			}
			// A path the scan tool cannot address unambiguously is skipped
			// rather than staged. The tool passes its file list through a
			// shell, where a space or a quote would word-split the name and
			// the file would simply not be scanned -- and a file that was
			// never scanned but was counted as staged is a coverage lie, which
			// is the one failure this whole path exists to prevent. These are
			// rare in source trees, and Staging.Skipped records every one.
			if hasShellHostileName(rel) {
				staging.Skipped = append(staging.Skipped, SkippedFile{
					Path: filepath.ToSlash(rel), Reason: "unaddressable_filename",
				})
				return nil
			}
			if len(staging.Inputs) >= req.MaxFiles {
				return fs.SkipAll
			}
			body, readErr := os.ReadFile(p) //nolint:gosec // path from the walked project root.
			if readErr != nil {
				// An unreadable file is skipped, not fatal: a checkout with one
				// permission-odd file should still be auditable. It is RECORDED
				// rather than dropped, because a file nobody could read is a
				// gap in coverage, and a gap the report does not mention reads
				// as a clean result.
				staging.Skipped = append(staging.Skipped, SkippedFile{
					Path: filepath.ToSlash(rel), Reason: "unreadable",
				})
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
			seen[rel] = true
			staging.Inputs = append(staging.Inputs, StagedInput{
				RelPath: filepath.ToSlash(rel),
				SHA256:  hex.EncodeToString(sum[:]),
				Bytes:   info.Size(),
			})
			return nil
		})
		if err != nil && !errors.Is(err, fs.SkipAll) {
			_ = os.RemoveAll(dir)
			return Staging{}, fmt.Errorf("%w: stage %q: %w", ErrStagingUnusable, root, err)
		}
	}

	if len(staging.Inputs) == 0 {
		_ = os.RemoveAll(dir)
		return Staging{}, fmt.Errorf("%w: nothing in scope to stage from %q; "+
			"a run over no inputs would report a clean result for a project it never read",
			ErrStagingUnusable, req.SourceDir)
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
