package skills

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/apierr"
	"github.com/aoagents/agent-orchestrator/backend/internal/skillimage"
	"github.com/aoagents/agent-orchestrator/backend/internal/skillrunner"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/store"
)

// runs.go — a skill run as a durable record (Frente 2 / 2B, migration 0172).
//
// # The contract
//
// A SkillRun exists if and only if AO ACCEPTED a request to execute: every
// check prepareRun makes (activation, mode, inputs, runner, authorization
// against AO's own attestation, an implemented mode, a stageable scope) passed.
// A request refused before that gets a 4xx and a run_refused audit row, exactly
// as the synchronous path always did, and no run -- there was never anything to
// execute, so there is nothing to track.
//
// States and transitions (store.SkillRunState; this file is the one writer):
//
//	queued  -> running    the executor claimed it (CAS; loses to a cancel)
//	queued  -> cancelled  a person cancelled it before it started
//	queued  -> failed     its daemon stopped before it started
//	running -> succeeded  the scan produced a report; report + findings + digest
//	                      are written in ONE transaction
//	running -> refused    the execution boundary declined to launch or to trust
//	                      the output (image not approved/revoked, staging
//	                      unusable, isolation not demonstrated)
//	running -> failed     execution produced no result AO can stand behind
//	                      (runtime error, timeout, invalid output), or its
//	                      daemon stopped while it ran
//	running -> cancelled  a person cancelled it and the container was stopped
//
// Terminal states have no outgoing transition, and every transition is a
// compare-and-set on the state the writer believes the row is in, so two
// writers can never both move one run.
//
// # Ownership and restarts
//
// A run is executed only by the daemon instance that accepted it
// (owner_instance). Nothing hands a run to another instance. A daemon that
// starts and finds a non-terminal run owned by a different instance knows that
// owner is gone and CANNOT know what happened after its last write: the scan
// may have finished and never been recorded. It does not guess. It removes the
// run's container and staged copy (both addressed by the run id alone, so no
// other run is touched) and ends the run failed with SKILL_RUN_INTERRUPTED,
// naming the state the run was last seen in. Rerunning is a person's decision.
//
// # Idempotency
//
// Two rules, both also enforced by unique indexes in the schema:
//
//   - the same idempotency key on the same project returns the run it created,
//     in whatever state it is now;
//   - while a run for (project, skill, mode) is queued or running, a second
//     request returns THAT run instead of starting a second container over the
//     same checkout. This is what makes a double click harmless.
//
// # Integrity
//
// A succeeded run stores the report as exact bytes with their SHA-256. Reading
// a run re-hashes those bytes; a mismatch is reported as
// integrity "mismatch", never silently served as the run's result.

// Run error codes recorded on a failed/refused/cancelled run.
const (
	RunErrInterrupted = "SKILL_RUN_INTERRUPTED"
	RunErrCancelled   = "SKILL_RUN_CANCELLED"
	RunErrShutdown    = "SKILL_RUN_DAEMON_SHUTDOWN"
	RunErrInvalid     = "SKILL_RUN_OUTPUT_INVALID"
)

// DefaultRunListLimit bounds a history listing.
const DefaultRunListLimit = 50

// RunStore is the durable half of runs. *store.Store implements it.
type RunStore interface {
	CreateSkillRun(ctx context.Context, rec store.SkillRunRecord) (store.SkillRunRecord, bool, error)
	GetSkillRun(ctx context.Context, id string) (store.SkillRunRecord, bool, error)
	GetSkillRunForProject(ctx context.Context, projectID domain.ProjectID, id string) (store.SkillRunRecord, bool, error)
	ListSkillRunsForProject(ctx context.Context, projectID domain.ProjectID, limit int) ([]store.SkillRunRecord, error)
	ListActiveSkillRuns(ctx context.Context) ([]store.SkillRunRecord, error)
	MarkSkillRunRunning(ctx context.Context, id string, at time.Time) (bool, error)
	FinishSkillRunSucceeded(ctx context.Context, id string, fin store.SkillRunSuccess) (bool, error)
	FinishSkillRunUnsuccessful(ctx context.Context, id string, fin store.SkillRunFailure) (bool, error)
	RequestSkillRunCancel(ctx context.Context, projectID domain.ProjectID, id string, at time.Time) (bool, error)
	ListSkillRunFindings(ctx context.Context, runID string) ([]store.SkillRunFindingRecord, error)
}

