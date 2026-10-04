package workflow

import (
	stdctx "context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

// evidence_snapshot.go — the one bounded evidence snapshot AO collects before
// it is allowed to say "I cannot prove what this worker is doing".
//
// The incident shape this exists for: every ambiguous_worker_state stop AO has
// ever written was correct in the narrow sense (AO really could not prove the
// thing it was asserting) and useless in the practical sense, because the
// checkpoint recorded the CONCLUSION and nothing else. A person reading
//
//	worker idle with no verifiable change — needs human review
//
// learns that AO looked at exactly one fact (a git observation) and nothing
// about the eleven other facts AO was already holding: whether the launch had
// even completed, whether the session was alive, which branch and HEAD the
// worker was on, whether any AO-owned mutation had been attributed to it,
// which artifacts the plan expected, or what the parent run was doing. Every
// one of those was durably available at the moment of the stop and none of
// them travelled with it.
//
// So the snapshot is:
//
//   - SINGLE — one collector, one shape, one place to add a fact to. There is
//     no second, thinner evidence gatherer for a different caller.
//   - BOUNDED — a hard byte ceiling, per-field caps and per-list caps, all
//     recorded in the snapshot itself, so an evidence bundle can never become
//     the reason a stop is expensive.
//   - SERIALIZABLE — it is written to the run's own ledger as JSON and is read
//     back by the Incident Advisor, so what a person is shown after a restart
//     is the evidence AO actually stood on, not a re-derivation of it.
//   - HONEST — every field carries its own status. A fact AO holds is
//     `observed`. A fact AO could not READ (a port that is not wired, a store
//     that predates the columns, a read that failed) is `unavailable` and says
//     which. A change AO has no provenance record for is `unattributed` — it
//     is never given the most plausible owner, because the most plausible
//     owner is exactly what a person would then act on.
//
// Note what "durable facts only" means here, and how strictly. The collector
// performs NO live reads at all: it does not probe a runtime, it does not shell
// out to git, it opens no port. Every field it fills comes from a row that was
// already written down.
//
// That is a stronger rule than "do not guess", and it is the right one. A live
// probe can be perfectly honest at the instant it answers and still be a fact
// AO cannot produce again: the daemon restarts, the pane is gone, the worktree
// is deleted, and the snapshot the stop was authorized on can no longer be
// reconstructed or checked. An answer that only exists in memory is not
// evidence, it is a memory of evidence.
//
// So the two live readings AO can take — the runtime liveness probe and the git
// worktree — are taken by the CALLER, persisted as an ObservedWorkerFacts row
// BEFORE the raise is authorized, and read back here from that row. Nothing
// recorded means `unavailable`, never an inference: silence is never converted
// into a fact anywhere in this file.

const (
	// EvidenceSnapshotVersion identifies the shape of a serialized snapshot.
	EvidenceSnapshotVersion = "worker-evidence/v1"

	// evidenceMaxSerializedBytes is the whole-snapshot ceiling, and it binds
	// the thing that actually exists: len(JSON()).
	//
	// It used to bind an internal tally of value+note lengths, which is not the
	// artifact. That tally excluded every field key and label, every status
	// string, every section key and title, the version, the ids, the digest,
	// the timestamps, the truncation lists, and all of JSON's own punctuation
	// and escaping — well over half of a real snapshot. A "5 KB ceiling" that
	// admitted a 12 KB row is not a bound, it is a number next to a row, and
	// the row is what gets written to the ledger and carried into a prompt.
	//
	// The figure is NOT comparable to the old one, because it measures a
	// different thing. A complete, ordinary snapshot — every section filled,
	// nothing pathological — serializes to roughly 5 KB, so the old "5 KB"
	// constant was in truth admitting about twice that. This is sized so a
	// complete ordinary snapshot fits whole, with room to spare, while a
	// pathological one (seventy fields each at the 1 KB per-field cap would be
	// ~72 KB) is still cut down hard. Preserving the useful evidence is the
	// point of a budget; a ceiling that mangles every normal snapshot is not a
	// tighter bound, it is a broken one.
	evidenceMaxSerializedBytes = 16 * 1024
	// evidenceMaxFieldBytes bounds any single field's value.
	evidenceMaxFieldBytes = 1024
	// evidenceMaxTruncatedKeys bounds the two accounting lists, which are
	// serialized too and would otherwise be the one unbounded thing left in a
	// snapshot whose whole point is being bounded.
	evidenceMaxTruncatedKeys = 12
	// evidenceDigestHexLen is the width of a sha256 hex digest. The budget pass
	// measures with a placeholder of exactly this width, so stamping the real
	// digest afterwards cannot change the serialized length.
	evidenceDigestHexLen = 64
	// evidenceMaxIdentifierBytes bounds each TOP-LEVEL identifier: the run,
	// step, attempt and session ids.
	//
	// They sit outside Sections, so the budget pass — which works by dropping
	// fields and then whole sections — could never reach them. With an
	// oversized id the loop would run out of things to drop and still be over
	// the ceiling, which is not a bound at all. Capping them is what makes the
	// floor of a fully-drained snapshot provably small.
	//
	// 128 bytes is far more than any real id needs (a run id is ~40) so
	// ordinary snapshots are untouched, and small enough that even a
	// pathological id made entirely of characters JSON must escape six-fold
	// leaves the drained floor at a couple of kilobytes.
	evidenceMaxIdentifierBytes = 128
	// evidenceIdentifierTruncationMark is appended to a capped identifier, so a
	// reader is never shown a prefix that looks like a whole id. It is counted
	// inside the cap, never added on top of it.
	evidenceIdentifierTruncationMark = "…[truncated]"
	// The list caps. Each one bounds a source whose natural size is unbounded.
	evidenceMaxCheckpoints       = 12
	evidenceMaxDispatchRecords   = 8
	evidenceMaxProvenanceRecords = 8
	evidenceMaxChangedFiles      = 25
	evidenceMaxArtifacts         = 20
)

// Section keys. They are exported because tests, the Advisor's renderer and
// any future reader all address fields by key rather than by title text.
const (
	EvidenceSectionWorkflow     = "workflow"
	EvidenceSectionStep         = "step"
	EvidenceSectionAttempt      = "attempt"
	EvidenceSectionSession      = "session"
	EvidenceSectionLiveness     = "liveness"
	EvidenceSectionLaunch       = "launch"
	EvidenceSectionGit          = "git"
	EvidenceSectionFingerprints = "fingerprints"
	EvidenceSectionProvenance   = "provenance"
	EvidenceSectionArtifacts    = "artifacts"
	EvidenceSectionResult       = "result"
	EvidenceSectionCheckpoints  = "checkpoints"
	EvidenceSectionRelationship = "relationship"
)

// EvidenceStatus is what AO is claiming about one field.
type EvidenceStatus string

const (
	// EvidenceObserved means AO holds this fact. The ABSENCE of an optional
	// thing is itself observed — "no parent run" and "no first hook signal has
	// ever arrived" are facts, not gaps.
	EvidenceObserved EvidenceStatus = "observed"
	// EvidenceUnavailable means AO could not READ the fact here. The note says
	// which port, store capability or read failed. It is never a synonym for
	// "the thing is not there".
	EvidenceUnavailable EvidenceStatus = "unavailable"
	// EvidenceUnattributed means a change exists and AO holds no provenance
	// record naming its owner. Reserved for attribution, and it is the only
	// honest answer there: an unattributed change given a plausible owner is how
	// a person ends up reverting the wrong work.
	EvidenceUnattributed EvidenceStatus = "unattributed"
)

// EvidenceField is one fact, with the claim AO is making about it.
type EvidenceField struct {
	Key    string         `json:"key"`
	Label  string         `json:"label"`
	Value  string         `json:"value,omitempty"`
	Status EvidenceStatus `json:"status"`
	// Note explains a non-observed field: which durable fact was absent, or
	// which capability AO does not have here.
	Note string `json:"note,omitempty"`
	// Truncated records that Value is a prefix, so nothing reads a capped
	// value as a complete one.
	Truncated bool `json:"truncated,omitempty"`
}

// Available reports whether this field carries a fact AO actually obtained.
func (f EvidenceField) Available() bool { return f.Status == EvidenceObserved }

// EvidenceSection is one titled group of fields.
type EvidenceSection struct {
	Key    string          `json:"key"`
	Title  string          `json:"title"`
	Fields []EvidenceField `json:"fields"`
}

// WorkerEvidenceSnapshot is the bounded, serializable correlation of everything
// AO durably knows about one step of one run at one instant.
//
// The `collected` marker is unexported and is set ONLY by CollectWorkerEvidence.
// It is what makes the ambiguous_worker_state raise path enforceable rather
// than merely conventional (see ambiguous_worker_state.go), and it is
// deliberately lost by a JSON round-trip: a snapshot read back off the ledger
// is evidence to SHOW a person, never authority to raise a fresh stop with.
type WorkerEvidenceSnapshot struct {
	Version     string    `json:"version"`
	CollectedAt time.Time `json:"collectedAt"`

	RunID     string `json:"runId"`
	StepID    string `json:"stepId,omitempty"`
	AttemptID string `json:"attemptId,omitempty"`
	SessionID string `json:"sessionId,omitempty"`

	Sections []EvidenceSection `json:"sections"`

	// Budget accounting, recorded so the snapshot's own cost is a fact.
	//
	// Bytes is len(JSON()) — the real serialized length of this very value,
	// not a tally of its parts — and MaxBytes is the ceiling that length is
	// held under.
	Bytes    int `json:"bytes"`
	MaxBytes int `json:"maxBytes"`
	// Truncated names fields whose VALUE was capped (bounded list).
	Truncated []string `json:"truncated,omitempty"`
	// DroppedForBudget names fields removed entirely to fit the ceiling, and
	// DroppedForBudgetCount is how many there were in total. The list is
	// bounded and the count is not, so a reader is never told a smaller number
	// than the truth even when the names themselves had to be cut.
	DroppedForBudget      []string `json:"droppedForBudget,omitempty"`
	DroppedForBudgetCount int      `json:"droppedForBudgetCount,omitempty"`
	// Digest identifies this exact evidence.
	Digest string `json:"digest"`

	collected bool
}

// Collected reports whether this value came from CollectWorkerEvidence in this
// process. A zero value, a hand-built literal and a deserialized snapshot all
// report false.
func (s WorkerEvidenceSnapshot) Collected() bool { return s.collected }

// Field returns one field by key.
func (s WorkerEvidenceSnapshot) Field(key string) (EvidenceField, bool) {
	for _, sec := range s.Sections {
		for _, f := range sec.Fields {
			if f.Key == key {
				return f, true
			}
		}
	}
	return EvidenceField{}, false
}

// Fields returns every field, in section order.
func (s WorkerEvidenceSnapshot) Fields() []EvidenceField {
	out := make([]EvidenceField, 0, len(s.Sections)*6)
	for _, sec := range s.Sections {
		out = append(out, sec.Fields...)
	}
	return out
}

// UnavailableKeys names every field AO could not read. An empty result is the
// property the Advisor's "nothing is reported as missing" test asserts.
func (s WorkerEvidenceSnapshot) UnavailableKeys() []string {
	var out []string
	for _, f := range s.Fields() {
		if f.Status == EvidenceUnavailable {
			out = append(out, f.Key)
		}
	}
	return out
}

// UnattributedKeys names every field AO refused to attribute.
func (s WorkerEvidenceSnapshot) UnattributedKeys() []string {
	var out []string
	for _, f := range s.Fields() {
		if f.Status == EvidenceUnattributed {
			out = append(out, f.Key)
		}
	}
	return out
}

// Render turns the snapshot into the text a person (or an agent) reads. Every
// field appears, including the ones AO could not obtain: an evidence bundle
// that silently omits what it lacks is how "insufficient evidence" becomes an
// unfalsifiable verdict.
func (s WorkerEvidenceSnapshot) Render() string {
	var b strings.Builder
	fmt.Fprintf(&b, "evidence snapshot %s (collected %s, digest %s)\n",
		s.Version, s.CollectedAt.UTC().Format(time.RFC3339), shortDigest(s.Digest))
	for _, sec := range s.Sections {
		fmt.Fprintf(&b, "\n[%s]\n", sec.Title)
		for _, f := range sec.Fields {
			switch f.Status {
			case EvidenceObserved:
				fmt.Fprintf(&b, "  %s: %s", f.Label, f.Value)
				if f.Truncated {
					b.WriteString(" [truncated — this is a prefix]")
				}
				b.WriteString("\n")
			case EvidenceUnattributed:
				fmt.Fprintf(&b, "  %s: UNATTRIBUTED — %s\n", f.Label, f.Note)
			default:
				fmt.Fprintf(&b, "  %s: NOT AVAILABLE — %s\n", f.Label, f.Note)
			}
		}
	}
	if s.DroppedForBudgetCount > 0 {
		fmt.Fprintf(&b, "\n%d field(s) were removed to fit the snapshot budget", s.DroppedForBudgetCount)
		if len(s.DroppedForBudget) > 0 {
			fmt.Fprintf(&b, ": %s", strings.Join(s.DroppedForBudget, ", "))
			if s.DroppedForBudgetCount > len(s.DroppedForBudget) {
				fmt.Fprintf(&b, " and %d more", s.DroppedForBudgetCount-len(s.DroppedForBudget))
			}
		}
		b.WriteString(".\n")
	}
	fmt.Fprintf(&b, "\nsnapshot size: %d serialized bytes, limit %d.\n", s.Bytes, s.MaxBytes)
	return b.String()
}

// JSON is the durable form written to the ledger.
func (s WorkerEvidenceSnapshot) JSON() string {
	b, err := json.Marshal(s)
	if err != nil {
		return "{}"
	}
	return string(b)
}

// DecodeWorkerEvidenceSnapshot reads a snapshot back off the ledger. The result
// is deliberately NOT Collected(): it is evidence to display, not authority to
// raise a new stop with.
func DecodeWorkerEvidenceSnapshot(raw string) (WorkerEvidenceSnapshot, bool) {
	if strings.TrimSpace(raw) == "" {
		return WorkerEvidenceSnapshot{}, false
	}
	var s WorkerEvidenceSnapshot
	if err := json.Unmarshal([]byte(raw), &s); err != nil {
		return WorkerEvidenceSnapshot{}, false
	}
	if s.Version == "" {
		return WorkerEvidenceSnapshot{}, false
	}
	return s, true
}

// EvidenceRequest names what one snapshot is about.
//
// It carries values rather than ids because every caller already holds them,
// and re-reading the run and the step here would make the snapshot describe a
// slightly later instant than the decision it is evidence for.
type EvidenceRequest struct {
	Run domain.WorkflowRun
	// Step is the step the question is about. The zero value is allowed and
	// produces a snapshot that says so, rather than one that quietly describes
	// a different step.
	Step domain.WorkflowStep
}

// WorkerObservationEvidencePhase is the durable home of the two readings AO can
// only take live: the git worktree and the runtime liveness probe.
//
// One append-only checkpoint per observation, written by whoever paid for it,
// BEFORE anything is authorized on it. It exists so that CollectWorkerEvidence
// can be a pure reader of durable rows: an observation that was never written
// down cannot be shown to anyone after a restart, cannot be compared against a
// later one, and cannot be checked by the person reading the stop it caused.
const WorkerObservationEvidencePhase = "worker_observation_evidence"

// ObservedWorkerFacts is the persisted form of one such observation.
//
// Every field is something a port actually reported. The two `*Known` flags are
// load-bearing: they distinguish "the reading was taken and said no" from "no
// reading was taken", which are different facts with different remedies, and
// collapsing them is how an unobserved worktree becomes a clean one.
type ObservedWorkerFacts struct {
	ObservedAt time.Time `json:"observedAt"`
	SessionID  string    `json:"sessionId,omitempty"`

	// WorkspaceKnown records that a git observation was actually obtained.
	WorkspaceKnown bool     `json:"workspaceKnown"`
	WorktreePath   string   `json:"worktreePath,omitempty"`
	Branch         string   `json:"branch,omitempty"`
	HeadSHA        string   `json:"headSha,omitempty"`
	Dirty          bool     `json:"dirty,omitempty"`
	Staged         bool     `json:"staged,omitempty"`
	Untracked      bool     `json:"untracked,omitempty"`
	ChangedFiles   []string `json:"changedFiles,omitempty"`
	Fingerprint    string   `json:"fingerprint,omitempty"`

	// LivenessKnown records that the runtime probe ANSWERED. False covers both
	// "no probe is wired" and "the probe could not tell", neither of which is
	// ever read as death.
	LivenessKnown bool `json:"livenessKnown"`
	LivenessAlive bool `json:"livenessAlive"`
}

// Empty reports whether this record holds no reading at all, in which case
// there is nothing worth writing down.
func (o ObservedWorkerFacts) Empty() bool { return !o.WorkspaceKnown && !o.LivenessKnown }

// NewObservedWorkspaceFacts turns a git observation a caller already paid for
// into the durable form. It is the only conversion; the raw port value never
// reaches the collector.
func NewObservedWorkspaceFacts(obs ports.WorkspaceObservation) ObservedWorkerFacts {
	facts := ObservedWorkerFacts{
		WorkspaceKnown: true,
		WorktreePath:   obs.Path,
		Branch:         obs.Branch,
		HeadSHA:        obs.HeadSHA,
		Dirty:          obs.Dirty,
		Staged:         obs.Staged,
		Untracked:      obs.Untracked,
		Fingerprint:    WorkspaceFingerprint(obs),
	}
	changes := obs.Changes
	if len(changes) > evidenceMaxChangedFiles {
		changes = changes[:evidenceMaxChangedFiles]
	}
	for _, ch := range changes {
		facts.ChangedFiles = append(facts.ChangedFiles, strings.TrimSpace(ch.Status+" "+ch.Path))
	}
	return facts
}

// DecodeObservedWorkerFacts reads one back off the ledger.
func DecodeObservedWorkerFacts(raw string) (ObservedWorkerFacts, bool) {
	if strings.TrimSpace(raw) == "" {
		return ObservedWorkerFacts{}, false
	}
	var o ObservedWorkerFacts
	if err := json.Unmarshal([]byte(raw), &o); err != nil {
		return ObservedWorkerFacts{}, false
	}
	return o, true
}

// newestObservedWorkerFacts finds the newest recorded observation FOR THIS STEP.
//
// It used to fall back to the run's newest observation when the step had none
// of its own, which is a fabrication with a plausible face on it: a run has a
// work step, a review step and a fix step, each with its own session and — in
// task execution — its own worktree. Handing the fix step's dirty tree to a
// question about the work step produces a snapshot that is complete, internally
// consistent, and about something else. That is worse than an empty one,
// because nothing in it looks wrong.
//
// So: exact WorkflowStepID, never borrowed. A legacy row that carries no step
// id at all is admissible only where it cannot mislead — the question is itself
// run-scoped, or the row names the very session this step ran under, which is
// durable proof that it is the same worker's observation.
func newestObservedWorkerFacts(cps []domain.WorkflowCheckpoint, stepID, sessionID string) (ObservedWorkerFacts, bool) {
	ordered := append([]domain.WorkflowCheckpoint(nil), cps...)
	sort.SliceStable(ordered, func(i, j int) bool { return ordered[i].CreatedAt.After(ordered[j].CreatedAt) })
	for _, cp := range ordered {
		if cp.DurablePhase != WorkerObservationEvidencePhase {
			continue
		}
		facts, ok := DecodeObservedWorkerFacts(cp.RetryState)
		if !ok {
			continue
		}
		if rowStep := deref(cp.WorkflowStepID); rowStep != "" {
			if stepID != "" && rowStep == stepID {
				return facts, true
			}
			continue
		}
		// Legacy, run-scoped row.
		if stepID == "" {
			return facts, true
		}
		if sessionID != "" && facts.SessionID == sessionID {
			return facts, true
		}
	}
	return ObservedWorkerFacts{}, false
}

// durableStepSessionID resolves the session a step ACTUALLY ran under.
//
// workflow_steps.session_id is written by exactly one path — work dispatch — so
// the review and fix steps have none, and reading only the column reports "no
// session" about steps that plainly have one. Their session identity lives on
// their own durable checkpoints instead, which is where this looks second.
//
// Exact WorkflowStepID only. Borrowing a session id from a neighbouring step
// would attach the wrong worker, the wrong liveness answer and the wrong
// worktree to the stop, which is the same fabrication in a different field.
func durableStepSessionID(cps []domain.WorkflowCheckpoint, step domain.WorkflowStep) string {
	if id := deref(step.SessionID); id != "" {
		return id
	}
	if step.ID == "" {
		return ""
	}
	ordered := append([]domain.WorkflowCheckpoint(nil), cps...)
	sort.SliceStable(ordered, func(i, j int) bool { return ordered[i].CreatedAt.After(ordered[j].CreatedAt) })
	for _, cp := range ordered {
		if deref(cp.WorkflowStepID) != step.ID {
			continue
		}
		if id := deref(cp.SessionID); id != "" {
			return id
		}
	}
	return ""
}

// DurableSessionForStep is the coordinator-level form: the session identity a
// step ran under, resolved from its own durable rows. Used by the review and
// fix ambiguity paths so the observation they persist is recorded for the
// session that actually exists rather than for an empty one.
func (c *Coordinator) DurableSessionForStep(ctx stdctx.Context, runID string, step domain.WorkflowStep) domain.SessionID {
	if id := deref(step.SessionID); id != "" {
		return domain.SessionID(id)
	}
	if runID == "" || step.ID == "" {
		return ""
	}
	cps, err := c.store.ListWorkflowCheckpoints(ctx, runID)
	if err != nil {
		return ""
	}
	return domain.SessionID(durableStepSessionID(cps, step))
}

// ProvenanceStore is the optional durable surface migration 0133 added: the
// dispatch checkpoints and the AO-owned mutation provenance.
//
// Type-asserted off the coordinator's Store rather than added to the Store
// interface, following this package's narrow-optional convention: a store (or
// test double) that predates 0133 keeps compiling and simply reports those
// facts as unavailable — which is the correct answer for it, and is very
// different from reporting them as absent.
type ProvenanceStore interface {
	ListWorkflowDispatchCheckpointsByStep(ctx stdctx.Context, stepID string) ([]domain.WorkflowDispatchCheckpoint, error)
	ListWorkflowMutationProvenanceByStep(ctx stdctx.Context, stepID string) ([]domain.WorkflowMutationProvenance, error)
	ListWorkflowMutationProvenanceByRun(ctx stdctx.Context, runID string) ([]domain.WorkflowMutationProvenance, error)
}

func (c *Coordinator) provenanceStore() (ProvenanceStore, bool) {
	ps, ok := c.store.(ProvenanceStore)
	return ps, ok
}

// CollectWorkerEvidence is THE evidence collector. There is exactly one, and
// everything that needs correlated durable facts about a step calls it.
//
// It never returns an error. A failure to read a fact is itself one of the
// facts the snapshot carries, and an evidence collector that could fail would
// simply move the "no evidence" case back to where it started: a stop with a
// conclusion and nothing under it.
func (c *Coordinator) CollectWorkerEvidence(ctx stdctx.Context, req EvidenceRequest) WorkerEvidenceSnapshot {
	b := &evidenceBuilder{}
	snap := WorkerEvidenceSnapshot{
		Version:     EvidenceSnapshotVersion,
		CollectedAt: c.clock(),
		RunID:       truncateIdentifier(req.Run.ID),
		StepID:      truncateIdentifier(req.Step.ID),
		MaxBytes:    evidenceMaxSerializedBytes,
	}
	checkpoints, cpErr := c.checkpointsForEvidence(ctx, req)
	ledger := newestCheckpointGitFacts(checkpoints, req.Step.ID)
	// The session this step actually ran under, from the step row when it has
	// one and from the step's OWN checkpoints when it does not (review and fix
	// steps never get the column). Resolved once, so every section below agrees
	// about whose worker this is. See durableStepSessionID.
	sessionID := durableStepSessionID(checkpoints, req.Step)
	snap.SessionID = truncateIdentifier(sessionID)

	c.evidenceWorkflow(b, req)
	c.evidenceStep(b, req, sessionID)
	attemptID := c.evidenceAttempt(ctx, b, req)
	snap.AttemptID = truncateIdentifier(attemptID)
	sess, sessFound := c.evidenceSession(ctx, b, sessionID)
	observed, observedOK := newestObservedWorkerFacts(checkpoints, req.Step.ID, sessionID)
	c.evidenceLiveness(b, observed, observedOK)
	c.evidenceLaunch(ctx, b, req)
	c.evidenceGit(b, sess, sessFound, observed, observedOK, ledger)
	c.evidenceFingerprints(b, checkpoints, cpErr, req.Step.ID, observed, observedOK)
	c.evidenceProvenance(ctx, b, req)
	c.evidenceArtifacts(ctx, b, req)
	c.evidenceResult(b, req, sess, sessFound, checkpoints, cpErr)
	c.evidenceCheckpoints(b, checkpoints, cpErr)
	c.evidenceRelationship(ctx, b, req)

	snap.Sections = b.sections
	snap.Truncated = b.truncated
	// Order matters and is the whole of the serialized bound: budget against a
	// fixed-width digest placeholder, stamp the real digest (identical width,
	// so the length does not move), then record the length that actually
	// resulted. See applyBudget.
	snap.applyBudget()
	snap.Digest = snap.digest()
	snap.settleBytes()
	snap.collected = true
	return snap
}

// ---- the builder ------------------------------------------------------------

type evidenceBuilder struct {
	sections  []EvidenceSection
	truncated []string
	current   int
}

func (b *evidenceBuilder) section(key, title string) {
	b.sections = append(b.sections, EvidenceSection{Key: key, Title: title})
	b.current = len(b.sections) - 1
}

func (b *evidenceBuilder) add(f EvidenceField) {
	if b.current < 0 || len(b.sections) == 0 {
		return
	}
	if len(f.Value) > evidenceMaxFieldBytes {
		f.Value = f.Value[:evidenceMaxFieldBytes]
		f.Truncated = true
		if len(b.truncated) < evidenceMaxTruncatedKeys {
			b.truncated = append(b.truncated, f.Key)
		}
	}
	b.sections[b.current].Fields = append(b.sections[b.current].Fields, f)
}

// observed records a fact AO holds. An empty value would be a claim about
// nothing, so it is recorded as the explicit fallback the caller supplies.
func (b *evidenceBuilder) observed(key, label, value string) {
	b.add(EvidenceField{Key: key, Label: label, Value: value, Status: EvidenceObserved})
}

// unavailable records a fact AO could not READ, and why.
func (b *evidenceBuilder) unavailable(key, label, note string) {
	b.add(EvidenceField{Key: key, Label: label, Status: EvidenceUnavailable, Note: note})
}

// unattributed records a change AO holds no owner for. Attribution is the one
// question this collector will not answer by inference.
func (b *evidenceBuilder) unattributed(key, label, note string) {
	b.add(EvidenceField{Key: key, Label: label, Status: EvidenceUnattributed, Note: note})
}

// ---- sections ---------------------------------------------------------------

func (c *Coordinator) evidenceWorkflow(b *evidenceBuilder, req EvidenceRequest) {
	b.section(EvidenceSectionWorkflow, "Workflow run")
	b.observed(EvidenceSectionWorkflow+".runId", "run", orNone(req.Run.ID))
	b.observed(EvidenceSectionWorkflow+".state", "state", string(req.Run.State))
	b.observed(EvidenceSectionWorkflow+".project", "project", orNone(req.Run.ProjectID))
	b.observed(EvidenceSectionWorkflow+".objective", "objective", oneLine(orNone(req.Run.Objective)))
	b.observed(EvidenceSectionWorkflow+".policyVersion", "policy version", orValue(req.Run.PolicyVersion, "unset"))
	b.observed(EvidenceSectionWorkflow+".plannedTaskId", "planned task",
		orValue(deref(req.Run.PlannedTaskID), "none — this run has no task decomposition"))
	b.observed(EvidenceSectionWorkflow+".createdAt", "created", stamp(req.Run.CreatedAt, "unknown"))
}

func (c *Coordinator) evidenceStep(b *evidenceBuilder, req EvidenceRequest, sessionID string) {
	b.section(EvidenceSectionStep, "Step")
	if req.Step.ID == "" {
		b.unavailable(EvidenceSectionStep+".id", "step",
			"no step was named for this snapshot, so nothing below is scoped to one")
		return
	}
	b.observed(EvidenceSectionStep+".id", "step", req.Step.ID)
	b.observed(EvidenceSectionStep+".kind", "kind", string(req.Step.Kind))
	b.observed(EvidenceSectionStep+".state", "state", string(req.Step.State))
	b.observed(EvidenceSectionStep+".harness", "assigned harness",
		orValue(req.Step.AssignedHarness, "none assigned"))
	// The resolved identity, with its provenance stated: a review or fix step
	// has no session_id column, so "recovered from this step's checkpoints" and
	// "on the step row" are different facts and a reader deserves to know which.
	switch {
	case deref(req.Step.SessionID) != "":
		b.observed(EvidenceSectionStep+".sessionId", "session", sessionID+" (on the step row)")
	case sessionID != "":
		b.observed(EvidenceSectionStep+".sessionId", "session",
			sessionID+" (recovered from this step's own checkpoints)")
	default:
		b.observed(EvidenceSectionStep+".sessionId", "session",
			"none recorded on this step or on any of its checkpoints")
	}
	b.observed(EvidenceSectionStep+".reviewRunId", "review run",
		orValue(deref(req.Step.ReviewRunID), "none — this step has no review run"))
	b.observed(EvidenceSectionStep+".expectedArtifactsVersion", "expected-artifacts version",
		orValue(req.Step.ExpectedArtifactsVersion, "unset"))
}

func (c *Coordinator) evidenceAttempt(ctx stdctx.Context, b *evidenceBuilder, req EvidenceRequest) string {
	b.section(EvidenceSectionAttempt, "Latest attempt")
	if req.Step.ID == "" {
		b.unavailable(EvidenceSectionAttempt+".id", "attempt", "no step was named, so no attempt can be resolved")
		return ""
	}
	attempt, found, err := c.store.GetLatestWorkflowAttempt(ctx, req.Step.ID)
	if err != nil {
		b.unavailable(EvidenceSectionAttempt+".id", "attempt", "reading the attempt failed: "+err.Error())
		return ""
	}
	if !found {
		b.observed(EvidenceSectionAttempt+".id", "attempt", "none recorded for this step")
		return ""
	}
	b.observed(EvidenceSectionAttempt+".id", "attempt", attempt.ID)
	b.observed(EvidenceSectionAttempt+".number", "attempt number", fmt.Sprint(attempt.AttemptNumber))
	b.observed(EvidenceSectionAttempt+".harness", "harness", orValue(attempt.Harness, "unrecorded"))
	b.observed(EvidenceSectionAttempt+".model", "model", orValue(attempt.Model, "unrecorded"))
	b.observed(EvidenceSectionAttempt+".outcome", "outcome",
		orValue(string(attempt.Outcome), "still in flight — nothing has concluded it"))
	b.observed(EvidenceSectionAttempt+".errorClass", "error class",
		orValue(string(attempt.ErrorClass), "none"))
	b.observed(EvidenceSectionAttempt+".startedAt", "started", stamp(attempt.StartedAt, "unrecorded"))
	b.observed(EvidenceSectionAttempt+".finishedAt", "finished",
		stampPtr(attempt.FinishedAt, "not finished"))
	// A nil deadline is never rendered as a default duration: "no deadline was
	// recorded" and "the deadline was the default" are different facts, and a
	// pre-0133 attempt only supports the first. See domain.WorkflowVerifyWindow.
	b.observed(EvidenceSectionAttempt+".deadlineAt", "deadline",
		stampPtr(attempt.DeadlineAt, "no deadline was recorded for this attempt"))
	b.observed(EvidenceSectionAttempt+".reviewTarget", "review target", renderReviewTarget(attempt.ReviewTarget))
	return attempt.ID
}

func renderReviewTarget(t domain.WorkflowReviewTarget) string {
	if t.Empty() {
		return "none recorded — this attempt is not judging a reviewed artifact"
	}
	parts := []string{}
	if t.ReviewRunID != nil && *t.ReviewRunID != "" {
		parts = append(parts, "review="+*t.ReviewRunID)
	}
	if t.Fingerprint != "" {
		parts = append(parts, "fingerprint="+t.Fingerprint)
	}
	if t.HeadSHA != "" {
		parts = append(parts, "head="+t.HeadSHA)
	}
	return strings.Join(parts, " ")
}

func (c *Coordinator) evidenceSession(
	ctx stdctx.Context, b *evidenceBuilder, sessionID string,
) (domain.SessionRecord, bool) {
	b.section(EvidenceSectionSession, "Session lifecycle")
	if c.sessionFacts == nil {
		b.unavailable(EvidenceSectionSession+".id", "session",
			"no session-facts port is wired into this coordinator")
		return domain.SessionRecord{}, false
	}
	if sessionID == "" {
		b.observed(EvidenceSectionSession+".id", "session",
			"none recorded on this step or on any of its checkpoints")
		return domain.SessionRecord{}, false
	}
	sess, found, err := c.sessionFacts.GetSession(ctx, domain.SessionID(sessionID))
	if err != nil {
		b.unavailable(EvidenceSectionSession+".id", "session", "reading the session failed: "+err.Error())
		return domain.SessionRecord{}, false
	}
	if !found {
		b.observed(EvidenceSectionSession+".id", "session", sessionID+" — the session row is gone")
		b.observed(EvidenceSectionSession+".found", "session row", "absent")
		return domain.SessionRecord{}, false
	}
	b.observed(EvidenceSectionSession+".id", "session", string(sess.ID))
	b.observed(EvidenceSectionSession+".found", "session row", "present")
	b.observed(EvidenceSectionSession+".harness", "harness", orValue(string(sess.Harness), "unrecorded"))
	b.observed(EvidenceSectionSession+".mode", "mode", orValue(string(sess.Mode), "unrecorded"))
	b.observed(EvidenceSectionSession+".activity", "activity state",
		orValue(string(sess.Activity.State), "unrecorded"))
	b.observed(EvidenceSectionSession+".lastActivityAt", "last activity",
		stamp(sess.Activity.LastActivityAt, "never — no activity has ever been reported"))
	// An absent first signal is a fact about the HOOK PIPELINE, never proof the
	// process died. See worker_signal_reconcile.go for the incident that rule
	// comes from; the wording here keeps the two claims apart.
	b.observed(EvidenceSectionSession+".firstSignalAt", "first hook signal",
		stamp(sess.FirstSignalAt, "never — no hook callback has arrived (a fact about the hook pipeline, not the process)"))
	b.observed(EvidenceSectionSession+".turnCompletedAt", "turn completed",
		stamp(sess.TurnCompletedAt, "no turn completion has been reported for the current turn"))
	b.observed(EvidenceSectionSession+".terminated", "terminated", fmt.Sprint(sess.IsTerminated))
	b.observed(EvidenceSectionSession+".branch", "session branch",
		orValue(sess.Metadata.Branch, "none recorded"))
	b.observed(EvidenceSectionSession+".workspacePath", "session workspace",
		orValue(sess.Metadata.WorkspacePath, "none recorded"))
	return sess, true
}

// evidenceLiveness reports the RECORDED liveness answer. It never probes: a
// probe taken here would be a fact only this process ever held, and the whole
// point of the snapshot is that a person can read the stop's evidence back
// after a restart. Whoever pays for a probe persists it first — see
// ObservedWorkerFacts.
func (c *Coordinator) evidenceLiveness(b *evidenceBuilder, observed ObservedWorkerFacts, observedOK bool) {
	b.section(EvidenceSectionLiveness, "Process liveness")
	switch {
	case !observedOK:
		b.unavailable(EvidenceSectionLiveness+".runtime", "runtime process",
			"no observation has been recorded for this step; AO does not probe a runtime from "+
				"inside the collector and never infers liveness from silence")
	case !observed.LivenessKnown:
		b.unavailable(EvidenceSectionLiveness+".runtime", "runtime process",
			"the recorded observation carries no liveness answer (no probe wired, or the probe "+
				"could not tell); an unanswered probe is never read as death")
	case observed.LivenessAlive:
		b.observed(EvidenceSectionLiveness+".runtime", "runtime process",
			"alive (probed "+stamp(observed.ObservedAt, "at an unrecorded time")+")")
	default:
		b.observed(EvidenceSectionLiveness+".runtime", "runtime process",
			"not running (probed "+stamp(observed.ObservedAt, "at an unrecorded time")+")")
	}
}

func (c *Coordinator) evidenceLaunch(ctx stdctx.Context, b *evidenceBuilder, req EvidenceRequest) {
	b.section(EvidenceSectionLaunch, "Harness launch / exit")
	ps, ok := c.provenanceStore()
	if !ok {
		b.unavailable(EvidenceSectionLaunch+".latest", "latest dispatch boundary",
			"this store cannot read dispatch checkpoints (it predates migration 0133)")
		return
	}
	if req.Step.ID == "" {
		b.unavailable(EvidenceSectionLaunch+".latest", "latest dispatch boundary",
			"no step was named, so no dispatch boundary can be resolved")
		return
	}
	records, err := ps.ListWorkflowDispatchCheckpointsByStep(ctx, req.Step.ID)
	if err != nil {
		b.unavailable(EvidenceSectionLaunch+".latest", "latest dispatch boundary",
			"reading the dispatch checkpoints failed: "+err.Error())
		return
	}
	if len(records) == 0 {
		b.observed(EvidenceSectionLaunch+".latest", "latest dispatch boundary",
			"none recorded for this step")
		return
	}
	latest := records[len(records)-1]
	b.observed(EvidenceSectionLaunch+".latest", "latest dispatch boundary", string(latest.Phase))
	b.observed(EvidenceSectionLaunch+".stage", "launch stage", string(latest.LaunchStage))
	b.observed(EvidenceSectionLaunch+".outcome", "launch outcome", string(latest.LaunchOutcome))
	b.observed(EvidenceSectionLaunch+".proven", "outcome proven", fmt.Sprint(latest.LaunchOutcome.Proven()))
	b.observed(EvidenceSectionLaunch+".harness", "launched harness", orValue(latest.Harness, "unrecorded"))
	b.observed(EvidenceSectionLaunch+".sessionId", "launched session",
		orValue(deref(latest.SessionID), "none recorded"))
	b.observed(EvidenceSectionLaunch+".idempotencyKey", "outbox key",
		orValue(latest.IdempotencyKey, "unrecorded"))
	b.observed(EvidenceSectionLaunch+".errorClass", "launch error class",
		orValue(string(latest.ErrorClass), "none"))
	b.observed(EvidenceSectionLaunch+".detail", "launch detail", oneLine(orValue(latest.Detail, "none recorded")))
	b.observed(EvidenceSectionLaunch+".history", "dispatch history", renderDispatchHistory(records))
}

func renderDispatchHistory(records []domain.WorkflowDispatchCheckpoint) string {
	if len(records) > evidenceMaxDispatchRecords {
		records = records[len(records)-evidenceMaxDispatchRecords:]
	}
	lines := make([]string, 0, len(records))
	for _, r := range records {
		lines = append(lines, fmt.Sprintf("%s %s/%s -> %s",
			r.CreatedAt.UTC().Format(time.RFC3339), r.Phase, r.LaunchStage, r.LaunchOutcome))
	}
	return strings.Join(lines, "; ")
}

func (c *Coordinator) evidenceGit(
	b *evidenceBuilder,
	sess domain.SessionRecord, sessFound bool,
	observed ObservedWorkerFacts, observedOK bool,
	ledger checkpointGitFacts,
) {
	b.section(EvidenceSectionGit, "Git branch / HEAD / status")
	obsOK := observedOK && observed.WorkspaceKnown

	branch := firstNonEmpty(pick(obsOK, observed.Branch), ledger.Branch, pick(sessFound, sess.Metadata.Branch))
	if branch == "" {
		b.unavailable(EvidenceSectionGit+".branch", "branch",
			"no branch was recorded on this step's session, checkpoints or observation")
	} else {
		b.observed(EvidenceSectionGit+".branch", "branch", branch)
	}

	head := firstNonEmpty(pick(obsOK, observed.HeadSHA), ledger.HeadSHA)
	if head == "" {
		b.unavailable(EvidenceSectionGit+".headSha", "HEAD",
			"no HEAD SHA was observed and none is recorded on this step's checkpoints")
	} else {
		b.observed(EvidenceSectionGit+".headSha", "HEAD", head)
	}

	if ledger.BaseSHA != "" {
		b.observed(EvidenceSectionGit+".baseSha", "dispatch base", ledger.BaseSHA)
	} else {
		b.unavailable(EvidenceSectionGit+".baseSha", "dispatch base",
			"no base SHA is recorded on any of this step's checkpoints")
	}

	path := firstNonEmpty(pick(obsOK, observed.WorktreePath), pick(sessFound, sess.Metadata.WorkspacePath), ledger.WorktreePath)
	if path == "" {
		b.unavailable(EvidenceSectionGit+".worktreePath", "worktree",
			"no worktree path was recorded on this step's session or checkpoints")
	} else {
		b.observed(EvidenceSectionGit+".worktreePath", "worktree", path)
	}

	if !obsOK {
		const why = "no workspace observation has been recorded for this step; AO does not " +
			"shell out to git from inside the collector, and never reads an unobserved tree as a clean one"
		b.unavailable(EvidenceSectionGit+".status", "git status", why)
		b.unavailable(EvidenceSectionGit+".changedFiles", "changed files", why)
		return
	}
	b.observed(EvidenceSectionGit+".status", "git status", fmt.Sprintf(
		"dirty=%t staged=%t untracked=%t changes=%d (observed %s)",
		observed.Dirty, observed.Staged, observed.Untracked, len(observed.ChangedFiles),
		stamp(observed.ObservedAt, "at an unrecorded time")))
	b.observed(EvidenceSectionGit+".changedFiles", "changed files",
		renderBoundedList(observed.ChangedFiles, "none — the observed tree had no reported change"))
}

func (c *Coordinator) evidenceFingerprints(
	b *evidenceBuilder,
	checkpoints []domain.WorkflowCheckpoint, cpErr error, stepID string,
	observed ObservedWorkerFacts, observedOK bool,
) {
	b.section(EvidenceSectionFingerprints, "Workspace fingerprints")
	if cpErr != nil {
		b.unavailable(EvidenceSectionFingerprints+".recorded", "recorded fingerprint",
			"reading the run's checkpoints failed: "+cpErr.Error())
	} else {
		before, after := newestFingerprints(checkpoints, stepID)
		b.observed(EvidenceSectionFingerprints+".addressed", "fingerprint under repair",
			orValue(before, "none recorded on this step's ledger"))
		b.observed(EvidenceSectionFingerprints+".recorded", "newest certified fingerprint",
			orValue(after, "none recorded on this step's ledger"))
	}
	if !observedOK || !observed.WorkspaceKnown || observed.Fingerprint == "" {
		b.unavailable(EvidenceSectionFingerprints+".observed", "observed fingerprint",
			"no recorded workspace observation carries a fingerprint for this step")
		return
	}
	b.observed(EvidenceSectionFingerprints+".observed", "observed fingerprint", observed.Fingerprint)
}

func (c *Coordinator) evidenceProvenance(ctx stdctx.Context, b *evidenceBuilder, req EvidenceRequest) {
	b.section(EvidenceSectionProvenance, "AO-owned mutation provenance")
	ps, ok := c.provenanceStore()
	if !ok {
		b.unavailable(EvidenceSectionProvenance+".owner", "attributed owner",
			"this store cannot read mutation provenance (it predates migration 0133)")
		return
	}
	// Exact step, or nothing. Reading the RUN's provenance and only narrowing to
	// the step when step-scoped rows happened to exist is how the fix step's
	// authorized mutation came to be reported as the work step's: a real record,
	// a real owner, attached to the wrong question. An absent step-scoped record
	// is `unattributed`, which is the honest answer and the one that does not
	// send anybody to revert somebody else's change.
	var records []domain.WorkflowMutationProvenance
	var err error
	scope := "this step"
	if req.Step.ID != "" {
		records, err = ps.ListWorkflowMutationProvenanceByStep(ctx, req.Step.ID)
	} else {
		scope = "this run"
		records, err = ps.ListWorkflowMutationProvenanceByRun(ctx, req.Run.ID)
	}
	if err != nil {
		b.unavailable(EvidenceSectionProvenance+".owner", "attributed owner",
			"reading the mutation provenance failed: "+err.Error())
		return
	}
	if len(records) == 0 {
		// The whole point of this branch. AO holds no record of who changed
		// this workspace, and the plausible owner — this run's own worker — is
		// exactly the attribution that would send a person to revert the wrong
		// work. So the field says unattributed and names nobody.
		note := "no AO-owned mutation provenance is recorded for " + scope +
			"; AO cannot say whose change this is and will not guess"
		b.unattributed(EvidenceSectionProvenance+".owner", "attributed owner", note)
		b.unattributed(EvidenceSectionProvenance+".class", "mutation class", note)
		b.unattributed(EvidenceSectionProvenance+".reason", "mutation reason", note)
		return
	}
	latest := records[len(records)-1]
	owner := orValue(latest.Harness, "unrecorded harness")
	if sid := deref(latest.SessionID); sid != "" {
		owner += " / session " + sid
	}
	b.observed(EvidenceSectionProvenance+".owner", "attributed owner", owner)
	b.observed(EvidenceSectionProvenance+".class", "mutation class", string(latest.Class))
	b.observed(EvidenceSectionProvenance+".authorized", "AO-authorized", fmt.Sprint(latest.Class.Authorized()))
	b.observed(EvidenceSectionProvenance+".branch", "mutated branch", orValue(latest.Branch, "unrecorded"))
	b.observed(EvidenceSectionProvenance+".worktreePath", "mutated worktree", orValue(latest.WorktreePath, "unrecorded"))
	b.observed(EvidenceSectionProvenance+".shas", "base -> head",
		orValue(latest.BaseSHA, "unrecorded")+" -> "+orValue(latest.HeadSHA, "unrecorded"))
	b.observed(EvidenceSectionProvenance+".fingerprints", "fingerprint before -> after",
		orValue(latest.FingerprintBefore, "unrecorded")+" -> "+orValue(latest.FingerprintAfter, "unrecorded"))
	b.observed(EvidenceSectionProvenance+".reason", "mutation reason", oneLine(orValue(latest.Reason, "unrecorded")))
	b.observed(EvidenceSectionProvenance+".observedAt", "observed at",
		stampPtr(latest.ObservedAt, "not recorded — the writer could not honestly say when"))
	b.observed(EvidenceSectionProvenance+".history", "provenance history", renderProvenanceHistory(records))
}

func renderProvenanceHistory(records []domain.WorkflowMutationProvenance) string {
	if len(records) > evidenceMaxProvenanceRecords {
		records = records[len(records)-evidenceMaxProvenanceRecords:]
	}
	lines := make([]string, 0, len(records))
	for _, r := range records {
		lines = append(lines, fmt.Sprintf("%s %s by %s",
			r.CreatedAt.UTC().Format(time.RFC3339), r.Class, orValue(r.Harness, "unrecorded")))
	}
	return strings.Join(lines, "; ")
}

func (c *Coordinator) evidenceArtifacts(ctx stdctx.Context, b *evidenceBuilder, req EvidenceRequest) {
	b.section(EvidenceSectionArtifacts, "Expected artifacts")
	artifact, err := c.planArtifactForRun(ctx, req.Run)
	if err != nil {
		b.unavailable(EvidenceSectionArtifacts+".files", "expected files",
			"reading the plan artifact failed: "+err.Error())
		b.unavailable(EvidenceSectionArtifacts+".commands", "expected commands",
			"reading the plan artifact failed: "+err.Error())
		return
	}
	b.observed(EvidenceSectionArtifacts+".version", "expected-artifacts version",
		orValue(req.Step.ExpectedArtifactsVersion, "unset"))
	b.observed(EvidenceSectionArtifacts+".files", "expected files", renderExpectedFiles(artifact.Verification.Files))
	b.observed(EvidenceSectionArtifacts+".commands", "expected commands", renderExpectedCommands(artifact.Verification.Commands))
	b.observed(EvidenceSectionArtifacts+".acceptanceCriteria", "acceptance criteria",
		renderBoundedList(artifact.AcceptanceCriteria, "none declared"))
}

func renderExpectedFiles(files []VerificationFileCheck) string {
	if len(files) == 0 {
		return "none declared by the plan"
	}
	out := make([]string, 0, len(files))
	for _, f := range files {
		out = append(out, f.Path)
	}
	return renderBoundedList(out, "none declared by the plan")
}

func renderExpectedCommands(cmds []VerificationCommandCheck) string {
	if len(cmds) == 0 {
		return "none declared by the plan"
	}
	out := make([]string, 0, len(cmds))
	for _, cmd := range cmds {
		out = append(out, strings.TrimSpace(cmd.Command+" "+strings.Join(cmd.Args, " ")))
	}
	return renderBoundedList(out, "none declared by the plan")
}

func renderBoundedList(items []string, empty string) string {
	if len(items) == 0 {
		return empty
	}
	more := 0
	if len(items) > evidenceMaxArtifacts {
		more = len(items) - evidenceMaxArtifacts
		items = items[:evidenceMaxArtifacts]
	}
	joined := strings.Join(items, "; ")
	if more > 0 {
		joined += fmt.Sprintf("; … (%d more)", more)
	}
	return oneLine(joined)
}

func (c *Coordinator) evidenceResult(
	b *evidenceBuilder, req EvidenceRequest,
	sess domain.SessionRecord, sessFound bool,
	checkpoints []domain.WorkflowCheckpoint, cpErr error,
) {
	b.section(EvidenceSectionResult, "Worker result / final signal")
	if cpErr != nil {
		b.unavailable(EvidenceSectionResult+".progress", "last observed progress",
			"reading the run's checkpoints failed: "+cpErr.Error())
	} else {
		phase, action := newestWorkerObservation(checkpoints, req.Step.ID)
		b.observed(EvidenceSectionResult+".progress", "last observed progress",
			orValue(phase, "no worker observation has been recorded for this step"))
		b.observed(EvidenceSectionResult+".nextAction", "last recorded next action",
			oneLine(orValue(action, "none recorded")))
	}
	if !sessFound {
		b.unavailable(EvidenceSectionResult+".finalSignal", "final turn signal",
			"there is no session row to read a turn boundary from")
		return
	}
	b.observed(EvidenceSectionResult+".finalSignal", "final turn signal",
		stamp(sess.TurnCompletedAt, "none — the worker has not reported that its turn ended"))
}

func (c *Coordinator) evidenceCheckpoints(
	b *evidenceBuilder, checkpoints []domain.WorkflowCheckpoint, cpErr error,
) {
	b.section(EvidenceSectionCheckpoints, "Recent checkpoints")
	if cpErr != nil {
		b.unavailable(EvidenceSectionCheckpoints+".recent", "recent checkpoints",
			"reading the run's checkpoints failed: "+cpErr.Error())
		return
	}
	if len(checkpoints) == 0 {
		b.observed(EvidenceSectionCheckpoints+".recent", "recent checkpoints",
			"none recorded for this run")
		return
	}
	ordered := append([]domain.WorkflowCheckpoint(nil), checkpoints...)
	sort.SliceStable(ordered, func(i, j int) bool { return ordered[i].CreatedAt.After(ordered[j].CreatedAt) })
	if len(ordered) > evidenceMaxCheckpoints {
		ordered = ordered[:evidenceMaxCheckpoints]
	}
	lines := make([]string, 0, len(ordered))
	for _, cp := range ordered {
		lines = append(lines, fmt.Sprintf("%s %s", cp.CreatedAt.UTC().Format(time.RFC3339), cp.DurablePhase))
	}
	b.observed(EvidenceSectionCheckpoints+".recent", "recent checkpoints", strings.Join(lines, "; "))
	b.observed(EvidenceSectionCheckpoints+".total", "checkpoints on this run", fmt.Sprint(len(checkpoints)))
}

func (c *Coordinator) evidenceRelationship(ctx stdctx.Context, b *evidenceBuilder, req EvidenceRequest) {
	b.section(EvidenceSectionRelationship, "Parent / child relationship")
	parentID := deref(req.Run.ParentWorkflowID)
	if parentID == "" {
		b.observed(EvidenceSectionRelationship+".parentRunId", "parent run",
			"none — this is a top-level run")
		b.observed(EvidenceSectionRelationship+".parentState", "parent state", "not applicable")
	} else {
		b.observed(EvidenceSectionRelationship+".parentRunId", "parent run", parentID)
		parent, found, err := c.store.GetWorkflowRun(ctx, parentID)
		switch {
		case err != nil:
			b.unavailable(EvidenceSectionRelationship+".parentState", "parent state",
				"reading the parent run failed: "+err.Error())
		case !found:
			b.observed(EvidenceSectionRelationship+".parentState", "parent state",
				"the parent run row is gone")
		default:
			b.observed(EvidenceSectionRelationship+".parentState", "parent state", string(parent.State))
		}
	}
	as, ok := c.archiveStore()
	if !ok {
		b.unavailable(EvidenceSectionRelationship+".children", "child runs",
			"this store cannot list child runs")
		return
	}
	children, err := as.ListChildWorkflowRuns(ctx, req.Run.ID)
	if err != nil {
		b.unavailable(EvidenceSectionRelationship+".children", "child runs",
			"listing the child runs failed: "+err.Error())
		return
	}
	if len(children) == 0 {
		b.observed(EvidenceSectionRelationship+".children", "child runs", "none — this run has no children")
		return
	}
	lines := make([]string, 0, len(children))
	for _, child := range children {
		lines = append(lines, child.ID+"="+string(child.State))
	}
	b.observed(EvidenceSectionRelationship+".children", "child runs", renderBoundedList(lines, "none"))
}

// ---- shared helpers ---------------------------------------------------------

func (c *Coordinator) checkpointsForEvidence(ctx stdctx.Context, req EvidenceRequest) ([]domain.WorkflowCheckpoint, error) {
	if req.Run.ID == "" {
		return nil, nil
	}
	return c.store.ListWorkflowCheckpoints(ctx, req.Run.ID)
}

// checkpointGitFacts is the workspace identity a step's ledger carries.
//
// Each field is the NEWEST NON-EMPTY value across the step's checkpoints, not
// the newest checkpoint's value. The difference is the whole of it: a
// bookkeeping row written after a dispatch (an attention stop, a reconciliation
// note) carries no branch or base SHA, and reading only the newest row would
// report facts AO plainly holds as missing the moment one of those lands.
type checkpointGitFacts struct {
	Branch       string
	WorktreePath string
	BaseSHA      string
	HeadSHA      string
}

func newestCheckpointGitFacts(cps []domain.WorkflowCheckpoint, stepID string) checkpointGitFacts {
	ordered := append([]domain.WorkflowCheckpoint(nil), cps...)
	sort.SliceStable(ordered, func(i, j int) bool { return ordered[i].CreatedAt.After(ordered[j].CreatedAt) })
	var out checkpointGitFacts
	for _, cp := range ordered {
		if stepID != "" && (cp.WorkflowStepID == nil || *cp.WorkflowStepID != stepID) {
			continue
		}
		out.Branch = firstNonEmpty(out.Branch, cp.Branch)
		out.WorktreePath = firstNonEmpty(out.WorktreePath, cp.WorktreePath)
		out.BaseSHA = firstNonEmpty(out.BaseSHA, cp.BaseSHA)
		out.HeadSHA = firstNonEmpty(out.HeadSHA, cp.HeadSHA)
	}
	return out
}

// newestFingerprints returns the newest non-empty FingerprintBefore and
// FingerprintAfter recorded ON THIS STEP.
//
// It used to scan the whole run, which meant a question about the work step
// could be answered with the fix step's fingerprint — two different trees at two
// different instants, presented as this step's own. A run-scoped row (one with
// no step id) is only admissible when the question is itself run-scoped.
func newestFingerprints(cps []domain.WorkflowCheckpoint, stepID string) (before, after string) {
	ordered := append([]domain.WorkflowCheckpoint(nil), cps...)
	sort.SliceStable(ordered, func(i, j int) bool { return ordered[i].CreatedAt.After(ordered[j].CreatedAt) })
	for _, cp := range ordered {
		if stepID != "" && deref(cp.WorkflowStepID) != stepID {
			continue
		}
		if before == "" && cp.FingerprintBefore != "" {
			before = cp.FingerprintBefore
		}
		if after == "" && cp.FingerprintAfter != "" {
			after = cp.FingerprintAfter
		}
		if before != "" && after != "" {
			break
		}
	}
	return before, after
}

// newestWorkerObservation finds the newest worker-observation checkpoint for a
// step, which is the closest thing AO has to "what the worker last did".
func newestWorkerObservation(cps []domain.WorkflowCheckpoint, stepID string) (phase, action string) {
	ordered := append([]domain.WorkflowCheckpoint(nil), cps...)
	sort.SliceStable(ordered, func(i, j int) bool { return ordered[i].CreatedAt.After(ordered[j].CreatedAt) })
	for _, cp := range ordered {
		if stepID != "" && (cp.WorkflowStepID == nil || *cp.WorkflowStepID != stepID) {
			continue
		}
		if !strings.HasPrefix(cp.DurablePhase, "worker_observed_") && cp.DurablePhase != workerSignalDelayedPhase {
			continue
		}
		return cp.DurablePhase, cp.NextAction
	}
	return "", ""
}

// applyBudget brings the snapshot under evidenceMaxSerializedBytes, measured on
// len(JSON()) — the artifact itself, not a tally of some of its parts.
//
// Three things make that measurable rather than circular:
//
//   - the digest is stamped as a fixed-width placeholder first, so replacing it
//     with the real one afterwards cannot change the length by a single byte;
//   - Bytes is measured at its widest possible value during the pass, so the
//     final (smaller or equal) number can only shrink the row, never grow it;
//   - a dropped field is REMOVED, not blanked. Blanking left the key, the
//     label, the status and a 47-byte explanation behind, so dropping every
//     field in a maximal snapshot still left several kilobytes of scaffolding
//     and the loop could not converge at all.
//
// Fields go lowest-value first: the last section backwards, so the workflow and
// step identity — without which the snapshot is about nothing — is the last
// thing to go. What went is recorded in DroppedForBudget, itself bounded, with
// an unbounded count beside it so the accounting cannot quietly under-report.
func (s *WorkerEvidenceSnapshot) applyBudget() {
	s.MaxBytes = evidenceMaxSerializedBytes
	s.Digest = strings.Repeat("0", evidenceDigestHexLen)
	s.Bytes = evidenceMaxSerializedBytes
	if len(s.Truncated) > evidenceMaxTruncatedKeys {
		s.Truncated = s.Truncated[:evidenceMaxTruncatedKeys]
	}

	for s.serializedLen() > evidenceMaxSerializedBytes {
		if !s.dropLowestValueField() {
			break
		}
	}
	// A section whose fields have all gone still costs its key and title. Once
	// there is nothing left to drop, they are what is left to drop.
	for s.serializedLen() > evidenceMaxSerializedBytes {
		if !s.dropTrailingEmptySection() {
			break
		}
	}
	// Last resort: the accounting lists. They are bounded already
	// (evidenceMaxTruncatedKeys entries of field keys, which are constants in
	// this file), so they cannot themselves prevent convergence — but they are
	// the only variable-length thing left once the sections have gone, and
	// dropping them costs nothing a reader needs: DroppedForBudgetCount is an
	// integer and survives, so the accounting still cannot under-report.
	if s.serializedLen() > evidenceMaxSerializedBytes {
		s.Truncated = nil
		s.DroppedForBudget = nil
	}
	// What remains is a floor that is bounded BY CONSTRUCTION rather than by
	// this loop: a constant version string, four identifiers each capped at
	// evidenceMaxIdentifierBytes, one RFC3339 timestamp, three integers and a
	// fixed-width digest. That is the whole reason the identifiers are capped
	// at all — without it this function could drop every field and every
	// section it has and still be over the ceiling, with nothing left to do.
}

// settleBytes stamps the true serialized length into Bytes.
//
// It iterates because Bytes is itself serialized: writing a shorter number
// makes the row shorter. It only ever shrinks (the budget pass measured with
// the widest value there can be), so it converges in at most a couple of
// rounds and can never push the row back over the ceiling.
func (s *WorkerEvidenceSnapshot) settleBytes() {
	for range 4 {
		n := s.serializedLen()
		if n == s.Bytes {
			return
		}
		s.Bytes = n
	}
}

func (s WorkerEvidenceSnapshot) serializedLen() int { return len(s.JSON()) }

// truncateIdentifier caps one identifier, on a rune boundary, with a mark that
// says it was capped.
//
// Rune-safe because a split multi-byte rune is invalid UTF-8, which
// encoding/json rewrites as U+FFFD — turning one byte into three at marshal
// time. The budget loop measures the real marshalled row so the ceiling would
// still hold, but the FLOOR would stop being predictable from the cap, and a
// bound you cannot compute in advance is a bound you cannot reason about.
func truncateIdentifier(id string) string {
	if len(id) <= evidenceMaxIdentifierBytes {
		return id
	}
	keep := evidenceMaxIdentifierBytes - len(evidenceIdentifierTruncationMark)
	if keep < 0 {
		keep = 0
	}
	// Back off to a rune boundary so the kept prefix stays valid UTF-8.
	for keep > 0 && !utf8.RuneStart(id[keep]) {
		keep--
	}
	return id[:keep] + evidenceIdentifierTruncationMark
}

// dropLowestValueField removes one field, last section first and last field
// within it first. Reports false when there is nothing left to remove.
func (s *WorkerEvidenceSnapshot) dropLowestValueField() bool {
	for i := len(s.Sections) - 1; i >= 0; i-- {
		fields := s.Sections[i].Fields
		if len(fields) == 0 {
			continue
		}
		dropped := fields[len(fields)-1]
		s.Sections[i].Fields = fields[:len(fields)-1]
		s.DroppedForBudgetCount++
		if len(s.DroppedForBudget) < evidenceMaxTruncatedKeys {
			s.DroppedForBudget = append(s.DroppedForBudget, dropped.Key)
		}
		return true
	}
	return false
}

// dropTrailingEmptySection removes the last section that has no fields left.
func (s *WorkerEvidenceSnapshot) dropTrailingEmptySection() bool {
	for i := len(s.Sections) - 1; i >= 0; i-- {
		if len(s.Sections[i].Fields) != 0 {
			continue
		}
		s.Sections = append(s.Sections[:i], s.Sections[i+1:]...)
		return true
	}
	return false
}

func (s WorkerEvidenceSnapshot) digest() string {
	var b strings.Builder
	b.WriteString(s.Version)
	b.WriteString(s.RunID)
	b.WriteString(s.StepID)
	for _, sec := range s.Sections {
		for _, f := range sec.Fields {
			b.WriteString("\n" + f.Key + "=" + string(f.Status) + ":" + f.Value)
		}
	}
	return contentDigest(b.String())
}

func shortDigest(d string) string {
	if len(d) <= 12 {
		return d
	}
	return d[:12]
}

func deref(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

func orNone(s string) string {
	return orValue(s, "none")
}

func orValue(s, fallback string) string {
	if strings.TrimSpace(s) == "" {
		return fallback
	}
	return s
}

func pick(ok bool, s string) string {
	if !ok {
		return ""
	}
	return s
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

func stamp(t time.Time, fallback string) string {
	if t.IsZero() {
		return fallback
	}
	return t.UTC().Format(time.RFC3339)
}

func stampPtr(t *time.Time, fallback string) string {
	if t == nil {
		return fallback
	}
	return stamp(*t, fallback)
}
