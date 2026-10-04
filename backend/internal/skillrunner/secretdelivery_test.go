package skillrunner

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/skillsecrets"
)

const syntheticSecret = "SYNTHETIC-SECRET-NEVER-A-REAL-CREDENTIAL"

func secretsRoot(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), secretsDirName)
}

func deliverOne(t *testing.T, root string) SecretDelivery {
	t.Helper()
	d, err := DeliverSecrets(root, "run-test", map[skillsecrets.Ref]skillsecrets.SecretValue{
		"SENTRY_DSN": skillsecrets.NewSecretValue(syntheticSecret),
	}, "lease-1")
	if err != nil {
		t.Fatalf("DeliverSecrets: %v", err)
	}
	t.Cleanup(func() { _ = d.Cleanup() })
	return d
}

// Files, not environment variables, with restrictive permissions — the rule
// internal/agentcred already established for the same reasons.
func TestDeliverSecrets_WritesPrivateFiles(t *testing.T) {
	root := secretsRoot(t)
	d := deliverOne(t, root)

	info, err := os.Stat(d.Dir)
	if err != nil {
		t.Fatalf("stat delivery dir: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o700 {
		t.Fatalf("delivery dir mode = %o, want 700", perm)
	}
	rootInfo, err := os.Stat(root)
	if err != nil {
		t.Fatalf("stat root: %v", err)
	}
	if perm := rootInfo.Mode().Perm(); perm != 0o700 {
		t.Fatalf("secrets root mode = %o, want 700", perm)
	}

	p := filepath.Join(d.Dir, "SENTRY_DSN")
	fileInfo, err := os.Stat(p)
	if err != nil {
		t.Fatalf("stat secret file: %v", err)
	}
	if perm := fileInfo.Mode().Perm(); perm != 0o600 {
		t.Fatalf("secret file mode = %o, want 600", perm)
	}
	body, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(body) != syntheticSecret {
		t.Fatal("the delivered file does not hold the value")
	}

	// The mount is read-only and is the only extra mount a run gets.
	args := strings.Join(d.MountArgs(), " ")
	if !strings.HasSuffix(args, ":"+ContainerSecretsPath+":ro") {
		t.Fatalf("mount args = %q", args)
	}
	// The struct itself carries names, never values, so it is safe to log.
	if strings.Contains(strings.Join(d.RefNames(), " "), syntheticSecret) {
		t.Fatal("the delivery's names carry a value")
	}
}

// A directory left behind by a crashed run must not be reused: its contents
// are unknown and its permissions may not be ours.
func TestDeliverSecrets_ClearsALeftoverDirectory(t *testing.T) {
	root := secretsRoot(t)
	stale := filepath.Join(root, "run-test")
	if err := os.MkdirAll(stale, 0o777); err != nil {
		t.Fatalf("mkdir stale: %v", err)
	}
	if err := os.WriteFile(filepath.Join(stale, "OLD_SECRET"), []byte("stale\n"), 0o644); err != nil {
		t.Fatalf("write stale: %v", err)
	}

	d := deliverOne(t, root)
	if _, err := os.Stat(filepath.Join(d.Dir, "OLD_SECRET")); !os.IsNotExist(err) {
		t.Fatalf("a stale secret survived: %v", err)
	}
	info, err := os.Stat(d.Dir)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o700 {
		t.Fatalf("the reused directory kept mode %o", perm)
	}
}

// Cleanup on every exit path is the only thing that stops a secret file from
// outliving the run that needed it.
func TestDeliverSecrets_CleanupOverwritesAndRemoves(t *testing.T) {
	root := secretsRoot(t)
	d := deliverOne(t, root)
	p := filepath.Join(d.Dir, "SENTRY_DSN")

	if err := d.Cleanup(); err != nil {
		t.Fatalf("Cleanup: %v", err)
	}
	if _, err := os.Stat(p); !os.IsNotExist(err) {
		t.Fatalf("the secret file survived cleanup: %v", err)
	}
	if _, err := os.Stat(d.Dir); !os.IsNotExist(err) {
		t.Fatalf("the delivery dir survived cleanup: %v", err)
	}
	// Safe to call twice, which is what makes "on every exit path" workable.
	if err := d.Cleanup(); err != nil {
		t.Fatalf("second Cleanup: %v", err)
	}
}

// Cleanup refuses a path outside AO's namespace, so a bug in root selection
// cannot turn teardown into a delete of somebody's files.
func TestSecretCleanup_RefusesAPathItDoesNotOwn(t *testing.T) {
	victim := t.TempDir()
	if err := os.WriteFile(filepath.Join(victim, "keep.txt"), []byte("keep\n"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	err := SecretDelivery{Dir: victim}.Cleanup()
	if !errors.Is(err, ErrSecretDelivery) || !strings.Contains(err.Error(), "not an AO secrets directory") {
		t.Fatalf("err = %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(victim, "keep.txt")); statErr != nil {
		t.Fatalf("Cleanup deleted a directory it does not own: %v", statErr)
	}
}

// A failure part-way through leaves nothing behind: a partial delivery is a
// directory of secrets nobody is tracking.
func TestDeliverSecrets_RefusalsLeaveNothingBehind(t *testing.T) {
	root := secretsRoot(t)
	cases := []struct {
		name    string
		values  map[skillsecrets.Ref]skillsecrets.SecretValue
		root    string
		wantSub string
	}{
		{
			"root outside AO's namespace", map[skillsecrets.Ref]skillsecrets.SecretValue{
				"SENTRY_DSN": skillsecrets.NewSecretValue(syntheticSecret),
			}, t.TempDir(), "not an AO secrets root",
		},
		{"nothing to deliver", map[skillsecrets.Ref]skillsecrets.SecretValue{}, root, "nothing to deliver"},
		{
			"a name that is not a valid reference", map[skillsecrets.Ref]skillsecrets.SecretValue{
				"SENTRY_DSN": skillsecrets.NewSecretValue(syntheticSecret),
				"../ESCAPE":  skillsecrets.NewSecretValue(syntheticSecret),
			}, root, "UPPER_SNAKE_CASE",
		},
		{
			"an empty value", map[skillsecrets.Ref]skillsecrets.SecretValue{
				"SENTRY_DSN": skillsecrets.NewSecretValue(""),
			}, root, "has no value",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := DeliverSecrets(tc.root, "run-neg", tc.values, "lease-1")
			if !errors.Is(err, ErrSecretDelivery) {
				t.Fatalf("err = %v, want ErrSecretDelivery", err)
			}
			if !strings.Contains(err.Error(), tc.wantSub) {
				t.Fatalf("err = %v, want %q", err, tc.wantSub)
			}
			if strings.Contains(err.Error(), syntheticSecret) {
				t.Fatalf("the refusal leaked a value: %v", err)
			}
			// Nothing partial is left.
			if entries, readErr := os.ReadDir(filepath.Join(root, "run-neg")); readErr == nil && len(entries) > 0 {
				t.Fatalf("a refused delivery left %d files behind", len(entries))
			}
		})
	}
}

// The secrets root is never the home directory and never AO's data dir.
func TestSecretsRootFor_NeverUsesHome(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skipf("no home directory: %v", err)
	}
	if _, err := SecretsRootFor(filepath.Join(home, "proj"), ""); err == nil {
		t.Fatal("secrets in the home directory were accepted")
	}

	root, err := SecretsRootFor("/Users/someone/code/proj", "")
	if err != nil {
		t.Fatalf("SecretsRootFor: %v", err)
	}
	if root != filepath.Join("/Users/someone/code", secretsDirName) {
		t.Fatalf("root = %q", root)
	}
	// It is a SEPARATE root from input staging, so a bug in one cannot expose
	// the other.
	stagingRoot, err := StagingRootFor("/Users/someone/code/proj", "")
	if err != nil {
		t.Fatalf("StagingRootFor: %v", err)
	}
	if root == stagingRoot {
		t.Fatal("secrets and inputs share a directory")
	}
	if strings.Contains(root, "/.ao/") || strings.HasSuffix(root, "/.ao") {
		t.Fatalf("secrets resolved into AO's data dir: %q", root)
	}
	if _, err := SecretsRootFor("relative", ""); err == nil {
		t.Fatal("a relative project path was accepted")
	}
}

// A container that has MORE secret files than AO delivered is worse than one
// with fewer, and neither may read as success.
func TestVerifyDelivered_RefusesAnyMismatch(t *testing.T) {
	d := SecretDelivery{Refs: []skillsecrets.Ref{"API_TOKEN", "SENTRY_DSN"}}

	if err := d.VerifyDelivered([]string{"SENTRY_DSN", "API_TOKEN"}); err != nil {
		t.Fatalf("an exact match was rejected: %v", err)
	}
	for name, seen := range map[string][]string{
		"mount did not arrive": {},
		"one file missing":     {"API_TOKEN"},
		"an extra file":        {"API_TOKEN", "SENTRY_DSN", "SOMETHING_ELSE"},
		"a different set":      {"API_TOKEN", "OTHER_NAME"},
	} {
		t.Run(name, func(t *testing.T) {
			if err := d.VerifyDelivered(seen); !errors.Is(err, ErrSecretDelivery) {
				t.Fatalf("err = %v, want ErrSecretDelivery", err)
			}
		})
	}
}

// The control is attested only when BOTH halves exist. A container alone
// proves nothing about where a secret would come from, and a caller cannot
// assert the control into existence.
func TestAttestation_SecretControlNeedsTheAuthorityToo(t *testing.T) {
	runtime := Runtime{Binary: "docker", ServerVersion: "29.2.1", CgroupVersion: "2", OSType: "linux"}

	withoutAuthority := (&Runner{runtime: runtime, runner: fakeCLI{}}).Attestation()
	if withoutAuthority.Provides(SecretControl) {
		t.Fatal("a runtime with no secret authority attested scoped_secret_delivery")
	}

	withAuthority := (&Runner{runtime: runtime, runner: fakeCLI{}}).
		WithSecretDelivery(true).Attestation()
	if !withAuthority.Provides(SecretControl) {
		t.Fatal("a wired authority did not attest scoped_secret_delivery")
	}
	// Confinement is still required alongside it; the control does not stand
	// in for the rest.
	if !withAuthority.Isolated() {
		t.Fatal("the attestation lost its confinement controls")
	}

	// And an UNUSABLE runtime attests nothing at all, whatever the authority
	// says — there is nowhere to deliver into.
	unusable := (&Runner{probeErr: errors.New("no daemon"), runner: fakeCLI{}}).
		WithSecretDelivery(true).Attestation()
	if len(unusable.Controls) != 0 {
		t.Fatalf("an unusable runtime attested %v", unusable.Controls)
	}
}
