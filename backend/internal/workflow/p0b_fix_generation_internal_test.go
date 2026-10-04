package workflow

// P0-B regression, fix-cycle half: the identity model for fix dispatch.
//
// The invariant these tests defend is the same one p0b_worker_generation_test.go
// defends for the worker launch, applied to the one mutation AO makes on
// purpose — writing a fix prompt into a live worker session:
//
//	No durable transition of a fix delivery may be taken by a pass that does
//	not hold the generation that delivery was made under, and no fix
//	generation may act once the review generation that authorized it is gone.
//
// The unit tests below pin the identity, the refusal rules and the recovery
// disposition table; the CAS tests at the bottom run against a REAL sqlite
// store, because the whole claim is about what a statement does to a row.

import (
	stdctx "context"
	"strings"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/sqlitetest"
)

// ---------------------------------------------------------------------------
// stubs
// ---------------------------------------------------------------------------

// stubReviewRuns answers GetReviewRun from a map and nothing else. Every other
// method of the interface is unreachable from the code under test here, and is
// left as a compile-time-only implementation rather than a fake nobody drives.
type stubReviewRuns struct {
	runs map[string]domain.ReviewRun
	err  error
}

func (s *stubReviewRuns) GetReviewRun(_ stdctx.Context, id string) (domain.ReviewRun, bool, error) {
	if s.err != nil {
		return domain.ReviewRun{}, false, s.err
	}
	rr, ok := s.runs[id]
	return rr, ok, nil
}

func (s *stubReviewRuns) GetReviewBySessionAndHarness(stdctx.Context, domain.SessionID, domain.ReviewerHarness) (domain.Review, bool, error) {
	return domain.Review{}, false, nil
}
func (s *stubReviewRuns) UpsertReview(stdctx.Context, domain.Review) error { return nil }
func (s *stubReviewRuns) InsertReviewRun(stdctx.Context, domain.ReviewRun) error {
	return nil
}
func (s *stubReviewRuns) GetReviewRunBySessionPRSHAAndHarness(stdctx.Context, domain.SessionID, string, string, domain.ReviewerHarness) (domain.ReviewRun, bool, error) {
	return domain.ReviewRun{}, false, nil
}
func (s *stubReviewRuns) ListReviewRunsBySession(stdctx.Context, domain.SessionID) ([]domain.ReviewRun, error) {
	return nil, nil
}
func (s *stubReviewRuns) CancelRunningReviewRunsBySessionAndHarness(stdctx.Context, domain.SessionID, domain.ReviewerHarness, string) (int64, error) {
	return 0, nil
}
func (s *stubReviewRuns) UpdateReviewRunResult(stdctx.Context, string, domain.ReviewRunStatus, domain.ReviewVerdict, string, string, bool) (bool, error) {
	return false, nil
}
func (s *stubReviewRuns) MarkReviewRunSupersededBy(stdctx.Context, string, string) (bool, error) {
	return false, nil
}

func changesRequestedReview() domain.ReviewRun {
	return domain.ReviewRun{
		ID:        "rr-1",
		SessionID: domain.SessionID("sess-worker"),
		TargetSHA: "sha-aaa",
		Status:    domain.ReviewRunComplete,
		Verdict:   domain.VerdictChangesRequested,
		Body:      "- fix the thing",
	}
}

// genFixture builds a coordinator whose only wired dependency is the review
// store, plus the generation a first dispatch of cycle 1 would mint.
func genFixture(t *testing.T, rr domain.ReviewRun) (*Coordinator, fixDispatchGeneration, fixFindingsRef) {
	t.Helper()
	c := &Coordinator{reviewRuns: &stubReviewRuns{runs: map[string]domain.ReviewRun{rr.ID: rr}}}
	run := domain.WorkflowRun{ID: "wf-1", ProjectID: "p"}
	fixStep := domain.WorkflowStep{ID: "wfs-fix", Kind: domain.WorkflowStepFix}
	findings := reviewFindingsRef(rr)
	gen := c.intendedFixDispatchGeneration(run, fixStep, rr, 1, 0, 0, findings)
	gen.ID = "wfg-1"
	return c, gen, findings
}

// ---------------------------------------------------------------------------
// The identity itself
// ---------------------------------------------------------------------------

