package workflow

import (
	"fmt"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

// TestPlannerCapacityGenerationSeparatesRevisionsFromRetries is the regression
// for the half of wf-7f8cc736 that made the objective UNPLANNABLE rather than
// merely badly explained.
//
// A capacity claim's dispatch key folds in this generation, and admission
// refuses any launch whose key names an already-released claim. When the
// generation was the retry count alone, a permanently-failed planner (which
// writes no retry checkpoint) left the count at zero, so the attempt after an
// operator-initiated RegeneratePlan reused the failed attempt's own key and was
// refused — reporting "no runtime execution slot is currently free" on an idle
// machine, permanently.
func TestPlannerCapacityGenerationSeparatesRevisionsFromRetries(t *testing.T) {
	rev := func(n int64) domain.WorkflowPlanRecord { return domain.WorkflowPlanRecord{Revision: n} }

	// The incident: same revision, no retries recorded, yet a NEW revision must
	// not reuse the spent generation.
	if plannerCapacityGeneration(rev(1), 0) == plannerCapacityGeneration(rev(2), 0) {
		t.Fatal("a regenerated plan reuses the failed revision's capacity generation; its launch would be refused forever")
	}

	// Automatic retries within one revision still separate from each other.
	seen := map[int64]string{}
	for revision := int64(1); revision <= 4; revision++ {
		for retries := 0; retries <= maxPlannerRetries+1; retries++ {
			got := plannerCapacityGeneration(rev(revision), retries)
			key := label(revision, retries)
			if prev, dup := seen[got]; dup {
				t.Fatalf("generation %d is shared by %s and %s", got, prev, key)
			}
			seen[got] = key
		}
	}

	// A zero/absent revision is treated as revision 1, not as revision 0, so a
	// plan row written before revisions existed keeps a stable identity.
	if plannerCapacityGeneration(rev(0), 2) != plannerCapacityGeneration(rev(1), 2) {
		t.Fatal("an unset revision must read as revision 1")
	}
}

func label(revision int64, retries int) string {
	return fmt.Sprintf("revision %d attempt %d", revision, retries)
}
