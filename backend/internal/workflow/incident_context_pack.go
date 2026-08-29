package workflow

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

// incident_context_pack.go — Checkpoint 8P-E.18.
//
// The pack is the ONLY thing a Diagnostic Agent is given. That is the point:
// an agent told to "go and look at the repository" reads whatever it feels
// like, costs whatever that costs, and produces a diagnosis nobody can
// reproduce because nobody knows what it saw. A pack is bounded, ordered,
// digestible and recorded — so a diagnosis can be tied to exactly the evidence
// it was handed (IncidentRecord.PackDigest), and a second run over the same
// pack is comparable to the first.
//
// # Budget
//
// Two limits, both hard, checked in this order:
//
//   - a per-section cap, so no single noisy section (a 4 MB diff, a runaway
//     stderr tail) can crowd out the sections that actually explain the stop;
//   - a whole-pack cap, applied by dropping WHOLE sections from the least
//     important upward, never by cutting a section in half. A half-truncated
//     git diff is worse than an absent one: it looks complete and is not.
//
// The token figure is an estimate and is labelled as one everywhere it appears.
// AO does not tokenize, and pretending otherwise would put a fake precision on
// a budget whose real purpose is "this cannot run away".

const (
	// incidentPackMaxBytes is the whole-pack ceiling. ~48 KB of dense,
	// pre-selected facts is a large prompt and a small context: it leaves the
	// model room to reason rather than to re-read.
	incidentPackMaxBytes = 48 * 1024
	// incidentPackMaxSectionBytes bounds any single section.
	incidentPackMaxSectionBytes = 8 * 1024
	// incidentPackMaxDiffBytes bounds the git diff specifically, which is the
	// one section whose natural size is unbounded.
	incidentPackMaxDiffBytes = 12 * 1024
	// incidentPackBytesPerToken is the estimator. Four bytes per token is the
	// usual English/code rule of thumb; it is used only to render an estimate
	// for the operator and for the durable record.
	incidentPackBytesPerToken = 4
	// incidentPackMaxCheckpoints bounds how much ledger history travels. The
	// newest rows explain a stop; the run's whole life does not.
	incidentPackMaxCheckpoints = 25
	// incidentPackMaxChangedFiles bounds the file list independently of the
	// diff, so "which files changed" survives even when the diff is dropped.
	incidentPackMaxChangedFiles = 60
)

// IncidentPackSection is one titled block of evidence. Sections are ordered by
// Priority (1 = most important) and dropped from the bottom when the whole-pack
// budget is exceeded.
type IncidentPackSection struct {
	Title string `json:"title"`
	Body  string `json:"body"`
	// Priority decides survival under budget pressure. It encodes a claim about
	// diagnosis: without the stop and the step states there is nothing to
	// reason about at all, while a diff is context that a competent diagnosis
	// can often do without.
	Priority int `json:"priority"`
	// Truncated records that this section was capped, so the agent is told it
	// is looking at a prefix rather than left to assume completeness.
	Truncated bool `json:"truncated,omitempty"`
	// Dropped records that the section was removed entirely for budget.
	Dropped bool `json:"dropped,omitempty"`
}

// IncidentContextPack is the bounded evidence bundle handed to a Diagnostic
// Agent, plus the accounting that makes it auditable.
type IncidentContextPack struct {
	Version    string    `json:"version"`
	IncidentID string    `json:"incidentId"`
	RunID      string    `json:"runId"`
	MasterID   string    `json:"masterRunId,omitempty"`
	ProjectID  string    `json:"projectId"`
	StopReason string    `json:"stopReason"`
	StopDetail string    `json:"stopDetail,omitempty"`
	Signature  string    `json:"signature"`
	BuiltAt    time.Time `json:"builtAt"`

	Sections []IncidentPackSection `json:"sections"`

	// Budget accounting, recorded so a pack's cost is a fact rather than a
	// guess after the event.
	Bytes           int `json:"bytes"`
	MaxBytes        int `json:"maxBytes"`
	EstimatedTokens int `json:"estimatedTokens"`
	// DroppedSections names what did not fit, so a diagnosis that says "I could
	// not tell" can be checked against what it was actually denied.
	DroppedSections []string `json:"droppedSections,omitempty"`
	// Digest identifies this exact pack.
	Digest string `json:"digest"`
}

