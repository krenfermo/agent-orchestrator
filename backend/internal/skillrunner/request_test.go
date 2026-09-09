package skillrunner

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func digestImage() string { return "alpine@sha256:" + strings.Repeat("a", 64) }

func populatedDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "input.txt"), []byte("synthetic\n"), 0o600); err != nil {
		t.Fatalf("seed input: %v", err)
	}
	return dir
}

// Every refusal here happens BEFORE a container starts, which is the point:
// a run that cannot be trusted must never begin.
func TestValidateRequest_RefusesBeforeStartingAnything(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(*Request)
		wantSub string
	}{
		{"tagged image", func(r *Request) { r.Image = "alpine:3.19" }, "pinned by digest"},
		{"no command", func(r *Request) { r.Argv = nil }, "a command is required"},
		{"no input dir", func(r *Request) { r.InputDir = "" }, "input directory is required"},
		{"relative input dir", func(r *Request) { r.InputDir = "relative/path" }, "must be absolute"},
		{"missing input dir", func(r *Request) { r.InputDir = filepath.Join(t.TempDir(), "nope") }, "read input directory"},
		{
			// The finding that motivated this check: on macOS a bind mount
			// from a path the VM does not share arrives EMPTY and silent, so a
			// security audit would report a clean result for a project it
			// never read.
			"empty input dir",
			func(r *Request) { r.InputDir = t.TempDir() },
			"a run over no inputs produces a clean report of nothing",
		},
		{
			"credential-shaped env name",
			func(r *Request) { r.Env = map[string]string{"GITHUB_TOKEN": "x"} },
			"refusing to forward",
		},
		{
			"another credential shape",
			func(r *Request) { r.Env = map[string]string{"SMTP_PASSWORD": "x"} },
			"refusing to forward",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := Request{Image: digestImage(), Argv: []string{"true"}, InputDir: populatedDir(t)}
			tc.mutate(&req)
			err := validateRequest(req)
			if err == nil {
				t.Fatal("request was accepted")
			}
			if !strings.Contains(err.Error(), tc.wantSub) {
				t.Fatalf("err = %v, want %q", err, tc.wantSub)
			}
		})
	}
}

func TestValidateRequest_AcceptsAWellFormedRequest(t *testing.T) {
	req := Request{
		Image:    digestImage(),
		Argv:     []string{"sh", "-c", "true"},
		InputDir: populatedDir(t),
		Env:      map[string]string{"AO_SKILL_MODE": "static-code"},
	}
	if err := validateRequest(req); err != nil {
		t.Fatalf("validateRequest: %v", err)
	}
}

// The flags are the boundary. A future edit that drops one silently weakens
// every control this runner attests, so the argv is asserted directly.
func TestContainerArgs_CarryEveryBoundarySetting(t *testing.T) {
	r := &Runner{runtime: Runtime{Binary: "docker"}}
	limits := Limits{MemoryBytes: 512 << 20, CPUs: 1.5, MaxPIDs: 64, MaxOutputBytes: 1024}
	args := strings.Join(r.containerArgs("ao-skillrun-test", Request{
		Image: digestImage(), Argv: []string{"sh", "-c", "echo hi"}, InputDir: "/host/in",
	}, limits), " ")

	for _, want := range []string{
		"--network none",
		"--user " + nobodyUser,
		"--read-only",
		"--cap-drop ALL",
		"--security-opt no-new-privileges",
		"--memory 536870912",
		// Equal swap is what makes the memory limit real rather than advisory.
		"--memory-swap 536870912",
		"--cpus 1.5",
		"--pids-limit 64",
		"--tmpfs /tmp:rw,noexec,nosuid,size=64m",
		"-v /host/in:/work:ro",
		"--label " + RunLabel + "=1",
	} {
		if !strings.Contains(args, want) {
			t.Fatalf("container args are missing %q:\n%s", want, args)
		}
	}
	// The label must not collide with the worker-session reaper's.
	if strings.Contains(args, "ao.session") {
		t.Fatalf("a skill run must not carry the worker-session label: %s", args)
	}
}

func TestTruncate_CapsOutput(t *testing.T) {
	got, truncated := truncate("0123456789", 4)
	if got != "0123" || !truncated {
		t.Fatalf("truncate = %q %v", got, truncated)
	}
	if got, truncated := truncate("short", 100); got != "short" || truncated {
		t.Fatalf("truncate = %q %v", got, truncated)
	}
}

// demonstratedControls compares the kernel's numbers against what AO asked
// for, so a flag the runtime silently ignored yields a MISSING control rather
// than a false one.
func TestDemonstratedControls_DerivesFromEvidenceNotFromFlags(t *testing.T) {
	limits := Limits{MemoryBytes: 512 << 20, MaxPIDs: 128}
	full := BoundaryEvidence{
		EffectiveUID: 65534, MemoryMaxBytes: 512 << 20, PIDsMax: 128,
		NetworkReachable: false, InputFilesVisible: 3, ReadOnlyRootFS: true,
		InheritedDaemonEnv: 0,
	}
	if got := demonstratedControls(full, limits); len(got) != 5 {
		t.Fatalf("a fully demonstrated run yielded %v", got)
	}

	cases := []struct {
		name   string
		mutate func(*BoundaryEvidence)
		absent string
	}{
		{"inputs never arrived", func(e *BoundaryEvidence) { e.InputFilesVisible = 0 }, "filesystem_isolation"},
		{"root filesystem was writable", func(e *BoundaryEvidence) { e.ReadOnlyRootFS = false }, "filesystem_isolation"},
		{"ran as root", func(e *BoundaryEvidence) { e.EffectiveUID = 0 }, "process_isolation"},
		{"daemon env leaked", func(e *BoundaryEvidence) { e.InheritedDaemonEnv = 2 }, "no_credential_inheritance"},
		{"memory limit ignored", func(e *BoundaryEvidence) { e.MemoryMaxBytes = 8 << 30 }, "resource_limits"},
		{"network was reachable", func(e *BoundaryEvidence) { e.NetworkReachable = true }, "egress_deny_all"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ev := full
			tc.mutate(&ev)
			for _, got := range demonstratedControls(ev, limits) {
				if string(got) == tc.absent {
					t.Fatalf("%s was still attested after %s", tc.absent, tc.name)
				}
			}
		})
	}
}

func TestBoundaryEvidence_VerifyNamesWhatWasNotDemonstrated(t *testing.T) {
	ev := BoundaryEvidence{Controls: demonstratedControls(BoundaryEvidence{
		EffectiveUID: 65534, MemoryMaxBytes: 1, PIDsMax: 1,
		InputFilesVisible: 1, ReadOnlyRootFS: true,
	}, Limits{MemoryBytes: 1, MaxPIDs: 1})}

	if err := ev.Verify(ev.Controls); err != nil {
		t.Fatalf("a run must verify against what it demonstrated: %v", err)
	}
	err := ev.Verify(append(ev.Controls, "egress_allowlist"))
	if err == nil || !strings.Contains(err.Error(), "egress_allowlist") {
		t.Fatalf("err = %v, want the missing control named", err)
	}
}
