package skillrunner

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/skillcatalog"
)

// fakeCLI answers one canned `info` response, so the probe's refusals can be
// tested without a container runtime present.
type fakeCLI struct {
	out []byte
	err error
}

func (f fakeCLI) Output(context.Context, string, ...string) ([]byte, error) {
	return f.out, f.err
}

// Requirement 7, at the probe layer: every way the runtime can be unusable
// resolves to one refusal, and none of them yields a partially usable runner.
func TestProbe_RefusesEveryUnusableRuntime(t *testing.T) {
	cases := []struct {
		name    string
		cli     fakeCLI
		wantSub string
	}{
		{
			"binary missing or daemon unreachable",
			fakeCLI{err: errors.New("Cannot connect to the Docker daemon")},
			"Cannot connect",
		},
		{
			"unreadable answer",
			fakeCLI{out: []byte("garbage\n")},
			"unreadable answer",
		},
		{
			"no server version means no daemon",
			fakeCLI{out: []byte("\n2\nlinux\nseccomp\n")},
			"no server version",
		},
		{
			"windows containers are not the boundary AO relies on",
			fakeCLI{out: []byte("29.2.1\n2\nwindows\n\n")},
			"Linux namespaces",
		},
		{
			"cgroup v1 cannot prove its own limits",
			fakeCLI{out: []byte("29.2.1\n1\nlinux\nseccomp\n")},
			"cgroup v1 is not supported",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rt, err := probeWith(context.Background(), tc.cli, "docker")
			if !errors.Is(err, ErrRuntimeUnavailable) {
				t.Fatalf("err = %v, want ErrRuntimeUnavailable", err)
			}
			if !strings.Contains(err.Error(), tc.wantSub) {
				t.Fatalf("error should say why: %v", err)
			}
			// A refused probe must attest nothing at all.
			if len(rt.Controls()) != 0 {
				t.Fatalf("an unusable runtime attested %v", rt.Controls())
			}
		})
	}
}

func TestProbe_AcceptsALinuxCgroupV2Runtime(t *testing.T) {
	rt, err := probeWith(context.Background(),
		fakeCLI{out: []byte("29.2.1\n2\nlinux\nname=seccomp,profile=builtin name=apparmor\n")}, "docker")
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	if rt.ServerVersion != "29.2.1" || rt.CgroupVersion != "2" || rt.OSType != "linux" {
		t.Fatalf("runtime = %#v", rt)
	}
	if len(rt.SecurityOptions) != 2 {
		t.Fatalf("security options = %#v", rt.SecurityOptions)
	}
	if !strings.Contains(rt.Describe(), "cgroup v2") {
		t.Fatalf("describe = %q", rt.Describe())
	}
}

// The controls this runtime claims must be exactly the ones the tests in this
// package demonstrate — and must NOT include the four that would unblock a
// capability nothing here implements. This is the tripwire for a future change
// that quietly adds one to the list without building it.
func TestRuntime_ClaimsOnlyDemonstratedControls(t *testing.T) {
	rt := Runtime{Binary: "docker", ServerVersion: "29.2.1", CgroupVersion: "2", OSType: "linux"}
	got := map[skillcatalog.Control]bool{}
	for _, c := range rt.Controls() {
		got[c] = true
	}
	for _, want := range []skillcatalog.Control{
		skillcatalog.ControlFilesystemIsolation,
		skillcatalog.ControlProcessIsolation,
		skillcatalog.ControlNoCredentialInheritance,
		skillcatalog.ControlResourceLimits,
		skillcatalog.ControlEgressDenyAll,
	} {
		if !got[want] {
			t.Fatalf("runtime does not claim %s", want)
		}
	}
	for _, forbidden := range []skillcatalog.Control{
		skillcatalog.ControlEgressAllowlist,
		skillcatalog.ControlWritableWorkspace,
		skillcatalog.ControlScopedSecretDelivery,
		skillcatalog.ControlArbitraryProcessExecution,
	} {
		if got[forbidden] {
			t.Fatalf("the runtime claims %s, which nothing in this package implements", forbidden)
		}
	}
}

// A Runner over an unusable runtime must attest nothing and refuse to run —
// never fall back to the host. This is requirement 7 at the Runner layer, and
// it runs on every machine, with or without a container runtime.
func TestRunner_UnavailableRuntimeRefusesWithoutExecuting(t *testing.T) {
	r := &Runner{probeErr: errors.New("Cannot connect to the Docker daemon"), runner: fakeCLI{}}

	if r.Available() {
		t.Fatal("a runner with a failed probe reported itself available")
	}
	att := r.Attestation()
	if len(att.Controls) != 0 || att.Isolated() || att.EgressControlled() {
		t.Fatalf("an unavailable runner attested %#v", att)
	}
	if att.RunnerID != "none" {
		t.Fatalf("runner id = %q, want none", att.RunnerID)
	}

	_, err := r.Run(context.Background(), Request{
		Image:    "alpine@sha256:" + strings.Repeat("a", 64),
		Argv:     []string{"echo", "hi"},
		InputDir: t.TempDir(),
	})
	if !errors.Is(err, ErrRuntimeUnavailable) {
		t.Fatalf("err = %v, want ErrRuntimeUnavailable", err)
	}
	if !strings.Contains(err.Error(), "Cannot connect") {
		t.Fatalf("the refusal should carry the probe's reason: %v", err)
	}
}

// Execute stays unimplemented: proving the boundary is not the same as having
// a contract for what a skill may run inside it.
func TestRunner_ExecuteIsStillRefused(t *testing.T) {
	r := &Runner{runtime: Runtime{Binary: "docker"}, runner: fakeCLI{}}
	_, err := r.Execute(context.Background(), skillcatalog.Plan{})
	if !errors.Is(err, skillcatalog.ErrNoRunner) {
		t.Fatalf("err = %v, want ErrNoRunner", err)
	}
	if !strings.Contains(err.Error(), "skill-image contract") {
		t.Fatalf("the refusal should name what is missing: %v", err)
	}
}