// The binding must separate every dimension the requirement names. If any of
// them collapses, two different deliveries become adoptable for one another,
// which is the whole failure mode.
func TestFixGenerationBindingSeparatesEveryDimension(t *testing.T) {
	base := fixDispatchGeneration{
		ID: "wfg-1", WorkflowRunID: "wf-1", TaskID: "task-1", FixStepID: "wfs-fix",
		ReviewRunID: "rr-1", ReviewGeneration: "revgen-1", CycleNumber: 2,
		TransportAttempt: 0, Redelivery: 0, SessionID: "sess-worker", FindingsDigest: "dig-1",
	}
	for name, mutate := range map[string]func(*fixDispatchGeneration){
		"workflow run":      func(g *fixDispatchGeneration) { g.WorkflowRunID = "wf-2" },
		"task":              func(g *fixDispatchGeneration) { g.TaskID = "task-2" },
		"fix step":          func(g *fixDispatchGeneration) { g.FixStepID = "wfs-other" },
		"review run":        func(g *fixDispatchGeneration) { g.ReviewRunID = "rr-2" },
		"review generation": func(g *fixDispatchGeneration) { g.ReviewGeneration = "revgen-2" },
		"cycle":             func(g *fixDispatchGeneration) { g.CycleNumber = 3 },
		"transport attempt": func(g *fixDispatchGeneration) { g.TransportAttempt = 1 },
		"redelivery":        func(g *fixDispatchGeneration) { g.Redelivery = 1 },
		"worker session":    func(g *fixDispatchGeneration) { g.SessionID = "sess-other" },
		"findings":          func(g *fixDispatchGeneration) { g.FindingsDigest = "dig-2" },
	} {
		other := base
		mutate(&other)
		if base.sameDispatch(other) {
			t.Errorf("a different %s produced the same binding: %s", name, base.binding())
		}
	}
	// The token and the attempt row are deliberately NOT part of the binding:
	// a recovery adopts the token it finds, and the attempt does not exist yet
	// when the generation is minted.
	same := base
	same.ID = "wfg-99"
	same.FixAttemptID = "wfa-1"
	if !base.sameDispatch(same) {
		t.Error("the token or the attempt id leaked into the binding; a recovery could then never adopt a generation")
	}
}

// The review authority token must change when — and only when — the authority
// itself changes. Requirement 5's "superseded", "no longer current" and
// "mismatched target" are all this one comparison.
func TestReviewGenerationTokenTracksTheAuthority(t *testing.T) {
	base := changesRequestedReview()
	token := reviewGenerationToken(base)

	reReviewed := base
	reReviewed.TargetSHA = "sha-bbb"
	if reviewGenerationToken(reReviewed) == token {
		t.Error("a review of a different commit produced the same review generation")
	}
	approved := base
	approved.Verdict = domain.VerdictApproved
	if reviewGenerationToken(approved) == token {
		t.Error("an approval produced the same review generation as the changes_requested it replaced")
	}
	other := base
	other.ID = "rr-2"
	if reviewGenerationToken(other) == token {
		t.Error("a different review run produced the same review generation")
	}
	// Same authority, re-read: identical token, so a healthy dispatch is never
	// refused for looking at the same review twice.
	if reviewGenerationToken(changesRequestedReview()) != token {
		t.Error("the same review produced two different generations")
	}
}

// ---------------------------------------------------------------------------
// The staleness gate
// ---------------------------------------------------------------------------

