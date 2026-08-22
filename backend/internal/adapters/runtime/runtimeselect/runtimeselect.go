// Package runtimeselect picks the correct runtime backend by platform:
// tmux on Darwin/Linux, conpty (ConPTY) on Windows.
package runtimeselect

import (
	"context"
	"log/slog"
	"runtime"

	"github.com/aoagents/agent-orchestrator/backend/internal/adapters/runtime/conpty"
	"github.com/aoagents/agent-orchestrator/backend/internal/adapters/runtime/tmux"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

// Runtime is the union interface that both tmux and conpty satisfy.
// It extends ports.Runtime (Create/Destroy/IsAlive) with the additional methods
// the daemon wires directly, including ports.Attacher (Attach) so the terminal
// layer can open a Stream against the selected runtime.
type Runtime interface {
	ports.Runtime // Create, Destroy, IsAlive
	ports.Attacher
	Interrupt(ctx context.Context, handle ports.RuntimeHandle) error
	SendInput(ctx context.Context, handle ports.RuntimeHandle, input string) error
	SendMessage(ctx context.Context, handle ports.RuntimeHandle, message string) error
	GetOutput(ctx context.Context, handle ports.RuntimeHandle, lines int) (string, error)
}

// Compile-time assertions: both adapters must implement the union interface.
var _ Runtime = (*tmux.Runtime)(nil)
var _ Runtime = (*conpty.Runtime)(nil)

// New returns the per-platform runtime: tmux on Darwin/Linux, conpty on
// Windows. log is accepted for signature stability with callers but is
// currently unused. tmuxSocket names the isolated tmux server (`tmux -L
// <tmuxSocket>`) every tmux session command runs against — see
// config.Config.TmuxSocket for how it is derived; it is ignored on Windows,
// where conpty has no tmux server to isolate. An empty tmuxSocket still
// isolates AO from the caller's default tmux server: tmux.New falls back to
// its own safe default rather than the unnamed default server.
//
// scratchDir is where the tmux runtime stages transient payload files —
// oversized prompts on their way into a paste buffer, oversized launch
// commands on their way into the pane's shell (Checkpoint 8P-E.13C). Callers
// pass an AO-owned directory (under the data dir) so nothing AO writes lands
// outside it; empty falls back to the OS temp dir. Ignored on Windows.
func New(_ *slog.Logger, tmuxSocket, scratchDir string) Runtime {
	if runtime.GOOS != "windows" {
		return tmux.New(tmux.Options{Socket: tmuxSocket, ScratchDir: scratchDir})
	}
	return conpty.New(conpty.Options{})
}
