package telemetrymeta

import "strings"

// NormalizeCommandPath canonicalizes command paths received from current CLIs
// and best-effort legacy loopback callers before cost-control classification.
func NormalizeCommandPath(commandPath string) string {
	return strings.ToLower(strings.Join(strings.Fields(commandPath), " "))
}

// IsRoutineInternalCLICommand reports whether a successful CLI invocation is
// routine desktop/agent plumbing rather than product usage.
func IsRoutineInternalCLICommand(commandPath string) bool {
	normalized := NormalizeCommandPath(commandPath)
	for _, routine := range routineInternalCLICommands {
		if normalized == routine || strings.HasPrefix(normalized, routine+" ") {
			return true
		}
	}
	return false
}

var routineInternalCLICommands = []string{
	"ao status",
	"ao session ls",
	"ao session get",
	"ao session agent-switch ls",
	"ao session handoff",
	"ao project ls",
	"ao project get",
	"ao orchestrator ls",
	"ao hooks",
	"ao pty-host",
}

// CLIActorType infers the actor for legacy loopback CLI telemetry requests that
// predate the explicit actor_type field. Unknown actor-less commands are treated
// as system activity so foreign/local automation cannot inflate DAU by default.
func CLIActorType(actorType, commandPath string) string {
	normalized := NormalizeCommandPath(commandPath)
	if _, ok := legacyActorlessSystemCLICommands[normalized]; ok {
		return "system"
	}

	switch actorType {
	case "agent", "user":
		return actorType
	case "system":
		return "system"
	}

	if _, ok := legacyActorlessUserCLICommands[normalized]; ok {
		return "user"
	}
	switch normalized {
	case "ao session agent-switch", "ao session agent-switch ls", "ao session switch-agent":
		return "user"
	}
	if normalized == "ao hooks" {
		return "agent"
	}
	return "system"
}

var legacyActorlessSystemCLICommands = map[string]struct{}{
	"ao agent-process":           {},
	"ao agent-process supervise": {},
	"ao completion":              {},
	"ao daemon":                  {},
	"ao server":                  {},
	"ao help":                    {},
	"ao pty-host":                {},
	"ao start":                   {},
}

var legacyActorlessUserCLICommands = map[string]struct{}{
	"ao admin":                  {},
	"ao admin reset-password":   {},
	"ao agent":                  {},
	"ao agent ls":               {},
	"ao browser":                {},
	"ao browser check":          {},
	"ao browser click":          {},
	"ao browser console":        {},
	"ao browser dblclick":       {},
	"ao browser devtools":       {},
	"ao browser devtools close": {},
	"ao browser devtools open":  {},
	"ao browser dialog":         {},
	"ao browser dialog accept":  {},
	"ao browser dialog dismiss": {},
	"ao browser dialog status":  {},
	"ao browser drag":           {},
	"ao browser errors":         {},
	"ao browser fill":           {},
	"ao browser focus":          {},
	"ao browser frame":          {},
	"ao browser get":            {},
	"ao browser highlight":      {},
	"ao browser hover":          {},
	"ao browser network":        {},
	"ao browser network clear":  {},
	"ao browser network list":   {},
	"ao browser network start":  {},
	"ao browser network status": {},
	"ao browser network stop":   {},
	"ao browser open":           {},
	"ao browser press":          {},
	"ao browser screenshot":     {},
	"ao browser scroll":         {},
	"ao browser scrollintoview": {},
	"ao browser select":         {},
	"ao browser snapshot":       {},
	"ao browser tab":            {},
	"ao browser tab close":      {},
	"ao browser tab new":        {},
	"ao browser tab select":     {},
	"ao browser status":         {},
	"ao browser tabs":           {},
	"ao browser type":           {},
	"ao browser uncheck":        {},
	"ao browser unhighlight":    {},
	"ao browser wait":           {},
	"ao decision":               {},
	"ao decision resolve":       {},
	"ao dev":                    {},
	"ao dev import-projects":    {},
	"ao doctor":                 {},
	"ao import":                 {},
	"ao incident":               {},
	"ao incident diagnose":      {},
	"ao incident submit":        {},
	// The P0-B operator recoveries. They are user activity by definition: each
	// one exists precisely because nothing automatic is allowed to take it.
	"ao workflow":                           {},
	"ao workflow recover":                   {},
	"ao workflow recover plan":              {},
	"ao workflow recover review-provenance": {},
	// P1-B's recovery surface. Same argument: every one of these is a person
	// deciding what happens to a stopped run.
	"ao workflow recover status":  {},
	"ao workflow resume":          {},
	"ao workflow repair":          {},
	"ao workflow plan":            {},
	"ao workflow plan reuse":      {},
	"ao workflow plan regenerate": {},
	// P1-C's operator commands. Inspecting capacity and sweeping runtimes are
	// both things a person does deliberately.
	"ao capacity":        {},
	"ao capacity status": {},
	"ao runtime":         {},
	"ao runtime gc":      {},
	// P1-D: the placement surface. Same sweep, same proofs, named for the
	// checkouts an operator is thinking about.
	"ao worktree":      {},
	"ao worktree list": {},
	"ao worktree gc":   {},
	// P1-D's read-only diagnosis commands. A person asking "where is this run's
	// work happening and why has it not started" is user activity: neither
	// command runs on a timer and neither is invoked by AO itself.
	"ao workflow placement": {},
	// P1-E's placement WRITE commands. A person choosing a placement, or
	// authorizing one generation to replace another, is the clearest case of
	// user activity there is: neither runs on a timer and AO never invokes
	// either itself.
	"ao workflow placement override":   {},
	"ao workflow placement transition": {},
	"ao provider":                      {},
	"ao provider attempts":             {},
	"ao launch":                        {},
	"ao orchestrator":                  {},
	"ao orchestrator done":             {},
	"ao pr":                            {},
	"ao pr merge":                      {},
	"ao pr resolve-comments":           {},
	"ao preview":                       {},
	"ao preview clear":                 {},
	"ao preview start":                 {},
	"ao preview status":                {},
	"ao preview stop":                  {},
	"ao project":                       {},
	"ao project add":                   {},
	"ao project rm":                    {},
	"ao project set-config":            {},
	"ao review":                        {},
	"ao review cancel":                 {},
	"ao review ls":                     {},
	"ao review submit":                 {},
	"ao review trigger":                {},
	"ao send":                          {},
	"ao session":                       {},
	"ao session claim-pr":              {},
	"ao session cleanup":               {},
	"ao session kill":                  {},
	"ao session rename":                {},
	"ao session restore":               {},
	"ao spawn":                         {},
	"ao stop":                          {},
	"ao version":                       {},

	// Legacy commands observed in PostHog's current billing-period data.
	"ao handoff":                   {},
	"ao project orchestration get": {},
	"ao project orchestration set": {},
	"ao smoke list":                {},
	"ao smoke set":                 {},
}