// IncidentPackInput is everything the builder is allowed to read. It is a plain
// value on purpose: the builder performs NO I/O, so it cannot widen its own
// evidence, and it is exhaustively testable.
//
// Note what is absent: the repository. The builder never walks the source tree,
// never greps, never opens a file the caller did not already hold. The only
// repository-derived facts here are the ones AO had already observed for its
// own purposes — branch, HEAD, the porcelain status and a bounded diff.
type IncidentPackInput struct {
	Detail     RunDetail
	Signature  string
	StopReason string
	StopDetail string
	IncidentID string
	// EffectiveSpec is RenderEffectiveSpecification's output for this run: the
	// approved amendments that reconcile the original objective with the
	// criteria in force. Empty when the task has none, which leaves the pack
	// byte-identical to what it was before amendments existed.
	EffectiveSpec string

	// Master, when this run is a child, is the parent's state — the single most
	// common thing a person needs and cannot see from the child.
	MasterState string
	MasterID    string

	// Workspace facts AO already holds.
	Branch       string
	HeadSHA      string
	GitStatus    string
	GitDiff      string
	WorktreePath string

	// Session facts for the run's worker, as AO recorded them.
	SessionID         string
	SessionHarness    string
	SessionActivity   string
	SessionLastAt     time.Time
	SessionTerminated bool

	// ReviewFindings is the newest reviewer verdict body, when one is relevant.
	ReviewFindings string
	ReviewVerdict  string

	// VerifyOutput is the newest verification result, when one is relevant.
	VerifyOutput string

	// ProviderNotes carries provider/capacity facts (health, wakes) when the
	// stop is about them.
	ProviderNotes string

	// ChildEvidence is the bounded evidence pack for the stopped CHILD a
	// parent's stop is about (incident_child_pack.go). Empty for every stop that
	// is not about a child, which leaves the pack byte-identical to what it was
	// before this existed. When it is present it is priority 1: a parent stopped
	// on child_needs_attention has no diagnosis to make without it, which is
	// exactly why the answer used to be "go and diagnose the child first".
	ChildEvidence string

	// EvidenceSnapshot is CollectWorkerEvidence's rendered, bounded snapshot for
	// the step this stop is about: workflow/step/attempt state, session
	// lifecycle, process liveness, harness launch and exit, branch and HEAD, the
	// git status summary, workspace fingerprints, AO-owned mutation provenance,
	// the expected artifacts, the worker's last result signal, the recent
	// checkpoints and the parent/child relationship — each field carrying its
	// own status.
	//
	// It is priority 2 and therefore never dropped. Its whole purpose is that a
	// person asking "¿Qué hago?" is shown every fact AO can actually obtain,
	// with the ones it cannot named as such, instead of a stop that recorded a
	// conclusion and nothing under it. See evidence_snapshot.go.
	EvidenceSnapshot string

	// Checkpoints is the run's ledger, newest-relevant-first selection is done
	// by the builder.
	Checkpoints []domain.WorkflowCheckpoint
}

