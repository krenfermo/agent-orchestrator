package workflow

import (
	stdctx "context"
	"fmt"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/service/questions"
)

// resolutionStalenessThreshold bounds how long a resolution attempt may sit
// at status=running before observeResolutionStep gives up waiting and forces
// the owning question to human_required instead of guessing. Mirrors
// reviewStalenessThreshold's "no longer confident, ask for attention"
// framing (review_progress.go) — scoped to the ONE question whose resolution
// went stale, never the whole run.
const resolutionStalenessThreshold = 30 * time.Minute

// reconcileDecisionResolvers is Checkpoint 8K-B pass 2's read-time
// dispatch+observe pass for every question currently at state=resolving
// (auto_resolvable). Called from reconcileQuestions, right after detection,
// mirroring the review<->fix cascade's "opportunistically observe and
// dispatch the next eligible step within this same call" convention — never
// a background poller.
//
// Returns a non-empty nextAction string when at least one resolving question
// is stuck waiting for provider capacity (Checkpoint 8K-B's
// "waiting_for_capacity" read-time-derived NextAction, never persisted as a
// new WorkflowRunState) — the caller (GetRun) only applies it when no
// higher-priority pending/human_required question override already won.
func (c *Coordinator) reconcileDecisionResolvers(ctx stdctx.Context, run domain.WorkflowRun, now time.Time) (string, error) {
	if c.questionsStore == nil || run.State.Terminal() {
		return "", nil
	}
	questions, err := c.questionsStore.ListWorkflowQuestionsByRun(ctx, run.ID)
	if err != nil {
		return "", err
	}
	policy := policyForRun(run)
	waitingForCapacity := ""
	for _, q := range questions {
		if q.State != domain.QuestionStateResolving {
			continue
		}
		if q.ResolvingRunID == nil {
			nextAction, derr := c.dispatchDecisionResolver(ctx, run, q, policy, now)
			if derr != nil {
				return "", derr
			}
			if nextAction != "" && waitingForCapacity == "" {
				waitingForCapacity = nextAction
			}
			continue
		}
		if err := c.observeResolutionStep(ctx, run, q, now); err != nil {
			return "", err
		}
	}
	return waitingForCapacity, nil
}

