package contextrouter

import (
	"log/slog"

	"github.com/aoagents/agent-orchestrator/backend/internal/codegraph"
	memory "github.com/aoagents/agent-orchestrator/backend/internal/projectmemory"
)

// Default builds the router AO itself uses: budgets from the environment, the
// git diff of the checkout, the native code graph index, and the durable
// project memory store, all rooted under AO's data dir.
//
// It exists so the daemon's composition root and the disabled-vs-enabled
// regression harness assemble the SAME router. A harness that measured a
// hand-built router would be measuring a configuration nothing ships.
//
// Every evidence source is optional and failing to build one costs only that
// source: a router with a diff but no code graph still routes and says so in
// its selections' notes. A rejected budget override, by contrast, is an error,
// because a router that silently fell back to default budgets would make an
// operator's override look applied when it was not.
func Default(log *slog.Logger) (*Router, error) {
	budgets, err := BudgetsFromEnv()
	if err != nil {
		return nil, err
	}
	opts := Options{
		Budgets: budgets,
		Diff:    NewGitDiffSource(),
		Log:     log,
	}
	if store, storeErr := codegraph.NewDefaultStore(); storeErr != nil {
		warn(log, "context router: graph evidence unavailable", storeErr)
	} else if indexer, indexErr := codegraph.NewNativeIndexer(store); indexErr != nil {
		warn(log, "context router: graph evidence unavailable", indexErr)
	} else {
		opts.Graph = indexer
	}
	if store, memErr := memory.NewDefaultStore(); memErr != nil {
		warn(log, "context router: memory evidence unavailable", memErr)
	} else {
		opts.Memory = store
	}
	return New(opts)
}

func warn(log *slog.Logger, msg string, err error) {
	if log != nil {
		log.Warn(msg, "err", err)
	}
}
