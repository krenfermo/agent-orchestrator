package contextrouter

import (
	"os"
	"strings"
)

// FlagEnv is the opt-in switch for role-aware context routing.
//
// Routing changes what an agent is handed, which is the most consequential
// thing AO can change about a dispatch. It is therefore off unless an operator
// explicitly turns it on: with the flag unset, wfrouter.Instrument returns the
// dependencies untouched and every provider adapter keeps receiving exactly
// the full context it received before this package existed.
const FlagEnv = "AO_CONTEXT_ROUTER"

// Enabled reports whether role-aware context routing is switched on. The
// default — an unset, empty, or unrecognised value — is disabled.
func Enabled() bool {
	return truthy(os.Getenv(FlagEnv))
}

// truthy accepts the spellings an operator is likely to reach for and nothing
// else. An unrecognised value reads as off rather than on, because the failure
// mode of "the flag I set did not take effect" is a puzzled operator, and the
// failure mode of the opposite is a silently changed dispatch payload.
func truthy(raw string) bool {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "1", "true", "yes", "on", "enabled":
		return true
	default:
		return false
	}
}
