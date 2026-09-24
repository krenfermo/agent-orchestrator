package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/gen"
)

// skill_run_store.go is the durable half of a skill run (migration 0172). The
// state machine and every decision about it live in service/skills/runs.go;
// this layer reads and writes rows, and every transition is a compare-and-set
// on the state the caller believes the row is in, so it reports whether it won.

// SkillRunState is where a run is in its life. See migration 0172 for the
// meaning of each terminal state.
type SkillRunState string

// The run states.
const (
	SkillRunQueued    SkillRunState = "queued"
	SkillRunRunning   SkillRunState = "running"
	SkillRunSucceeded SkillRunState = "succeeded"
	// SkillRunPartial is an audit (migration 0173) that consolidated a report
	// while not every mode it planned produced a verified result. It carries a
	// report and a reason, and it is never the same thing as succeeded.
	SkillRunPartial   SkillRunState = "partial"
	SkillRunFailed    SkillRunState = "failed"
	SkillRunRefused   SkillRunState = "refused"
	SkillRunCancelled SkillRunState = "cancelled"
)

// Terminal reports whether no further transition is possible.
func (s SkillRunState) Terminal() bool {
	switch s {
	case SkillRunSucceeded, SkillRunPartial, SkillRunFailed, SkillRunRefused, SkillRunCancelled:
		return true
	}
	return false
}

// SkillRunRecord is one run as persisted. The report is carried as the exact
// bytes stored, so a digest check is over what is on disk and not over a
// re-encoding of it.
type SkillRunRecord struct {
	ID               string
	ProjectID        domain.ProjectID
	SkillID          string
	SkillVersion     string
	ModeID           string
	Tool             string
	State            SkillRunState
	IdempotencyKey   string
	RequestedBy      string
	InputsJSON       string
	CapabilitiesJSON string
	PackageDigest    string
	RunnerID         string
	RunnerControls   string
	OwnerInstance    string
	ImageDigest      string
	ApprovalID       string
	ApprovedBy       string
	Summary          string
	FindingCount     int
	Truncated        bool
	ReportJSON       []byte
	ReportSHA256     string
	ErrorCode        string
	ErrorMessage     string
	CancelRequested  bool
	CreatedAt        time.Time
	StartedAt        *time.Time
	FinishedAt       *time.Time
	UpdatedAt        time.Time
	// ParentRunID is the audit this run is a child of (migration 0173); empty
	// for a run that is not part of one.
	ParentRunID string
}

// SkillRunFindingRecord is one persisted finding.
type SkillRunFindingRecord struct {
	RunID          string
	Ordinal        int
	RuleID         string
	Severity       string
	Category       string
	Title          string
	Path           string
	Line           int
	Recommendation string
	Confidence     string
}

// SkillRunImage is what the runner reported about the image it resolved.
type SkillRunImage struct {
	Digest     string
	ApprovalID string
	ApprovedBy string
}

// SkillRunSuccess is everything a successful run finishes with, written in one
// transaction so a reader can never see a succeeded run without its findings.
type SkillRunSuccess struct {
	Image        SkillRunImage
	Summary      string
	Truncated    bool
	ReportJSON   []byte
	ReportSHA256 string
	Findings     []SkillRunFindingRecord
	FinishedAt   time.Time
}

// SkillRunFailure is how a run ends without a report.
type SkillRunFailure struct {
	State        SkillRunState
	Image        SkillRunImage
	Summary      string
	ErrorCode    string
	ErrorMessage string
	FinishedAt   time.Time
}

