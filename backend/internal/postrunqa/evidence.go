package postrunqa

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/adapters/agent/hookutil"
	"github.com/aoagents/agent-orchestrator/backend/internal/branchlock"
	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

// This file is the gate's evidence collector: the read-only pass that answers
// "what did this execution actually leave behind" before anything classifies
// it into Findings.
//
// The load-bearing rule is where the answers come from. Every fact below is
// read from the structured source that already owns it — the branch-lock
// manager's own held-lock and retention state, the wake scheduler's own
// schedule rows, the workspace preflighter's own porcelain status, the hook
// launcher's own shim and target, the session cleanup facts table, the
// daemon's and the runtimes' own error records — never by grepping an agent's
// log for prose that looks like a failure. A component's structured record of
// its own failure is a fact; a line in a transcript that reads like one is not. Log text is
// unversioned, harness-specific, and lies by omission the moment a provider
// reformats a message; the durable records do not.
//
// The one exception is deliberate and is the last field of EvidenceReport: the
// agent's own closing report is carried through verbatim. The collector does
// not parse it, summarize it, or decide anything from it. It is captured so
// downstream classification can compare what the agent SAID it did against
// what the structured sources say actually happened — a comparison that is
// impossible if the text is thrown away here.

// ErrUnknownExecution reports that Collect was asked about an execution the
// ExecutionSource has no record of. It is an error rather than an empty report
// because "no evidence" and "no such execution" must not read the same to a
// gate deciding whether a subject is clean.
var ErrUnknownExecution = fmt.Errorf("postrunqa: unknown execution")

// RepositoryTarget is one repository+branch pair an execution was permitted to
// write. It mirrors the pair a branch lock is keyed on, so the repositories the
// collector probes are the repositories the execution actually held.
type RepositoryTarget struct {
	RepoPath string `json:"repoPath"`
	Branch   string `json:"branch"`
}

// Execution is the durable scope of one execution the gate collects evidence
// for: what it was allowed to write, where it ran, when it ended, and the
// closing report its agent left. It is resolved once, up front, because every
// other source is keyed on some part of it.
type Execution struct {
	// ID is the execution's identity — the workflow run that executed a task
	// (workflow_tasks.execution_run_id).
	ID string
	// WorkflowRunID is the run whose branch locks and wake schedules belong to
	// this execution. Usually equal to ID; kept separate so an execution that
	// is not itself a run row can still name its owning run.
	WorkflowRunID string
	// SessionIDs are the agent sessions this execution ran in.
	SessionIDs []string
	// Repositories are the repository+branch pairs it was permitted to write.
	Repositories []RepositoryTarget
	// DataDir is AO's state root (~/.ao/data), which is where the hook
	// launcher shim lives. Empty disables the hook-launcher probe.
	DataDir string
	// EndedAt is when the execution stopped. Zero means it is still running,
	// which changes what counts as an anomaly: a held branch lock and an open
	// wake are normal for a live execution and are leftovers for a finished
	// one.
	EndedAt time.Time
	// FinalAgentReport is the closing report text the agent produced, exactly
	// as it produced it.
	FinalAgentReport string
}

// Ended reports whether the execution has stopped.
func (e Execution) Ended() bool { return !e.EndedAt.IsZero() }

// EvidenceSource names one structured origin of evidence, used to attribute a
// SourceError to the source that failed.
type EvidenceSource string

// Evidence sources.
const (
	SourceGit          EvidenceSource = "git"
	SourceBranchLock   EvidenceSource = "branch_lock"
	SourceHookLauncher EvidenceSource = "hook_launcher"
	SourceWake         EvidenceSource = "wake"
	SourceProcess      EvidenceSource = "process"
	SourceSession      EvidenceSource = "session"
	// SourceRuntimeLog is the daemon's and the runtimes' own error records --
	// the failures that never reach a git status, a lock row, or a process
	// exit code because the component that hit them logged them and carried
	// on. A gate that cannot see them reports "nothing left behind" for an
	// execution whose runtime was throwing errors the whole time.
	SourceRuntimeLog EvidenceSource = "runtime_log"
)

// SourceError records that one source could not be read. Collect keeps going
// after one: a report missing the wake schedules is still worth having, and
// silently returning it as if the wake source had reported "nothing" would
// turn an unreadable source into false evidence of cleanliness.
type SourceError struct {
	Source EvidenceSource `json:"source"`
	// Subject is what was being read when it failed (a repository path, a
	// session id); empty when the source failed as a whole.
	Subject string `json:"subject,omitempty"`
	Message string `json:"message"`
}