// dispatchDecisionResolver mints a fresh workflow_question_resolutions row
// and launches a read-only resolver session for one auto_resolvable question
// that has no current resolution attempt yet. If no provider is currently
// usable (per selectDecisionResolverProvider), it makes no changes and
// returns the "waiting_for_capacity" NextAction string — the question stays
// at state=resolving and dispatch is retried on the next reconcile pass.
func (c *Coordinator) dispatchDecisionResolver(ctx stdctx.Context, run domain.WorkflowRun, q domain.WorkflowQuestion, policy domain.WorkflowPolicy, now time.Time) (string, error) {
	if c.decisionResolverLauncher == nil {
		return "waiting_for_capacity: resolver unavailable (no launcher configured)", nil
	}
	selection, err := c.selectDecisionResolverProvider(ctx, q.AskingHarness, policy, now)
	if err != nil {
		return "", err
	}
	if !selection.Available {
		return "waiting_for_capacity: resolver unavailable", nil
	}

	var branch, worktreePath, fingerprint string
	if q.WorkflowStepID != nil {
		if cp, hasCP, cerr := c.store.GetLatestWorkflowCheckpointByStep(ctx, string(*q.WorkflowStepID)); cerr == nil && hasCP {
			branch, worktreePath = cp.Branch, cp.WorktreePath
			fingerprint = cp.FingerprintAfter
			if fingerprint == "" {
				fingerprint = cp.FingerprintBefore
			}
		}
	}
	if worktreePath == "" {
		// No usable checkout to hand the resolver a workspace over: surface
		// ambiguity via the same capacity-wait channel rather than guessing a
		// path, and let a later pass (once a checkpoint exists) retry.
		return "waiting_for_capacity: resolver unavailable (no worktree recorded yet)", nil
	}

	resolutionID := "wqr-" + c.newID()
	resolverSessionID := domain.SessionID("decision-resolver-" + resolutionID)
	var askingSessionID *domain.SessionID
	if q.SessionID != nil {
		v := *q.SessionID
		askingSessionID = &v
	}
	resolution := domain.WorkflowQuestionResolution{
		ID:                 domain.WorkflowQuestionResolutionID(resolutionID),
		WorkflowQuestionID: q.ID,
		WorkflowRunID:      q.WorkflowRunID,
		AskingSessionID:    askingSessionID,
		ResolverHarness:    selection.Harness,
		Status:             domain.ResolutionStatusPending,
		CreatedAt:          now,
		UpdatedAt:          now,
	}
	if _, err := c.questionsStore.InsertWorkflowQuestionResolution(ctx, resolution); err != nil {
		return "", err
	}
	if _, err := c.questionsStore.SetWorkflowQuestionResolvingRunID(ctx, string(q.ID), strPtr(resolutionID)); err != nil {
		return "", err
	}

	// Checkpoint 8M §9: the decision resolver's own minimal-evidence context
	// pack — objective/acceptance criteria/fingerprint, reusing
	// BuildTaskCheckpointSummary now that it lives in this same package (see
	// task_checkpoint_summary.go's doc comment: this was previously blocked
	// by an import cycle through httpd/controllers, resolved by moving the
	// builder here in this checkpoint). No worker transcript, never a new
	// fetch beyond the plan artifact this dispatch already needs.
	var acceptanceCriteria []string
	var contextPackText string
	if artifact, aerr := c.planArtifactForRun(ctx, run); aerr == nil {
		acceptanceCriteria = artifact.AcceptanceCriteria
		facts := BuildTaskCheckpointSummary(TaskCheckpointSummaryInput{
			Detail: RunDetail{Run: run}, Artifact: &artifact,
		})
		facts.CurrentFingerprint = fingerprint
		contextPackText = RenderContextPackForRole(BuildSessionContextPack(domain.WorkflowRoleDecisionResolver, facts))
	}

	prompt := BuildDecisionResolverPrompt(DecisionResolverPromptInput{
		Objective:          run.Objective,
		AcceptanceCriteria: acceptanceCriteria,
		QuestionText:       q.QuestionText,
		Choices:            q.StructuredChoices,
		Branch:             branch,
		WorktreePath:       worktreePath,
		PolicyVersion:      run.PolicyVersion,
		AllowSameProvider:  selection.SameProvider,
		ResolverSessionID:  string(resolverSessionID),
		ResolutionRunID:    resolutionID,
		ContextPack:        contextPackText,
	})

	// Record the resolver's identity (transition to running, set
	// resolver_session_id) BEFORE calling Preflight/Launch, not after: the
	// prompt already carries this exact identity, so a resolver that
	// responds immediately after being spawned (or, in tests, a fake
	// launcher whose Launch synchronously simulates the callback) must be
	// able to validate against it the instant Launch returns — recording it
	// only after Launch would otherwise be a real, if narrow, race between
	// "resolver started" and "coordinator finished bookkeeping".
	if _, err := c.questionsStore.TransitionResolutionStatus(ctx, resolutionID, domain.ResolutionStatusPending, domain.ResolutionStatusRunning,
		"", "", nil, nil, false, now, nil); err != nil {
		return "", err
	}
	if _, err := c.questionsStore.SetResolutionResolverSessionID(ctx, resolutionID, string(resolverSessionID)); err != nil {
		return "", err
	}

	if err := c.decisionResolverLauncher.Preflight(ctx, selection.Harness, worktreePath); err != nil {
		return c.recordResolutionFailure(ctx, resolutionID, fmt.Errorf("resolver preflight: %w", err), now)
	}
	if _, err := c.decisionResolverLauncher.Launch(ctx, DecisionResolverLaunchRequest{
		Harness:           selection.Harness,
		AskingSessionID:   derefSessionID(askingSessionID),
		ProjectID:         domain.ProjectID(run.ProjectID),
		ResolutionID:      resolutionID,
		ResolverSessionID: resolverSessionID,
		WorkspacePath:     worktreePath,
		Prompt:            prompt,
	}); err != nil {
		return c.recordResolutionFailure(ctx, resolutionID, fmt.Errorf("launch resolver: %w", err), now)
	}
	return "", nil
}

// recordResolutionFailure marks a resolution attempt failed when
// preflight/launch itself could not even start a session (the attempt was
// already transitioned to running so the resolver's baked-in identity could
// be validated the instant it responds — see dispatchDecisionResolver). The
// owning question is left at state=resolving; observeResolutionStep is what
// promotes a failed attempt's OWNING question to human_required, keeping
// exactly one transition path for that escalation rather than two.
func (c *Coordinator) recordResolutionFailure(ctx stdctx.Context, resolutionID string, cause error, now time.Time) (string, error) {
	if _, err := c.questionsStore.TransitionResolutionStatus(ctx, resolutionID, domain.ResolutionStatusRunning, domain.ResolutionStatusFailed,
		"", cause.Error(), nil, nil, false, now, &now); err != nil {
		return "", err
	}
	if c.log != nil {
		c.log.Warn("workflow: decision resolver launch failed", "resolution", resolutionID, "err", cause)
	}
	return "", nil
}

