package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/aoagents/agent-orchestrator/backend/internal/postrunqa"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/gen"
)

// Post-Run QA gate state (post_run_qa_runs, migration 0126).
//
// The gate stores its envelope here rather than in a file of its own, for the
// same reason every other lifecycle fact does: a daemon restart has to be able
// to answer "was this subject's gate still open, and where had it got to?"
// from the one database that already survives the restart.
//
// Unlike WorkflowWakeSchedule, this store defines no row struct of its own: it
// reads and writes postrunqa.QARun directly. The state model is already the
// narrow, documented shape the gate uses, and duplicating it here would give
// the same envelope two definitions to keep in sync for no gain.

// SaveQARun inserts one Post-Run QA gate pass, or updates it in place when a
// run with the same ID already exists -- advancing a live pass is the common
// case (pending -> checking -> auto_fixing -> verdict), and each step rewrites
// the same row. Defaults are applied before the write, so a caller that left
// MaxRepairCycles at zero still gets a stored budget of
// postrunqa.DefaultMaxRepairCycles rather than a pass that can never repair
// anything.
func (s *Store) SaveQARun(ctx context.Context, run postrunqa.QARun) (postrunqa.QARun, error) {
	run = run.WithDefaults()
	if err := run.Validate(); err != nil {
		return postrunqa.QARun{}, err
	}

	findingsJSON, err := marshalQAFindings(run.Findings)
	if err != nil {
		return postrunqa.QARun{}, fmt.Errorf("marshal post-run qa findings: %w", err)
	}

	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	row, err := s.qw.UpsertPostRunQARun(ctx, gen.UpsertPostRunQARunParams{
		ID:               run.ID,
		SubjectKind:      string(run.SubjectKind),
		SubjectID:        run.SubjectID,
		Phase:            string(run.Phase),
		FindingsJson:     findingsJSON,
		RepairCycleCount: int64(run.RepairCycleCount),
		MaxRepairCycles:  int64(run.MaxRepairCycles),
		Result:           string(run.Result),
		StartedAt:        run.StartedAt,
		CompletedAt:      timePtrToNullTime(run.CompletedAt),
	})
	if err != nil {
		return postrunqa.QARun{}, fmt.Errorf("save post-run qa run %s: %w", run.ID, err)
	}
	return qaRunFromRow(row)
}

// LoadQARun reads one gate pass by ID. ok is false, with no error, when no such
// pass exists.
func (s *Store) LoadQARun(ctx context.Context, id string) (postrunqa.QARun, bool, error) {
	row, err := s.qr.GetPostRunQARun(ctx, id)
	if errors.Is(err, sql.ErrNoRows) {
		return postrunqa.QARun{}, false, nil
	}
	if err != nil {
		return postrunqa.QARun{}, false, fmt.Errorf("load post-run qa run %s: %w", id, err)
	}
	out, derr := qaRunFromRow(row)
	if derr != nil {
		return postrunqa.QARun{}, false, derr
	}
	return out, true, nil
}

// LatestQARunForSubject reads the most recently started gate pass for one
// subject. ok is false, with no error, when the subject has never entered the
// gate.
func (s *Store) LatestQARunForSubject(ctx context.Context, kind postrunqa.SubjectKind, subjectID string) (postrunqa.QARun, bool, error) {
	row, err := s.qr.GetLatestPostRunQARunForSubject(ctx, gen.GetLatestPostRunQARunForSubjectParams{
		SubjectKind: string(kind),
		SubjectID:   subjectID,
	})
	if errors.Is(err, sql.ErrNoRows) {
		return postrunqa.QARun{}, false, nil
	}
	if err != nil {
		return postrunqa.QARun{}, false, fmt.Errorf("load latest post-run qa run for %s %s: %w", kind, subjectID, err)
	}
	out, derr := qaRunFromRow(row)
	if derr != nil {
		return postrunqa.QARun{}, false, derr
	}
	return out, true, nil
}

// marshalQAFindings always produces a JSON array, never SQL NULL: the column is
// NOT NULL, and both "nothing checked yet" and "checked, found nothing" are the
// empty list.
func marshalQAFindings(findings []postrunqa.Finding) (string, error) {
	if len(findings) == 0 {
		return "[]", nil
	}
	b, err := json.Marshal(findings)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

func qaRunFromRow(r gen.PostRunQaRun) (postrunqa.QARun, error) {
	var findings []postrunqa.Finding
	if r.FindingsJson != "" {
		if err := json.Unmarshal([]byte(r.FindingsJson), &findings); err != nil {
			return postrunqa.QARun{}, fmt.Errorf("unmarshal findings for post-run qa run %s: %w", r.ID, err)
		}
	}
	// Defaults are re-applied on read as well as on write, so a row written by
	// a build that predates a default still loads as a usable envelope.
	return postrunqa.QARun{
		ID:               r.ID,
		SubjectKind:      postrunqa.SubjectKind(r.SubjectKind),
		SubjectID:        r.SubjectID,
		Phase:            postrunqa.QAPhase(r.Phase),
		Findings:         findings,
		RepairCycleCount: int(r.RepairCycleCount),
		MaxRepairCycles:  int(r.MaxRepairCycles),
		Result:           postrunqa.QAResult(r.Result),
		StartedAt:        r.StartedAt,
		CompletedAt:      nullTimeToTimePtr(r.CompletedAt),
	}.WithDefaults(), nil
}