// RunReaper removes what a run left behind when its daemon died. The real
// runner implements it; nil means the installation has nothing to reap.
type RunReaper interface {
	ReapRun(ctx context.Context, runID, projectPath, stagingOverride string) (skillrunner.ReapReport, error)
}

// runEngine is the durable-run state held by a Service.
type runEngine struct {
	store  RunStore
	owner  string
	reaper RunReaper
	log    *slog.Logger

	// base is cancelled when the daemon shuts down; every executor derives from
	// it, never from the HTTP request that started the run.
	base     context.Context
	stopBase context.CancelFunc
	mu       sync.Mutex
	inflight map[string]*inflightRun
	wg       sync.WaitGroup
	closed   bool
}

type inflightRun struct {
	cancel          context.CancelFunc
	cancelRequested bool
}

// WithDurableRuns enables durable, asynchronous runs owned by the daemon
// instance owner. Without it StartRun refuses, and only the synchronous
// RunSkill exists -- which is what every installation had before 2B.
func WithDurableRuns(st RunStore, owner string, reaper RunReaper, log *slog.Logger) Option {
	return func(s *Service) {
		if st == nil || strings.TrimSpace(owner) == "" {
			return
		}
		if log == nil {
			log = slog.Default()
		}
		base, stop := context.WithCancel(context.Background())
		s.runs = &runEngine{
			store: st, owner: owner, reaper: reaper, log: log,
			base: base, stopBase: stop, inflight: map[string]*inflightRun{},
		}
	}
}

// StartRunRequest is a RunRequest plus the caller's optional idempotency key.
type StartRunRequest struct {
	RunRequest
	IdempotencyKey string
}

// SkillRun is a run as the API reports it.
type SkillRun struct {
	store.SkillRunRecord
	Inputs       map[string]string
	Capabilities []string
	Controls     []string
}

// SkillRunDetail adds what only a single-run read carries.
type SkillRunDetail struct {
	SkillRun
	Findings []store.SkillRunFindingRecord
	// Report is the stored report, decoded, when the run succeeded and its
	// bytes still hash to the recorded digest.
	Report *skillrunner.StaticScanReport
	// Integrity is "verified", "mismatch", or "none" (no report to verify).
	Integrity string
}

var idempotencyKeyRe = func(k string) bool {
	if len(k) == 0 || len(k) > 128 {
		return false
	}
	for _, c := range k {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || strings.ContainsRune("-_.:", c)) {
			return false
		}
	}
	return true
}