// GitEvidence is one repository's working-tree state after the execution,
// read from the workspace preflighter's porcelain status.
type GitEvidence struct {
	RepoPath         string                  `json:"repoPath"`
	ConfiguredBranch string                  `json:"configuredBranch,omitempty"`
	CurrentBranch    string                  `json:"currentBranch,omitempty"`
	HeadSHA          string                  `json:"headSha,omitempty"`
	Dirty            bool                    `json:"dirty"`
	Changes          []ports.WorkspaceChange `json:"changes,omitempty"`
	// Anomaly is why this repository is not clean, or "" when it is.
	Anomaly string `json:"anomaly,omitempty"`
}

// BranchLockEvidence is one execution lock the branch-lock manager still
// reports as held for this execution, together with what reconciliation would
// decide about it.
type BranchLockEvidence struct {
	LockID     string                 `json:"lockId"`
	LockKey    string                 `json:"lockKey"`
	RepoPath   string                 `json:"repoPath"`
	Branch     string                 `json:"branch"`
	Owner      string                 `json:"owner"`
	State      domain.BranchLockState `json:"state"`
	AcquiredAt time.Time              `json:"acquiredAt"`
	RenewedAt  time.Time              `json:"renewedAt"`
	// RetentionDecision and RetentionReason come from the manager's own
	// Inspect, never from a rule re-implemented here. A lock the manager
	// itself would release is by definition stale.
	RetentionDecision branchlock.RetentionDecision `json:"retentionDecision,omitempty"`
	RetentionReason   string                       `json:"retentionReason,omitempty"`
	// Leaked means the lock outlived what legitimately held it: the execution
	// finished and never released it, or the manager considers it stale.
	Leaked  bool   `json:"leaked"`
	Anomaly string `json:"anomaly,omitempty"`
}

// HookLauncherEvidence is the state of AO's hook launcher shim and the binary
// it execs. The defect it exists to catch is a shim still pinned to a Go
// build-cache binary that no longer exists, which kills every hook that fires
// afterwards while the shim itself still looks perfectly well-formed.
type HookLauncherEvidence struct {
	// Probed is false when no data dir was known, so "not checked" never reads
	// as "checked and healthy".
	Probed           bool   `json:"probed"`
	Path             string `json:"path,omitempty"`
	Present          bool   `json:"present"`
	Executable       bool   `json:"executable"`
	Target           string `json:"target,omitempty"`
	TargetPresent    bool   `json:"targetPresent"`
	TargetExecutable bool   `json:"targetExecutable"`
	TargetEphemeral  bool   `json:"targetEphemeral"`
	Anomaly          string `json:"anomaly,omitempty"`
}

// WakeScheduleRecord is one still-open (pending or claimed) wake schedule, in
// the narrow shape the collector needs. It mirrors the durable fields of
// wake.Schedule, which is where every value comes from.
//
// It is a copy rather than the wake package's own type for one structural
// reason: internal/workflow/wake sits ABOVE the sqlite store, and the sqlite
// store imports this package to persist QARun. Importing wake here would close
// that loop. Declaring the record next to its consumer, exactly as Store and
// ProcessRecord are declared, keeps this package below the store and leaves the
// field-for-field copy from wake.Schedule to the wiring site.
type WakeScheduleRecord struct {
	ID     string
	StepID string
	// Reason and Status carry the wake package's own vocabulary values
	// (wake.Reason / wake.Status) as strings.
	Reason       string
	Status       string
	ScheduledAt  time.Time
	AttemptCount int64
	LastError    string
}

// WakeEvidence is one wake schedule this execution left open.
type WakeEvidence struct {
	ScheduleID   string    `json:"scheduleId"`
	StepID       string    `json:"stepId,omitempty"`
	Reason       string    `json:"reason"`
	Status       string    `json:"status"`
	ScheduledAt  time.Time `json:"scheduledAt"`
	AttemptCount int64     `json:"attemptCount"`
	LastError    string    `json:"lastError,omitempty"`
	// Overdue means the wake's own scheduled time has already passed and it is
	// still open — the poller was due to fire it and did not.
	Overdue bool   `json:"overdue"`
	Anomaly string `json:"anomaly,omitempty"`
}