func TestFixGenerationStaleRefusal(t *testing.T) {
	t.Run("current generation may deliver", func(t *testing.T) {
		rr := changesRequestedReview()
		c, gen, findings := genFixture(t, rr)
		if refusal := c.fixGenerationStaleRefusal(stdctx.Background(), gen, rr, findings); refusal != "" {
			t.Fatalf("a current fix generation was refused: %s", refusal)
		}
	})

	// Requirement 5: a superseded review makes the fix generation inert. The
	// review run is the same row and still asks for changes — only the commit
	// it reviewed moved — so nothing but the generation token catches this.
	t.Run("superseded review generation is inert", func(t *testing.T) {
		rr := changesRequestedReview()
		c, gen, findings := genFixture(t, rr)
		superseded := rr
		superseded.TargetSHA = "sha-bbb"
		c.reviewRuns = &stubReviewRuns{runs: map[string]domain.ReviewRun{rr.ID: superseded}}
		refusal := c.fixGenerationStaleRefusal(stdctx.Background(), gen, rr, findings)
		if !strings.Contains(refusal, "superseded") {
			t.Fatalf("refusal = %q, want it to name the superseded review generation", refusal)
		}
	})

	// Requirement 5 again: an approval is a different authority token, so a fix
	// generation minted under changes_requested cannot act on it.
	t.Run("approved review makes an older fix generation inert", func(t *testing.T) {
		rr := changesRequestedReview()
		c, gen, findings := genFixture(t, rr)
		approved := rr
		approved.Verdict = domain.VerdictApproved
		c.reviewRuns = &stubReviewRuns{runs: map[string]domain.ReviewRun{rr.ID: approved}}
		if refusal := c.fixGenerationStaleRefusal(stdctx.Background(), gen, rr, findings); refusal == "" {
			t.Fatal("a fix generation authorized by changes_requested was allowed to act on an approval")
		}
	})

	// Requirement 5: a mismatched findings digest. Same cycle, same review,
	// different payload — the delivery record would then describe bytes that
	// were never sent.
	t.Run("findings digest mismatch is refused", func(t *testing.T) {
		rr := changesRequestedReview()
		c, gen, _ := genFixture(t, rr)
		other := rr
		other.Body = "- something else entirely"
		refusal := c.fixGenerationStaleRefusal(stdctx.Background(), gen, rr, reviewFindingsRef(other))
		if !strings.Contains(refusal, "findings digest") {
			t.Fatalf("refusal = %q, want it to name the findings digest mismatch", refusal)
		}
	})

	// Requirement 5: a mismatched target. The generation names one worker
	// session; the cycle in hand is aimed at another.
	t.Run("wrong worker session is refused", func(t *testing.T) {
		rr := changesRequestedReview()
		c, gen, findings := genFixture(t, rr)
		moved := rr
		moved.SessionID = domain.SessionID("sess-somebody-else")
		refusal := c.fixGenerationStaleRefusal(stdctx.Background(), gen, moved, findings)
		if !strings.Contains(refusal, "worker session") {
			t.Fatalf("refusal = %q, want it to name the session mismatch", refusal)
		}
	})

	// The default is refusal: a store AO cannot read is not a licence to write
	// into somebody's worktree.
	t.Run("an unreadable review store refuses", func(t *testing.T) {
		rr := changesRequestedReview()
		c, gen, findings := genFixture(t, rr)
		c.reviewRuns = &stubReviewRuns{runs: map[string]domain.ReviewRun{}}
		if refusal := c.fixGenerationStaleRefusal(stdctx.Background(), gen, rr, findings); refusal == "" {
			t.Fatal("a fix generation whose review run could not be read was allowed to deliver")
		}
	})

	// A generation-less record carries no authority token to compare, and this
	// gate must not invent one. fixAuthorityRefusal still applies to it.
	t.Run("a legacy generation is not refused by this gate", func(t *testing.T) {
		rr := changesRequestedReview()
		c, _, findings := genFixture(t, rr)
		if refusal := c.fixGenerationStaleRefusal(stdctx.Background(), fixDispatchGeneration{}, rr, findings); refusal != "" {
			t.Fatalf("a generation-less delivery was refused by the generation gate: %s", refusal)
		}
	})
}

// ---------------------------------------------------------------------------
// Recovery: whose dispatch is on disk?
// ---------------------------------------------------------------------------

func intentRecord(gen fixDispatchGeneration, reviewRunID, digest string) promptDeliveryRecord {
	return promptDeliveryRecord{
		CycleNumber:      gen.CycleNumber,
		TransportAttempt: gen.TransportAttempt,
		Generation:       gen,
		Findings:         FixFindingsEvidence{ReviewRunID: reviewRunID, Digest: digest},
	}
}