// StartRun accepts a run and executes it asynchronously. created reports
// whether this call created the run (false: an idempotent or in-flight match).
func (s *Service) StartRun(ctx context.Context, req StartRunRequest) (SkillRun, bool, error) {
	if req.IdempotencyKey != "" && !idempotencyKeyRe(req.IdempotencyKey) {
		return SkillRun{}, false, apierr.Invalid("SKILL_RUN_IDEMPOTENCY_KEY_INVALID",
			"idempotencyKey must be 1-128 characters of [A-Za-z0-9-_.:]", nil)
	}
	// Every refusal prepareRun can make comes first, so the answer names the
	// real problem (not enabled, no runner, capability not granted) rather than
	// a property of this installation that would not have helped.
	prep, err := s.prepareRun(ctx, req.RunRequest)
	if err != nil {
		return SkillRun{}, false, err
	}
	if s.runs == nil {
		return SkillRun{}, false, apierr.Conflict("SKILL_RUNS_UNAVAILABLE",
			"this installation does not record skill runs", nil)
	}
	s.runs.mu.Lock()
	closed := s.runs.closed
	s.runs.mu.Unlock()
	if closed {
		return SkillRun{}, false, apierr.Conflict("SKILL_RUNS_SHUTTING_DOWN",
			"the daemon is shutting down and accepts no new runs", nil)
	}
	inputsJSON, _ := json.Marshal(prep.inputs)
	caps := make([]string, 0, len(prep.plan.Decision.Granted))
	for _, c := range prep.plan.Decision.Granted {
		caps = append(caps, string(c))
	}
	sort.Strings(caps)
	capsJSON, _ := json.Marshal(caps)
	controls := make([]string, 0, len(prep.plan.Runner.Controls))
	for _, c := range prep.plan.Runner.Controls {
		controls = append(controls, string(c))
	}
	sort.Strings(controls)
	controlsJSON, _ := json.Marshal(controls)

	now := s.now()
	rec, created, err := s.runs.store.CreateSkillRun(ctx, store.SkillRunRecord{
		ID:               newRunID(),
		ProjectID:        req.ProjectID,
		SkillID:          prep.resolved.Package.Manifest.ID,
		SkillVersion:     prep.scope.Version,
		ModeID:           prep.mode.ID,
		Tool:             string(prep.tool),
		IdempotencyKey:   req.IdempotencyKey,
		RequestedBy:      req.Actor,
		InputsJSON:       string(inputsJSON),
		CapabilitiesJSON: string(capsJSON),
		PackageDigest:    prep.resolved.Package.Digest,
		RunnerID:         prep.plan.Runner.RunnerID,
		RunnerControls:   string(controlsJSON),
		OwnerInstance:    s.runs.owner,
		CreatedAt:        now,
	})
	if err != nil {
		return SkillRun{}, false, err
	}
	if created {
		s.dispatch(rec.ID, prep)
	}
	return toSkillRun(rec), created, nil
}

// dispatch starts the executor for a freshly created run.
func (s *Service) dispatch(runID string, prep preparedRun) {
	e := s.runs
	ctx, cancel := context.WithCancel(e.base)
	e.mu.Lock()
	e.inflight[runID] = &inflightRun{cancel: cancel}
	e.wg.Add(1)
	e.mu.Unlock()
	go func() {
		defer e.wg.Done()
		defer func() {
			cancel()
			e.mu.Lock()
			delete(e.inflight, runID)
			e.mu.Unlock()
		}()
		s.execute(ctx, runID, prep)
	}()
}

