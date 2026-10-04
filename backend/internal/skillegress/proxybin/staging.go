package proxybin

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// staging.go — put the selected proxy somewhere the container runtime can
// actually mount it, and nowhere else.
//
// # What the mount may contain
//
// Exactly two files: the proxy binary and the policy it enforces. Not a
// directory that happens to also hold them. The phase-7 test bind-mounted a
// scratch directory it also used for other things; a product cannot, because a
// read-only mount still exposes everything under it to a process AO does not
// trust, and "the proxy's directory" has to mean one thing.
//
// This is why neither the home directory nor AO's data dir is ever the mount
// source. AO's data dir holds the database, running.json and every project's
// state; mounting it to deliver one 6 MiB binary would put all of that inside
// a container to save creating a directory.
//
// # Why the permissions look loose and are not
//
// The container runs as uid 65534, which owns nothing on the host. For it to
// exec the proxy the binary must be executable by "other" -- 0555 -- and for
// the proxy to read its policy the policy must be readable by "other" -- 0444.
// What matters is what is NOT there: no write bit for anybody, and the mount
// itself is read-only. The run directory is 0711: traversable by a process that
// knows the path, and not listable.
//
// The policy is not a secret. It carries destinations, ports and a lease
// window, never a credential -- that is scoped_secret_delivery's job and it
// uses a different, 0600 path.

// ErrStagingUnusable means AO has nowhere safe to put the proxy. Like every
// other refusal in this area, the answer is not to run.
var ErrStagingUnusable = errors.New("skillegress/proxybin: no usable proxy staging directory")

// stagingDirName marks a root as AO's own. Cleanup refuses to remove anything
// whose path does not contain it, so a misconfigured root cannot turn teardown
// into a delete of somebody's work.
const stagingDirName = ".ao-egress-proxy"

// ContainerDir is where the staged directory is bind-mounted, read-only.
const ContainerDir = "/aoproxy"

// BinaryName is what the artifact is called inside the container. It is a
// fixed name rather than the architecture-suffixed one, so the entrypoint AO
// passes to the runtime does not vary with the host.
const BinaryName = "ao-egress-proxy"

// PolicyName is the policy file inside the container.
const PolicyName = "policy.json"

// StagingRootFor picks where to stage the proxy for one project.
//
// The choice is constrained by a fact AO does not control: on macOS the
// container runtime is a VM that shares only certain host paths, and a bind
// mount from any other path arrives EMPTY with no error. The project's parent
// directory is the one place AO can reason about -- if the project is mountable
// at all, its parent is inside the shared tree -- so it is the default, the same
// choice skillrunner.StagingRootFor makes for inputs and for the same reason.
//
// It refuses the home directory and refuses AO's data dir, including any path
// under either.
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

// refusedRoot names the roots AO will not mount, or "" when the base is fine.
//
// The home directory is not AO's to expose. AO's own data dir is worse: it
// holds the database and every project's state, and a read-only mount of it
// would hand all of that to a container that needed one binary.
func refusedRoot(base string) string {
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		home = filepath.Clean(home)
		if base == home {
			return fmt.Sprintf("refusing to stage the proxy directly in the home directory %q; "+
				"set an explicit staging root", home)
		}
		if within(base, filepath.Join(home, ".ao")) {
			return fmt.Sprintf("refusing to stage the proxy under AO's own state directory %q; "+
				"a read-only mount of it exposes the database and every project's state",
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
			return fmt.Sprintf("refusing to stage the proxy under %s (%q); "+
				"AO's data directory is not a mount source", env, v)
		}
	}
	return ""
}

// within reports whether path is dir or sits under it. It compares cleaned
// paths element-wise, so "/a/bc" is not treated as being under "/a/b".
func within(path, dir string) bool {
	path, dir = filepath.Clean(path), filepath.Clean(dir)
	return path == dir || strings.HasPrefix(path, dir+string(filepath.Separator))
}

