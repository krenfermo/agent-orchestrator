package workflow

import (
	"fmt"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

// presentation_invariants.go — P3-B §26: the states the projection may never
// produce.
//
// These are not defensive checks against bad input. They are the properties the
// derivation is supposed to guarantee, written down where a test can execute
// them, so a future change that reintroduces "completed and needs you at the
// same time" fails in CI rather than on somebody's board. Nothing here mutates
// anything: it reports, and the caller decides.
//
// Every invariant below corresponds to a projection AO shipped, or nearly
// shipped, at some point:
//
//	completed + requiresHuman               — a finished run still nagging
//	repair active + Repair enabled          — a second repair one click away
//	direct branch + integration pending     — asking to merge work already there
//	terminal + a current stage              — a finished run "reviewing"
//	requiresHuman=false + a mandatory action — a demand nobody has to meet

// CheckPresentationInvariants returns one message per violated invariant, and
// nil for a projection that holds all of them.
func CheckPresentationInvariants(p Presentation) []string {
	var out []string
	fail := func(format string, args ...any) { out = append(out, fmt.Sprintf(format, args...)) }

	// A terminal run is over. Nothing about it can be somebody's turn, and AO
	// cannot be acting on it.
	if p.Stage.Terminal() && p.RequiresHuman {
		fail("stage %q is terminal but requiresHuman is true", p.Stage)
	}
	if p.Stage.Terminal() && p.AutomaticActionActive {
		fail("stage %q is terminal but automaticActionActive is true", p.Stage)
	}

	// A terminal run has no current stage in the progression: whatever it was
	// doing has stopped happening.
	if p.Stage.Terminal() {
		for _, st := range p.Progress {
			if st.State == ProgressCurrent {
				fail("stage %q is terminal but progress marks %q as current", p.Stage, st.Stage)
			}
		}
	}

	// §9/§15: a direct-branch placement has nothing to integrate, ever.
	if p.Placement.Known && p.Placement.Type == domain.PlacementDirectBranch {
		if p.Placement.IntegrationRequired {
			fail("direct-branch placement reports integrationRequired")
		}
		if p.Placement.Integration != IntegrationNotRequired {
			fail("direct-branch placement reports integration %q", p.Placement.Integration)
		}
	}
	// The boolean and the vocabulary are two names for one fact.
	if p.Placement.Known {
		wantRequired := p.Placement.Integration == IntegrationPending ||
			p.Placement.Integration == IntegrationInProgress ||
			p.Placement.Integration == IntegrationFailed
		if wantRequired != p.Placement.IntegrationRequired {
			fail("integration %q disagrees with integrationRequired=%v",
				p.Placement.Integration, p.Placement.IntegrationRequired)
		}
	}

	// §5: while AO is repairing, no second remedy may be pressable.
	if p.AutomaticActionActive && !p.Stage.Terminal() {
		if p.RequiresHuman {
			fail("automaticActionActive is true but requiresHuman is also true")
		}
		for _, a := range p.Actions {
			if a.ID == ActionRepair && a.Enabled {
				fail("Repair is enabled while an automatic action is active")
			}
		}
	}

	// A run that needs nobody must not be offering a mandatory remedy: a
	// recommended action that is a demand contradicts requiresHuman=false.
	if !p.RequiresHuman && mandatoryAction(p.RecommendedAction) {
		fail("requiresHuman is false but %q is recommended", p.RecommendedAction)
	}

	// An action AO recommends must be one it also offers, and offers enabled.
	if p.RecommendedAction != "" {
		found := false
		for _, a := range p.Actions {
			if a.ID != p.RecommendedAction {
				continue
			}
			found = true
			if !a.Enabled {
				fail("recommended action %q is offered disabled (%s)", a.ID, a.DisabledReason)
			}
		}
		if !found {
			fail("recommended action %q is not among the offered actions", p.RecommendedAction)
		}
	}

	// A disabled action must say why. "Greyed out for no stated reason" is the
	// thing §4 exists to prevent.
	for _, a := range p.Actions {
		if !a.Enabled && a.DisabledReason == "" {
			fail("action %q is disabled with no reason", a.ID)
		}
	}

	// SummaryCode is what every surface renders its sentence from. An empty one
	// is a blank card.
	if p.SummaryCode == "" {
		fail("summaryCode is empty")
	}
	return out
}

// mandatoryAction reports whether an action, if recommended, is a demand on a
// person rather than an offer.
//
// Wait and ViewChanges are not demands: the first recommends leaving a queue
// alone, the second is an inspection. Integrate is deliberately absent too — a
// completed isolated run offers it, and offering somebody the last step of
// their own work is not the same as being blocked on them.
func mandatoryAction(id ActionID) bool {
	switch id {
	case ActionCommitAndContinue, ActionAuthenticate, ActionRevalidatePlan, ActionRegeneratePlan:
		return true
	default:
		return false
	}
}