// execute runs one accepted run to a terminal state.
func (s *Service) execute(ctx context.Context, runID string, prep preparedRun) {
	e := s.runs
	// Every write below uses a context that survives cancellation of ctx: the
	// terminal transition is exactly what must still be recorded when the run
	// is being cancelled.
	wctx := context.WithoutCancel(ctx)

	won, err := e.store.MarkSkillRunRunning(wctx, runID, s.now())
	if err != nil {
		e.log.Error("skills: could not mark run running", "run", runID, "err", err)
		s.finishUnsuccessful(wctx, runID, store.SkillRunFailed, RunErrInterrupted,
			"AO could not record that the run started, so it did not start it", store.SkillRunImage{})
		return
	}
	if !won {
		// Cancelled (or otherwise ended) before it started. A queued run that
		// had a cancel requested ends cancelled here; any other state is
		// already terminal and there is nothing to do.
		s.finishUnsuccessful(wctx, runID, store.SkillRunCancelled, RunErrCancelled,
			"cancelled before it started", store.SkillRunImage{})
		return
	}
	report, err := s.executeScan(ctx, prep, runID)
	if err != nil {
		image := store.SkillRunImage{}
		switch {
		case s.cancelWasRequested(runID):
			s.finishUnsuccessful(wctx, runID, store.SkillRunCancelled, RunErrCancelled,
				"cancelled while running; the container was stopped and removed", image)
		case e.base.Err() != nil:
			s.finishUnsuccessful(wctx, runID, store.SkillRunFailed, RunErrShutdown,
				"the daemon shut down while the run was executing; the container was stopped and removed", image)
		default:
			state, code := classifyExecutionError(err)
			if state == store.SkillRunRefused {
				s.recordRun(wctx, store.SkillAuditRunRefused, prep.req, prep.scope.Version, prep.mode.ID, "", err.Error())
			}
			s.finishUnsuccessful(wctx, runID, state, code, err.Error(), image)
		}
		return
	}
	if s.cancelWasRequested(runID) {
		// The scan finished in the same instant a person asked to stop it. The
		// person's decision wins: the result is not recorded as a success they
		// asked not to have.
		s.finishUnsuccessful(wctx, runID, store.SkillRunCancelled, RunErrCancelled,
			"cancelled while running; the scan completed but its result was discarded",
			store.SkillRunImage{Digest: report.ImageDigest, ApprovalID: report.ApprovalID, ApprovedBy: report.ApprovedBy})
		return
	}

	reportJSON, err := json.Marshal(report)
	if err != nil {
		s.finishUnsuccessful(wctx, runID, store.SkillRunFailed, RunErrInvalid,
			"the scan's report could not be encoded: "+err.Error(), store.SkillRunImage{})
		return
	}
	sum := sha256.Sum256(reportJSON)
	findings := make([]store.SkillRunFindingRecord, 0, len(report.Findings))
	for i, f := range report.Findings {
		findings = append(findings, store.SkillRunFindingRecord{
			RunID: runID, Ordinal: i, RuleID: f.RuleID, Severity: f.Severity, Category: f.Category,
			Title: f.Title, Path: f.Path, Line: f.Line, Recommendation: f.Recommendation,
			Confidence: f.Confidence,
		})
	}
	summary := fmt.Sprintf("%s scanned %d of %d staged files, %d findings",
		prep.tool, report.Coverage.FilesScanned, report.Coverage.FilesStaged, len(report.Findings))
	if report.Truncated {
		summary += " (output truncated: the finding list may be incomplete)"
	}
	if report.ApprovalRevokedDuringRun {
		summary += " (the image approval was revoked while the run executed)"
	}
	ok, err := e.store.FinishSkillRunSucceeded(wctx, runID, store.SkillRunSuccess{
		Image:        store.SkillRunImage{Digest: report.ImageDigest, ApprovalID: report.ApprovalID, ApprovedBy: report.ApprovedBy},
		Summary:      summary,
		Truncated:    report.Truncated,
		ReportJSON:   reportJSON,
		ReportSHA256: hex.EncodeToString(sum[:]),
		Findings:     findings,
		FinishedAt:   s.now(),
	})
	if err != nil {
		e.log.Error("skills: could not record run result", "run", runID, "err", err)
		s.finishUnsuccessful(wctx, runID, store.SkillRunFailed, RunErrInterrupted,
			"the scan completed but AO could not record its result", store.SkillRunImage{})
		return
	}
	if ok {
		s.recordExecuted(wctx, prep, report)
	}
}

func (s *Service) finishUnsuccessful(ctx context.Context, runID string, state store.SkillRunState,
	code, message string, image store.SkillRunImage,
) {
	if _, err := s.runs.store.FinishSkillRunUnsuccessful(ctx, runID, store.SkillRunFailure{
		State: state, Image: image, ErrorCode: code, ErrorMessage: message, FinishedAt: s.now(),
	}); err != nil {
		s.runs.log.Error("skills: could not record run end", "run", runID, "state", state, "err", err)
	}
}

func (s *Service) cancelWasRequested(runID string) bool {
	e := s.runs
	e.mu.Lock()
	defer e.mu.Unlock()
	if r, ok := e.inflight[runID]; ok {
		return r.cancelRequested
	}
	return false
}

