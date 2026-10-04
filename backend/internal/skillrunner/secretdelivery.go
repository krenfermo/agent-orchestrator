package skillrunner

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/aoagents/agent-orchestrator/backend/internal/skillcatalog"
	"github.com/aoagents/agent-orchestrator/backend/internal/skillsecrets"
)

// secretdelivery.go — handing one run its authorized secrets, and nothing else.
//
// The mechanism is a per-run directory, mode 0700, holding one 0600 file per
// reference, bind-mounted read-only at /run/secrets. Not an environment
// variable: `internal/agentcred` already records why, and the reasons have not
// changed — an env var is readable from the process table by anything running
// as this user, is inherited by every child the process spawns, and lands in
// crash dumps and `ps eww` output.
//
// # What is deliberately NOT mounted
//
// Not the home directory, not the Keychain, not a credential file, not AO's
// data dir. The mount is one directory AO created for this run and will delete,
// containing only what the lease authorized.
//
// # The residual risk, stated
//
// The value transits the host filesystem. A container runtime cannot pre-fill a
// tmpfs, so a bind mount is the mechanism available; the file exists for the
// length of the run with restrictive permissions and is overwritten and removed
// on every exit path. A real vault would deliver over a socket the value never
// leaves; AO is not one, and this comment is where somebody deciding whether
// that is good enough finds out.

// ErrSecretDelivery marks a delivery that could not be made safely.
var ErrSecretDelivery = errors.New("skillrunner: secret delivery failed")

// secretsDirName marks a directory as AO's secret staging. Cleanup refuses any
// path without it, so a bug in root selection cannot turn teardown into a
// delete of somebody's files.
const secretsDirName = ".ao-skill-secrets"

// ContainerSecretsPath is where the delivery is mounted inside the container.
const ContainerSecretsPath = "/run/secrets"

// SecretDelivery is one run's materialized secrets.
type SecretDelivery struct {
	// Dir is the host directory bind-mounted read-only at ContainerSecretsPath.
	Dir string
	// Refs are the names delivered, sorted. Names only — this struct never
	// holds a value, so it can be logged and embedded in evidence.
	Refs []skillsecrets.Ref
	// LeaseID records which single-use right was redeemed, for the audit line.
	LeaseID string
}

// SecretsRootFor picks where to materialize secrets for one project.
//
// Same constraint as input staging: the container runtime may be a VM that
// shares only certain host paths, and the project's parent is the one place AO
// can reason about. It is a SEPARATE root from the input staging directory so a
// bug in one cannot expose the other, and it is never the home directory and
// never AO's data dir.
func SecretsRootFor(projectPath, override string) (string, error) {
	if trimmed := strings.TrimSpace(override); trimmed != "" {
		if !filepath.IsAbs(trimmed) {
			return "", fmt.Errorf("%w: override %q must be absolute", ErrSecretDelivery, trimmed)
		}
		return filepath.Join(trimmed, secretsDirName), nil
	}
	if strings.TrimSpace(projectPath) == "" || !filepath.IsAbs(projectPath) {
		return "", fmt.Errorf("%w: project path %q must be absolute", ErrSecretDelivery, projectPath)
	}
	parent := filepath.Dir(filepath.Clean(projectPath))
	if parent == "/" || parent == "." {
		return "", fmt.Errorf("%w: project path %q has no usable parent", ErrSecretDelivery, projectPath)
	}
	home, err := os.UserHomeDir()
	if err == nil {
		cleanHome := filepath.Clean(home)
		if filepath.Clean(parent) == cleanHome {
			return "", fmt.Errorf("%w: refusing to materialize secrets directly in the home "+
				"directory %q", ErrSecretDelivery, home)
		}
	}
	return filepath.Join(parent, secretsDirName), nil
}