func TestResolveOwningFixGeneration(t *testing.T) {
	rr := changesRequestedReview()
	c, gen, _ := genFixture(t, rr)
	digest := FindingsDigest(rr.Body)
	dispatched := func(token string) domain.WorkflowOutboxEntry {
		return domain.WorkflowOutboxEntry{
			ID: "wfo-1", Status: domain.WorkflowOutboxDispatched, DispatchGeneration: token,
		}
	}

	// Crash boundary B: the claim is durable, the pre-delivery record is not.
	// The claim's token is adopted, never re-minted.
	t.Run("claim with no record adopts the claim's token", func(t *testing.T) {
		got, disp, why := c.resolveOwningFixGeneration(dispatched("wfg-claimed"), gen, nil)
		if disp != fixGenerationOwned || why != "" {
			t.Fatalf("disposition = %v (%s), want owned", disp, why)
		}
		if got.ID != "wfg-claimed" {
			t.Fatalf("generation id = %q, want the claim's own token", got.ID)
		}
		if !got.sameDispatch(gen) {
			t.Fatal("the adopted generation does not describe the dispatch this pass derived")
		}
	})

	// Crash boundary C/D: the record survives, and the acknowledge that cleared
	// the row's token may or may not have landed. The record is authoritative.
	t.Run("record with a cleared claim is still owned", func(t *testing.T) {
		got, disp, why := c.resolveOwningFixGeneration(
			domain.WorkflowOutboxEntry{ID: "wfo-1", Status: domain.WorkflowOutboxAcknowledged},
			gen, []promptDeliveryRecord{intentRecord(gen, rr.ID, digest)})
		if disp != fixGenerationOwned || why != "" {
			t.Fatalf("disposition = %v (%s), want owned", disp, why)
		}
		if got.ID != gen.ID {
			t.Fatalf("generation id = %q, want %q", got.ID, gen.ID)
		}
	})

	// Two different generations for one cycle is unprovable by construction.
	t.Run("two recorded generations fail closed", func(t *testing.T) {
		second := gen
		second.ID = "wfg-2"
		_, disp, why := c.resolveOwningFixGeneration(dispatched(""), gen,
			[]promptDeliveryRecord{intentRecord(gen, rr.ID, digest), intentRecord(second, rr.ID, digest)})
		if disp != fixGenerationUnprovable {
			t.Fatalf("disposition = %v, want unprovable", disp)
		}
		if !strings.Contains(why, "different dispatch generations") {
			t.Fatalf("reason = %q, want it to name the competing generations", why)
		}
	})

	// The row and the ledger disagree about who owns the delivery.
	t.Run("claim token disagreeing with the record fails closed", func(t *testing.T) {
		_, disp, why := c.resolveOwningFixGeneration(dispatched("wfg-somebody-else"), gen,
			[]promptDeliveryRecord{intentRecord(gen, rr.ID, digest)})
		if disp != fixGenerationUnprovable {
			t.Fatalf("disposition = %v, want unprovable", disp)
		}
		if !strings.Contains(why, "claimed by generation") {
			t.Fatalf("reason = %q, want it to name the disagreement", why)
		}
	})

	// A recorded generation for a DIFFERENT delivery must never be adopted for
	// this one, however similar the two look.
	t.Run("a record for another dispatch fails closed", func(t *testing.T) {
		other := gen
		other.SessionID = "sess-somebody-else"
		_, disp, why := c.resolveOwningFixGeneration(dispatched(""), gen,
			[]promptDeliveryRecord{intentRecord(other, rr.ID, digest)})
		if disp != fixGenerationUnprovable {
			t.Fatalf("disposition = %v, want unprovable", disp)
		}
		if !strings.Contains(why, "not the dispatch this pass derived") {
			t.Fatalf("reason = %q, want it to say the dispatch does not match", why)
		}
	})

	// Requirement 9: a generation-less delivery that maps deterministically onto
	// this cycle recovers safely, under the empty token the row actually holds.
	t.Run("a consistent legacy delivery is adopted without fabricating a token", func(t *testing.T) {
		got, disp, why := c.resolveOwningFixGeneration(dispatched(""), gen,
			[]promptDeliveryRecord{intentRecord(fixDispatchGeneration{CycleNumber: 1}, rr.ID, digest)})
		if disp != fixGenerationLegacyAdopted || why != "" {
			t.Fatalf("disposition = %v (%s), want legacy adoption", disp, why)
		}
		if got.ID != "" {
			t.Fatalf("generation id = %q, want empty: recovery must not fabricate a generation", got.ID)
		}
	})

	// Requirement 9: and one that does NOT map deterministically fails closed
	// with a named condition, rather than guessing.
	t.Run("a legacy delivery that cannot be mapped fails closed", func(t *testing.T) {
		_, disp, why := c.resolveOwningFixGeneration(dispatched(""), gen,
			[]promptDeliveryRecord{intentRecord(fixDispatchGeneration{CycleNumber: 1}, "rr-somebody-else", digest)})
		if disp != fixGenerationUnprovable {
			t.Fatalf("disposition = %v, want unprovable", disp)
		}
		if !strings.Contains(why, "review run") {
			t.Fatalf("reason = %q, want it to name what disagrees", why)
		}
	})

	// A claim token over an older generation-less delivery: nothing was sent
	// under the claim, something was sent for this cycle, and AO cannot say
	// whether they are the same event.
	t.Run("a claim over a legacy delivery fails closed", func(t *testing.T) {
		_, disp, why := c.resolveOwningFixGeneration(dispatched("wfg-claimed"), gen,
			[]promptDeliveryRecord{intentRecord(fixDispatchGeneration{CycleNumber: 1}, rr.ID, digest)})
		if disp != fixGenerationUnprovable {
			t.Fatalf("disposition = %v, want unprovable", disp)
		}
		if !strings.Contains(why, "generation-less") {
			t.Fatalf("reason = %q, want it to name the generation-less records", why)
		}
	})
}