// ProcessRecord is one background command or process the execution ran, as the
// layer that ran it recorded it. Declared here, like Store, so the collector
// depends on the fact rather than on whichever runner produced it.
type ProcessRecord struct {
	// Label is the command as it was invoked.
	Label string `json:"label"`
	// ExitCode is the process's status; nil when it never exited.
	ExitCode *int `json:"exitCode,omitempty"`
	// Running reports the process was still alive when the record was taken.
	Running    bool      `json:"running,omitempty"`
	TimedOut   bool      `json:"timedOut,omitempty"`
	StartedAt  time.Time `json:"startedAt,omitzero"`
	EndedAt    time.Time `json:"endedAt,omitzero"`
	StderrTail string    `json:"stderrTail,omitempty"`
}

// ProcessEvidence is a ProcessRecord plus the collector's verdict on it.
type ProcessEvidence struct {
	ProcessRecord
	Anomaly string `json:"anomaly,omitempty"`
}

// Runtime error levels, as the daemon and the runtime adapters record them.
// Any other value, and the empty string, is treated as LevelError: this source
// is an error log, and a record whose level nobody set is not thereby harmless.
const (
	RuntimeLevelWarn  = "warn"
	RuntimeLevelError = "error"
	RuntimeLevelFatal = "fatal"
)

// RuntimeErrorRecord is one error the daemon or a runtime adapter recorded
// while this execution was running, as the component that hit it recorded it.
// It is declared here, like ProcessRecord and WakeScheduleRecord, so the
// collector depends on the fact rather than on whichever logger produced it.
//
// It is deliberately not a log line. A grep over an agent's transcript is the
// one thing this package refuses to do; this is the structured record a
// component wrote about its own failure, which is why it can carry a stable
// Code and an occurrence count instead of prose that changes with a reformat.
type RuntimeErrorRecord struct {
	// Component is the daemon subsystem or runtime adapter that reported it
	// ("lifecycle.reaper", "runtime.tmux"). It is the finding's subject, so it
	// must identify the component, not the incident.
	Component string `json:"component"`
	// Code is the component's own stable code for this failure. When it is
	// set, it -- and not the rendered message -- is what makes two records the
	// same error, which is what keeps a baseline comparison from turning on a
	// message someone rephrased.
	Code string `json:"code,omitempty"`
	// Message is the rendered error, kept verbatim as evidence for a human.
	Message string `json:"message"`
	// Level is one of the RuntimeLevel constants.
	Level string `json:"level,omitempty"`
	// SessionID names the session the error belongs to, when it belongs to
	// one. Empty for a daemon-wide failure.
	SessionID string `json:"sessionId,omitempty"`
	// Count is how many times this error was recorded. It is evidence for a
	// human and is deliberately excluded from the signature: an error that
	// fired three times before the execution and seven times during it is the
	// same pre-existing error, and a signature that moved with the counter
	// would report every recurrence as newly introduced.
	Count       int64     `json:"count,omitempty"`
	FirstSeenAt time.Time `json:"firstSeenAt,omitzero"`
	LastSeenAt  time.Time `json:"lastSeenAt,omitzero"`
}

// Fatal reports whether the record is at the level that makes its component
// unusable rather than merely degraded.
func (r RuntimeErrorRecord) Fatal() bool {
	return strings.EqualFold(strings.TrimSpace(r.Level), RuntimeLevelFatal)
}

// Warning reports whether the record was explicitly logged below error level.
func (r RuntimeErrorRecord) Warning() bool {
	return strings.EqualFold(strings.TrimSpace(r.Level), RuntimeLevelWarn)
}

// RuntimeErrorEvidence is a RuntimeErrorRecord plus the collector's verdict.
type RuntimeErrorEvidence struct {
	RuntimeErrorRecord
	Anomaly string `json:"anomaly,omitempty"`
}

// SessionEvidence is one session's teardown state, read from the durable
// session row and its cleanup facts rather than from whether a pane looks
// gone.
type SessionEvidence struct {
	SessionID string `json:"sessionId"`
	// Found is false when the session row is missing entirely.
	Found      bool `json:"found"`
	Terminated bool `json:"terminated"`
	// CleanupRecorded is false when a terminated session has no cleanup facts
	// row at all — the finalizer never ran for it.
	CleanupRecorded      bool                        `json:"cleanupRecorded"`
	RuntimeReleased      bool                        `json:"runtimeReleased"`
	WorkspaceDisposition domain.WorkspaceDisposition `json:"workspaceDisposition,omitempty"`
	CleanupAttempts      int64                       `json:"cleanupAttempts,omitempty"`
	FailureCode          string                      `json:"failureCode,omitempty"`
	Anomaly              string                      `json:"anomaly,omitempty"`
}

