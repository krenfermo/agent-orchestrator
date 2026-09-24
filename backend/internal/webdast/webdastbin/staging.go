package webdastbin

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/aoagents/agent-orchestrator/backend/internal/skillegress/proxybin"
)

// staging.go — put the selected checker and its run config somewhere the
// container runtime can mount, and nowhere else. It mirrors proxybin's staging
// exactly, for the same reasons: exactly two files (the checker binary and its
// config), a read-only mount, and a directory that is AO's own and marked as
// such so cleanup can never become a delete of somebody's work.

// ErrStagingUnusable means AO has nowhere safe to put the checker.
var ErrStagingUnusable = errors.New("webdast/webdastbin: no usable checker staging directory")

// stagingDirName marks a root as AO's own. Cleanup refuses to remove anything
// whose path does not contain it.
const stagingDirName = ".ao-web-dast"

// ContainerDir is where the staged directory is bind-mounted, read-only.
const ContainerDir = "/aowebdast"

// BinaryName is the checker's fixed name inside the container.
const BinaryName = "ao-web-dast"

// ConfigName is the run config the checker reads inside the container. Unlike
// the proxy's policy this is NOT authority — the config's target is checked
// against the persisted authorization before it is written — but it is still
// mounted read-only and holds no secret.
const ConfigName = "config.json"

// StagingRootFor picks where to stage the checker for one project. Same
// constraints as proxybin: the project's parent directory (the one path AO can
// reason is inside the runtime's shared tree on macOS), never the home
// directory, never AO's data dir.
func StagingRootFor(projectPath, override string) (string, error) {
	base := strings.TrimSpace(override)
	if base == "" {
		if strings.TrimSpace(projectPath) == "" || !filepath.IsAbs(projectPath) {
			return "", fmt.Errorf("%w: project path %q must be absolute", ErrStagingUnusable, projectPath)
		}
		base = filepath.Dir(filepath.Clean(projectPath))
	}
	if !filepath.IsAbs(base) {
		return "", fmt.Errorf("%w: %q must be absolute", ErrStagingUnusable, base)
	}
	base = filepath.Clean(base)
	if base == "/" || base == "." {
		return "", fmt.Errorf("%w: %q has no usable parent", ErrStagingUnusable, projectPath)
	}
	if reason := refusedRoot(base); reason != "" {
		return "", fmt.Errorf("%w: %s", ErrStagingUnusable, reason)
	}
	return filepath.Join(base, stagingDirName), nil
}

func refusedRoot(base string) string {
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		home = filepath.Clean(home)
		if base == home {
			return fmt.Sprintf("refusing to stage the checker directly in the home directory %q; "+
				"set an explicit staging root", home)
		}
		if within(base, filepath.Join(home, ".ao")) {
			return fmt.Sprintf("refusing to stage the checker under AO's own state directory %q",
				filepath.Join(home, ".ao"))
		}
	}
	for _, env := range []string{"AO_DATA_DIR", "AO_RUN_FILE"} {
		v := strings.TrimSpace(os.Getenv(env))
		if v == "" || !filepath.IsAbs(v) {
			continue
		}
		if env == "AO_RUN_FILE" {
			v = filepath.Dir(v)
		}
		if within(base, filepath.Clean(v)) {
			return fmt.Sprintf("refusing to stage the checker under %s (%q); AO's data directory is not a mount source", env, v)
		}
	}
	return ""
}

func within(path, dir string) bool {
	path, dir = filepath.Clean(path), filepath.Clean(dir)
	return path == dir || strings.HasPrefix(path, dir+string(filepath.Separator))
}

// StageRequest describes one checker staging.
type StageRequest struct {
	Store       Store
	Root        string
	RunID       string
	RuntimeArch string
	// Config is the encoded run config the checker reads. An empty config is
	// refused: a checker with no config has no target and should not start.
	Config []byte
}

// Staged is one prepared checker mount.
type Staged struct {
	Dir        string
	BinaryPath string
	ConfigPath string
	Artifact   proxybin.Artifact
	Root       string
}

// ContainerBinary is the entrypoint AO passes to the runtime.
func (s Staged) ContainerBinary() string { return ContainerDir + "/" + BinaryName }

// ContainerConfig is the -config argument AO passes to the checker.
func (s Staged) ContainerConfig() string { return ContainerDir + "/" + ConfigName }

