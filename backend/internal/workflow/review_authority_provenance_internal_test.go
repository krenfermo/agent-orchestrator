package workflow

// The two predicates review_authority_provenance.go adds, pinned directly.
//
// They are unit tests rather than fixture drives because each one is a claim
// about a decision made from durable facts alone — "may this run take the
// pointer", "would this link close a loop" — and a claim like that is worth
// stating once, exhaustively, where every branch is visible.

import (
	stdctx "context"
	"errors"
	"strings"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

func reviewProvenanceCoordinator(runs map[string]domain.ReviewRun, err error) *Coordinator {
	return New(Deps{ReviewRuns: &stubReviewRuns{runs: runs, err: err}})
}

// A released predecessor may retake authority on exactly one showing: a
// readable, unsuperseded verdict for the target this step is asking about.
func TestPredecessorMayRetakeAuthorityOnlyWithAProvableCurrentVerdict(t *testing.T) {
	replacement := domain.ReviewRun{ID: "rr-replacement", TargetSHA: "fp-current"}

	for _, tc := range []struct {
		name      string
		pred      domain.ReviewRun
		present   bool
		readErr   error
		want      bool
		wantWhyIn string
	}{
		{
			name:    "late verdict for the current target wins",
			pred:    domain.ReviewRun{ID: "rr-pred", TargetSHA: "fp-current", Status: domain.ReviewRunCancelled, LateVerdict: domain.VerdictApproved},
			present: true,
			want:    true,
		},
		{
			name:    "an on-time verdict for the current target wins too",
			pred:    domain.ReviewRun{ID: "rr-pred", TargetSHA: "fp-current", Status: domain.ReviewRunComplete, Verdict: domain.VerdictChangesRequested},
			present: true,
			want:    true,
		},
		{
			name: "already superseded is history, not authority",
			pred: domain.ReviewRun{ID: "rr-pred", TargetSHA: "fp-current", Status: domain.ReviewRunCancelled,
				LateVerdict: domain.VerdictApproved, SupersededBy: "rr-someone-else"},
			present:   true,
			want:      false,
			wantWhyIn: "already superseded",
		},
		{
			name:      "no verdict at all won nothing",
			pred:      domain.ReviewRun{ID: "rr-pred", TargetSHA: "fp-current", Status: domain.ReviewRunCancelled},
			present:   true,
			want:      false,
			wantWhyIn: "produced no verdict",
		},
		{
			name: "a verdict for an obsolete target certifies work this step is no longer about",
			pred: domain.ReviewRun{ID: "rr-pred", TargetSHA: "fp-obsolete", Status: domain.ReviewRunCancelled,
				LateVerdict: domain.VerdictApproved},
			present:   true,
			want:      false,
			wantWhyIn: "is asking about",
		},
		{
			name:      "a predecessor that does not exist is not provenance",
			present:   false,
			want:      false,
			wantWhyIn: "does not exist",
		},
		{
			name:      "a predecessor AO cannot read is not provenance either",
			present:   true,
			readErr:   errors.New("boom"),
			want:      false,
			wantWhyIn: "could not be read",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			runs := map[string]domain.ReviewRun{}
			if tc.present {
				pred := tc.pred
				if pred.ID == "" {
					pred.ID = "rr-pred"
				}
				runs[pred.ID] = pred
			}
			c := reviewProvenanceCoordinator(runs, tc.readErr)
			ok, why := c.predecessorMayRetakeAuthority(stdctx.Background(), "rr-pred", replacement)
			if ok != tc.want {
				t.Fatalf("mayRetake = %v, want %v (why=%q)", ok, tc.want, why)
			}
			if tc.want {
				if why != "" {
					t.Fatalf("a permitted retake still named a refusal: %q", why)
				}
				return
			}
			if why == "" {
				t.Fatal("a refusal must name the fact that produced it")
			}
			if !strings.Contains(why, tc.wantWhyIn) {
				t.Fatalf("refusal = %q, want it to name %q", why, tc.wantWhyIn)
			}
		})
	}
}

// The supersession chain is a history, and a history cannot contain a loop.
// The storage guard only refuses a self-link, so this is where two-node and
// longer cycles are actually refused.
func TestSupersessionCyclesAreRefused(t *testing.T) {
	for _, tc := range []struct {
		name                    string
		runs                    map[string]domain.ReviewRun
		predecessor, replacemnt string
		readErr                 error
		want                    bool
	}{
		{
			name:        "the incident: A was superseded by B, and B must not be superseded by A",
			runs:        map[string]domain.ReviewRun{"A": {ID: "A", SupersededBy: "B"}, "B": {ID: "B"}},
			predecessor: "B", replacemnt: "A",
			want: true,
		},
		{
			name:        "an ordinary replacement is not a cycle",
			runs:        map[string]domain.ReviewRun{"A": {ID: "A"}, "B": {ID: "B"}},
			predecessor: "A", replacemnt: "B",
			want: false,
		},
		{
			name:        "a longer chain that terminates is not a cycle",
			runs:        map[string]domain.ReviewRun{"A": {ID: "A"}, "B": {ID: "B", SupersededBy: "C"}, "C": {ID: "C"}},
			predecessor: "A", replacemnt: "B",
			want: false,
		},
		{
			name:        "a chain that already loops cannot be extended",
			runs:        map[string]domain.ReviewRun{"A": {ID: "A"}, "B": {ID: "B", SupersededBy: "C"}, "C": {ID: "C", SupersededBy: "B"}},
			predecessor: "A", replacemnt: "B",
			want: true,
		},
		{
			name:        "a run cannot supersede itself",
			runs:        map[string]domain.ReviewRun{"A": {ID: "A"}},
			predecessor: "A", replacemnt: "A",
			want: true,
		},
		{
			name:        "an unreadable chain is unprovable, so the link is refused",
			runs:        map[string]domain.ReviewRun{"A": {ID: "A"}, "B": {ID: "B"}},
			predecessor: "A", replacemnt: "B",
			readErr: errors.New("boom"),
			want:    true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := reviewProvenanceCoordinator(tc.runs, tc.readErr)
			if got := c.supersessionWouldCycle(stdctx.Background(), tc.predecessor, tc.replacemnt); got != tc.want {
				t.Fatalf("supersessionWouldCycle = %v, want %v", got, tc.want)
			}
		})
	}
}

// The refusal is in the closed attention vocabulary, so the Board renders it as
// a decision with an action rather than as an unclassified stop.
func TestStaleReviewAuthorityIsANamedHumanDecision(t *testing.T) {
	d, ok := attentionDispositions[ReasonReviewAuthorityStale]
	if !ok {
		t.Fatal("review_authority_stale is not in the attention vocabulary")
	}
	if d.SelfRemediable {
		t.Fatal("review_authority_stale is marked self-remediable: AO has nothing left to try")
	}
	if strings.TrimSpace(d.HumanAction) == "" {
		t.Fatal("review_authority_stale has no human action, so it cannot be a human decision")
	}
}
