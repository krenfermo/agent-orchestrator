// Package postrunqa defines the durable state model for the Post-Run QA gate:
// the automated check-and-repair pass that runs after a task's or a workflow's
// work looks finished, but before AO reports it complete.
//
// This package owns only the state model — the phase a gate run is in, the
// findings it collected, how many repair cycles it has spent, and how it ended.
// It deliberately contains no check runners, no repair dispatch, and no
// scheduling: those land on top of this envelope. Persistence is likewise not
// implemented here. The gate stores its state through the daemon's existing
// lifecycle store (internal/storage/sqlite/store), which implements the Store
// interface declared at the bottom of this file, so a QARun survives a daemon
// restart in the same database as every other lifecycle fact and no separate
// on-disk format exists.
package postrunqa

import (
	"context"
	"fmt"
	"time"
)

// DefaultMaxRepairCycles is the repair budget a QARun gets when none was set
// explicitly. Two is deliberate and low: the gate exists to clear mechanical
// breakage the agent left behind (a build error, a failing vet), and anything
// that survives two automated repair attempts is a judgement call that belongs
// with a human, not a third machine attempt.
const DefaultMaxRepairCycles = 2

// QAPhase is the durable phase of one gate run. It is stored, not derived:
// after a daemon restart the gate has to know whether it had already started
// checking, was mid-repair, or had already reached a verdict.
type QAPhase string

const (
	// PhasePending means the gate run exists but no check has run yet.
	PhasePending QAPhase = "pending"
	// PhaseChecking means checks are running and findings are being collected.
	PhaseChecking QAPhase = "checking"
	// PhaseAutoFixing means the gate is attempting an automated repair of the
	// findings it collected. The "(n)" of AutoFixing(n) is not part of the
	// phase value: n is QARun.RepairCycleCount, so the phase stays a small
	// closed set and the cycle count stays a single number to compare against
	// QARun.MaxRepairCycles.
	PhaseAutoFixing QAPhase = "auto_fixing"
	// PhaseClean is the terminal phase for a subject the gate cleared.
	PhaseClean QAPhase = "clean"
	// PhaseNeedsAttention is the terminal phase for a subject the gate could
	// not clear — findings remain, or the repair budget ran out.
	PhaseNeedsAttention QAPhase = "needs_attention"
)

// Valid reports whether a phase value is persistable.
func (p QAPhase) Valid() bool {
	switch p {
	case PhasePending, PhaseChecking, PhaseAutoFixing, PhaseClean, PhaseNeedsAttention:
		return true
	default:
		return false
	}
}

// Terminal reports whether the phase is a final verdict that will never
// transition again.
func (p QAPhase) Terminal() bool {
	return p == PhaseClean || p == PhaseNeedsAttention
}

// SubjectKind names what a gate run is gating. The gate runs over the same two
// lifecycle subjects that carry durable status transitions today.
type SubjectKind string

const (
	// SubjectTask is one workflow task (workflow_tasks.id).
	SubjectTask SubjectKind = "task"
	// SubjectWorkflow is a whole workflow run (workflow_runs.id).
	SubjectWorkflow SubjectKind = "workflow"
)

// Valid reports whether a subject kind is persistable.
func (k SubjectKind) Valid() bool {
	return k == SubjectTask || k == SubjectWorkflow
}

// Attribution records whether a finding is this run's fault. Without it the
// gate would block a task on breakage it never caused — a repository that was
// already failing `go vet` before the agent touched it must not become that
// agent's problem.
type Attribution string

const (
	// AttributionNew means the signal is absent from the baseline and appeared
	// with this subject's changes.
	AttributionNew Attribution = "new"
	// AttributionBaseline means the same signal is present in the baseline, so
	// this subject did not introduce it.
	AttributionBaseline Attribution = "baseline"
	// AttributionAmbiguous means the comparison could not decide — no usable
	// baseline, or the signal changed shape enough that matching it is a guess. Never silently promoted to new or demoted to baseline: an
	// undecidable comparison is its own durable answer.
	AttributionAmbiguous Attribution = "ambiguous"
)

// Valid reports whether an attribution value is persistable.
func (a Attribution) Valid() bool {
	switch a {
	case AttributionNew, AttributionBaseline, AttributionAmbiguous:
		return true
	default:
		return false
	}
}

// Severity is how badly one finding matters.
type Severity string

const (
	// SeverityBlocker is breakage that makes the subject unusable — the build
	// or the test suite fails.
	SeverityBlocker Severity = "blocker"
	// SeverityMajor is a real defect that does not stop the build.
	SeverityMajor Severity = "major"
	// SeverityMinor is a small or stylistic problem.
	SeverityMinor Severity = "minor"
	// SeverityInfo is context worth recording that is not itself a problem.
	SeverityInfo Severity = "info"
)

// Valid reports whether a severity value is persistable.
func (s Severity) Valid() bool {
	switch s {
	case SeverityBlocker, SeverityMajor, SeverityMinor, SeverityInfo:
		return true
	default:
		return false
	}
}

// QAResult is the gate's verdict, recorded once the run reaches a terminal
// phase. It is stored separately from the phase rather than re-derived from it
// so a completed run reads its own outcome without the reader having to know
// which phases count as success.
type QAResult string

const (
	// ResultUnset is the result of a run that has not finished yet.
	ResultUnset QAResult = ""
	// ResultClean means the gate cleared the subject.
	ResultClean QAResult = "clean"
	// ResultNeedsAttention means the gate handed the subject back to a human.
	ResultNeedsAttention QAResult = "needs_attention"
)

// Valid reports whether a result value is persistable.
func (r QAResult) Valid() bool {
	switch r {
	case ResultUnset, ResultClean, ResultNeedsAttention:
		return true
	default:
		return false
	}
}

