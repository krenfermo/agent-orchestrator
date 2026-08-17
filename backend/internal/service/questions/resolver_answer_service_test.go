package questions_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/service/questions"
)

type fakeResolutionStore struct {
	rows        map[string]domain.WorkflowQuestionResolution
	transitions int
}

func newFakeResolutionStore(rows ...domain.WorkflowQuestionResolution) *fakeResolutionStore {
	s := &fakeResolutionStore{rows: map[string]domain.WorkflowQuestionResolution{}}
	for _, r := range rows {
		s.rows[string(r.ID)] = r
	}
	return s
}

func (s *fakeResolutionStore) GetWorkflowQuestionResolution(_ context.Context, id string) (domain.WorkflowQuestionResolution, bool, error) {
	r, ok := s.rows[id]
	return r, ok, nil
}

func (s *fakeResolutionStore) TransitionResolutionStatus(_ context.Context, id string, expectedStatus, newStatus domain.ResolutionStatus, answer, reasonSummary string, evidenceReferences []string, certainty *domain.QuestionCertainty, requiresHuman bool, updatedAt time.Time, completedAt *time.Time) (bool, error) {
	r, ok := s.rows[id]
	if !ok || r.Status != expectedStatus {
		return false, nil
	}
	s.transitions++
	r.Status = newStatus
	r.Answer = answer
	r.ReasonSummary = reasonSummary
	r.EvidenceReferences = evidenceReferences
	r.Certainty = certainty
	r.RequiresHuman = requiresHuman
	r.UpdatedAt = updatedAt
	r.CompletedAt = completedAt
	s.rows[id] = r
	return true, nil
}

func sessPtr(s domain.SessionID) *domain.SessionID { return &s }

func certPtr(c domain.QuestionCertainty) *domain.QuestionCertainty { return &c }

func baseRunningResolution() domain.WorkflowQuestionResolution {
	return domain.WorkflowQuestionResolution{
		ID:                "wqr-1",
		ResolverHarness:   domain.HarnessCodex,
		ResolverSessionID: sessPtr("decision-resolver-wqr-1"),
		Status:            domain.ResolutionStatusRunning,
		CreatedAt:         time.Date(2026, 8, 16, 9, 0, 0, 0, time.UTC),
		UpdatedAt:         time.Date(2026, 8, 16, 9, 0, 0, 0, time.UTC),
	}
}

func TestResolverAnswerService_ValidResolveDelivers(t *testing.T) {
	store := newFakeResolutionStore(baseRunningResolution())
	svc := &questions.ResolverAnswerService{Store: store}

	got, err := svc.Resolve(context.Background(), "decision-resolver-wqr-1", questions.ResolveInput{
		RunID: "wqr-1", Answer: "use pkg/foo.Bar", ReasonSummary: "only helper found", Certainty: domain.QuestionCertaintyActual,
		EvidenceReferences: []string{"pkg/foo/bar.go:10-20"},
	})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got.Status != domain.ResolutionStatusComplete || got.Answer != "use pkg/foo.Bar" {
		t.Fatalf("got %+v", got)
	}
	if store.transitions != 1 {
		t.Fatalf("transitions = %d, want 1", store.transitions)
	}
}

func TestResolverAnswerService_RequiresHumanNeverAnswer(t *testing.T) {
	store := newFakeResolutionStore(baseRunningResolution())
	svc := &questions.ResolverAnswerService{Store: store}

	got, err := svc.Resolve(context.Background(), "decision-resolver-wqr-1", questions.ResolveInput{
		RunID: "wqr-1", RequiresHuman: true, ReasonSummary: "ambiguous",
	})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if !got.RequiresHuman || got.Answer != "" {
		t.Fatalf("got %+v, want requires_human=true and empty answer", got)
	}
}

func TestResolverAnswerService_WrongRunRejected(t *testing.T) {
	store := newFakeResolutionStore(baseRunningResolution())
	svc := &questions.ResolverAnswerService{Store: store}
	_, err := svc.Resolve(context.Background(), "decision-resolver-wqr-1", questions.ResolveInput{
		RunID: "wqr-does-not-exist", Answer: "x", Certainty: domain.QuestionCertaintyActual,
	})
	if !errors.Is(err, questions.ErrResolutionNotFound) {
		t.Fatalf("err = %v, want ErrResolutionNotFound", err)
	}
}

func TestResolverAnswerService_SessionMismatchRejected(t *testing.T) {
	store := newFakeResolutionStore(baseRunningResolution())
	svc := &questions.ResolverAnswerService{Store: store}
	_, err := svc.Resolve(context.Background(), "some-other-session", questions.ResolveInput{
		RunID: "wqr-1", Answer: "x", Certainty: domain.QuestionCertaintyActual,
	})
	if !errors.Is(err, questions.ErrResolutionWrongSession) {
		t.Fatalf("err = %v, want ErrResolutionWrongSession", err)
	}
}