// EvidenceReport is everything the collector observed about one execution. It
// is a snapshot of facts, not a verdict: nothing here decides whether the gate
// blocks. The JSON tags are the on-disk format for a persisted report and must
// not be renamed casually, exactly as for Finding.
type EvidenceReport struct {
	ExecutionID   string    `json:"executionId"`
	WorkflowRunID string    `json:"workflowRunId,omitempty"`
	CollectedAt   time.Time `json:"collectedAt"`
	// ExecutionEnded mirrors Execution.Ended at collection time, because every
	// "left behind" judgement below depends on it.
	ExecutionEnded bool `json:"executionEnded"`

	Git          []GitEvidence        `json:"git,omitempty"`
	BranchLocks  []BranchLockEvidence `json:"branchLocks,omitempty"`
	HookLauncher HookLauncherEvidence `json:"hookLauncher"`
	Wakes        []WakeEvidence       `json:"wakes,omitempty"`
	Processes    []ProcessEvidence    `json:"processes,omitempty"`
	Sessions     []SessionEvidence    `json:"sessions,omitempty"`
	// RuntimeErrors are the daemon's and the runtimes' own error records for
	// this execution.
	RuntimeErrors []RuntimeErrorEvidence `json:"runtimeErrors,omitempty"`

	// FinalAgentReport is the agent's closing report, verbatim. The collector
	// never parses it; downstream classification compares it against the
	// structured evidence above.
	FinalAgentReport string `json:"finalAgentReport,omitempty"`

	// SourceErrors names every source that could not be read.
	SourceErrors []SourceError `json:"sourceErrors,omitempty"`
}

// Anomalies returns every non-empty anomaly in the report, in a stable
// source-by-source order. A report with no anomalies and no SourceErrors is
// the only shape that means "this execution left nothing behind".
func (r EvidenceReport) Anomalies() []string {
	var out []string
	add := func(prefix, anomaly string) {
		if anomaly != "" {
			out = append(out, prefix+": "+anomaly)
		}
	}
	for _, g := range r.Git {
		add("git "+g.RepoPath, g.Anomaly)
	}
	for _, l := range r.BranchLocks {
		add("branch lock "+l.LockKey, l.Anomaly)
	}
	add("hook launcher", r.HookLauncher.Anomaly)
	for _, w := range r.Wakes {
		add("wake "+w.ScheduleID, w.Anomaly)
	}
	for _, p := range r.Processes {
		add("process "+p.Label, p.Anomaly)
	}
	for _, s := range r.Sessions {
		add("session "+s.SessionID, s.Anomaly)
	}
	for _, e := range r.RuntimeErrors {
		add("runtime "+e.Component, e.Anomaly)
	}
	return out
}

// HasAnomalies reports whether the collector observed anything that is not
// clean.
func (r EvidenceReport) HasAnomalies() bool { return len(r.Anomalies()) > 0 }

// ExecutionSource resolves an execution id into the scope every other source
// is keyed on, and carries the agent's verbatim closing report.
type ExecutionSource interface {
	LoadExecution(ctx context.Context, executionID string) (Execution, bool, error)
}

// GitSource is the read-only repository probe. It is deliberately
// ports.WorkspacePreflighter's exact shape, so the workspace adapter AO
// already uses satisfies it with no adapter in between.
type GitSource interface {
	PreflightRepository(ctx context.Context, repoPath, branch string) (ports.WorkspacePreflight, error)
}

// BranchLockSource is the branch-lock manager's read side: which locks this
// execution still holds, and what reconciliation would decide about them.
// *branchlock.Manager satisfies it directly.
type BranchLockSource interface {
	HeldByRun(ctx context.Context, runID string) ([]domain.BranchLock, error)
	HeldBySession(ctx context.Context, sessionID string) ([]domain.BranchLock, error)
	Inspect(ctx context.Context) ([]branchlock.LockStatus, error)
}