// observeResolutionStep is the pure fact-based observation of one question's
// current resolution attempt, mirroring observeReviewStep: re-reads the
// resolution row (a pure DB fact written by the real `ao decision resolve`
// CLI call hitting the real HTTP endpoint) and applies the corresponding
// question-state transition. Called every reconcile pass for any resolving
// question with a current resolution row.
func (c *Coordinator) observeResolutionStep(ctx stdctx.Context, run domain.WorkflowRun, q domain.WorkflowQuestion, now time.Time) error {
	if q.ResolvingRunID == nil {
		return nil
	}
	resolution, found, err := c.questionsStore.GetWorkflowQuestionResolution(ctx, string(*q.ResolvingRunID))
	if err != nil {
		return err
	}
	if !found {
		// Defensive: dangling pointer. Force human_required rather than
		// silently stalling forever.
		return c.forceQuestionHumanRequired(ctx, q, "resolver_dangling_reference: resolution row referenced by resolving_run_id no longer exists", now)
	}

	switch resolution.Status {
	case domain.ResolutionStatusPending, domain.ResolutionStatusRunning:
		if now.Sub(resolution.UpdatedAt) > resolutionStalenessThreshold {
			if _, err := c.questionsStore.TransitionResolutionStatus(ctx, string(resolution.ID), resolution.Status, domain.ResolutionStatusFailed,
				"", "resolver did not respond within the staleness window", nil, nil, false, now, &now); err != nil {
				return err
			}
			return c.forceQuestionHumanRequired(ctx, q, "resolver_stale: no result within the staleness window", now)
		}
		return nil

	case domain.ResolutionStatusComplete:
		if resolution.RequiresHuman {
			return c.forceQuestionHumanRequired(ctx, q, "resolver_requires_human: "+resolution.ReasonSummary, now)
		}
		answerText := resolution.Answer
		if answerText == "" {
			// Defensive: a complete, non-requires-human resolution with no
			// answer text should not happen given the resolve-service's own
			// validation, but never invent an answer.
			return c.forceQuestionHumanRequired(ctx, q, "ambiguous_resolution_state: resolution completed with no answer and requires_human=false", now)
		}
		ok, err := c.questionsStore.AnswerWorkflowQuestion(ctx, string(q.ID), domain.QuestionStateResolving, domain.QuestionStateAnswered,
			domain.AnswerSourceResolver, answerText, "", now)
		if err != nil {
			return err
		}
		if !ok {
			// Lost a race (e.g. run cancelled concurrently) — not an error.
			return nil
		}
		if c.messageSender != nil {
			// Immediate delivery attempt, mirroring detectQuestionForStep's
			// own "answer just computed, sweep again now for responsiveness"
			// pattern (questions_wiring.go) — the top-of-reconcileQuestions
			// sweep also covers this on the very next call (restart
			// recovery), the delivered flag makes both calls safe.
			if _, err := questions.DeliverAnswered(ctx, c.questionsStore, questionMessageSender{c.messageSender}, run.ID, now); err != nil {
				return err
			}
		}
		return nil

	case domain.ResolutionStatusFailed:
		return c.forceQuestionHumanRequired(ctx, q, "resolver_failed: "+resolution.ReasonSummary, now)

	case domain.ResolutionStatusCancelled:
		// Cancellation only ever originates from CancelRun, which already
		// moves the question to cancelled directly — nothing further to do.
		return nil

	default:
		return nil
	}
}

// forceQuestionHumanRequired transitions a resolving question to
// human_required (resolver failed/stale/dangling/requires_human) without
// ever writing an answer_text/answer_source — the resolver's advisory
// answer (if any) stays only on the resolution row for pass 3's UI, clearly
// labeled, never delivered as a decision.
func (c *Coordinator) forceQuestionHumanRequired(ctx stdctx.Context, q domain.WorkflowQuestion, reason string, now time.Time) error {
	_, err := c.questionsStore.TransitionWorkflowQuestionState(ctx, string(q.ID), domain.QuestionStateResolving, domain.QuestionStateHumanRequired, reason, now)
	return err
}

func strPtr(s string) *string { return &s }

func derefSessionID(id *domain.SessionID) domain.SessionID {
	if id == nil {
		return ""
	}
	return *id
}
