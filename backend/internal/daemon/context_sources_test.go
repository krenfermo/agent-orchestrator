package daemon

import (
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/projectmemory"
)

// Frente 3 / 3C, I3: the arm a run records is what its dispatches will get,
// not what an operator asked for. A requested mode that produced no
// provisioner is recorded as off.
func TestEffectiveContextSourcesRecordsWhatDispatchGets(t *testing.T) {
	for _, tc := range []struct {
		name        string
		provisioned bool
		mode        projectmemory.MemoryMode
		routing     bool
		external    bool
		want        domain.ContextSourcesSnapshot
	}{
		{"default daemon", false, projectmemory.ModeOff, false, true, domain.ContextSourcesSnapshot{MemoryMode: "off", ContextRouter: "off", ExternalContext: "off"}},
		{"assisted arm", true, projectmemory.ModeAssisted, false, true, domain.ContextSourcesSnapshot{MemoryMode: "assisted", ContextRouter: "off", ExternalContext: "github"}},
		{"assisted arm, external off (3D)", true, projectmemory.ModeAssisted, false, false, domain.ContextSourcesSnapshot{MemoryMode: "assisted", ContextRouter: "off", ExternalContext: "off"}},
		{"preferred with router", true, projectmemory.ModePreferred, true, true, domain.ContextSourcesSnapshot{MemoryMode: "preferred", ContextRouter: "on", ExternalContext: "github"}},
		{"requested but not provisioned", false, projectmemory.ModeAssisted, false, true, domain.ContextSourcesSnapshot{MemoryMode: "off", ContextRouter: "off", ExternalContext: "off"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := effectiveContextSources(tc.provisioned, tc.mode, tc.routing, tc.external)
			if got != tc.want || !got.Recorded() {
				t.Fatalf("effectiveContextSources = %+v, want %+v (recorded)", got, tc.want)
			}
		})
	}
}
