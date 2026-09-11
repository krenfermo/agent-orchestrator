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
	Calls    []Call                   `json:"calls"`
}

// Metrics is everything a before/after comparison needs, and nothing derived
// from an assumption.
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
	var firstAt, lastAt time.Time
	for _, c := range s.Calls {
		m.CumulativeInput += c.ContextTokens
		m.OutputTokens += c.OutputTokens
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
	return m
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
		if i > 0 {
			delta := s.Calls[i].ContextTokens - s.Calls[i-1].ContextTokens
			if out.Calls[i].Segment != segment {
				// A boundary: the conversation is replaced, so this call pays
				// the post-compaction level rather than everything before it.
				segment = out.Calls[i].Segment
				level = p.PostCompactContextTokens
			} else {
				level += delta
			}
		}
		if level < 0 {
			level = 0
		}
		out.Calls[i].ContextTokens = level
	}
	return out
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