// HookLauncherProbe reads the hook launcher installed under a data directory.
// hookutil.InspectLauncher satisfies it directly.
type HookLauncherProbe func(dataDir string) hookutil.LauncherInspection

// WakeSource is the wake scheduler's read side: every wake this run left open.
// It is backed by (*wake.Scheduler).PendingForRun, adapted at the wiring site
// for the import-direction reason on WakeScheduleRecord.
type WakeSource interface {
	PendingWakes(ctx context.Context, workflowRunID string) ([]WakeScheduleRecord, error)
}

// ProcessSource reports the background commands and processes one execution
// ran, with their exit status.
type ProcessSource interface {
	ProcessRecords(ctx context.Context, executionID string) ([]ProcessRecord, error)
}

// RuntimeErrorSource reports the errors the daemon and the runtime adapters
// recorded for one execution. Like ProcessSource it is keyed on the execution,
// so a deployment that keeps its runtime errors somewhere this package must
// not import adapts at the wiring site rather than here.
type RuntimeErrorSource interface {
	RuntimeErrors(ctx context.Context, executionID string) ([]RuntimeErrorRecord, error)
}

// SessionSource is the durable session read side: the session row and its
// cleanup facts. *sqlite.Store satisfies it directly.
type SessionSource interface {
	GetSession(ctx context.Context, id domain.SessionID) (domain.SessionRecord, bool, error)
	GetSessionCleanupFacts(ctx context.Context, id domain.SessionID) (domain.SessionCleanupRecord, bool, error)
}

// EvidenceDeps wires the collector's sources. Every source except Executions
// is optional: a nil source contributes no evidence and no SourceError, which
// is what a deployment that genuinely has no such source should produce. It is
// not a substitute for a source that failed — that is a SourceError.
type EvidenceDeps struct {
	Executions    ExecutionSource
	Git           GitSource
	BranchLocks   BranchLockSource
	HookLauncher  HookLauncherProbe
	Wakes         WakeSource
	Processes     ProcessSource
	Sessions      SessionSource
	RuntimeErrors RuntimeErrorSource
	Clock         func() time.Time
}

// EvidenceCollector assembles one EvidenceReport per execution.
type EvidenceCollector struct {
	deps  EvidenceDeps
	clock func() time.Time
}

// NewEvidenceCollector builds a collector. Executions is required: without it
// there is no scope to collect against.
func NewEvidenceCollector(deps EvidenceDeps) (*EvidenceCollector, error) {
	if deps.Executions == nil {
		return nil, fmt.Errorf("postrunqa: evidence collector requires an execution source")
	}
	clock := deps.Clock
	if clock == nil {
		clock = time.Now
	}
	return &EvidenceCollector{deps: deps, clock: clock}, nil
}

// Collect reads every wired source and returns one report for the execution.
//
// It fails only when the execution itself cannot be resolved. A source that
// errors is recorded in SourceErrors and collection continues, so one
// unavailable source never costs the gate the evidence the others had.
func (c *EvidenceCollector) Collect(ctx context.Context, executionID string) (EvidenceReport, error) {
	executionID = strings.TrimSpace(executionID)
	if executionID == "" {
		return EvidenceReport{}, fmt.Errorf("postrunqa: execution id is required")
	}
	exec, ok, err := c.deps.Executions.LoadExecution(ctx, executionID)
	if err != nil {
		return EvidenceReport{}, fmt.Errorf("postrunqa: load execution %s: %w", executionID, err)
	}
	if !ok {
		return EvidenceReport{}, fmt.Errorf("%w: %s", ErrUnknownExecution, executionID)
	}
	if exec.ID == "" {
		exec.ID = executionID
	}

	report := EvidenceReport{
		ExecutionID:      exec.ID,
		WorkflowRunID:    exec.WorkflowRunID,
		CollectedAt:      c.clock(),
		ExecutionEnded:   exec.Ended(),
		FinalAgentReport: exec.FinalAgentReport,
	}
	now := report.CollectedAt

	report.Git = c.collectGit(ctx, exec, &report)
	report.BranchLocks = c.collectBranchLocks(ctx, exec, &report)
	report.HookLauncher = c.collectHookLauncher(exec)
	report.Wakes = c.collectWakes(ctx, exec, now, &report)
	report.Processes = c.collectProcesses(ctx, exec, &report)
	report.Sessions = c.collectSessions(ctx, exec, &report)
	report.RuntimeErrors = c.collectRuntimeErrors(ctx, exec, &report)
	return report, nil
}

