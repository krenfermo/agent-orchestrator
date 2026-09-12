// Package turnbench replays a recorded run's SHAPE so a change to AO's turn
// economy can be argued from a number instead of from a hope.
//
// It exists because the only honest way to prove a saving is to measure one,
// and the only way to measure one on a real defect is to pay for it twice.
// This package is the third option: take the call series a real run actually
// produced -- how big the conversation was on each call, what the call did,
// which dispatch owned it -- and recompute what that same series would have
// billed under a different policy. Nothing is simulated about the WORK: the
// calls, their order, their outputs and their classes are the ones that
// happened. What is recomputed is only the quantity a policy changes, which is
// how much conversation each of those calls had to re-read.
//
// WHAT A REPLAY CAN AND CANNOT SAY.
//
// It can say exactly what an unchanged run costs, because that is arithmetic
// over observed rows: billable input is the sum of the context over the calls,
// and every term of that sum is recorded. It can also say what the SAME
// sequence of calls would have cost had the conversation been reset at a
// dispatch boundary, because resetting a conversation does not change what the
// next call is about -- only how much history it carries.
//
// It cannot say whether the agent would have made the same calls. A compacted
// agent might need one more turn to re-find a file, or one fewer because it is
// not re-reading its own dead ends. So a replay's figure is a projection of
// ONE lever held while the other is frozen, and the honest way to report it is
// exactly that. Where this package is used to state a saving, it is stated as
// "replay-derived", never as "measured in production".
//
// No content is replayed and none is stored. A series is numbers and a closed
// vocabulary: context tokens, output tokens, a turn class, a segment label.
package turnbench