// CreateSkillRun inserts a queued run, or returns the run a previous request
// already created.
//
// Two rules decide "already created", and both are also enforced by unique
// indexes in the schema, so a race between two requests cannot produce two
// rows:
//
//   - the same idempotency key on the same project returns that run, whatever
//     state it is in;
//   - an in-flight (queued or running) run for the same project, skill and mode
//     is returned instead of a second one.
//
// created is true only when this call inserted the row.
func (s *Store) CreateSkillRun(ctx context.Context, rec SkillRunRecord) (SkillRunRecord, bool, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	var (
		out     SkillRunRecord
		created bool
	)
	err := s.inTx(ctx, "create skill run", func(q *gen.Queries) error {
		if rec.IdempotencyKey != "" {
			row, err := q.GetSkillRunByIdempotencyKey(ctx, gen.GetSkillRunByIdempotencyKeyParams{
				ProjectID:      rec.ProjectID,
				IdempotencyKey: sql.NullString{String: rec.IdempotencyKey, Valid: true},
			})
			switch {
			case err == nil:
				out = skillRunFromRow(row)
				return nil
			case !errors.Is(err, sql.ErrNoRows):
				return err
			}
		}
		row, err := q.GetActiveSkillRun(ctx, gen.GetActiveSkillRunParams{
			ProjectID: rec.ProjectID, SkillID: rec.SkillID, ModeID: rec.ModeID,
		})
		switch {
		case err == nil:
			out = skillRunFromRow(row)
			return nil
		case !errors.Is(err, sql.ErrNoRows):
			return err
		}
		var key sql.NullString
		if rec.IdempotencyKey != "" {
			key = sql.NullString{String: rec.IdempotencyKey, Valid: true}
		}
		if err := q.InsertSkillRun(ctx, gen.InsertSkillRunParams{
			ID:               rec.ID,
			ProjectID:        rec.ProjectID,
			SkillID:          rec.SkillID,
			SkillVersion:     rec.SkillVersion,
			ModeID:           rec.ModeID,
			Tool:             rec.Tool,
			IdempotencyKey:   key,
			RequestedBy:      rec.RequestedBy,
			InputsJson:       orDefault(rec.InputsJSON, "{}"),
			CapabilitiesJson: orDefault(rec.CapabilitiesJSON, "[]"),
			PackageDigest:    rec.PackageDigest,
			RunnerID:         rec.RunnerID,
			RunnerControls:   orDefault(rec.RunnerControls, "[]"),
			OwnerInstance:    rec.OwnerInstance,
			CreatedAt:        rec.CreatedAt,
			UpdatedAt:        rec.CreatedAt,
			ParentRunID:      sql.NullString{String: rec.ParentRunID, Valid: rec.ParentRunID != ""},
		}); err != nil {
			return err
		}
		row, err = q.GetSkillRun(ctx, rec.ID)
		if err != nil {
			return err
		}
		out, created = skillRunFromRow(row), true
		return nil
	})
	if err != nil {
		return SkillRunRecord{}, false, fmt.Errorf("create skill run: %w", err)
	}
	return out, created, nil
}

// GetSkillRun returns one run by id, or (zero, false, nil).
func (s *Store) GetSkillRun(ctx context.Context, id string) (SkillRunRecord, bool, error) {
	row, err := s.qr.GetSkillRun(ctx, id)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return SkillRunRecord{}, false, nil
		}
		return SkillRunRecord{}, false, fmt.Errorf("get skill run: %w", err)
	}
	return skillRunFromRow(row), true, nil
}

// GetSkillRunForProject returns one run only if it belongs to the project. A run
// id from another project is "not found", never a different project's run.
func (s *Store) GetSkillRunForProject(ctx context.Context, projectID domain.ProjectID, id string) (SkillRunRecord, bool, error) {
	row, err := s.qr.GetSkillRunForProject(ctx, gen.GetSkillRunForProjectParams{ProjectID: projectID, ID: id})
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return SkillRunRecord{}, false, nil
		}
		return SkillRunRecord{}, false, fmt.Errorf("get project skill run: %w", err)
	}
	return skillRunFromRow(row), true, nil
}

// ListSkillRunsForProject returns a project's runs, newest first.
func (s *Store) ListSkillRunsForProject(ctx context.Context, projectID domain.ProjectID, limit int) ([]SkillRunRecord, error) {
	rows, err := s.qr.ListSkillRunsForProject(ctx, gen.ListSkillRunsForProjectParams{
		ProjectID: projectID, Limit: int64(limit),
	})
	if err != nil {
		return nil, fmt.Errorf("list project skill runs: %w", err)
	}
	out := make([]SkillRunRecord, 0, len(rows))
	for _, row := range rows {
		out = append(out, skillRunFromRow(row))
	}
	return out, nil
}

// ListActiveSkillRuns returns every queued or running run, installation-wide.
func (s *Store) ListActiveSkillRuns(ctx context.Context) ([]SkillRunRecord, error) {
	rows, err := s.qr.ListActiveSkillRuns(ctx)
	if err != nil {
		return nil, fmt.Errorf("list active skill runs: %w", err)
	}
	out := make([]SkillRunRecord, 0, len(rows))
	for _, row := range rows {
		out = append(out, skillRunFromRow(row))
	}
	return out, nil
}