// StageRequest describes one staging.
type StageRequest struct {
	// Store is where the artifact comes from. The zero Store packages nothing,
	// which fails closed rather than reaching for a host binary.
	Store Store
	// Root is the staging root from StagingRootFor.
	Root string
	// RunID names this run's directory. It must be a plain token: it becomes a
	// path element and a container argument.
	RunID string
	// RuntimeArch is what the CONTAINER RUNTIME reported it runs, verbatim --
	// "aarch64", "x86_64" -- not a GOARCH and not the daemon's own
	// architecture. Stage maps it itself, so there is no way for a caller to
	// ask for an architecture the runtime did not report: on macOS the daemon
	// and the VM can differ, and a caller allowed to name the GOARCH is a
	// caller who can name the wrong one.
	RuntimeArch string
	// Policy is the encoded policy the proxy will enforce. An empty policy is
	// refused: a proxy with no policy allows nothing and should not start, and
	// finding that out from a container's exit code is finding it out late.
	Policy []byte
}

// Staged is one prepared proxy mount.
type Staged struct {
	// Dir is the host directory to bind-mount read-only at ContainerDir. It
	// holds the binary and the policy, and nothing else.
	Dir string
	// BinaryPath and PolicyPath are the host paths, kept for the tests and for
	// an operator reading a failure.
	BinaryPath string
	PolicyPath string
	// Artifact is what was staged, with the digest that was verified twice.
	Artifact Artifact
	// Root is the staging root Dir lives under.
	Root string
}

// ContainerBinary is the entrypoint AO passes to the runtime.
func (s Staged) ContainerBinary() string { return ContainerDir + "/" + BinaryName }

// ContainerPolicy is the -policy argument AO passes to the proxy.
func (s Staged) ContainerPolicy() string { return ContainerDir + "/" + PolicyName }

// Stage writes the selected proxy and its policy into a fresh per-run
// directory, and verifies on the way out that what is on disk is what was
// selected.
//
// The digest is checked twice on purpose. Select checks the embedded bytes;
// this checks the file AFTER it was written, by reading it back. The two are
// different claims -- "AO carried the right binary" and "the right binary is
// what the container will mount" -- and only the second one is the mount.
//
// It refuses to reuse a directory that already exists. A leftover from a
// crashed attempt is somebody's evidence, and writing over it destroys that
// while also making "whose bytes are these" unanswerable.
func Stage(req StageRequest) (Staged, error) {
	if strings.TrimSpace(req.Root) == "" || !filepath.IsAbs(req.Root) {
		return Staged{}, fmt.Errorf("%w: root %q must be absolute", ErrStagingUnusable, req.Root)
	}
	if !strings.Contains(req.Root, stagingDirName) {
		return Staged{}, fmt.Errorf("%w: root %q is not an AO proxy staging root (it must end in %s)",
			ErrStagingUnusable, req.Root, stagingDirName)
	}
	if reason := refusedRoot(filepath.Dir(filepath.Clean(req.Root))); reason != "" {
		return Staged{}, fmt.Errorf("%w: %s", ErrStagingUnusable, reason)
	}
	if err := validRunID(req.RunID); err != nil {
		return Staged{}, err
	}
	if len(req.Policy) == 0 {
		return Staged{}, fmt.Errorf("%w: no policy to stage; a proxy with no policy allows nothing",
			ErrStagingUnusable)
	}
	goarch, err := ArchFromRuntime(req.RuntimeArch)
	if err != nil {
		return Staged{}, err
	}

	// Select BEFORE creating anything: a build with no proxy, or the wrong
	// architecture, must not leave a directory behind to explain.
	artifact, body, err := req.Store.Select(goarch)
	if err != nil {
		return Staged{}, err
	}

	// 0711, not 0750: the container runs as uid 65534, which is neither the
	// owner nor in the group, and it must be able to TRAVERSE to the binary it
	// execs. 0711 grants exactly that and no more — the directory cannot be
	// listed, and nothing in it can be written. gosec's rule assumes a
	// directory only its owner's processes reach; a bind mount is the case it
	// does not model.
	if err := os.MkdirAll(req.Root, 0o711); err != nil { //nolint:gosec // see above: a bind-mounted dir needs o+x.
		return Staged{}, fmt.Errorf("%w: create root %q: %w", ErrStagingUnusable, req.Root, err)
	}
	dir := filepath.Join(req.Root, req.RunID)
	if err := os.Mkdir(dir, 0o711); err != nil { //nolint:gosec // as above: the container must traverse to /aoproxy.
		if os.IsExist(err) {
			return Staged{}, fmt.Errorf("%w: %q already exists; a leftover attempt is kept, never reused",
				ErrStagingUnusable, dir)
		}
		return Staged{}, fmt.Errorf("%w: create %q: %w", ErrStagingUnusable, dir, err)
	}
	staged := Staged{
		Dir:        dir,
		BinaryPath: filepath.Join(dir, BinaryName),
		PolicyPath: filepath.Join(dir, PolicyName),
		Artifact:   artifact,
		Root:       req.Root,
	}
	// Any failure from here removes what was written. A half-staged directory
	// is a mount that would start a proxy with no policy, or no proxy at all.
	fail := func(err error) (Staged, error) {
		_ = staged.Cleanup()
		return Staged{}, err
	}
	if err := writeExclusive(staged.BinaryPath, body, 0o555); err != nil {
		return fail(err)
	}
	if err := writeExclusive(staged.PolicyPath, req.Policy, 0o444); err != nil {
		return fail(err)
	}

	// Read back what is actually on disk. This is the check that covers the
	// window between selecting the bytes and mounting the file.
	onDisk, err := os.ReadFile(staged.BinaryPath) //nolint:gosec // path is AO's own staging directory.
	if err != nil {
		return fail(fmt.Errorf("%w: read back %q: %w", ErrStagingUnusable, staged.BinaryPath, err))
	}
	if got := Digest(onDisk); got != artifact.SHA256 {
		return fail(fmt.Errorf("%w: %s hashes to %s on disk, provenance records %s",
			ErrDigestMismatch, staged.BinaryPath, got, artifact.SHA256))
	}
	return staged, nil
}

