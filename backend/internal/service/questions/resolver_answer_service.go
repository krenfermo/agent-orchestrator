package questions

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

// Sentinel errors for the Decision Resolver callback (Checkpoint 8K-B, pass
// 2), mirroring internal/service/review's submitOne validation-order
// sentinels (ErrInvalid/ErrNotFound/session-mismatch/status-gated) as
// closely as possible — see ResolverAnswerService.Resolve's doc comment for
// the exact order these are checked in.
var (
	ErrResolutionNotFound     = errors.New("questions: resolution run not found")
	ErrResolutionWrongSession = errors.New("questions: resolution run does not belong to this resolver session")
	ErrResolutionNotRunning   = errors.New("questions: resolution run is not running")
	ErrResolutionConflict     = errors.New("questions: resolution run already recorded a different result")
	ErrResolutionInvalid      = errors.New("questions: invalid decision resolve payload")
)

// Resolver callback payload caps (Checkpoint 8K-B, pass 2): reject an
// oversized submission rather than silently truncating it, so a misbehaving
// resolver's mistake is visible instead of quietly losing data.
const (
	MaxEvidenceReferences   = 10
	MaxEvidenceReferenceLen = 500
	MaxReasonSummaryLen     = 2000
	MaxAnswerLen            = 4000
)

// ResolutionStore is the persistence surface the resolver callback needs.
// Satisfied by *store.Store (workflow_question_resolutions_store.go).
type ResolutionStore interface {
	GetWorkflowQuestionResolution(ctx context.Context, id string) (domain.WorkflowQuestionResolution, bool, error)
	TransitionResolutionStatus(ctx context.Context, id string, expectedStatus, newStatus domain.ResolutionStatus, answer, reasonSummary string, evidenceReferences []string, certainty *domain.QuestionCertainty, requiresHuman bool, updatedAt time.Time, completedAt *time.Time) (bool, error)
}

// ResolveInput is one `ao decision resolve` submission, already decoded from
// the HTTP request body.
type ResolveInput struct {
	RunID              string
	Answer             string
	ReasonSummary      string
	EvidenceReferences []string
	Certainty          domain.QuestionCertainty
	RequiresHuman      bool
}

// ResolverAnswerService implements Checkpoint 8K-B pass 2's resolver-callback
// use case: the controller-facing surface for POST
// .../decisions/resolve. Deliberately thin and persist-only — it never
// transitions the owning workflow_question's state or attempts delivery
// inline; that happens in the reconcile loop's observeResolutionStep, same
// poll-based-not-push-based discipline 8K-A already established.
type ResolverAnswerService struct {
	Store ResolutionStore
	Clock func() time.Time
}

func (s *ResolverAnswerService) now() time.Time {
	if s.Clock != nil {
		return s.Clock()
	}
	return time.Now().UTC()
}