func (r *EvidenceReport) sourceFailed(source EvidenceSource, subject string, err error) {
	r.SourceErrors = append(r.SourceErrors, SourceError{Source: source, Subject: subject, Message: err.Error()})
}

func (c *EvidenceCollector) collectGit(ctx context.Context, exec Execution, report *EvidenceReport) []GitEvidence {
	if c.deps.Git == nil || len(exec.Repositories) == 0 {
		return nil
	}
	out := make([]GitEvidence, 0, len(exec.Repositories))
	for _, target := range exec.Repositories {
		pf, err := c.deps.Git.PreflightRepository(ctx, target.RepoPath, target.Branch)
		if err != nil {
			report.sourceFailed(SourceGit, target.RepoPath, err)
			continue
		}
		ev := GitEvidence{
			RepoPath:         firstNonEmpty(pf.RepoPath, target.RepoPath),
			ConfiguredBranch: firstNonEmpty(pf.ConfiguredBranch, target.Branch),
			CurrentBranch:    pf.CurrentBranch,
			HeadSHA:          pf.HeadSHA,
			Dirty:            pf.Dirty,
			Changes:          pf.Changes,
		}
		var reasons []string
		if pf.Dirty {
			reasons = append(reasons, fmt.Sprintf("working tree has %d uncommitted change(s)", len(pf.Changes)))
		}
		if ev.CurrentBranch != "" && ev.ConfiguredBranch != "" && ev.CurrentBranch != ev.ConfiguredBranch {
			reasons = append(reasons, fmt.Sprintf("checked out on %q, not the configured %q", ev.CurrentBranch, ev.ConfiguredBranch))
		}
		ev.Anomaly = strings.Join(reasons, "; ")
		out = append(out, ev)
	}
	return out
}

func (c *EvidenceCollector) collectBranchLocks(ctx context.Context, exec Execution, report *EvidenceReport) []BranchLockEvidence {
	if c.deps.BranchLocks == nil {
		return nil
	}
	locks := make(map[string]domain.BranchLock)
	if exec.WorkflowRunID != "" {
		held, err := c.deps.BranchLocks.HeldByRun(ctx, exec.WorkflowRunID)
		if err != nil {
			report.sourceFailed(SourceBranchLock, "run "+exec.WorkflowRunID, err)
		}
		for _, l := range held {
			locks[l.ID] = l
		}
	}
	for _, sessionID := range exec.SessionIDs {
		held, err := c.deps.BranchLocks.HeldBySession(ctx, sessionID)
		if err != nil {
			report.sourceFailed(SourceBranchLock, "session "+sessionID, err)
			continue
		}
		for _, l := range held {
			locks[l.ID] = l
		}
	}
	if len(locks) == 0 {
		return nil
	}
	// Inspect is the manager's own staleness verdict. It covers every held
	// lock, so it is read once and joined by id rather than re-deciding
	// retention here.
	retention := map[string]branchlock.Retention{}
	if statuses, err := c.deps.BranchLocks.Inspect(ctx); err != nil {
		report.sourceFailed(SourceBranchLock, "inspect", err)
	} else {
		for _, s := range statuses {
			retention[s.Lock.ID] = s.Retention
		}
	}

	out := make([]BranchLockEvidence, 0, len(locks))
	for _, lock := range locks {
		ev := BranchLockEvidence{
			LockID:     lock.ID,
			LockKey:    lock.LockKey,
			RepoPath:   lock.RepoPath,
			Branch:     lock.Branch,
			Owner:      lock.OwnerDescription(),
			State:      lock.State,
			AcquiredAt: lock.AcquiredAt,
			RenewedAt:  lock.RenewedAt,
		}
		if r, ok := retention[lock.ID]; ok {
			ev.RetentionDecision = r.Decision
			ev.RetentionReason = r.Reason
		}
		var reasons []string
		if exec.Ended() && lock.Held() {
			ev.Leaked = true
			reasons = append(reasons, fmt.Sprintf("still held by %s after the execution ended", ev.Owner))
		}
		if ev.RetentionDecision == branchlock.RetentionRelease {
			ev.Leaked = true
			reasons = append(reasons, "reconciliation considers it stale ("+ev.RetentionReason+")")
		}
		ev.Anomaly = strings.Join(reasons, "; ")
		out = append(out, ev)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].LockID < out[j].LockID })
	return out
}

