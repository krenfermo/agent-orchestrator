package skillrunner

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/skillsecrets"
)

// These drive a REAL container. Every value is synthetic; nothing here reads a
// credential belonging to anyone.

func liveDelivery(t *testing.T, values map[skillsecrets.Ref]skillsecrets.SecretValue) SecretDelivery {
	t.Helper()
	root := filepath.Join(repoScratchRoot(t), secretsDirName)
	d, err := DeliverSecrets(root, "run-"+randomToken(), values, "lease-live")
	if err != nil {
		t.Fatalf("DeliverSecrets: %v", err)
	}
	t.Cleanup(func() { _ = d.Cleanup() })
	return d
}

func runWithSecrets(t *testing.T, r *Runner, d *SecretDelivery, script string) Result {
	t.Helper()
	inputDir, _ := workspace(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	res, err := r.Run(ctx, Request{
		Image: pinnedImage(t, r),
		// Every workload emits the shared evidence line, so the runner can
		// verify what the container actually saw.
		Argv:     []string{"sh", "-c", SecretEvidenceScript + "\n" + script},
		InputDir: inputDir, Secrets: d, Limits: DefaultLimits(),
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	return res
}

// The delivery arrives, is readable, and is the ONLY thing mounted beyond the
// inputs. The home directory, the Keychain, credential files and AO's data dir
// are all absent.
func TestLiveSecrets_DeliversOnlyWhatWasAuthorized(t *testing.T) {
	r := liveRunner(t)
	requireAlpine(t, r)
	d := liveDelivery(t, map[skillsecrets.Ref]skillsecrets.SecretValue{
		"SENTRY_DSN": skillsecrets.NewSecretValue(syntheticSecret),
	})

	res := runWithSecrets(t, r, &d, `
ls -1 /run/secrets
echo "---"
cat /run/secrets/SENTRY_DSN
echo "---not mounted---"
ls /root 2>&1 | head -1
ls ~/Library/Keychains 2>&1 | head -1
ls /Users 2>&1 | head -1
ls /host 2>&1 | head -1
`)
	if res.ExitCode != 0 {
		t.Fatalf("exit %d: %s", res.ExitCode, res.Stderr)
	}
	if !strings.Contains(res.Stdout, "SENTRY_DSN") {
		t.Fatalf("the delivery did not arrive:\n%s", res.Stdout)
	}
	if !strings.Contains(res.Stdout, syntheticSecret) {
		t.Fatalf("the value was not readable inside the container:\n%s", res.Stdout)
	}
	// Nothing else is reachable. Each probe must have FAILED — asserting on
	// the denial rather than on the absence of a word, because `ls` echoes the
	// path it could not open and a substring check would match its own error.
	after := res.Stdout[strings.Index(res.Stdout, "---not mounted---"):]
	for _, probe := range []string{"/root", "Keychains", "/Users", "/host"} {
		line := ""
		for _, l := range strings.Split(after, "\n") {
			if strings.Contains(l, probe) {
				line = l
				break
			}
		}
		if line == "" {
			t.Fatalf("no result for probe %q:\n%s", probe, after)
		}
		if !strings.Contains(line, "No such file") && !strings.Contains(line, "Permission denied") {
			t.Fatalf("the container reached %q: %s", probe, line)
		}
	}
	// AO's own evidence names the reference and never the value.
	if len(res.Evidence.SecretRefsDelivered) != 1 ||
		res.Evidence.SecretRefsDelivered[0] != "SENTRY_DSN" {
		t.Fatalf("evidence refs = %v", res.Evidence.SecretRefsDelivered)
	}
	if err := d.VerifyDelivered(res.Evidence.SecretRefsDelivered); err != nil {
		t.Fatalf("VerifyDelivered: %v", err)
	}
}

// The mount is read-only: a run cannot rewrite its own secret, and cannot
// plant a file for a later run to read.
func TestLiveSecrets_MountIsReadOnly(t *testing.T) {
	r := liveRunner(t)
	requireAlpine(t, r)
	d := liveDelivery(t, map[skillsecrets.Ref]skillsecrets.SecretValue{
		"SENTRY_DSN": skillsecrets.NewSecretValue(syntheticSecret),
	})

	res := runWithSecrets(t, r, &d, `
echo tampered > /run/secrets/SENTRY_DSN 2>&1 || echo "OVERWRITE DENIED"
echo planted > /run/secrets/PLANTED 2>&1 || echo "CREATE DENIED"
rm -f /run/secrets/SENTRY_DSN 2>&1 || echo "DELETE DENIED"
`)
	for _, want := range []string{"OVERWRITE DENIED", "CREATE DENIED", "DELETE DENIED"} {
		if !strings.Contains(res.Stdout, want) {
			t.Fatalf("expected %q:\n%s\n%s", want, res.Stdout, res.Stderr)
		}
	}
	// The host copy is untouched.
	body, err := os.ReadFile(filepath.Join(d.Dir, "SENTRY_DSN"))
	if err != nil {
		t.Fatalf("read host copy: %v", err)
	}
	if string(body) != syntheticSecret {
		t.Fatal("the container rewrote the host's copy")
	}
}

// A symlink inside the delivery must not become a way to read the host. AO
// only ever writes regular files, and the mount is read-only, so a run cannot
// create one either — this asserts both halves.
func TestLiveSecrets_NoSymlinkEscapeFromTheDelivery(t *testing.T) {
	r := liveRunner(t)
	requireAlpine(t, r)
	d := liveDelivery(t, map[skillsecrets.Ref]skillsecrets.SecretValue{
		"SENTRY_DSN": skillsecrets.NewSecretValue(syntheticSecret),
	})

	// Everything AO wrote is a regular file.
	entries, err := os.ReadDir(d.Dir)
	if err != nil {
		t.Fatalf("read delivery: %v", err)
	}
	for _, e := range entries {
		info, infoErr := e.Info()
		if infoErr != nil {
			t.Fatalf("stat %s: %v", e.Name(), infoErr)
		}
		if !info.Mode().IsRegular() {
			t.Fatalf("%s is not a regular file: %v", e.Name(), info.Mode())
		}
	}

	res := runWithSecrets(t, r, &d, `
ln -s /etc/passwd /run/secrets/ESCAPE 2>&1 || echo "SYMLINK DENIED"
cat /run/secrets/../../etc/hostname 2>&1 | head -1
`)
	if !strings.Contains(res.Stdout, "SYMLINK DENIED") {
		t.Fatalf("a symlink was created inside the delivery:\n%s\n%s", res.Stdout, res.Stderr)
	}
	if strings.Contains(res.Stdout, syntheticSecret) {
		t.Fatalf("a traversal read the secret back:\n%s", res.Stdout)
	}
}

// The value must never be an environment variable — the rule agentcred set for
// reasons that have not changed.
func TestLiveSecrets_NeverReachTheEnvironment(t *testing.T) {
	r := liveRunner(t)
	requireAlpine(t, r)
	t.Setenv("AO_TEST_DAEMON_SECRET", syntheticSecret)
	d := liveDelivery(t, map[skillsecrets.Ref]skillsecrets.SecretValue{
		"SENTRY_DSN": skillsecrets.NewSecretValue(syntheticSecret),
	})

	res := runWithSecrets(t, r, &d, `env; echo "---"; cat /proc/self/environ | tr '\0' '\n'`)
	if strings.Contains(res.Stdout, syntheticSecret) {
		t.Fatalf("the value appeared in the environment:\n%s", res.Stdout)
	}
	if strings.Contains(res.Stdout, "AO_TEST_DAEMON_SECRET") {
		t.Fatalf("the daemon's environment leaked in:\n%s", res.Stdout)
	}
	if res.Evidence.InheritedDaemonEnv != 0 {
		t.Fatalf("%d credential-shaped variables leaked", res.Evidence.InheritedDaemonEnv)
	}
}

// A run with no delivery sees no secrets directory at all — the mount is not
// created "empty just in case".
func TestLiveSecrets_AbsentWhenNothingWasAuthorized(t *testing.T) {
	r := liveRunner(t)
	requireAlpine(t, r)
	inputDir, _ := workspace(t)

	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	res, err := r.Run(ctx, Request{
		Image: pinnedImage(t, r), Argv: []string{"sh", "-c", "ls -la /run/secrets 2>&1 | head -2"},
		InputDir: inputDir, Limits: DefaultLimits(),
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !strings.Contains(res.Stdout, "No such file") && !strings.Contains(res.Stderr, "No such file") {
		t.Fatalf("a run with no authorization got a secrets mount:\n%s\n%s", res.Stdout, res.Stderr)
	}
	if len(res.Evidence.SecretRefsDelivered) != 0 {
		t.Fatalf("evidence claims delivery: %v", res.Evidence.SecretRefsDelivered)
	}
}

// Cleanup runs on the timeout path too, which is the one somebody forgets.
func TestLiveSecrets_CleanedUpAfterATimeout(t *testing.T) {
	r := liveRunner(t)
	requireAlpine(t, r)
	root := filepath.Join(repoScratchRoot(t), secretsDirName)
	d, err := DeliverSecrets(root, "run-"+randomToken(), map[skillsecrets.Ref]skillsecrets.SecretValue{
		"SENTRY_DSN": skillsecrets.NewSecretValue(syntheticSecret),
	}, "lease-timeout")
	if err != nil {
		t.Fatalf("DeliverSecrets: %v", err)
	}
	inputDir, _ := workspace(t)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	res, err := r.Run(ctx, Request{
		Image: pinnedImage(t, r), Argv: []string{"sh", "-c", "sleep 600"},
		InputDir: inputDir, Secrets: &d,
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

	// The caller is responsible for cleanup on every exit path; this is that
	// call, and it must leave nothing.
	if err := d.Cleanup(); err != nil {
		t.Fatalf("Cleanup after timeout: %v", err)
	}
	if _, err := os.Stat(d.Dir); !os.IsNotExist(err) {
		t.Fatalf("the delivery survived a timeout: %v", err)
	}
	if leftover := listSkillRunContainers(t, r); leftover != "" {
		t.Fatalf("containers survived: %s", leftover)
	}
}

// A delivery whose mount does not arrive must fail the run, not proceed with a
// skill that will read nothing and behave in ways nobody designed.
func TestLiveSecrets_MountFailureIsDetected(t *testing.T) {
	r := liveRunner(t)
	requireAlpine(t, r)
	d := liveDelivery(t, map[skillsecrets.Ref]skillsecrets.SecretValue{
		"SENTRY_DSN": skillsecrets.NewSecretValue(syntheticSecret),
		"API_TOKEN":  skillsecrets.NewSecretValue(syntheticSecret),
	})

	// Simulate the mount delivering only part of the set by checking the
	// verification directly against what a partial mount would report.
	res := runWithSecrets(t, r, &d, `true`)
	if err := d.VerifyDelivered(res.Evidence.SecretRefsDelivered); err != nil {
		t.Fatalf("a complete delivery failed verification: %v", err)
	}
	if err := d.VerifyDelivered([]string{"SENTRY_DSN"}); err == nil {
		t.Fatal("a partial delivery passed verification")
	}
	if err := d.VerifyDelivered(nil); err == nil {
		t.Fatal("an absent mount passed verification")
	}
}