// Resolve validates and persists one resolver result, mirroring
// internal/service/review's submitOne validation order as closely as
// possible:
//  1. --run required; resolution row exists -> ErrResolutionNotFound if not.
//  2. resolution.ResolverSessionID == pathSessionID -> reject cross-session
//     (ErrResolutionWrongSession).
//  3. resolution.Status == running -> CAS-transition to complete/failed.
//     Exact-match resubmit (same answer/certainty/requiresHuman/evidence) is
//     an idempotent no-op, no second write. Differing resubmit is rejected
//     (ErrResolutionConflict). Any other status is rejected
//     (ErrResolutionNotRunning).
//  4. Payload validation: exactly one of (answer set) or
//     (requires_human=true); certainty must be a valid enum (when answering,
//     not required for requires_human); evidence_references
//     count/length-capped; reason_summary length-capped.
func (s *ResolverAnswerService) Resolve(ctx context.Context, pathSessionID string, in ResolveInput) (domain.WorkflowQuestionResolution, error) {
	if in.RunID == "" {
		return domain.WorkflowQuestionResolution{}, fmt.Errorf("%w: --run is required", ErrResolutionInvalid)
	}
	if err := validateResolvePayload(in); err != nil {
		return domain.WorkflowQuestionResolution{}, err
	}

	resolution, ok, err := s.Store.GetWorkflowQuestionResolution(ctx, in.RunID)
	if err != nil {
		return domain.WorkflowQuestionResolution{}, err
	}
	if !ok {
		return domain.WorkflowQuestionResolution{}, fmt.Errorf("%w: resolution run %q", ErrResolutionNotFound, in.RunID)
	}
	if resolution.ResolverSessionID == nil || string(*resolution.ResolverSessionID) != pathSessionID {
		return domain.WorkflowQuestionResolution{}, fmt.Errorf("%w: resolution run %q does not belong to resolver session %q", ErrResolutionWrongSession, in.RunID, pathSessionID)
	}

	switch resolution.Status {
	case domain.ResolutionStatusRunning:
		now := s.now()
		newStatus := domain.ResolutionStatusComplete
		ok, err := s.Store.TransitionResolutionStatus(ctx, in.RunID, domain.ResolutionStatusRunning, newStatus,
			in.Answer, in.ReasonSummary, in.EvidenceReferences, certaintyPtr(in.Certainty), in.RequiresHuman, now, &now)
		if err != nil {
			return domain.WorkflowQuestionResolution{}, err
		}
		if !ok {
			// Lost a race (e.g. a concurrent staleness sweep already moved it
			// to failed between the read above and here) — surface as
			// not-running rather than silently retrying.
			return domain.WorkflowQuestionResolution{}, fmt.Errorf("%w: resolution run %q", ErrResolutionNotRunning, in.RunID)
		}
		resolution.Status = newStatus
		resolution.Answer = in.Answer
		resolution.ReasonSummary = in.ReasonSummary
		resolution.EvidenceReferences = in.EvidenceReferences
		resolution.Certainty = certaintyPtr(in.Certainty)
		resolution.RequiresHuman = in.RequiresHuman
		resolution.UpdatedAt = now
		resolution.CompletedAt = &now
		return resolution, nil

	case domain.ResolutionStatusComplete:
		if resolveResultMatches(resolution, in) {
			// Exact-match resubmit: idempotent no-op, no second write.
			return resolution, nil
		}
		return domain.WorkflowQuestionResolution{}, fmt.Errorf("%w: resolution run %q already recorded a different result", ErrResolutionConflict, in.RunID)

	default:
		return domain.WorkflowQuestionResolution{}, fmt.Errorf("%w: resolution run %q is not running", ErrResolutionNotRunning, in.RunID)
	}
}

func validateResolvePayload(in ResolveInput) error {
	hasAnswer := in.Answer != ""
	if hasAnswer == in.RequiresHuman {
		return fmt.Errorf("%w: exactly one of --answer or --requires-human is required", ErrResolutionInvalid)
	}
	if len(in.Answer) > MaxAnswerLen {
		return fmt.Errorf("%w: --answer exceeds %d characters", ErrResolutionInvalid, MaxAnswerLen)
	}
	if hasAnswer && in.Certainty != "" && !in.Certainty.Valid() {
		return fmt.Errorf("%w: --certainty must be actual, inferred, or unknown", ErrResolutionInvalid)
	}
	if hasAnswer && in.Certainty == "" {
		return fmt.Errorf("%w: --certainty is required when --answer is set", ErrResolutionInvalid)
	}
	if len(in.ReasonSummary) > MaxReasonSummaryLen {
		return fmt.Errorf("%w: --reason exceeds %d characters", ErrResolutionInvalid, MaxReasonSummaryLen)
	}
	if len(in.EvidenceReferences) > MaxEvidenceReferences {
		return fmt.Errorf("%w: at most %d --evidence references are allowed", ErrResolutionInvalid, MaxEvidenceReferences)
	}
	for _, ref := range in.EvidenceReferences {
		if len(ref) > MaxEvidenceReferenceLen {
			return fmt.Errorf("%w: an --evidence reference exceeds %d characters", ErrResolutionInvalid, MaxEvidenceReferenceLen)
		}
	}
	return nil
}

func resolveResultMatches(resolution domain.WorkflowQuestionResolution, in ResolveInput) bool {
	if resolution.Answer != in.Answer || resolution.ReasonSummary != in.ReasonSummary || resolution.RequiresHuman != in.RequiresHuman {
		return false
	}
	existingCertainty := domain.QuestionCertainty("")
	if resolution.Certainty != nil {
		existingCertainty = *resolution.Certainty
	}
	if existingCertainty != in.Certainty {
		return false
	}
	if len(resolution.EvidenceReferences) != len(in.EvidenceReferences) {
		return false
	}
	for i, ref := range resolution.EvidenceReferences {
		if ref != in.EvidenceReferences[i] {
			return false
		}
	}
	return true
}

func certaintyPtr(c domain.QuestionCertainty) *domain.QuestionCertainty {
	if c == "" {
		return nil
	}
	return &c
}