// Finding is one thing the gate noticed. The JSON tags are load-bearing: the
// finding list is persisted as a JSON document in one column, so these names
// are the on-disk format and must not be renamed casually.
type Finding struct {
	// Source is the check that produced the finding, as the gate invoked it
	// ("go build ./...", "go vet ./...", "npm run lint").
	Source string `json:"source"`
	// Signal is the one-line normalized statement of what Source reported. It
	// is what attribution compares against the baseline, so it should be
	// stable across runs (no timestamps, no absolute temp paths).
	Signal string `json:"signal"`
	// Evidence is the verbatim excerpt from the check's output that backs the
	// signal. Kept so a human can judge the finding without re-running
	// anything; the gate never invents it.
	Evidence string `json:"evidence"`
	// Attribution is whether this subject introduced the signal.
	Attribution Attribution `json:"attribution"`
	// Severity is how badly the finding matters.
	Severity Severity `json:"severity"`
}

// Validate reports whether the finding's enum fields are persistable.
func (f Finding) Validate() error {
	if !f.Attribution.Valid() {
		return fmt.Errorf("postrunqa: invalid attribution %q", f.Attribution)
	}
	if !f.Severity.Valid() {
		return fmt.Errorf("postrunqa: invalid severity %q", f.Severity)
	}
	return nil
}

// QARun is one durable pass of the gate over one subject: the envelope that
// has to survive a daemon restart for the gate to be resumable at all.
type QARun struct {
	// ID identifies this pass. A subject re-entering the gate gets a new
	// QARun rather than overwriting the previous one, so the history of what
	// the gate decided about a subject is not destroyed by a retry.
	ID string
	// SubjectKind and SubjectID name what is being gated. The pair is
	// polymorphic across two lifecycle tables, so it carries no foreign key.
	SubjectKind SubjectKind
	SubjectID   string
	// Phase is where this pass currently stands.
	Phase QAPhase
	// Findings is everything the checks reported this pass, including
	// baseline-attributed findings — they are the evidence for *not* blocking,
	// and dropping them would make that decision unauditable later.
	Findings []Finding
	// RepairCycleCount is how many automated repair attempts this pass has
	// already spent — the "n" in AutoFixing(n). MaxRepairCycles is the budget
	// it is checked against; see DefaultMaxRepairCycles.
	RepairCycleCount int
	MaxRepairCycles  int
	// Result is the verdict; ResultUnset until the pass reaches a terminal
	// phase.
	Result QAResult
	// StartedAt is when the pass was created. CompletedAt is nil until it
	// reaches a terminal phase.
	StartedAt   time.Time
	CompletedAt *time.Time
}

// WithDefaults returns the run with its unset fields resolved to the documented
// defaults: a zero Phase becomes PhasePending, and a zero (or negative)
// MaxRepairCycles becomes DefaultMaxRepairCycles. It is applied on both write
// and read, so a row written by an older build that predates a default — or a
// caller that simply did not fill the field in — still loads as a usable
// envelope instead of a run with a zero repair budget that can never fix
// anything.
func (r QARun) WithDefaults() QARun {
	if r.Phase == "" {
		r.Phase = PhasePending
	}
	if r.MaxRepairCycles <= 0 {
		r.MaxRepairCycles = DefaultMaxRepairCycles
	}
	if r.RepairCycleCount < 0 {
		r.RepairCycleCount = 0
	}
	return r
}

// RepairBudgetExhausted reports whether the run has spent its repair budget.
// The gate calls this instead of comparing counts at the call site so the
// "one more cycle allowed?" question has exactly one answer in the codebase.
func (r QARun) RepairBudgetExhausted() bool {
	return r.RepairCycleCount >= r.WithDefaults().MaxRepairCycles
}

// Validate reports whether the run is persistable. Callers should apply
// WithDefaults first; Validate does not fill anything in, it only rejects.
func (r QARun) Validate() error {
	if r.ID == "" {
		return fmt.Errorf("postrunqa: qa run id is required")
	}
	if !r.SubjectKind.Valid() {
		return fmt.Errorf("postrunqa: invalid subject kind %q", r.SubjectKind)
	}
	if r.SubjectID == "" {
		return fmt.Errorf("postrunqa: subject id is required")
	}
	if !r.Phase.Valid() {
		return fmt.Errorf("postrunqa: invalid phase %q", r.Phase)
	}
	if !r.Result.Valid() {
		return fmt.Errorf("postrunqa: invalid result %q", r.Result)
	}
	if r.MaxRepairCycles < 0 || r.RepairCycleCount < 0 {
		return fmt.Errorf("postrunqa: repair cycle counts must not be negative")
	}
	if r.StartedAt.IsZero() {
		return fmt.Errorf("postrunqa: started_at is required")
	}
	for i, f := range r.Findings {
		if err := f.Validate(); err != nil {
			return fmt.Errorf("postrunqa: finding %d: %w", i, err)
		}
	}
	return nil
}

// Store is the durable persistence contract for the gate. It is declared here,
// next to the state it stores, and implemented by the daemon's existing
// lifecycle store (internal/storage/sqlite/store) — the gate introduces no
// storage of its own.
type Store interface {
	// SaveQARun inserts the run, or updates it in place if a run with the same
	// ID already exists, and returns what was stored.
	SaveQARun(ctx context.Context, run QARun) (QARun, error)
	// LoadQARun reads one run by ID. ok is false, with no error, when no such
	// run exists.
	LoadQARun(ctx context.Context, id string) (QARun, bool, error)
	// LatestQARunForSubject reads the most recently started run for a subject,
	// which is what a restarting daemon needs to decide whether that subject's
	// gate is still open.
	LatestQARunForSubject(ctx context.Context, kind SubjectKind, subjectID string) (QARun, bool, error)
}