func (c *EvidenceCollector) collectHookLauncher(exec Execution) HookLauncherEvidence {
	if c.deps.HookLauncher == nil || strings.TrimSpace(exec.DataDir) == "" {
		return HookLauncherEvidence{}
	}
	insp := c.deps.HookLauncher(exec.DataDir)
	ev := HookLauncherEvidence{
		Probed:           true,
		Path:             insp.Path,
		Present:          insp.Present,
		Executable:       insp.Executable,
		Target:           insp.Target,
		TargetPresent:    insp.TargetPresent,
		TargetExecutable: insp.TargetExecutable,
		TargetEphemeral:  insp.TargetEphemeral,
	}
	var reasons []string
	switch {
	case insp.Error != "":
		reasons = append(reasons, "launcher could not be read: "+insp.Error)
	case !insp.Present:
		reasons = append(reasons, "launcher shim is missing at "+insp.Path)
	default:
		if !insp.Executable {
			reasons = append(reasons, "launcher shim is not executable")
		}
		switch {
		case insp.Target == "":
			reasons = append(reasons, "launcher shim names no target binary")
		case insp.TargetEphemeral:
			// The fd9b87fae defect: the shim still resolves today only
			// because the build-cache binary has not been swept yet.
			reasons = append(reasons, "launcher targets an ephemeral Go build-cache binary ("+insp.Target+"); hooks will fail once it is removed")
		case !insp.TargetPresent:
			reasons = append(reasons, "launcher target does not exist ("+insp.Target+")")
		case !insp.TargetExecutable:
			reasons = append(reasons, "launcher target is not executable ("+insp.Target+")")
		}
	}
	ev.Anomaly = strings.Join(reasons, "; ")
	return ev
}

func (c *EvidenceCollector) collectWakes(ctx context.Context, exec Execution, now time.Time, report *EvidenceReport) []WakeEvidence {
	if c.deps.Wakes == nil || exec.WorkflowRunID == "" {
		return nil
	}
	schedules, err := c.deps.Wakes.PendingWakes(ctx, exec.WorkflowRunID)
	if err != nil {
		report.sourceFailed(SourceWake, "run "+exec.WorkflowRunID, err)
		return nil
	}
	out := make([]WakeEvidence, 0, len(schedules))
	for _, s := range schedules {
		ev := WakeEvidence{
			ScheduleID:   s.ID,
			StepID:       s.StepID,
			Reason:       s.Reason,
			Status:       s.Status,
			ScheduledAt:  s.ScheduledAt,
			AttemptCount: s.AttemptCount,
			LastError:    s.LastError,
			Overdue:      s.ScheduledAt.Before(now),
		}
		var reasons []string
		if exec.Ended() {
			reasons = append(reasons, fmt.Sprintf("still %s after the execution ended", s.Status))
		}
		if ev.Overdue {
			reasons = append(reasons, fmt.Sprintf("overdue by %s (scheduled %s)", now.Sub(s.ScheduledAt).Round(time.Second), s.ScheduledAt.UTC().Format(time.RFC3339)))
		}
		if s.LastError != "" {
			reasons = append(reasons, fmt.Sprintf("retrying after %d attempt(s): %s", s.AttemptCount, s.LastError))
		}
		ev.Anomaly = strings.Join(reasons, "; ")
		out = append(out, ev)
	}
	return out
}

func (c *EvidenceCollector) collectProcesses(ctx context.Context, exec Execution, report *EvidenceReport) []ProcessEvidence {
	if c.deps.Processes == nil {
		return nil
	}
	records, err := c.deps.Processes.ProcessRecords(ctx, exec.ID)
	if err != nil {
		report.sourceFailed(SourceProcess, exec.ID, err)
		return nil
	}
	out := make([]ProcessEvidence, 0, len(records))
	for _, rec := range records {
		ev := ProcessEvidence{ProcessRecord: rec}
		var reasons []string
		switch {
		case rec.TimedOut:
			reasons = append(reasons, "timed out")
		case rec.ExitCode != nil && *rec.ExitCode != 0:
			reasons = append(reasons, fmt.Sprintf("exited with status %d", *rec.ExitCode))
		case rec.Running && exec.Ended():
			reasons = append(reasons, "still running after the execution ended")
		case rec.ExitCode == nil && !rec.Running && exec.Ended():
			reasons = append(reasons, "no exit status was ever recorded")
		}
		ev.Anomaly = strings.Join(reasons, "; ")
		out = append(out, ev)
	}
	return out
}

