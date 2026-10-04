package tmux

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

func TestRuntimeIntegration(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux unavailable")
	}

	ctx := context.Background()
	id := strings.ReplaceAll(t.Name(), "/", "_")
	r := New(Options{Timeout: 5 * time.Second})

	// Ensure clean slate: ignore errors (session may not exist).
	_ = r.Destroy(ctx, ports.RuntimeHandle{ID: id})

	t.Cleanup(func() {
		// Always destroy so a test failure never leaks a tmux session.
		_ = r.Destroy(context.Background(), ports.RuntimeHandle{ID: id})
	})

	h, err := r.Create(ctx, ports.RuntimeConfig{
		SessionID:     domain.SessionID(id),
		WorkspacePath: t.TempDir(),
		// Run a trivial command then drop into an interactive shell (the keep-alive
		// exec is added by buildLaunchCommand, but we also verify here that output
		// appears).
		Argv: []string{"sh", "-c", "echo hello-from-tmux"},
		Env:  map[string]string{"AO_SESSION_ID": id},
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	alive, err := r.IsAlive(ctx, h)
	if err != nil {
		t.Fatalf("IsAlive: %v", err)
	}
	if !alive {
		t.Fatal("alive = false, want true after create")
	}

	// Wait for the echo output to appear (the session may take a moment to
	// write it to the pane history).
	out := waitForOutput(t, r, h, "hello-from-tmux", 5*time.Second)
	if !strings.Contains(out, "hello-from-tmux") {
		t.Fatalf("output = %q, want hello-from-tmux", out)
	}

	// Send a command and verify it echoes back.
	if err := r.SendMessage(ctx, h, "echo hello-send"); err != nil {
		t.Fatalf("SendMessage: %v", err)
	}
	out = waitForOutput(t, r, h, "hello-send", 5*time.Second)
	if !strings.Contains(out, "hello-send") {
		t.Fatalf("output after SendMessage = %q, want hello-send", out)
	}

	// Destroy and verify liveness goes false. When this was the server's last
	// session the server itself exits with it, and the probe reports the
	// server-level outage as an inconclusive ErrRuntimeUnavailable rather than
	// a per-session death (issue #3475); both outcomes mean the handle is gone.
	if err := r.Destroy(ctx, h); err != nil {
		t.Fatalf("Destroy: %v", err)
	}
	alive, err = r.IsAlive(ctx, h)
	if err != nil && !errors.Is(err, ports.ErrRuntimeUnavailable) {
		t.Fatalf("IsAlive after destroy: %v", err)
	}
	if alive {
		t.Fatal("alive after destroy = true, want false")
	}
}

// TestRuntimeIntegrationExactSessionParsing verifies that IsAlive uses exact
// session matching and does not treat a prefix as a live session.
func TestRuntimeIntegrationExactSessionParsing(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux unavailable")
	}

	ctx := context.Background()
	base := strings.ReplaceAll(t.Name(), "/", "_")
	longID := base + "_long"
	prefixID := base

	r := New(Options{Timeout: 5 * time.Second})
	_ = r.Destroy(ctx, ports.RuntimeHandle{ID: longID})
	_ = r.Destroy(ctx, ports.RuntimeHandle{ID: prefixID})

	t.Cleanup(func() {
		_ = r.Destroy(context.Background(), ports.RuntimeHandle{ID: longID})
		_ = r.Destroy(context.Background(), ports.RuntimeHandle{ID: prefixID})
	})

	h, err := r.Create(ctx, ports.RuntimeConfig{
		SessionID:     domain.SessionID(longID),
		WorkspacePath: t.TempDir(),
		Argv:          []string{"sh", "-c", "echo ready"},
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	// tmux has-session -t <prefix> should NOT match <longID> because tmux
	// requires the exact session name when using -t with a plain string (not a
	// glob). Verify by probing the prefix handle directly.
	prefixAlive, err := r.IsAlive(ctx, ports.RuntimeHandle{ID: prefixID})
	if err != nil {
		// tmux may return an error (session not found) rather than exit 0.
		// That is acceptable here: the point is the prefix must not be alive.
		t.Logf("IsAlive prefix returned error (acceptable): %v", err)
	}
	if prefixAlive {
		_ = r.Destroy(ctx, h)
		t.Fatal("prefix handle reported alive; tmux session matching is not exact")
	}
}

func TestRuntimeIntegrationSupervisedExitKeepsInteractiveShell(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux unavailable")
	}

	ctx := context.Background()
	id := strings.ReplaceAll(t.Name(), "/", "_")
	const launchID = "launch-1"
	r := New(Options{Timeout: 5 * time.Second})
	tmuxID := SessionName(id)
	workspace := t.TempDir()
	_ = r.Destroy(ctx, ports.RuntimeHandle{ID: tmuxID})
	t.Cleanup(func() { _ = r.Destroy(context.Background(), ports.RuntimeHandle{ID: tmuxID}) })

	// Re-run this test binary as a long-lived helper with the same controlled
	// command-line identity as AO's supervisor. The CLI package separately tests
	// that the real supervisor waits for and reports its child.
	h, err := r.Create(ctx, ports.RuntimeConfig{
		SessionID:     domain.SessionID(id),
		WorkspacePath: workspace,
		Argv:          []string{os.Args[0], "-test.run=TestSupervisorProcessHelper", "--", "agent-process", "supervise", "--session", id, "--launch", launchID, "--"},
		Env:           map[string]string{"AO_TMUX_SUPERVISOR_HELPER": "1"},
	})
	if err != nil {
		t.Fatal(err)
	}
	ref := ports.SupervisedProcessRef{SessionID: domain.SessionID(id), LaunchID: launchID}
	deadline := time.Now().Add(5 * time.Second)
	for {
		alive, probeErr := r.IsSupervisedProcessAlive(ctx, h, ref)
		if probeErr != nil {
			t.Fatal(probeErr)
		}
		if alive {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("supervised workload did not appear in the tmux process tree")
		}
		time.Sleep(100 * time.Millisecond)
	}

	// The helper exits normally, matching Codex /exit or EOF. The launch shell
	// must then execute AO's keep-alive interactive shell.
	deadline = time.Now().Add(5 * time.Second)
	for {
		alive, probeErr := r.IsSupervisedProcessAlive(ctx, h, ref)
		if probeErr != nil {
			t.Fatal(probeErr)
		}
		if !alive {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("supervised workload remained alive after normal exit")
		}
		time.Sleep(100 * time.Millisecond)
	}
	if alive, err := r.IsAlive(ctx, h); err != nil || !alive {
		t.Fatalf("tmux after workload exit = (%v, %v), want (true, nil)", alive, err)
	}
	if err := r.SendMessage(ctx, h, "echo shell-after-agent-exit"); err != nil {
		t.Fatal(err)
	}
	out := waitForOutput(t, r, h, "shell-after-agent-exit", 5*time.Second)
	if !strings.Contains(out, "shell-after-agent-exit") {
		t.Fatalf("post-exit shell output = %q", out)
	}

	restarted, err := r.Restart(ctx, h, ports.RuntimeConfig{
		SessionID:     domain.SessionID(id),
		WorkspacePath: workspace,
		Argv:          []string{"sh", "-c", "echo managed-agent-resumed"},
	})
	if err != nil {
		t.Fatalf("Restart: %v", err)
	}
	if restarted != h {
		t.Fatalf("restart handle = %+v, want existing handle %+v", restarted, h)
	}
	out = waitForOutput(t, r, restarted, "managed-agent-resumed", 5*time.Second)
	if !strings.Contains(out, "managed-agent-resumed") {
		t.Fatalf("restart output = %q, want managed-agent-resumed", out)
	}
	if err := r.SendMessage(ctx, restarted, "echo shell-after-managed-resume"); err != nil {
		t.Fatal(err)
	}
	out = waitForOutput(t, r, restarted, "shell-after-managed-resume", 5*time.Second)
	if !strings.Contains(out, "shell-after-managed-resume") {
		t.Fatalf("post-resume shell output = %q", out)
	}
}

// TestRuntimeIntegrationServerIsolation is the E2E proof for AO's tmux server
// isolation (checkpoint 8I.1): AO's tmux runtime must never share, list, or
// be able to kill sessions on the operator's own default tmux server, and
// vice versa. It:
//  1. creates a "user-control" session directly on the plain default tmux
//     server (no -L), simulating a session that predates and is unrelated to
//     AO — e.g. the decades-old shared server with dozens of unrelated
//     sessions that the original incident hit;
//  2. creates an AO session through the Runtime, pinned to its own isolated
//     socket via Options.Socket;
//  3. asserts the AO session is invisible to a plain `tmux ls` (default
//     server) and the user-control session is invisible to `tmux -L <ao
//     socket> ls`;
//  4. destroys the AO session and asserts the default server's user-control
//     session is still alive — an AO teardown must never reach the default
//     server.
func TestRuntimeIntegrationServerIsolation(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux unavailable")
	}

	base := strings.ReplaceAll(t.Name(), "/", "_")
	controlSession := base + "_user_control"
	aoSocket := "ao-test-" + base
	aoID := base + "_ao_session"

	// Clean slate on both servers before asserting anything.
	_ = exec.Command("tmux", "kill-session", "-t", controlSession).Run()
	_ = exec.Command("tmux", "-L", aoSocket, "kill-server").Run()
	t.Cleanup(func() {
		_ = exec.Command("tmux", "kill-session", "-t", controlSession).Run()
		_ = exec.Command("tmux", "-L", aoSocket, "kill-server").Run()
	})

	// 1. A session on the operator's own default tmux server, wholly unrelated
	// to AO. This is the server AO must never touch.
	if out, err := exec.Command("tmux", "new-session", "-d", "-s", controlSession).CombinedOutput(); err != nil {
		t.Fatalf("seed default-server control session: %v: %s", err, out)
	}

	// 2. AO creates its own session, pinned to an isolated socket distinct from
	// the default server.
	ctx := context.Background()
	r := New(Options{Timeout: 5 * time.Second, Socket: aoSocket})
	h, err := r.Create(ctx, ports.RuntimeConfig{
		SessionID:     domain.SessionID(aoID),
		WorkspacePath: t.TempDir(),
		Argv:          []string{"sh", "-c", "echo isolation-ready"},
	})
	if err != nil {
		t.Fatalf("Create on isolated AO socket: %v", err)
	}

	// 3a. The AO session must be invisible on the default server.
	if err := exec.Command("tmux", "has-session", "-t", SessionName(aoID)).Run(); err == nil {
		t.Fatal("AO session is visible on the default tmux server; isolation broken")
	}
	defaultLs, _ := exec.Command("tmux", "ls").CombinedOutput()
	if strings.Contains(string(defaultLs), SessionName(aoID)) {
		t.Fatalf("plain `tmux ls` lists the AO session: %s", defaultLs)
	}
	if !strings.Contains(string(defaultLs), controlSession) {
		t.Fatalf("plain `tmux ls` lost the pre-existing default-server session: %s", defaultLs)
	}

	// 3b. The default server's control session must be invisible on AO's socket.
	if err := exec.Command("tmux", "-L", aoSocket, "has-session", "-t", controlSession).Run(); err == nil {
		t.Fatal("default-server session is visible on AO's isolated socket; isolation broken")
	}
	aoLs, err := exec.Command("tmux", "-L", aoSocket, "ls").CombinedOutput()
	if err != nil {
		t.Fatalf("list AO socket sessions: %v: %s", err, aoLs)
	}
	if !strings.Contains(string(aoLs), SessionName(aoID)) {
		t.Fatalf("AO socket does not list its own session: %s", aoLs)
	}
	if strings.Contains(string(aoLs), controlSession) {
		t.Fatalf("AO socket lists the default-server control session: %s", aoLs)
	}

	// 4. Destroying the AO session must never touch the default server.
	if err := r.Destroy(ctx, h); err != nil {
		t.Fatalf("Destroy: %v", err)
	}
	if err := exec.Command("tmux", "has-session", "-t", controlSession).Run(); err != nil {
		t.Fatal("default-server control session died after an AO Destroy; isolation broken")
	}
}