// The fail-closed condition must be a NAMED, actionable stop with a human
// action — requirement 9's "named actionable condition", and attention.go's
// standing rule that human_decision without an action is not expressible.
func TestFixGenerationUnprovableIsANamedActionableStop(t *testing.T) {
	d, ok := attentionDispositions[ReasonFixGenerationUnprovable]
	if !ok {
		t.Fatal("fix_generation_unprovable is not in the canonical attention vocabulary")
	}
	if d.SelfRemediable {
		t.Error("fix_generation_unprovable must not be self-remediable: nothing AO does by itself changes the ledger")
	}
	if strings.TrimSpace(d.HumanAction) == "" {
		t.Error("fix_generation_unprovable has no human action, so it cannot be a human decision")
	}
}

// ---------------------------------------------------------------------------
// The CAS statements, against a real store
// ---------------------------------------------------------------------------

func fixOutboxFixture(t *testing.T) (*sqlite.Store, stdctx.Context, domain.WorkflowOutboxEntry) {
	t.Helper()
	st := sqlitetest.MustOpen(t)
	ctx := stdctx.Background()
	now := time.Now().UTC()
	if err := st.UpsertProject(ctx, domain.ProjectRecord{ID: "p", Path: t.TempDir(), RegisteredAt: now}); err != nil {
		t.Fatal(err)
	}
	run := domain.WorkflowRun{ID: "wf-fixgen", ProjectID: "p", Objective: "o",
		State: domain.WorkflowRunRunning, PolicyVersion: policyVersionV1, PolicySnapshot: "{}",
		CreatedAt: now, UpdatedAt: now}
	step := domain.WorkflowStep{ID: "wfs-fix", WorkflowRunID: run.ID, Kind: domain.WorkflowStepFix,
		Ordinal: 1, State: domain.WorkflowStepReady, ArtifactJSON: "{}", CreatedAt: now, UpdatedAt: now}
	if _, _, err := st.CreateWorkflowRun(ctx, run, []domain.WorkflowStep{step}); err != nil {
		t.Fatal(err)
	}
	stepID := step.ID
	entry, _, err := st.EnqueueWorkflowOutboxEntry(ctx, domain.WorkflowOutboxEntry{
		ID: "wfo-fixgen", WorkflowRunID: run.ID, WorkflowStepID: &stepID,
		IdempotencyKey: "workflow-step-fix:wfs-fix:cycle1",
		CommandType:    domain.WorkflowOutboxSendMessage, Payload: "{}", CreatedAt: now,
	})
	if err != nil {
		t.Fatal(err)
	}
	return st, ctx, entry
}

