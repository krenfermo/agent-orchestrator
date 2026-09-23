package daemon

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/adapters/agent/claudecode"
	"github.com/aoagents/agent-orchestrator/backend/internal/config"
	"github.com/aoagents/agent-orchestrator/backend/internal/skillagent"
)

// newSkillAgentExecutor builds the host agent executor for skill AGENT modes
// (Frente 2 / 2C, ADR 0010).
//
// It follows the planner's wiring on purpose: the same provider CLI, found
// through the same well-known-location resolver every Claude adapter uses, the
// same pinned credential contract (cfg.ProviderAuthMode), and environment
// overrides named after the planner's. It is not a second agent system -- it is
// one more headless, schema-bound invocation of the provider AO already runs.
//
// It is probed once, at boot, with a bounded context. A probe failure is not
// fatal: the executor exists, attests nothing, and every agent mode is refused
// with the reason -- the shape a fail-closed component needs.
//
// Copies are staged under <dataDir>/skill-agent/.ao-skill-staging: AO-owned
// state under AO's data dir. Unlike the container path there is no VM that
// must see the path, and nothing but the agent (confined to its own run's
// directory) ever reads it.
func newSkillAgentExecutor(cfg config.Config, log *slog.Logger) *skillagent.Executor {
	c := skillagent.Config{
		Binary:          os.Getenv("AO_SKILL_AGENT_BIN"),
		Model:           os.Getenv("AO_SKILL_AGENT_MODEL"),
		StagingRoot:     filepath.Join(cfg.DataDir, "skill-agent", ".ao-skill-staging"),
		AuthMode:        cfg.ProviderAuthMode,
		ResolveFallback: claudecode.ResolveClaudeBinary,
	}
	if c.Binary != "" && !strings.Contains(strings.ToLower(filepath.Base(c.Binary)), "claude") {
		// The confinement is Claude Code's flags; another CLI would be handed
		// flags it does not have. The probe would refuse it anyway, but the
		// reason here is the true one.
		log.Warn("skills: AO_SKILL_AGENT_BIN is not a Claude Code binary; agent modes stay refused",
			"bin", c.Binary)
	}
	if v := os.Getenv("AO_SKILL_AGENT_TIMEOUT"); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d > 0 {
			c.Timeout = d
		}
	}
	if v := os.Getenv("AO_SKILL_AGENT_MAX_BUDGET_USD"); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil && f > 0 {
			c.MaxBudgetUSD = f
		}
	}
	if err := os.MkdirAll(c.StagingRoot, 0o700); err != nil {
		log.Warn("skills: could not create the agent staging root", "root", c.StagingRoot, "err", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()
	e := skillagent.New(ctx, c)
	if u := e.Unavailable(); u != "" {
		log.Warn("skills: host agent executor unavailable; agent modes are refused", "reason", u)
	} else {
		log.Info("skills: host agent executor ready", "runner", skillagent.RunnerID, "model", e.Model())
	}
	return e
}