// TestRuntimeIntegrationServerIsolationSurvivesRestart proves recovery after
// a daemon restart: a fresh Runtime built with the same Options.Socket (the
// only thing a restarted daemon carries forward — it is re-derived from the
// stable DataDir, see config.resolveTmuxSocket) reconnects to the same tmux
// server and finds the session a prior "boot" created, without needing any
// other persisted state.
func TestRuntimeIntegrationServerIsolationSurvivesRestart(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux unavailable")
	}

	base := strings.ReplaceAll(t.Name(), "/", "_")
	aoSocket := "ao-test-" + base
	aoID := base + "_ao_session"

	_ = exec.Command("tmux", "-L", aoSocket, "kill-server").Run()
	t.Cleanup(func() { _ = exec.Command("tmux", "-L", aoSocket, "kill-server").Run() })

	ctx := context.Background()

	// "Boot 1": create the session.
	rBoot1 := New(Options{Timeout: 5 * time.Second, Socket: aoSocket})
	h, err := rBoot1.Create(ctx, ports.RuntimeConfig{
		SessionID:     domain.SessionID(aoID),
		WorkspacePath: t.TempDir(),
		Argv:          []string{"sh", "-c", "echo pre-restart"},
	})
	if err != nil {
		t.Fatalf("Create (boot 1): %v", err)
	}

	// "Daemon restart": a brand-new Runtime value, as a fresh process boot
	// would build, sharing nothing with rBoot1 except the socket name.
	rBoot2 := New(Options{Timeout: 5 * time.Second, Socket: aoSocket})
	alive, err := rBoot2.IsAlive(ctx, h)
	if err != nil {
		t.Fatalf("IsAlive after restart: %v", err)
	}
	if !alive {
		t.Fatal("session not found by a fresh Runtime on the same socket after simulated restart")
	}

	out := waitForOutput(t, rBoot2, h, "pre-restart", 5*time.Second)
	if !strings.Contains(out, "pre-restart") {
		t.Fatalf("recovered session output = %q, want pre-restart", out)
	}

	if err := rBoot2.Destroy(ctx, h); err != nil {
		t.Fatalf("Destroy after restart: %v", err)
	}
}

func TestSupervisorProcessHelper(t *testing.T) {
	if os.Getenv("AO_TMUX_SUPERVISOR_HELPER") != "1" {
		return
	}
	time.Sleep(2 * time.Second)
}

// waitForOutput polls GetOutput until out contains want or the deadline passes.
func waitForOutput(t *testing.T, r *Runtime, h ports.RuntimeHandle, want string, deadline time.Duration) string {
	t.Helper()
	end := time.Now().Add(deadline)
	var out string
	for time.Now().Before(end) {
		var err error
		out, err = r.GetOutput(context.Background(), h, 50)
		if err != nil {
			t.Fatalf("GetOutput: %v", err)
		}
		if strings.Contains(out, want) {
			return out
		}
		time.Sleep(100 * time.Millisecond)
	}
	return out
}