// collectRuntimeErrors reads the daemon's and the runtimes' own error records.
//
// Every record the source returns is an anomaly: it is an error log, and the
// component that wrote the entry had already decided something went wrong. The
// collector's only judgement here is how to phrase it, and whether an
// explicitly-warn record is worth saying "error" about.
func (c *EvidenceCollector) collectRuntimeErrors(ctx context.Context, exec Execution, report *EvidenceReport) []RuntimeErrorEvidence {
	if c.deps.RuntimeErrors == nil {
		return nil
	}
	records, err := c.deps.RuntimeErrors.RuntimeErrors(ctx, exec.ID)
	if err != nil {
		report.sourceFailed(SourceRuntimeLog, exec.ID, err)
		return nil
	}
	out := make([]RuntimeErrorEvidence, 0, len(records))
	for _, rec := range records {
		ev := RuntimeErrorEvidence{RuntimeErrorRecord: rec}
		severity := "error"
		switch {
		case rec.Fatal():
			severity = "fatal error"
		case rec.Warning():
			severity = "warning"
		}
		reason := fmt.Sprintf("%s recorded a %s", firstNonEmpty(rec.Component, "an unnamed component"), severity)
		if rec.Code != "" {
			reason += " (" + rec.Code + ")"
		}
		if rec.Count > 1 {
			reason += fmt.Sprintf(" %d times", rec.Count)
		}
		if rec.Message != "" {
			reason += ": " + rec.Message
		}
		ev.Anomaly = reason
		out = append(out, ev)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Component != out[j].Component {
			return out[i].Component < out[j].Component
		}
		return out[i].Code < out[j].Code
	})
	return out
}

func (c *EvidenceCollector) collectSessions(ctx context.Context, exec Execution, report *EvidenceReport) []SessionEvidence {
	if c.deps.Sessions == nil || len(exec.SessionIDs) == 0 {
		return nil
	}
	out := make([]SessionEvidence, 0, len(exec.SessionIDs))
	for _, id := range exec.SessionIDs {
		ev := SessionEvidence{SessionID: id}
		session, ok, err := c.deps.Sessions.GetSession(ctx, domain.SessionID(id))
		if err != nil {
			report.sourceFailed(SourceSession, id, err)
			continue
		}
		ev.Found = ok
		ev.Terminated = session.IsTerminated
		facts, hasFacts, err := c.deps.Sessions.GetSessionCleanupFacts(ctx, domain.SessionID(id))
		if err != nil {
			report.sourceFailed(SourceSession, id, err)
			continue
		}
		ev.CleanupRecorded = hasFacts
		ev.RuntimeReleased = hasFacts && !facts.RuntimeReleasedAt.IsZero()
		ev.WorkspaceDisposition = facts.WorkspaceDisposition
		ev.CleanupAttempts = facts.AttemptCount
		ev.FailureCode = facts.FailureCode

		var reasons []string
		switch {
		case !ok:
			reasons = append(reasons, "session row is missing")
		case !ev.Terminated && exec.Ended():
			reasons = append(reasons, "session was never terminated")
		case ev.Terminated && !hasFacts:
			reasons = append(reasons, "terminated with no cleanup facts recorded")
		case ev.Terminated:
			if !ev.RuntimeReleased {
				reasons = append(reasons, "runtime was never confirmed released")
			}
			switch facts.WorkspaceDisposition {
			case domain.DispositionPending:
				reasons = append(reasons, fmt.Sprintf("workspace teardown is still pending after %d attempt(s)", facts.AttemptCount))
			case domain.DispositionFailed:
				reasons = append(reasons, "workspace teardown failed")
			case domain.DispositionPreservedDirty:
				reasons = append(reasons, "workspace was preserved because it holds uncommitted changes")
			case domain.DispositionRemoved, domain.DispositionNotApplicable:
			}
			if facts.FailureCode != "" {
				reasons = append(reasons, "last teardown failure: "+facts.FailureCode)
			}
		}
		ev.Anomaly = strings.Join(reasons, "; ")
		out = append(out, ev)
	}
	return out
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}