// The claim is exclusive, and it stamps the token. Two passes racing to deliver
// the same fix cycle: exactly one may send.
func TestFixDispatchClaimIsExclusiveAndStamped(t *testing.T) {
	st, ctx, entry := fixOutboxFixture(t)
	now := time.Now().UTC()

	first, err := st.ClaimWorkflowOutboxDispatch(ctx, entry.ID, now, "wfg-A")
	if err != nil || !first {
		t.Fatalf("first claim = %v, err=%v, want true", first, err)
	}
	second, err := st.ClaimWorkflowOutboxDispatch(ctx, entry.ID, now, "wfg-B")
	if err != nil {
		t.Fatal(err)
	}
	if second {
		t.Fatal("a second pass claimed a fix dispatch that was already claimed; both would have called Send")
	}
	entries, err := st.ListWorkflowOutboxByRun(ctx, entry.WorkflowRunID)
	if err != nil || len(entries) != 1 {
		t.Fatalf("outbox = %+v, err=%v", entries, err)
	}
	if entries[0].DispatchGeneration != "wfg-A" {
		t.Fatalf("dispatch generation = %q, want wfg-A: the claim must say whose it is", entries[0].DispatchGeneration)
	}
}

// Every transition off a claimed fix dispatch names the token back. A pass that
// does not hold the claim can neither complete it, fail it, nor release it.
func TestStaleFixGenerationCannotCompleteFailOrReleaseADispatch(t *testing.T) {
	for _, tc := range []struct {
		name string
		act  func(*sqlite.Store, stdctx.Context, string, string) (bool, error)
	}{
		{"acknowledge", func(st *sqlite.Store, ctx stdctx.Context, id, gen string) (bool, error) {
			return st.AcknowledgeWorkflowOutboxDispatch(ctx, id, domain.WorkflowOutboxDispatched, time.Now().UTC(), gen)
		}},
		{"fail", func(st *sqlite.Store, ctx stdctx.Context, id, gen string) (bool, error) {
			return st.FailWorkflowOutboxWithGeneration(ctx, id, domain.WorkflowOutboxDispatched, time.Now().UTC(), "prompt_delivery_failed", gen, gen)
		}},
		{"release", func(st *sqlite.Store, ctx stdctx.Context, id, gen string) (bool, error) {
			return st.ReleaseDispatchedWorkflowOutboxGeneration(ctx, id, "", gen)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st, ctx, entry := fixOutboxFixture(t)
			if ok, err := st.ClaimWorkflowOutboxDispatch(ctx, entry.ID, time.Now().UTC(), "wfg-live"); err != nil || !ok {
				t.Fatalf("claim: %v %v", ok, err)
			}
			stale, err := tc.act(st, ctx, entry.ID, "wfg-stale")
			if err != nil {
				t.Fatal(err)
			}
			if stale {
				t.Fatalf("a stale fix generation was allowed to %s a live dispatch", tc.name)
			}
			live, err := tc.act(st, ctx, entry.ID, "wfg-live")
			if err != nil || !live {
				t.Fatalf("the owning generation could not %s its own dispatch: %v %v", tc.name, live, err)
			}
		})
	}
}

// The generation-less rows already on disk: an unclaimed row is completed by an
// unclaimed acknowledge and by nothing else. This is what makes requirement 9's
// safe legacy recovery possible without fabricating a token.
func TestLegacyUnclaimedFixDispatchIsCompletedOnlyByAnUnclaimedAcknowledge(t *testing.T) {
	st, ctx, entry := fixOutboxFixture(t)
	now := time.Now().UTC()
	// A row moved to `dispatched` by the pre-generation status CAS: no token.
	if ok, err := st.UpdateWorkflowOutboxStatus(ctx, entry.ID, domain.WorkflowOutboxPending, domain.WorkflowOutboxDispatched, now, ""); err != nil || !ok {
		t.Fatalf("seed legacy dispatched row: %v %v", ok, err)
	}
	tokened, err := st.AcknowledgeWorkflowOutboxDispatch(ctx, entry.ID, domain.WorkflowOutboxDispatched, now, "wfg-invented")
	if err != nil {
		t.Fatal(err)
	}
	if tokened {
		t.Fatal("an invented generation completed a generation-less dispatch; recovery could then fabricate ownership")
	}
	legacy, err := st.AcknowledgeWorkflowOutboxDispatch(ctx, entry.ID, domain.WorkflowOutboxDispatched, now, "")
	if err != nil || !legacy {
		t.Fatalf("the unclaimed acknowledge could not complete an unclaimed row: %v %v", legacy, err)
	}
}