// classifyExecutionError decides refused versus failed for an executor error.
// A refusal is the boundary saying no: nothing an operator should retry
// without changing something. Everything else is a failure to produce a result.
func classifyExecutionError(err error) (store.SkillRunState, string) {
	code := "SKILL_RUN_FAILED"
	var coded *apierr.Error
	if errors.As(executionError(err), &coded) {
		code = coded.Code
	}
	switch {
	case errors.Is(err, skillrunner.ErrImageNotApproved), errors.Is(err, skillimage.ErrNotApproved),
		errors.Is(err, skillrunner.ErrToolNotApproved), errors.Is(err, skillrunner.ErrStagingUnusable),
		errors.Is(err, skillrunner.ErrRuntimeUnavailable):
		return store.SkillRunRefused, code
	}
	// A lower layer (the trust root) may answer with an already-coded API
	// error rather than a sentinel. The code is the contract, so it decides.
	if refusalCodes[code] {
		return store.SkillRunRefused, code
	}
	return store.SkillRunFailed, code
}

// refusalCodes are the boundary's "no": each one is a policy or precondition
// an operator must change before a rerun can do anything different.
var refusalCodes = map[string]bool{
	"SKILL_IMAGE_NOT_APPROVED":  true,
	"SKILL_RUNTIME_UNAVAILABLE": true,
	"SKILL_STAGING_UNUSABLE":    true,
	"SKILL_TOOL_NOT_APPROVED":   true,
}

// CancelRun asks a queued or running run of this project to stop.
func (s *Service) CancelRun(ctx context.Context, projectID domain.ProjectID, runID string) (SkillRun, error) {
	if s.runs == nil {
		return SkillRun{}, apierr.Conflict("SKILL_RUNS_UNAVAILABLE", "this installation does not record skill runs", nil)
	}
	e := s.runs
	rec, ok, err := e.store.GetSkillRunForProject(ctx, projectID, runID)
	if err != nil {
		return SkillRun{}, err
	}
	if !ok {
		return SkillRun{}, apierr.NotFound("SKILL_RUN_NOT_FOUND", fmt.Sprintf("no skill run %q in project %q", runID, projectID))
	}
	if rec.State.Terminal() {
		return SkillRun{}, apierr.Conflict("SKILL_RUN_ALREADY_TERMINAL",
			fmt.Sprintf("run %s already ended %s", runID, rec.State), nil)
	}
	if _, err := e.store.RequestSkillRunCancel(ctx, projectID, runID, s.now()); err != nil {
		return SkillRun{}, err
	}
	e.mu.Lock()
	in, local := e.inflight[runID]
	if local {
		in.cancelRequested = true
		in.cancel()
	}
	e.mu.Unlock()
	if !local && rec.State == store.SkillRunQueued {
		// Not claimed by any executor in this process: it can only have been
		// left queued, and ends cancelled now.
		s.finishUnsuccessful(context.WithoutCancel(ctx), runID, store.SkillRunCancelled, RunErrCancelled,
			"cancelled before it started", store.SkillRunImage{})
	}
	rec, _, err = e.store.GetSkillRunForProject(ctx, projectID, runID)
	if err != nil {
		return SkillRun{}, err
	}
	return toSkillRun(rec), nil
}

// ListRuns returns a project's run history, newest first.
func (s *Service) ListRuns(ctx context.Context, projectID domain.ProjectID, limit int) ([]SkillRun, error) {
	if s.runs == nil {
		return []SkillRun{}, nil
	}
	if limit <= 0 || limit > 500 {
		limit = DefaultRunListLimit
	}
	recs, err := s.runs.store.ListSkillRunsForProject(ctx, projectID, limit)
	if err != nil {
		return nil, err
	}
	out := make([]SkillRun, 0, len(recs))
	for _, r := range recs {
		out = append(out, toSkillRun(r))
	}
	return out, nil
}