// MarkSkillRunRunning moves a queued run to running. It loses (false) when the
// run is no longer queued or a cancellation was requested first.
func (s *Store) MarkSkillRunRunning(ctx context.Context, id string, at time.Time) (bool, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	n, err := s.qw.MarkSkillRunRunning(ctx, gen.MarkSkillRunRunningParams{
		StartedAt: sql.NullTime{Time: at, Valid: true}, ID: id,
	})
	if err != nil {
		return false, fmt.Errorf("mark skill run running: %w", err)
	}
	return n == 1, nil
}

// FinishSkillRunSucceeded moves a running run to succeeded and records its
// findings, in one transaction. It loses (false) when the run is not running.
func (s *Store) FinishSkillRunSucceeded(ctx context.Context, id string, fin SkillRunSuccess) (bool, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	won := false
	err := s.inTx(ctx, "finish skill run", func(q *gen.Queries) error {
		n, err := q.FinishSkillRunSucceeded(ctx, gen.FinishSkillRunSucceededParams{
			ImageDigest:  fin.Image.Digest,
			ApprovalID:   fin.Image.ApprovalID,
			ApprovedBy:   fin.Image.ApprovedBy,
			Summary:      fin.Summary,
			FindingCount: int64(len(fin.Findings)),
			Truncated:    boolToInt64(fin.Truncated),
			ReportJson:   sql.NullString{String: string(fin.ReportJSON), Valid: true},
			ReportSha256: fin.ReportSHA256,
			FinishedAt:   sql.NullTime{Time: fin.FinishedAt, Valid: true},
			ID:           id,
		})
		if err != nil {
			return err
		}
		if n != 1 {
			return nil
		}
		for i, f := range fin.Findings {
			if err := q.InsertSkillRunFinding(ctx, gen.InsertSkillRunFindingParams{
				RunID: id, Ordinal: int64(i), RuleID: f.RuleID, Severity: f.Severity,
				Category: f.Category, Title: f.Title, Path: f.Path, Line: int64(f.Line),
				Recommendation: f.Recommendation, Confidence: f.Confidence,
			}); err != nil {
				return err
			}
		}
		won = true
		return nil
	})
	if err != nil {
		return false, fmt.Errorf("finish skill run: %w", err)
	}
	return won, nil
}

// SkillRunPartialResult is how an audit ends with a report that does not cover
// every mode it planned: the report, its findings and the reason, in one
// transaction.
type SkillRunPartialResult struct {
	Summary      string
	ReportJSON   []byte
	ReportSHA256 string
	Findings     []SkillRunFindingRecord
	ErrorCode    string
	ErrorMessage string
	FinishedAt   time.Time
}