// BuildIncidentContextPack assembles the pack under its budget.
//
// Every section is built first and capped individually, then the whole pack is
// brought under budget by dropping complete low-priority sections. Both
// operations are recorded in the pack itself, because "the agent was not shown
// the diff" is a fact a reader of the diagnosis needs.
func BuildIncidentContextPack(in IncidentPackInput) IncidentContextPack {
	pack := IncidentContextPack{
		Version:    "incident-pack/v1",
		IncidentID: in.IncidentID,
		RunID:      in.Detail.Run.ID,
		MasterID:   in.MasterID,
		ProjectID:  in.Detail.Run.ProjectID,
		StopReason: in.StopReason,
		StopDetail: in.StopDetail,
		Signature:  in.Signature,
		MaxBytes:   incidentPackMaxBytes,
	}

	add := func(priority int, title, body string, limit int) {
		body = strings.TrimSpace(body)
		if body == "" {
			return
		}
		section := IncidentPackSection{Title: title, Priority: priority}
		if limit <= 0 || limit > incidentPackMaxSectionBytes {
			limit = incidentPackMaxSectionBytes
		}
		if len(body) > limit {
			body = body[:limit]
			section.Truncated = true
		}
		section.Body = body
		pack.Sections = append(pack.Sections, section)
	}

	// Priority 1 — without these there is no diagnosis to make.
	add(1, "Stop", fmt.Sprintf("run: %s\nstate: %s\nreason: %s\ndetail: %s",
		in.Detail.Run.ID, in.Detail.Run.State, in.StopReason, in.StopDetail), 0)
	add(1, "Steps", renderIncidentSteps(in.Detail), 0)
	// Priority 1, and never dropped: when a parent stops BECAUSE a child
	// stopped, the child's reason IS the parent's reason. Without it the only
	// honest diagnosis is "diagnose the child first", which is not a diagnosis.
	add(1, "Stopped child task (bounded evidence)", in.ChildEvidence, incidentPackMaxSectionBytes)

	// Priority 2, and never dropped: the durable evidence snapshot. Everything
	// below it is context for a stop; this IS the stop's evidence.
	add(2, "Durable evidence snapshot (bounded)", in.EvidenceSnapshot, incidentPackMaxSectionBytes)

	// Priority 2 — the immediate mechanics of the stop.
	add(2, "Recent checkpoints", renderIncidentCheckpoints(in.Checkpoints), 0)
	add(2, "Worker session", renderIncidentSession(in), 0)

	// Priority 2 as well: an approved amendment is often the whole explanation
	// for a stop, because a reviewer blocking on a requirement a person already
	// retired looks identical to a reviewer blocking on a real defect. The
	// advisor is given the same authoritative reading every other role gets
	// (RenderEffectiveSpecification), never the historical requirement alone.
	add(2, "Approved amendments to the task's requirements", in.EffectiveSpec, 0)

	// Priority 3 — the surrounding state a stop is usually explained by.
	if in.MasterState != "" {
		add(3, "Parent objective", fmt.Sprintf("master run: %s\nstate: %s", in.MasterID, in.MasterState), 0)
	}
	add(3, "Workspace", renderIncidentWorkspace(in), 0)
	if in.ProviderNotes != "" {
		add(3, "Provider health", in.ProviderNotes, 0)
	}

	// Priority 4 — verdicts and outputs, large and often but not always needed.
	if in.ReviewFindings != "" {
		add(4, "Latest review ("+in.ReviewVerdict+")", in.ReviewFindings, 0)
	}
	if in.VerifyOutput != "" {
		add(4, "Latest verification output", in.VerifyOutput, 0)
	}

	// Priority 5 — the diff. Genuinely useful, genuinely unbounded, and the
	// first thing to go.
	add(5, "Changed files", renderIncidentChangedFiles(in.GitStatus), 0)
	add(6, "Diff (bounded)", in.GitDiff, incidentPackMaxDiffBytes)

	pack.applyBudget()
	pack.BuiltAt = time.Time{} // set by the caller's clock; see AttachBuiltAt
	pack.Digest = pack.digest()
	return pack
}

// AttachBuiltAt stamps the pack's build time from the caller's clock, keeping
// BuildIncidentContextPack a pure function that tests can compare byte-for-byte.
func (p *IncidentContextPack) AttachBuiltAt(at time.Time) { p.BuiltAt = at }

// applyBudget brings the pack under incidentPackMaxBytes by dropping whole
// sections, lowest priority first and, within a priority, largest first.
//
// Dropping rather than shrinking is the load-bearing choice. A truncated
// section that still LOOKS complete invites a confident wrong diagnosis, which
// is the failure mode this whole feature exists to stop.
func (p *IncidentContextPack) applyBudget() {
	total := p.sectionBytes()
	if total <= incidentPackMaxBytes {
		p.Bytes = total
		p.EstimatedTokens = total / incidentPackBytesPerToken
		return
	}
	order := make([]int, len(p.Sections))
	for i := range order {
		order[i] = i
	}
	sort.SliceStable(order, func(a, b int) bool {
		x, y := p.Sections[order[a]], p.Sections[order[b]]
		if x.Priority != y.Priority {
			return x.Priority > y.Priority // least important first
		}
		return len(x.Body) > len(y.Body)
	})
	for _, idx := range order {
		if total <= incidentPackMaxBytes {
			break
		}
		if p.Sections[idx].Dropped || p.Sections[idx].Priority <= 2 {
			// Priority 1 and 2 are never dropped: a pack without the stop and
			// the steps is not a smaller pack, it is a useless one. If those
			// alone exceed the budget the pack goes over, and says so.
			continue
		}
		total -= len(p.Sections[idx].Body)
		p.DroppedSections = append(p.DroppedSections, p.Sections[idx].Title)
		p.Sections[idx].Body = ""
		p.Sections[idx].Dropped = true
		p.Sections[idx].Truncated = false
	}
	p.Bytes = p.sectionBytes()
	p.EstimatedTokens = p.Bytes / incidentPackBytesPerToken
}

func (p IncidentContextPack) sectionBytes() int {
	n := 0
	for _, s := range p.Sections {
		n += len(s.Body)
	}
	return n
}

// digest identifies the exact evidence a diagnosis was taken against.
func (p IncidentContextPack) digest() string {
	var b strings.Builder
	b.WriteString(p.Version)
	b.WriteString(p.Signature)
	for _, s := range p.Sections {
		b.WriteString("\n##" + s.Title + "\n")
		b.WriteString(s.Body)
	}
	return contentDigest(b.String())
}