// Stage writes the selected checker and its config into a fresh per-run
// directory, and verifies on the way out that the binary on disk hashes to what
// was selected — the check that covers the window between selecting the bytes
// and mounting the file.
func Stage(req StageRequest) (Staged, error) {
	if strings.TrimSpace(req.Root) == "" || !filepath.IsAbs(req.Root) {
		return Staged{}, fmt.Errorf("%w: root %q must be absolute", ErrStagingUnusable, req.Root)
	}
	if !strings.Contains(req.Root, stagingDirName) {
		return Staged{}, fmt.Errorf("%w: root %q is not an AO checker staging root (it must end in %s)",
			ErrStagingUnusable, req.Root, stagingDirName)
	}
	if reason := refusedRoot(filepath.Dir(filepath.Clean(req.Root))); reason != "" {
		return Staged{}, fmt.Errorf("%w: %s", ErrStagingUnusable, reason)
	}
	if err := validRunID(req.RunID); err != nil {
		return Staged{}, err
	}
	if len(req.Config) == 0 {
		return Staged{}, fmt.Errorf("%w: no config to stage; a checker with no config has no target", ErrStagingUnusable)
	}
	goarch, err := ArchFromRuntime(req.RuntimeArch)
	if err != nil {
		return Staged{}, err
	}
	artifact, body, err := req.Store.Select(goarch)
	if err != nil {
		return Staged{}, err
	}

	if err := os.MkdirAll(req.Root, 0o711); err != nil { //nolint:gosec // a bind-mounted dir needs o+x for uid 65534 to traverse.
		return Staged{}, fmt.Errorf("%w: create root %q: %w", ErrStagingUnusable, req.Root, err)
	}
	dir := filepath.Join(req.Root, req.RunID)
	if err := os.Mkdir(dir, 0o711); err != nil { //nolint:gosec // as above.
		if os.IsExist(err) {
			return Staged{}, fmt.Errorf("%w: %q already exists; a leftover attempt is kept, never reused",
				ErrStagingUnusable, dir)
		}
		return Staged{}, fmt.Errorf("%w: create %q: %w", ErrStagingUnusable, dir, err)
	}
	staged := Staged{
		Dir:        dir,
		BinaryPath: filepath.Join(dir, BinaryName),
		ConfigPath: filepath.Join(dir, ConfigName),
		Artifact:   artifact,
		Root:       req.Root,
	}
	fail := func(err error) (Staged, error) {
		_ = staged.Cleanup()
		return Staged{}, err
	}
	if err := writeExclusive(staged.BinaryPath, body, 0o555); err != nil {
		return fail(err)
	}
	if err := writeExclusive(staged.ConfigPath, req.Config, 0o444); err != nil {
		return fail(err)
	}
	onDisk, err := os.ReadFile(staged.BinaryPath) //nolint:gosec // AO's own staging directory.
	if err != nil {
		return fail(fmt.Errorf("%w: read back %q: %w", ErrStagingUnusable, staged.BinaryPath, err))
	}
	if got := proxybin.Digest(onDisk); got != artifact.SHA256 {
		return fail(fmt.Errorf("%w: %s hashes to %s on disk, provenance records %s",
			ErrDigestMismatch, staged.BinaryPath, got, artifact.SHA256))
	}
	return staged, nil
}

func writeExclusive(path string, body []byte, mode os.FileMode) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, mode)
	if err != nil {
		return fmt.Errorf("%w: create %q: %w", ErrStagingUnusable, path, err)
	}
	if _, err := f.Write(body); err != nil {
		_ = f.Close()
		return fmt.Errorf("%w: write %q: %w", ErrStagingUnusable, path, err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("%w: close %q: %w", ErrStagingUnusable, path, err)
	}
	if err := os.Chmod(path, mode); err != nil {
		return fmt.Errorf("%w: chmod %q: %w", ErrStagingUnusable, path, err)
	}
	return nil
}

func validRunID(id string) error {
	if id == "" || len(id) > 64 {
		return fmt.Errorf("%w: run id %q must be 1..64 characters", ErrStagingUnusable, id)
	}
	for _, r := range id {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
		default:
			return fmt.Errorf("%w: run id %q may hold only letters, digits, '-' and '_'", ErrStagingUnusable, id)
		}
	}
	return nil
}

// Cleanup removes this run's directory, refusing any path that is not inside an
// AO checker staging root. The ROOT is kept (an empty marker directory) because
// removing and recreating a directory the runtime's shared mount is caching
// makes the next run's mount source intermittently fail on virtiofs.
func (s Staged) Cleanup() error {
	if s.Dir == "" {
		return nil
	}
	if !strings.Contains(s.Dir, stagingDirName) {
		return fmt.Errorf("webdast/webdastbin: refusing to remove %q: not an AO checker staging directory", s.Dir)
	}
	if err := os.RemoveAll(s.Dir); err != nil {
		return fmt.Errorf("webdast/webdastbin: remove %q: %w", s.Dir, err)
	}
	return nil
}