// FinishSkillRunPartial moves a running audit to partial and records its
// consolidated findings, in one transaction. It loses (false) when the run is
// not running.
func (s *Store) FinishSkillRunPartial(ctx context.Context, id string, fin SkillRunPartialResult) (bool, error) {
	if strings.TrimSpace(fin.ErrorCode) == "" {
		return false, fmt.Errorf("finish skill run partial: a partial run must say why")
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	won := false
	err := s.inTx(ctx, "finish skill run partial", func(q *gen.Queries) error {
		n, err := q.FinishSkillRunPartial(ctx, gen.FinishSkillRunPartialParams{
			Summary:      fin.Summary,
			FindingCount: int64(len(fin.Findings)),
			ReportJson:   sql.NullString{String: string(fin.ReportJSON), Valid: true},
			ReportSha256: fin.ReportSHA256,
			ErrorCode:    fin.ErrorCode,
			ErrorMessage: fin.ErrorMessage,
			FinishedAt:   sql.NullTime{Time: fin.FinishedAt, Valid: true},
			ID:           id,
		})
		if err != nil {
			return err
		}
		if n != 1 {
			return nil
		}
		for i, f := range fin.Findings {
			if err := q.InsertSkillRunFinding(ctx, gen.InsertSkillRunFindingParams{
				RunID: id, Ordinal: int64(i), RuleID: f.RuleID, Severity: f.Severity,
				Category: f.Category, Title: f.Title, Path: f.Path, Line: int64(f.Line),
				Recommendation: f.Recommendation, Confidence: f.Confidence,
			}); err != nil {
				return err
			}
		}
		won = true
		return nil
	})
	if err != nil {
		return false, fmt.Errorf("finish skill run partial: %w", err)
	}
	return won, nil
}

// ListSkillRunChildren returns the child runs of one audit, oldest first.
func (s *Store) ListSkillRunChildren(ctx context.Context, parentID string) ([]SkillRunRecord, error) {
	rows, err := s.qr.ListSkillRunChildren(ctx, sql.NullString{String: parentID, Valid: true})
	if err != nil {
		return nil, fmt.Errorf("list skill run children: %w", err)
	}
	out := make([]SkillRunRecord, 0, len(rows))
	for _, r := range rows {
		out = append(out, skillRunFromRow(r))
	}
	return out, nil
}

// FinishSkillRunUnsuccessful moves a queued or running run to failed, refused or
// cancelled. It loses (false) when the run is already terminal.
func (s *Store) FinishSkillRunUnsuccessful(ctx context.Context, id string, fin SkillRunFailure) (bool, error) {
	switch fin.State {
	case SkillRunFailed, SkillRunRefused, SkillRunCancelled:
	default:
		return false, fmt.Errorf("finish skill run: %q is not an unsuccessful terminal state", fin.State)
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	n, err := s.qw.FinishSkillRunUnsuccessful(ctx, gen.FinishSkillRunUnsuccessfulParams{
		NewState:     string(fin.State),
		ImageDigest:  fin.Image.Digest,
		ApprovalID:   fin.Image.ApprovalID,
		ApprovedBy:   fin.Image.ApprovedBy,
		Summary:      fin.Summary,
		ErrorCode:    fin.ErrorCode,
		ErrorMessage: fin.ErrorMessage,
		FinishedAt:   sql.NullTime{Time: fin.FinishedAt, Valid: true},
		ID:           id,
	})
	if err != nil {
		return false, fmt.Errorf("finish skill run: %w", err)
	}
	return n == 1, nil
}

// RequestSkillRunCancel records that a person asked a non-terminal run of this
// project to stop. It reports false when there was nothing to cancel.
func (s *Store) RequestSkillRunCancel(ctx context.Context, projectID domain.ProjectID, id string, at time.Time) (bool, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	n, err := s.qw.RequestSkillRunCancel(ctx, gen.RequestSkillRunCancelParams{
		UpdatedAt: at, ProjectID: projectID, ID: id,
	})
	if err != nil {
		return false, fmt.Errorf("request skill run cancel: %w", err)
	}
	return n == 1, nil
}

// ListSkillRunFindings returns a run's findings in report order.
func (s *Store) ListSkillRunFindings(ctx context.Context, runID string) ([]SkillRunFindingRecord, error) {
	rows, err := s.qr.ListSkillRunFindings(ctx, runID)
	if err != nil {
		return nil, fmt.Errorf("list skill run findings: %w", err)
	}
	out := make([]SkillRunFindingRecord, 0, len(rows))
	for _, r := range rows {
		out = append(out, SkillRunFindingRecord{
			RunID: r.RunID, Ordinal: int(r.Ordinal), RuleID: r.RuleID, Severity: r.Severity,
			Category: r.Category, Title: r.Title, Path: r.Path, Line: int(r.Line),
			Recommendation: r.Recommendation, Confidence: r.Confidence,
		})
	}
	return out, nil
}

func skillRunFromRow(r gen.SkillRun) SkillRunRecord {
	rec := SkillRunRecord{
		ID: r.ID, ProjectID: r.ProjectID, SkillID: r.SkillID, SkillVersion: r.SkillVersion,
		ModeID: r.ModeID, Tool: r.Tool, State: SkillRunState(r.State),
		IdempotencyKey: r.IdempotencyKey.String, RequestedBy: r.RequestedBy,
		InputsJSON: r.InputsJson, CapabilitiesJSON: r.CapabilitiesJson,
		PackageDigest: r.PackageDigest, RunnerID: r.RunnerID, RunnerControls: r.RunnerControls,
		OwnerInstance: r.OwnerInstance, ImageDigest: r.ImageDigest, ApprovalID: r.ApprovalID,
		ApprovedBy: r.ApprovedBy, Summary: r.Summary, FindingCount: int(r.FindingCount),
		Truncated: r.Truncated == 1, ReportSHA256: r.ReportSha256,
		ErrorCode: r.ErrorCode, ErrorMessage: r.ErrorMessage,
		CancelRequested: r.CancelRequested == 1, CreatedAt: r.CreatedAt, UpdatedAt: r.UpdatedAt,
		ParentRunID: r.ParentRunID.String,
	}
	if r.ReportJson.Valid {
		rec.ReportJSON = []byte(r.ReportJson.String)
	}
	if r.StartedAt.Valid {
		t := r.StartedAt.Time
		rec.StartedAt = &t
	}
	if r.FinishedAt.Valid {
		t := r.FinishedAt.Time
		rec.FinishedAt = &t
	}
	return rec
}

func orDefault(v, def string) string {
	if strings.TrimSpace(v) == "" {
		return def
	}
	return v
}
