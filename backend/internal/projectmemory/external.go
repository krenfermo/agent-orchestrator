package projectmemory

// external.go -- P4-F: bounded EXTERNAL context beside AO's own memory.
//
// Project memory is what AO derived from a repository and can vouch for. This
// is the other kind of context: facts that live on a service AO does not own,
// are true only for the next few minutes, and must never be written down as if
// they were durable. A failing check is not a project fact; it is the weather.
//
// So it rides alongside the pack rather than inside it, and the separation is
// load-bearing in three places:
//
//   - The pack CACHE is keyed on the indexed commit and the memory generation,
//     which are exactly the things external state does not move with. Putting
//     PR status inside a cached pack would serve a stale review decision for
//     the cache's whole lifetime. External evidence is assembled after the
//     cache, every time.
//   - Project MEMORY is fail-closed about provenance. Nothing here is
//     promoted, ever; the memory subsystem is not even told about it.
//   - The METRICS report it as its own source category, the way the code graph
//     is, so "how many of this dispatch's bytes came from GitHub" stays an
//     answerable question rather than being absorbed into PackBytes.
//
// The whole thing is optional. A provisioner with no external provider behaves
// byte-for-byte as it did before P4-F.

import (
	"context"
	"strings"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

// MaxExternalContextBytes is the hard ceiling on external evidence in one
// dispatch, enforced here rather than trusted to the provider. A provider that
// returns more is truncated and told so; a context channel must not be
// floodable by a service AO does not control.
const MaxExternalContextBytes = 4000

// ExternalContextRequest is what a provider is told about the dispatch.
//
// It carries no memory and no legacy documents: an external provider decides
// what GitHub says about a branch, not what AO already knows.
type ExternalContextRequest struct {
	ProjectID domain.ProjectID
	// RepoPath is the canonical repository root.
	RepoPath string
	// Workspace is the checkout the agent is actually working in, when that is
	// not the root — a task's isolated worktree, whose branch is the one whose
	// pull request matters.
	Workspace string
	// Role decides how much and which of the external facts are relevant: a
	// planner wants the issue and the branch's state, a reviewer wants the
	// pull request's review decision and its failing checks.
	Role PackRole
	// HeadSHA is the commit this role is reasoning about, when known.
	HeadSHA string
	// IssueRef is the tracker issue this dispatch is against, in whatever form
	// the boundary has it — a number, "owner/repo#12", or a URL. It is passed
	// through verbatim and interpreted by the provider, because what counts as
	// an issue reference is the external system's business, not memory's.
	//
	// Empty is common and correct: a planner is not working on one issue.
	IssueRef string
}

// ExternalEvidence is one provider's bounded contribution.
type ExternalEvidence struct {
	// Source names the system this came from, e.g. "github". It is recorded in
	// the metrics so a dispatch's context can be attributed.
	Source string
	// Rendered is the text to attach. Empty means the provider had nothing,
	// which is a normal answer and never an error.
	Rendered string
	// Bytes and EstimatedTokens measure what was attached. EstimatedTokens is
	// named as an estimate because that is what it is.
	Bytes           int
	EstimatedTokens int
	// Reason says why the contribution is empty or partial, in AO's own words.
	// It is carried even on success paths where something was degraded.
	Reason string
	// Degraded reports that the provider answered with less than the whole
	// picture — a GitHub outage, a missing token, a rate limit.
	Degraded bool
	// Truncated reports that the ceiling above bound the contribution.
	Truncated bool
}

// Empty reports whether there is anything to attach.
func (e ExternalEvidence) Empty() bool { return strings.TrimSpace(e.Rendered) == "" }

// Render returns the block to attach, with its own heading so a reader can
// tell external state from AO's durable facts at a glance. The disclaimer is
// not decoration: an agent that treats a check result as a project fact will
// make decisions on it long after it stopped being true.
func (e ExternalEvidence) Render() string {
	if e.Empty() {
		return ""
	}
	var b strings.Builder
	b.WriteString("EXTERNAL CONTEXT (")
	b.WriteString(e.Source)
	b.WriteString(") — live state read at dispatch time, not durable project knowledge.\n")
	b.WriteString(strings.TrimRight(e.Rendered, "\n"))
	b.WriteString("\n")
	if e.Truncated {
		b.WriteString("(truncated to AO's external-context budget)\n")
	}
	if e.Degraded && e.Reason != "" {
		b.WriteString("(partial: " + e.Reason + ")\n")
	}
	return b.String()
}

// ExternalContextProvider supplies external evidence for one dispatch.
//
// Its contract is the same as Provision's own: it never fails. A provider that
// cannot reach its service returns evidence with a Reason and no text, and the
// dispatch proceeds with exactly the context it would have had.
type ExternalContextProvider interface {
	ExternalContext(ctx context.Context, req ExternalContextRequest) ExternalEvidence
}

// clampExternal enforces the ceiling and fills in the measurements, so no
// provider has to be trusted to do either.
func clampExternal(in ExternalEvidence) ExternalEvidence {
	rendered := strings.TrimSpace(in.Rendered)
	if len(rendered) > MaxExternalContextBytes {
		// Cut on a line boundary so the tail is never half a fact.
		cut := strings.LastIndexByte(rendered[:MaxExternalContextBytes], '\n')
		if cut <= 0 {
			cut = MaxExternalContextBytes
		}
		rendered = strings.TrimRight(rendered[:cut], "\n")
		in.Truncated = true
	}
	in.Rendered = rendered
	out := in
	if rendered == "" {
		out.Bytes, out.EstimatedTokens = 0, 0
		return out
	}
	// The measurement is of the RENDERED block, heading and all, because that
	// is what the dispatch actually carries.
	out.Bytes = len(out.Render())
	out.EstimatedTokens = EstimateTokens(out.Bytes)
	return out
}