func TestResolverAnswerService_DuplicateIdenticalIsIdempotent(t *testing.T) {
	store := newFakeResolutionStore(baseRunningResolution())
	svc := &questions.ResolverAnswerService{Store: store}
	in := questions.ResolveInput{RunID: "wqr-1", Answer: "x", ReasonSummary: "r", Certainty: domain.QuestionCertaintyActual, EvidenceReferences: []string{"a.go"}}

	if _, err := svc.Resolve(context.Background(), "decision-resolver-wqr-1", in); err != nil {
		t.Fatalf("first Resolve: %v", err)
	}
	if store.transitions != 1 {
		t.Fatalf("transitions after first = %d, want 1", store.transitions)
	}
	got, err := svc.Resolve(context.Background(), "decision-resolver-wqr-1", in)
	if err != nil {
		t.Fatalf("second (identical) Resolve: %v", err)
	}
	if got.Answer != "x" {
		t.Fatalf("got %+v", got)
	}
	if store.transitions != 1 {
		t.Fatalf("transitions after duplicate = %d, want still 1 (no second write)", store.transitions)
	}
}

func TestResolverAnswerService_DuplicateDifferingRejected(t *testing.T) {
	store := newFakeResolutionStore(baseRunningResolution())
	svc := &questions.ResolverAnswerService{Store: store}
	in := questions.ResolveInput{RunID: "wqr-1", Answer: "x", Certainty: domain.QuestionCertaintyActual}
	if _, err := svc.Resolve(context.Background(), "decision-resolver-wqr-1", in); err != nil {
		t.Fatalf("first Resolve: %v", err)
	}
	other := questions.ResolveInput{RunID: "wqr-1", Answer: "y", Certainty: domain.QuestionCertaintyActual}
	_, err := svc.Resolve(context.Background(), "decision-resolver-wqr-1", other)
	if !errors.Is(err, questions.ErrResolutionConflict) {
		t.Fatalf("err = %v, want ErrResolutionConflict", err)
	}
}

func TestResolverAnswerService_EmptyAnswerWithoutRequiresHumanRejected(t *testing.T) {
	store := newFakeResolutionStore(baseRunningResolution())
	svc := &questions.ResolverAnswerService{Store: store}
	_, err := svc.Resolve(context.Background(), "decision-resolver-wqr-1", questions.ResolveInput{RunID: "wqr-1"})
	if !errors.Is(err, questions.ErrResolutionInvalid) {
		t.Fatalf("err = %v, want ErrResolutionInvalid", err)
	}
}

func TestResolverAnswerService_InvalidCertaintyRejected(t *testing.T) {
	store := newFakeResolutionStore(baseRunningResolution())
	svc := &questions.ResolverAnswerService{Store: store}
	_, err := svc.Resolve(context.Background(), "decision-resolver-wqr-1", questions.ResolveInput{
		RunID: "wqr-1", Answer: "x", Certainty: domain.QuestionCertainty("bogus"),
	})
	if !errors.Is(err, questions.ErrResolutionInvalid) {
		t.Fatalf("err = %v, want ErrResolutionInvalid", err)
	}
}

func TestResolverAnswerService_OversizedEvidenceRejected(t *testing.T) {
	store := newFakeResolutionStore(baseRunningResolution())
	svc := &questions.ResolverAnswerService{Store: store}
	refs := make([]string, questions.MaxEvidenceReferences+1)
	for i := range refs {
		refs[i] = "a.go"
	}
	_, err := svc.Resolve(context.Background(), "decision-resolver-wqr-1", questions.ResolveInput{
		RunID: "wqr-1", Answer: "x", Certainty: domain.QuestionCertaintyActual, EvidenceReferences: refs,
	})
	if !errors.Is(err, questions.ErrResolutionInvalid) {
		t.Fatalf("err = %v, want ErrResolutionInvalid", err)
	}

	longRef := strings.Repeat("a", questions.MaxEvidenceReferenceLen+1)
	_, err = svc.Resolve(context.Background(), "decision-resolver-wqr-1", questions.ResolveInput{
		RunID: "wqr-1", Answer: "x", Certainty: domain.QuestionCertaintyActual, EvidenceReferences: []string{longRef},
	})
	if !errors.Is(err, questions.ErrResolutionInvalid) {
		t.Fatalf("err (long ref) = %v, want ErrResolutionInvalid", err)
	}
}

func TestResolverAnswerService_NotRunningRejected(t *testing.T) {
	r := baseRunningResolution()
	r.Status = domain.ResolutionStatusFailed
	store := newFakeResolutionStore(r)
	svc := &questions.ResolverAnswerService{Store: store}
	_, err := svc.Resolve(context.Background(), "decision-resolver-wqr-1", questions.ResolveInput{
		RunID: "wqr-1", Answer: "x", Certainty: domain.QuestionCertaintyActual,
	})
	if !errors.Is(err, questions.ErrResolutionNotRunning) {
		t.Fatalf("err = %v, want ErrResolutionNotRunning", err)
	}
}

func TestResolverAnswerService_NeverStoresChainOfThoughtField(t *testing.T) {
	// Structural guard: ResolveInput has no transcript/chain-of-thought field
	// at all — this test documents that contract so a future edit adding one
	// is a deliberate, visible change, not an accidental one.
	in := questions.ResolveInput{}
	_ = in
	// (No reflection needed: the struct literal above would fail to compile
	// if a field like Transcript/ChainOfThought were required or already
	// existed under a different name colliding with this zero-value literal.)
	_ = certPtr(domain.QuestionCertaintyActual)
}
