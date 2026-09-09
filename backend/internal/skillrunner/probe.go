package skillrunner

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/skillcatalog"
)

// ErrRuntimeUnavailable is returned whenever the container runtime cannot be
// used. It is the fail-closed path: a caller that sees it must refuse the run,
// never fall back to the host.
var ErrRuntimeUnavailable = errors.New("skillrunner: no usable container runtime")

// probeTimeout bounds every probe call. A wedged docker daemon must make the
// runner report unavailable, not hang the caller — the same reason
// dockerreap.reapTimeout exists.
const probeTimeout = 15 * time.Second

// Runtime is the container CLI this package drives. Only the docker CLI is
// implemented; podman and nerdctl are compatible enough that adding them is a
// constant, not a redesign, but neither is claimed until it is probed.
type Runtime struct {
	// Binary is the CLI name, resolved on PATH.
	Binary string
	// ServerVersion is the daemon's reported version.
	ServerVersion string
	// CgroupVersion is "1" or "2". Only 2 is accepted: cgroup v1 does not give
	// the memory and pid accounting the resource-limit control depends on.
	CgroupVersion string
	// OSType is the daemon's kernel family. It must be linux — on macOS that
	// means a VM is running, which is exactly what makes Linux namespaces
	// available on a Mac at all.
	OSType string
	// SecurityOptions are what the daemon reports (seccomp, apparmor, ...).
	SecurityOptions []string
}

// commandRunner abstracts exec for tests, matching the pattern
// internal/adapters/container/dockerreap already establishes.
type commandRunner interface {
	Output(ctx context.Context, name string, args ...string) ([]byte, error)
}

type execRunner struct{}

func (execRunner) Output(ctx context.Context, name string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return stdout.Bytes(), fmt.Errorf("%s %s: %w: %s",
			name, strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return stdout.Bytes(), nil
}

// Probe asks the runtime what it is and what it supports.
//
// It returns ErrRuntimeUnavailable for every failure mode — binary missing,
// daemon unreachable, wrong kernel family, cgroup v1 — because from the
// caller's side those are one condition: this host cannot contain a skill
// right now.
func Probe(ctx context.Context) (Runtime, error) {
	return probeWith(ctx, execRunner{}, "docker")
}

func probeWith(ctx context.Context, runner commandRunner, binary string) (Runtime, error) {
	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()

	// One call: an unreachable daemon fails here rather than three times.
	out, err := runner.Output(ctx, binary, "info", "--format",
		"{{.ServerVersion}}\n{{.CgroupVersion}}\n{{.OSType}}\n{{range .SecurityOptions}}{{.}} {{end}}")
	if err != nil {
		return Runtime{}, fmt.Errorf("%w: %w", ErrRuntimeUnavailable, err)
	}
	lines := strings.Split(strings.TrimRight(string(out), "\n"), "\n")
	if len(lines) < 3 {
		return Runtime{}, fmt.Errorf("%w: %s info returned an unreadable answer", ErrRuntimeUnavailable, binary)
	}
	rt := Runtime{
		Binary:        binary,
		ServerVersion: strings.TrimSpace(lines[0]),
		CgroupVersion: strings.TrimSpace(lines[1]),
		OSType:        strings.TrimSpace(lines[2]),
	}
	if len(lines) > 3 {
		rt.SecurityOptions = strings.Fields(lines[3])
	}
	if rt.ServerVersion == "" {
		return Runtime{}, fmt.Errorf("%w: %s reported no server version, so no daemon is reachable",
			ErrRuntimeUnavailable, binary)
	}
	if rt.OSType != "linux" {
		return Runtime{}, fmt.Errorf("%w: %s runs %q containers; the boundary AO relies on is Linux namespaces",
			ErrRuntimeUnavailable, binary, rt.OSType)
	}
	// cgroup v1 splits memory and pid accounting across hierarchies in ways
	// that make "the limit was enforced" hard to prove from inside. AO refuses
	// rather than attest a resource limit it cannot read back.
	if rt.CgroupVersion != "2" {
		return Runtime{}, fmt.Errorf("%w: cgroup v%s is not supported; AO needs v2 to read back the limits it set",
			ErrRuntimeUnavailable, rt.CgroupVersion)
	}
	return rt, nil
}

// Controls are the guarantees this runtime supports.
//
// Every entry here is demonstrated by a test in this package (see
// runner_isolation_test.go), which is the rule ADR 0004 sets: a control may be
// attested only when a test proves it.
//
// What is deliberately ABSENT is the point of the list:
//
//   - egress_allowlist — deny-all is implemented; limiting traffic to declared
//     destinations needs the forward proxy, which is designed and not built.
//   - writable_workspace — every mount is read-only and there is no reviewed
//     path to return changes to the host.
//   - scoped_secret_delivery — the container receives no secret by any route.
//   - arbitrary_process_execution — the runner runs a fixed command in a
//     pinned image; a skill cannot yet author what runs.
//
// So this runtime unblocks nothing today. repo.write, process.exec,
// secrets.read, net.egress and net.active_scan all still name a control that
// is not in this list.
func (r Runtime) Controls() []skillcatalog.Control {
	if r.Binary == "" {
		return nil
	}
	return []skillcatalog.Control{
		skillcatalog.ControlFilesystemIsolation,
		skillcatalog.ControlProcessIsolation,
		skillcatalog.ControlNoCredentialInheritance,
		skillcatalog.ControlResourceLimits,
		skillcatalog.ControlEgressDenyAll,
	}
}

// Describe renders the runtime for an operator, in one line.
func (r Runtime) Describe() string {
	if r.Binary == "" {
		return "none"
	}
	return fmt.Sprintf("%s %s (%s, cgroup v%s)", r.Binary, r.ServerVersion, r.OSType, r.CgroupVersion)
}