import (
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

// Call is one provider call, reduced to what a cost shape is made of.
type Call struct {
	// ContextTokens is the whole conversation the provider re-read on this
	// call: the term this call contributes to billable input.
	ContextTokens int64 `json:"contextTokens"`
	OutputTokens  int64 `json:"outputTokens"`
	// UncachedInputTokens, CacheReadTokens and CacheWriteTokens partition
	// ContextTokens the way the provider bills it. They are OPTIONAL: a series
	// recorded before P7.1 carries none of them, and such a series can still
	// answer every token question -- it simply cannot answer a money one,
	// because the three dimensions are priced at rates that differ by a factor
	// of twelve and a sum over them is not a cost.
	//
	// A series where they are present must have them add up to ContextTokens.
	// SplitKnown is what enforces that, and Cost refuses to price a series
	// where it does not hold rather than pricing the difference away.
	UncachedInputTokens int64 `json:"uncachedInputTokens,omitempty"`
	CacheReadTokens     int64 `json:"cacheReadTokens,omitempty"`
	CacheWriteTokens    int64 `json:"cacheWriteTokens,omitempty"`
	// Class is what the call did. See domain.TurnClass.
	Class domain.TurnClass `json:"class"`
	// Segment names the dispatch that owned the call -- "work", "fix/1". It is
	// the boundary a session policy can act at, and the only structure a
	// replay needs beyond the series itself.
	Segment string `json:"segment"`
	// ObservedAt places the call. Optional: a series without times reports an
	// unknown elapsed rather than a zero one.
	ObservedAt string `json:"observedAt,omitempty"`
}

// Series is one run's calls in provider order.
type Series struct {
	// Source names the run this shape came from, so a figure can be traced
	// back to the thing that produced it.
	Source   string                   `json:"source"`
	Note     string                   `json:"note,omitempty"`
	Strategy domain.ExecutionStrategy `json:"strategy,omitempty"`
	// ModelID is the model the calls were made on. Required to price a series
	// and irrelevant to measuring one, which is exactly the asymmetry this
	// package now has to carry.
	ModelID string `json:"modelId,omitempty"`
	Calls   []Call `json:"calls"`
	// Compactions are the conversation replacements this series performed.
	// Empty for a recorded run that never compacted -- which, until P7, was
	// every recorded run.
	Compactions []Compaction `json:"compactions,omitempty"`
	// Unattributed is spend the harness charged itself for that NO call in
	// this series accounts for: the residual between the harness's own
	// end-of-session rollup and what the usage pipeline attributed.
	//
	// On a session that never compacted it is noise -- retries, a title
	// generated on a small model. On a session that compacted it is dominated
	// by the summarization turns, and it is the only MEASURED figure for them
	// that exists, because the harness reports its spend once per session and
	// never once per compaction.
	//
	// When present it is preferred over the modelled compaction cost, and the
	// result says which of the two it used.
	Unattributed *UnattributedSpend `json:"unattributed,omitempty"`
}

// UnattributedSpend is the measured residual and the model to price it as.
type UnattributedSpend struct {
	// ModelID is the model the HARNESS rolled the session up under, which is
	// routinely not the spelling its own per-call records use. It is carried
	// separately for exactly that reason: a long-context variant is priced
	// differently from the base model, and pricing this residual as the base
	// model would be substituting a rate rather than looking one up.
	ModelID string                  `json:"modelId"`
	Tokens  domain.UsageTokenTotals `json:"tokens"`
}

// Compaction is one conversation replacement, in the shape a cost question
// needs it.
//
// It is deliberately NOT a Call. A compaction is a model turn -- it re-reads
// the conversation and generates a summary -- but no assistant record is
// written for it, so it never becomes a usage event and it never appears in a
// recorded call series. Folding it into Calls would make it invisible in
// exactly the way the ledger already makes it invisible.
type Compaction struct {
	// PreTokens is the conversation the summarization turn read; PostTokens is
	// what it left behind. Both are the harness's own figures.
	PreTokens  int64 `json:"preTokens"`
	PostTokens int64 `json:"postTokens"`
	DurationMs int64 `json:"durationMs,omitempty"`
	// SummaryOutputTokens is what the turn GENERATED, at output prices, and it
	// is the largest single term in the cost of compacting.
	//
	// SummaryOutputKnown is false when nobody measured it. The cost fold then
	// refuses to produce a total rather than treating the unmeasured term as
	// zero -- which is the precise error that made a 39% token reduction read
	// as a 39% saving.
	SummaryOutputTokens int64 `json:"summaryOutputTokens,omitempty"`
	SummaryOutputKnown  bool  `json:"summaryOutputKnown,omitempty"`
}

// Metrics is everything a before/after comparison needs, and nothing derived
// from an assumption.
//
// Everything here is a TOKEN figure. Cost lives in CostMetrics, in cost.go,
// behind a rate card and a split this type does not require -- so that a
// reduction measured here can never be quoted as a saving without someone
// having asked for the other type by name.
type Metrics struct {
	Calls int64
	// FirstContext / LastContext / PeakContext are the series' own ends and
	// its largest single call.
	FirstContext int64
	LastContext  int64
	PeakContext  int64
	// CumulativeInput is the sum of the context over the calls: the number a
	// bill is proportional to, and the one a policy moves.
	CumulativeInput int64
	OutputTokens    int64
	// Elapsed is the wall time the series covers, and whether it is knowable.
	Elapsed      time.Duration
	ElapsedKnown bool
	// Turns is the class fold. Work and coordination split the classified
	// calls; see domain.TurnMix.
	Turns domain.TurnMix

	// Tokens is the billed vector over the calls, when the series carries the
	// split. SplitKnown is false when it does not, and every dimension of
	// Tokens except InputTokens and OutputTokens is then meaningless.
	Tokens     domain.UsageTokenTotals
	SplitKnown bool

	// Compactions and the three figures under it fold Series.Compactions.
	// SummaryOutputKnown is false when ANY compaction left its generated
	// tokens unmeasured, because a partial sum of them is worse than none.
	Compactions          int64
	CompactionPreTokens  int64
	CompactionPostTokens int64
	CompactionDurationMs int64
	SummaryOutputTokens  int64
	SummaryOutputKnown   bool
}

// Measure folds a series into its metrics. Pure: the same series always
// measures the same.
func Measure(s Series) Metrics {
	m := Metrics{Calls: int64(len(s.Calls))}
	if m.Calls == 0 {
		return m
	}
	m.FirstContext = s.Calls[0].ContextTokens
	m.LastContext = s.Calls[len(s.Calls)-1].ContextTokens
	m.SplitKnown = true
	var firstAt, lastAt time.Time
	for _, c := range s.Calls {
		m.CumulativeInput += c.ContextTokens
		m.OutputTokens += c.OutputTokens
		if !c.splitConsistent() {
			m.SplitKnown = false
		}
		m.Tokens.InputTokens += c.ContextTokens
		m.Tokens.UncachedInputTokens += c.UncachedInputTokens
		m.Tokens.CacheReadTokens += c.CacheReadTokens
		m.Tokens.CacheWriteTokens += c.CacheWriteTokens
		m.Tokens.OutputTokens += c.OutputTokens
		if c.ContextTokens > m.PeakContext {
			m.PeakContext = c.ContextTokens
		}
		m.Turns.AddTurn(c.Class)
		if c.ObservedAt == "" {
			continue
		}
		at, err := time.Parse(time.RFC3339Nano, c.ObservedAt)
		if err != nil {
			continue
		}
		if firstAt.IsZero() || at.Before(firstAt) {
			firstAt = at
		}
		if at.After(lastAt) {
			lastAt = at
		}
	}
	if !firstAt.IsZero() && !lastAt.IsZero() && !lastAt.Before(firstAt) {
		m.Elapsed, m.ElapsedKnown = lastAt.Sub(firstAt), true
	}
	m.Tokens.EventCount = m.Calls
	m.SummaryOutputKnown = true
	for _, compaction := range s.Compactions {
		m.Compactions++
		m.CompactionPreTokens += compaction.PreTokens
		m.CompactionPostTokens += compaction.PostTokens
		m.CompactionDurationMs += compaction.DurationMs
		if !compaction.SummaryOutputKnown {
			m.SummaryOutputKnown = false
			continue
		}
		m.SummaryOutputTokens += compaction.SummaryOutputTokens
	}
	if !m.SummaryOutputKnown {
		m.SummaryOutputTokens = 0
	}
	return m
}

// splitConsistent reports whether this call's billed dimensions add up to the
// context it declares. A call that carries none of them is not consistent: it
// is unsplit, which is a different and equally unpriceable state.
func (c Call) splitConsistent() bool {
	return c.UncachedInputTokens+c.CacheReadTokens+c.CacheWriteTokens == c.ContextTokens
}

// Policy is the change whose effect is being replayed.
//
// Deliberately tiny. Every field here corresponds to something AO can actually
// do to a live session; a knob with no implementation behind it would let this
// package produce a number nobody could collect.
type Policy struct {
	// CompactAtSegmentBoundary replays what happens when AO asks the session
	// to replace its conversation at each dispatch boundary after the first --
	// which is exactly what workflow.maybeCompactBeforeFix does when the
	// lifecycle decision reaches COMPACT.
	CompactAtSegmentBoundary bool
	// PostCompactContextTokens is the size the conversation resumes at: the
	// harness's standing overhead (its system prompt, the project's
	// instructions, the tool schemas) plus the summary and the fact pack AO
	// sends after it.
	//
	// It is an INPUT, not a constant, because it is the one quantity a replay
	// cannot observe -- no call in the recorded series was ever made against a
	// compacted conversation. A caller states it, and a sensitivity test
	// states how much the answer moves when it is wrong.
	PostCompactContextTokens int64
	// PostCompactCacheWriteTokens is how much of PostCompactContextTokens the
	// first call after a compaction has to WRITE rather than read from cache:
	// the summary, the fact pack and everything the harness re-attaches. The
	// rest is the standing prefix, which stays cached across the replacement.
	//
	// It matters out of all proportion to its size, because a written token is
	// priced at twelve and a half times a read one. Leaving it zero says "the
	// whole post-compaction conversation was already cached", which is false
	// for every compaction ever observed -- so a zero here makes the cost fold
	// report the series as unsplit rather than cheap.
	PostCompactCacheWriteTokens int64
	// CompactionSummaryOutputTokens is what the summarization turn GENERATES.
	// CompactionSummaryOutputKnown must be set for it to count: an unset value
	// leaves the replayed series' cost explicitly unknown instead of quietly
	// omitting the largest term in it.
	CompactionSummaryOutputTokens int64
	CompactionSummaryOutputKnown  bool
}

// Apply replays a series under a policy, returning the series that policy
// would have produced.
//
// The per-call DELTAS inside a segment are preserved exactly: what a call adds
// to the conversation is a property of the work it did, and the work is not
// being replayed. Only the level the segment starts from moves. That is the
// whole model, and its one assumption is stated in the package comment.
func Apply(s Series, p Policy) Series {
	out := s
	out.Calls = make([]Call, len(s.Calls))
	copy(out.Calls, s.Calls)
	if !p.CompactAtSegmentBoundary || len(out.Calls) == 0 {
		return out
	}
	segment := out.Calls[0].Segment
	level := out.Calls[0].ContextTokens
	for i := range out.Calls {
		boundary := false
		if i > 0 {
			delta := s.Calls[i].ContextTokens - s.Calls[i-1].ContextTokens
			if out.Calls[i].Segment != segment {
				// A boundary: the conversation is replaced, so this call pays
				// the post-compaction level rather than everything before it.
				segment = out.Calls[i].Segment
				level = p.PostCompactContextTokens
				boundary = true
			} else {
				level += delta
			}
		}
		if level < 0 {
			level = 0
		}
		out.Calls[i].ContextTokens = level
		if boundary {
			previous := s.Calls[i-1]
			out.Compactions = append(out.Compactions, Compaction{
				// The summarization turn reads the conversation as it stood
				// plus the answer that had just been added to it, which is
				// what the harness's own preTokens figure counts.
				PreTokens:           previous.ContextTokens + previous.OutputTokens,
				PostTokens:          p.PostCompactContextTokens,
				SummaryOutputTokens: p.CompactionSummaryOutputTokens,
				SummaryOutputKnown:  p.CompactionSummaryOutputKnown,
			})
			// The first call after a replacement writes the new prefix; only
			// what the policy names as written is written.
			out.Calls[i].CacheWriteTokens = p.PostCompactCacheWriteTokens
		}
		rebillCall(&out.Calls[i], s.Calls[i])
	}
	return out
}

// rebillCall redistributes a replayed call's billed dimensions to match the
// context it now carries.
//
// The uncached and written halves are properties of the WORK -- a tool result
// is the same size whatever came before it -- so they survive the replay
// untouched. Only the cached read moves, because only the cached read is the
// history. A source call whose split was never recorded leaves the replayed
// call unsplit too: a replay cannot invent a partition the recording did not
// have, and the cost fold refuses to price what it cannot partition.
func rebillCall(out *Call, source Call) {
	if !source.splitConsistent() {
		out.UncachedInputTokens, out.CacheReadTokens, out.CacheWriteTokens = 0, 0, 0
		return
	}
	read := out.ContextTokens - out.UncachedInputTokens - out.CacheWriteTokens
	if read < 0 {
		// The policy asked for a post-compaction conversation smaller than
		// what this call must write into it. Clamp the write rather than
		// report a negative read, and leave the context as stated.
		read = 0
		out.CacheWriteTokens = out.ContextTokens - out.UncachedInputTokens
		if out.CacheWriteTokens < 0 {
			out.CacheWriteTokens = 0
			out.UncachedInputTokens = out.ContextTokens
		}
	}
	out.CacheReadTokens = read
}

// Comparison is a before/after pair and the percentages between them.
type Comparison struct {
	Before Metrics
	After  Metrics
}

// Compare measures both sides.
func Compare(before, after Series) Comparison {
	return Comparison{Before: Measure(before), After: Measure(after)}
}

// ReductionPercent is how much smaller `after` is than `before`, as a
// percentage, for one metric. A zero or negative before returns 0 -- there is
// no percentage of nothing, and reporting one would be the arithmetic version
// of dividing by zero quietly.
func ReductionPercent(before, after int64) float64 {
	if before <= 0 {
		return 0
	}
	return 100 * float64(before-after) / float64(before)
}

// Segments lists the series' segment labels in first-appearance order, with
// the calls belonging to each.
func Segments(s Series) ([]string, map[string]Series) {
	order := []string{}
	by := map[string]Series{}
	for _, c := range s.Calls {
		if _, seen := by[c.Segment]; !seen {
			order = append(order, c.Segment)
			by[c.Segment] = Series{Source: s.Source, Strategy: s.Strategy}
		}
		seg := by[c.Segment]
		seg.Calls = append(seg.Calls, c)
		by[c.Segment] = seg
	}
	return order, by
}

// LoadSeries reads a recorded shape.
func LoadSeries(r io.Reader) (Series, error) {
	var s Series
	if err := json.NewDecoder(r).Decode(&s); err != nil {
		return Series{}, fmt.Errorf("decode series: %w", err)
	}
	if len(s.Calls) == 0 {
		return Series{}, fmt.Errorf("series %q has no calls", s.Source)
	}
	sort.SliceStable(s.Calls, func(i, j int) bool {
		a, b := s.Calls[i].ObservedAt, s.Calls[j].ObservedAt
		if a == "" || b == "" {
			return false
		}
		return a < b
	})
	return s, nil
}