// DeliverSecrets writes the values to a private directory for one run.
//
// It refuses before writing anything when a reference is not a valid name,
// when a value is empty, or when the directory cannot be made private. A
// partial delivery is never left behind: any failure removes everything.
func DeliverSecrets(
	root, runID string, values map[skillsecrets.Ref]skillsecrets.SecretValue, leaseID string,
) (SecretDelivery, error) {
	if strings.TrimSpace(root) == "" || !strings.Contains(root, secretsDirName) {
		return SecretDelivery{}, fmt.Errorf("%w: %q is not an AO secrets root", ErrSecretDelivery, root)
	}
	if len(values) == 0 {
		return SecretDelivery{}, fmt.Errorf("%w: nothing to deliver", ErrSecretDelivery)
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		return SecretDelivery{}, fmt.Errorf("%w: create secrets root: %w", ErrSecretDelivery, err)
	}
	// Re-assert the mode: MkdirAll leaves an existing directory's permissions
	// alone, so a root created loosely by something else stays loose.
	//nolint:gosec // G302: 0700 is required, not excessive -- this is a
	// DIRECTORY, and the execute bit is what makes it traversable by its
	// owner at all. The files inside are 0600.
	if err := os.Chmod(root, 0o700); err != nil {
		return SecretDelivery{}, fmt.Errorf("%w: restrict secrets root: %w", ErrSecretDelivery, err)
	}

	dir := filepath.Join(root, runID)
	// A leftover directory from a crashed run must not be reused: its contents
	// are unknown and its permissions may not be ours.
	if err := os.RemoveAll(dir); err != nil {
		return SecretDelivery{}, fmt.Errorf("%w: clear %q: %w", ErrSecretDelivery, dir, err)
	}
	if err := os.Mkdir(dir, 0o700); err != nil {
		return SecretDelivery{}, fmt.Errorf("%w: create %q: %w", ErrSecretDelivery, dir, err)
	}
	delivery := SecretDelivery{Dir: dir, LeaseID: leaseID}

	fail := func(err error) (SecretDelivery, error) {
		_ = delivery.Cleanup()
		return SecretDelivery{}, err
	}
	for ref, value := range values {
		if _, err := skillsecrets.ParseRef(string(ref)); err != nil {
			return fail(fmt.Errorf("%w: %w", ErrSecretDelivery, err))
		}
		if value.IsZero() {
			return fail(fmt.Errorf("%w: %s has no value", ErrSecretDelivery, ref))
		}
		// The name is validated UPPER_SNAKE_CASE, so it cannot contain a
		// separator and the join cannot escape the directory. Asserted anyway,
		// because "cannot" is a claim worth checking where a secret is written.
		target := filepath.Join(dir, string(ref))
		if filepath.Dir(target) != dir {
			return fail(fmt.Errorf("%w: %s does not resolve inside the delivery", ErrSecretDelivery, ref))
		}
		if err := os.WriteFile(target, []byte(value.Reveal()), 0o600); err != nil {
			// The error is not wrapped with anything derived from the value.
			return fail(fmt.Errorf("%w: could not write %s", ErrSecretDelivery, ref))
		}
		delivery.Refs = append(delivery.Refs, ref)
	}
	sort.Slice(delivery.Refs, func(i, j int) bool { return delivery.Refs[i] < delivery.Refs[j] })
	return delivery, nil
}

// Cleanup overwrites and removes the delivery. It is safe to call twice and on
// every exit path — success, failure, timeout and cancellation — which is the
// only way a secret file does not outlive the run that needed it.
func (d SecretDelivery) Cleanup() error {
	if d.Dir == "" {
		return nil
	}
	if !strings.Contains(d.Dir, secretsDirName) {
		return fmt.Errorf("%w: refusing to remove %q: not an AO secrets directory", ErrSecretDelivery, d.Dir)
	}
	// Overwrite before unlinking. On a copy-on-write or journaling filesystem
	// this does not guarantee the bytes are gone — that is a property of the
	// medium, not of this call — but it removes them from anything that reads
	// the file by path, which is every reader AO can influence.
	entries, err := os.ReadDir(d.Dir)
	if err == nil {
		for _, entry := range entries {
			p := filepath.Join(d.Dir, entry.Name())
			if info, statErr := entry.Info(); statErr == nil && info.Mode().IsRegular() {
				_ = os.WriteFile(p, make([]byte, info.Size()), 0o600)
			}
		}
	}
	if err := os.RemoveAll(d.Dir); err != nil {
		return fmt.Errorf("%w: remove %q: %w", ErrSecretDelivery, d.Dir, err)
	}
	return nil
}

// MountArgs are the runtime flags that expose the delivery, read-only.
func (d SecretDelivery) MountArgs() []string {
	if d.Dir == "" {
		return nil
	}
	return []string{"-v", d.Dir + ":" + ContainerSecretsPath + ":ro"}
}

// RefNames renders the delivered references for an audit line. Names only.
func (d SecretDelivery) RefNames() []string {
	out := make([]string, 0, len(d.Refs))
	for _, r := range d.Refs {
		out = append(out, string(r))
	}
	return out
}

// SecretEvidenceScript is the shell that reports which secret references the
// container can see. It is one definition shared by every workload — the
// generic evidence probe, the static scanner, and any future tool — because
// three copies of it would drift and the drift would show up as a delivery
// that verified against the wrong thing.
//
// It emits NAMES only. A value never reaches evidence, and neither does a
// prefix or a hash of one: a prefix is enough to confirm a guess.
const SecretEvidenceScript = `echo "ao_secret_refs=$(ls -1 ` + ContainerSecretsPath +
	` 2>/dev/null | sort | tr '
' ',' | sed 's/,$//')"`

// SecretControl is the control this delivery mechanism provides, and it is
// attested only when a delivery has actually been verified inside a container
// (see VerifyDelivered). A runtime that merely COULD do this attests nothing.
const SecretControl = skillcatalog.ControlScopedSecretDelivery

// VerifyDelivered checks that the container saw exactly the references AO
// delivered — no more, no fewer.
//
// `seen` is the sorted list of filenames the run reported from inside
// ContainerSecretsPath. Fewer means the mount did not arrive, which is the
// silent-empty-mount failure again. MORE means the container has a file AO did
// not put there, which is worse and must never be treated as success.
func (d SecretDelivery) VerifyDelivered(seen []string) error {
	want := d.RefNames()
	got := append([]string(nil), seen...)
	sort.Strings(got)
	if len(got) != len(want) {
		return fmt.Errorf("%w: the container saw %d secret files and AO delivered %d",
			ErrSecretDelivery, len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			return fmt.Errorf("%w: the container's secret set does not match what AO delivered",
				ErrSecretDelivery)
		}
	}
	return nil
}