// GetRun returns one run of this project, with its findings and verified report.
func (s *Service) GetRun(ctx context.Context, projectID domain.ProjectID, runID string) (SkillRunDetail, error) {
	if s.runs == nil {
		return SkillRunDetail{}, apierr.NotFound("SKILL_RUN_NOT_FOUND", fmt.Sprintf("no skill run %q", runID))
	}
	rec, ok, err := s.runs.store.GetSkillRunForProject(ctx, projectID, runID)
	if err != nil {
		return SkillRunDetail{}, err
	}
	if !ok {
		return SkillRunDetail{}, apierr.NotFound("SKILL_RUN_NOT_FOUND",
			fmt.Sprintf("no skill run %q in project %q", runID, projectID))
	}
	findings, err := s.runs.store.ListSkillRunFindings(ctx, runID)
	if err != nil {
		return SkillRunDetail{}, err
	}
	detail := SkillRunDetail{SkillRun: toSkillRun(rec), Findings: findings, Integrity: "none"}
	if rec.ReportJSON != nil {
		sum := sha256.Sum256(rec.ReportJSON)
		if hex.EncodeToString(sum[:]) != rec.ReportSHA256 {
			detail.Integrity = "mismatch"
		} else {
			var report skillrunner.StaticScanReport
			if err := json.Unmarshal(rec.ReportJSON, &report); err != nil {
				detail.Integrity = "mismatch"
			} else {
				detail.Integrity = "verified"
				detail.Report = &report
			}
		}
	}
	return detail, nil
}

// ReconcileRuns accounts for runs a previous daemon instance left non-terminal.
// It is called once at boot, before the daemon serves requests.
func (s *Service) ReconcileRuns(ctx context.Context) (int, error) {
	if s.runs == nil {
		return 0, nil
	}
	e := s.runs
	active, err := e.store.ListActiveSkillRuns(ctx)
	if err != nil {
		return 0, err
	}
	ended := 0
	for _, rec := range active {
		if rec.OwnerInstance == e.owner {
			continue
		}
		projectPath := ""
		if p, ok, perr := s.project(ctx, rec.ProjectID); perr == nil && ok {
			projectPath = p.Path
		}
		if e.reaper != nil && skillrunner.ValidRunID(rec.ID) {
			rep, rerr := e.reaper.ReapRun(ctx, rec.ID, projectPath, s.stagingRoot)
			if rerr != nil {
				e.log.Warn("skills: could not reap an interrupted run's leftovers", "run", rec.ID, "err", rerr)
			} else if len(rep.ContainersRemoved) > 0 || rep.StagingRemoved != "" {
				e.log.Info("skills: removed an interrupted run's leftovers", "run", rec.ID,
					"containers", len(rep.ContainersRemoved), "staging", rep.StagingRemoved != "")
			}
		}
		msg := fmt.Sprintf("the daemon that owned this run (%s) stopped while it was %s; "+
			"AO cannot prove what happened after that, so the run is not reported as complete",
			rec.OwnerInstance, rec.State)
		won, ferr := e.store.FinishSkillRunUnsuccessful(ctx, rec.ID, store.SkillRunFailure{
			State: store.SkillRunFailed, ErrorCode: RunErrInterrupted, ErrorMessage: msg, FinishedAt: s.now(),
		})
		if ferr != nil {
			return ended, ferr
		}
		if won {
			ended++
		}
	}
	return ended, nil
}

// CloseRuns stops accepting runs, cancels every in-flight executor (each
// records its own terminal state) and waits for them, up to timeout.
func (s *Service) CloseRuns(timeout time.Duration) {
	if s.runs == nil {
		return
	}
	e := s.runs
	e.mu.Lock()
	e.closed = true
	e.mu.Unlock()
	e.stopBase()
	done := make(chan struct{})
	go func() { e.wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(timeout):
		e.log.Warn("skills: runs did not stop before the shutdown deadline; the next boot will reconcile them")
	}
}

func toSkillRun(rec store.SkillRunRecord) SkillRun {
	out := SkillRun{SkillRunRecord: rec, Inputs: map[string]string{}, Capabilities: []string{}, Controls: []string{}}
	_ = json.Unmarshal([]byte(rec.InputsJSON), &out.Inputs)
	_ = json.Unmarshal([]byte(rec.CapabilitiesJSON), &out.Capabilities)
	_ = json.Unmarshal([]byte(rec.RunnerControls), &out.Controls)
	// The raw report never travels in a list or summary; GetRun verifies it.
	out.ReportJSON = nil
	return out
}

func newRunID() string {
	var b [12]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(fmt.Sprintf("skills: read random run id: %v", err))
	}
	return "skr-" + hex.EncodeToString(b[:])
}