// writeExclusive creates a file that must not already exist and gives it the
// mode explicitly, so a permissive umask cannot widen it.
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
	// O_CREATE applies the umask; chmod does not. Set the mode we mean.
	if err := os.Chmod(path, mode); err != nil {
		return fmt.Errorf("%w: chmod %q: %w", ErrStagingUnusable, path, err)
	}
	return nil
}

// validRunID bounds what may become a path element. It is a token, not a path:
// a separator, a dot segment or a shell-hostile character is refused outright
// rather than sanitized, because a sanitized name is one whose meaning changed
// silently.
func validRunID(id string) error {
	if id == "" || len(id) > 64 {
		return fmt.Errorf("%w: run id %q must be 1..64 characters", ErrStagingUnusable, id)
	}
	for _, r := range id {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
		default:
			return fmt.Errorf("%w: run id %q may hold only letters, digits, '-' and '_'",
				ErrStagingUnusable, id)
		}
	}
	return nil
}

// Cleanup removes this run's directory. It refuses any path that is not inside
// an AO proxy staging root, so a bug in root selection cannot become a delete
// of somebody's files.
//
// The ROOT is kept, deliberately, for the reason skillrunner records: removing
// and recreating a directory the container runtime's shared mount is caching
// makes the next run's mount source intermittently fail to resolve on virtiofs.
// An empty marker directory is a much smaller cost than an unrunnable proxy.
func (s Staged) Cleanup() error {
	if s.Dir == "" {
		return nil
	}
	if !strings.Contains(s.Dir, stagingDirName) {
		return fmt.Errorf("skillegress/proxybin: refusing to remove %q: not an AO proxy staging directory", s.Dir)
	}
	// The files are 0555 and 0444; the directory is AO's, so removal works
	// without widening them first. Nothing here follows a link: RemoveAll
	// unlinks, and a symlink inside would be unlinked rather than traversed.
	if err := os.RemoveAll(s.Dir); err != nil {
		return fmt.Errorf("skillegress/proxybin: remove %q: %w", s.Dir, err)
	}
	return nil
}
