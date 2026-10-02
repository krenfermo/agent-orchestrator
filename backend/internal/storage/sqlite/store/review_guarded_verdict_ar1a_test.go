package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

// AR-1a / D-SEC-2 (Codex AR1A-02), against the real schema: the header-less
// verdict write lands only while no reviewer identity speaks for the run, and
// that is decided in the write itself.
func TestTheGuardedVerdictWriteRespectsTheReviewersIdentity(t *testing.T) {
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)

	t.Run("no identity expected, no credential: lands", func(t *testing.T) {
		s := newTestStore(t)
		seedReviewRun(t, s, "run-free", domain.ReviewRunRunning)
		ok, err := s.UpdateReviewRunResultWithoutReviewerIdentity(ctx, "run-free", domain.ReviewRunComplete, domain.VerdictApproved, "", "", false)
		if err != nil || !ok {
			t.Fatalf("guarded write = %v, %v; want it to land", ok, err)
		}
	})

	t.Run("unrevoked reviewer credential: refused", func(t *testing.T) {
		s := newTestStore(t)
		owner := seedCredentialOwner(t, s)
		seedReviewRun(t, s, "run-cred", domain.ReviewRunRunning)
		if _, err := s.InsertAgentCredential(ctx, credential(owner, "agc-1", "hash-1", "run-cred", now)); err != nil {
			t.Fatal(err)
		}
		ok, err := s.UpdateReviewRunResultWithoutReviewerIdentity(ctx, "run-cred", domain.ReviewRunComplete, domain.VerdictApproved, "", "", false)
		if err != nil || ok {
			t.Fatalf("guarded write = %v, %v; want it refused", ok, err)
		}
		run, _, _ := s.GetReviewRun(ctx, "run-cred")
		if run.Status != domain.ReviewRunRunning || run.Verdict != domain.VerdictNone {
			t.Fatalf("run = %s/%s, want untouched", run.Status, run.Verdict)
		}
		// Revoked (e.g. at a failed launch hand-over): no identity speaks.
		if _, err := s.RevokeAgentCredential(ctx, "agc-1", now); err != nil {
			t.Fatal(err)
		}
		ok, err = s.UpdateReviewRunResultWithoutReviewerIdentity(ctx, "run-cred", domain.ReviewRunComplete, domain.VerdictApproved, "", "", false)
		if err != nil || !ok {
			t.Fatalf("guarded write after revocation = %v, %v; want it to land", ok, err)
		}
	})

	t.Run("identity expected, not yet minted: refused", func(t *testing.T) {
		s := newTestStore(t)
		sessionID := seedReviewRun(t, s, "run-seed", domain.ReviewRunRunning)
		if err := s.InsertReviewRun(ctx, domain.ReviewRun{
			ID: "run-expect", ReviewID: "rev-run-seed", SessionID: sessionID, Harness: domain.ReviewerCodex,
			PRURL: "", TargetSHA: "sha-expect", Status: domain.ReviewRunRunning, Verdict: domain.VerdictNone,
			CreatedAt: now, ReviewerIdentityExpected: true,
		}); err != nil {
			t.Fatal(err)
		}
		got, ok, err := s.GetReviewRun(ctx, "run-expect")
		if err != nil || !ok || !got.ReviewerIdentityExpected {
			t.Fatalf("marker did not round-trip: %+v %v %v", got, ok, err)
		}
		landed, err := s.UpdateReviewRunResultWithoutReviewerIdentity(ctx, "run-expect", domain.ReviewRunComplete, domain.VerdictApproved, "", "", false)
		if err != nil || landed {
			t.Fatalf("guarded write = %v, %v; want it refused before the credential exists", landed, err)
		}
		// The reviewer's own (agent) path is unaffected.
		if landed, err := s.UpdateReviewRunResult(ctx, "run-expect", domain.ReviewRunComplete, domain.VerdictApproved, "", "", false); err != nil || !landed {
			t.Fatalf("agent write = %v, %v; want it to land", landed, err)
		}
	})
}
