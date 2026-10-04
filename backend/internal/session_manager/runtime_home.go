package sessionmanager

import (
	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/runtimehome"
)

// applyUserRuntimeEnv overlays the launch owner's isolated runtime-home
// (HOME, CLAUDE_CONFIG_DIR, CODEX_HOME, ...) onto env, so a provider
// subprocess launched on that user's behalf can never resolve into another
// AO user's -- or the daemon host's own real -- credential locations
// (Checkpoint 8P-B; see internal/runtimehome).
//
// A no-op when owner is empty: today's call sites (worker/reviewer/planner/
// decision-resolver launchers) do not yet resolve and pass an owner through
// every dispatch path -- doing so is explicitly deferred so this checkpoint
// does not also change routing/dispatch scope. Callers that already have a
// resolved domain.UserID (e.g. from workflow_runs.user_id) may call this
// directly; it is intentionally side-effect-free (never os.Setenv) so it is
// safe to call speculatively.
func (m *Manager) applyUserRuntimeEnv(env map[string]string, owner domain.UserID) map[string]string {
	if owner == "" || env == nil {
		return env
	}
	home, err := runtimehome.Prepare(m.dataDir, owner)
	if err != nil {
		if m.logger != nil {
			m.logger.Warn("could not prepare user runtime-home; provider subprocess env left unisolated for this launch",
				"user", owner, "error", err)
		}
		return env
	}
	for k, v := range home.SubprocessEnv() {
		env[k] = v
	}
	return env
}