// Render turns the pack into the text an agent actually receives. Dropped
// sections are still listed, by title, with the reason — the agent must know
// what it was not shown, or "insufficient evidence" becomes unusable as a
// verdict.
func (p IncidentContextPack) Render() string {
	var b strings.Builder
	fmt.Fprintf(&b, "# Incident context pack (%s)\n", p.Version)
	fmt.Fprintf(&b, "incident: %s\nrun: %s\nproject: %s\nstop: %s\n\n",
		p.IncidentID, p.RunID, p.ProjectID, p.StopReason)
	for _, s := range p.Sections {
		if s.Dropped {
			continue
		}
		fmt.Fprintf(&b, "## %s\n%s\n", s.Title, s.Body)
		if s.Truncated {
			b.WriteString("[truncated to fit the pack budget — this section is a prefix, not the whole thing]\n")
		}
		b.WriteString("\n")
	}
	if len(p.DroppedSections) > 0 {
		fmt.Fprintf(&b, "## Evidence NOT included\nThese sections exceeded the pack budget and were omitted in full: %s.\nIf your conclusion depends on one of them, say so and classify as unsafe_or_insufficient_evidence.\n\n",
			strings.Join(p.DroppedSections, ", "))
	}
	fmt.Fprintf(&b, "Pack size: %d bytes (~%d tokens estimated), limit %d bytes.\n",
		p.Bytes, p.EstimatedTokens, p.MaxBytes)
	return b.String()
}

// ---- section renderers ------------------------------------------------------

func renderIncidentSteps(d RunDetail) string {
	steps := make([]string, 0, len(d.Steps))
	for _, sd := range d.Steps {
		line := fmt.Sprintf("%s = %s", sd.Step.Kind, sd.Step.State)
		if n := len(sd.Attempts); n > 0 {
			last := sd.Attempts[n-1]
			line += fmt.Sprintf(" (attempts=%d, last harness=%s", n, last.Harness)
			if last.ErrorClass != "" {
				line += fmt.Sprintf(", error=%s", last.ErrorClass)
			}
			line += ")"
		}
		steps = append(steps, line)
	}
	return strings.Join(steps, "\n")
}

func renderIncidentCheckpoints(cps []domain.WorkflowCheckpoint) string {
	ordered := append([]domain.WorkflowCheckpoint(nil), cps...)
	sort.SliceStable(ordered, func(a, b int) bool { return ordered[a].CreatedAt.After(ordered[b].CreatedAt) })
	if len(ordered) > incidentPackMaxCheckpoints {
		ordered = ordered[:incidentPackMaxCheckpoints]
	}
	lines := make([]string, 0, len(ordered))
	for _, cp := range ordered {
		line := fmt.Sprintf("%s  %s", cp.CreatedAt.UTC().Format(time.RFC3339), cp.DurablePhase)
		if cp.NextAction != "" {
			line += "  | " + oneLine(cp.NextAction)
		}
		lines = append(lines, line)
	}
	return strings.Join(lines, "\n")
}

func renderIncidentSession(in IncidentPackInput) string {
	if in.SessionID == "" {
		return ""
	}
	last := "never"
	if !in.SessionLastAt.IsZero() {
		last = in.SessionLastAt.UTC().Format(time.RFC3339)
	}
	return fmt.Sprintf("session: %s\nharness: %s\nactivity: %s\nlast activity: %s\nterminated: %t",
		in.SessionID, in.SessionHarness, in.SessionActivity, last, in.SessionTerminated)
}

func renderIncidentWorkspace(in IncidentPackInput) string {
	if in.Branch == "" && in.HeadSHA == "" && in.WorktreePath == "" {
		return ""
	}
	return fmt.Sprintf("worktree: %s\nbranch: %s\nHEAD: %s", in.WorktreePath, in.Branch, in.HeadSHA)
}

// renderIncidentChangedFiles keeps the file list separate from the diff so the
// cheap, high-signal half survives when the expensive half is dropped.
func renderIncidentChangedFiles(status string) string {
	lines := strings.Split(strings.TrimSpace(status), "\n")
	out := make([]string, 0, len(lines))
	for _, l := range lines {
		if strings.TrimSpace(l) == "" {
			continue
		}
		out = append(out, l)
		if len(out) >= incidentPackMaxChangedFiles {
			out = append(out, fmt.Sprintf("… (%d more)", len(lines)-len(out)))
			break
		}
	}
	return strings.Join(out, "\n")
}

func oneLine(s string) string {
	return strings.Join(strings.Fields(strings.ReplaceAll(s, "\n", " ")), " ")
}

func contentDigest(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}
