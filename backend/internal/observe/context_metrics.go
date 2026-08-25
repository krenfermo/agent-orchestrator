package observe

import (
	baseline "github.com/aoagents/agent-orchestrator/backend/internal/observe/projectmemory"
)

// ContextSelectionSummary aggregates the context-selection metrics of a set of
// per-dispatch evidence records into one picture of a run.
//
// A single record answers "what did this dispatch get?". The question a run
// asks is "how much context did this whole run spend, and how much of it did
// the router avoid sending?", and that is a sum across records — which is
// exactly where the baseline's measured/estimated discipline is easiest to
// lose. Two rules keep it:
//
//   - Only measured bytes are summed. A metric a dispatch could not measure
//     contributes nothing to a total and is counted in Unmeasured instead, so
//     a partial total is visible as a partial total rather than reading as a
//     complete one.
//   - Nothing here sums a token figure. Token counts in the evidence are a
//     mix of provider-reported (measured) and byte-derived (estimated), and
//     adding those together would produce a number that is neither. The token
//     views below are derived from the byte totals with the baseline's own
//     heuristic and are labeled estimated at every call site.
//
// It is a read model over records that already exist; nothing here records or
// re-derives a metric. See internal/observe/projectmemory for the schema and
// docs/context-router-metrics.md for what each size means.
type ContextSelectionSummary struct {
	// Dispatches is how many records were summarized.
	Dispatches int `json:"dispatches"`
	// Routed is how many of them a context-router selection actually shaped;
	// Unrouted is how many carried a payload it did not. The two need not add
	// up to Dispatches: a dispatch that carries no payload at all (a verify
	// command run) has no routing story and is neither.
	Routed   int `json:"routed"`
	Unrouted int `json:"unrouted"`
	// Unmeasured counts records that carried a size this summary could not add
	// because the dispatch surface could not measure it. It is the honesty
	// counter: a nonzero value means every total below is a lower bound.
	Unmeasured int `json:"unmeasured"`

	// SentBytes is the measured total AO actually handed providers across
	// these dispatches, from each record's own context measurement — the
	// figure that exists whether or not a router was involved.
	SentBytes int64 `json:"sentBytes"`
	// PotentialBytes is what those dispatches could have been sent, and
	// SelectedBytes what the router chose to send, summed from the routing
	// blocks. ReusedBytes is the part of the selection drawn from stores AO
	// had already built (code graph, project memory) and NewBytes the part
	// read for the dispatch; the two sum to SelectedBytes.
	PotentialBytes int64 `json:"potentialBytes"`
	SelectedBytes  int64 `json:"selectedBytes"`
	ReusedBytes    int64 `json:"reusedBytes"`
	NewBytes       int64 `json:"newBytes"`
}

// SummarizeContextSelection aggregates the context-selection metrics of
// records. A record with no routing block still contributes its measured
// payload size to SentBytes: a run's context spend is a fact about the run,
// not about whether the router was switched on for it.
func SummarizeContextSelection(records []baseline.EvidenceRecord) ContextSelectionSummary {
	summary := ContextSelectionSummary{Dispatches: len(records)}
	for _, record := range records {
		summary.addSent(record)
		if record.Routing == nil {
			continue
		}
		if record.Routing.Enabled {
			summary.Routed++
		} else {
			summary.Unrouted++
		}
		summary.add(&summary.PotentialBytes, record.Routing.PotentialBytes)
		summary.add(&summary.SelectedBytes, record.Routing.SelectedBytes)
		summary.add(&summary.ReusedBytes, record.Routing.ReusedBytes)
		summary.add(&summary.NewBytes, record.Routing.NewBytes)
	}
	return summary
}

// addSent folds a record's own measured payload size in. A surface that cannot
// measure what it sent is not counted as having sent nothing.
func (s *ContextSelectionSummary) addSent(record baseline.EvidenceRecord) {
	s.add(&s.SentBytes, record.Context.ContextSentBytes)
}

// add folds one measured metric into a total, or counts it as unmeasured. An
// estimated metric is not summed either: these totals are byte counts, and
// every byte count in the evidence is measured or absent.
func (s *ContextSelectionSummary) add(total *int64, metric baseline.Metric) {
	if metric.Value == nil || metric.Basis != baseline.BasisMeasured {
		s.Unmeasured++
		return
	}
	*total += *metric.Value
}

// Complete reports that every size these totals cover was measured. A summary
// that is not complete is a lower bound, and any figure derived from it should
// be presented as one.
func (s ContextSelectionSummary) Complete() bool { return s.Unmeasured == 0 }

// ReductionPercent reports how much of the potential context the router did
// not send, as a percentage of the potential size, and whether the figure has
// a basis at all.
//
// It returns ok=false rather than 0 when nothing measured a potential size:
// "the router saved nothing" and "no measurement supports a saving figure" are
// different findings, and reporting the second as 0% would state a measurement
// that was never made.
func (s ContextSelectionSummary) ReductionPercent() (float64, bool) {
	return percentOf(s.PotentialBytes-s.SelectedBytes, s.PotentialBytes)
}

// ReusedSharePercent reports how much of what was sent came out of AO's own
// stores rather than being read for the dispatch. It is the counterweight to
// ReductionPercent: a payload that shrank because the router replaced read
// content with indexed content is a different result from one that shrank
// because less was sent.
func (s ContextSelectionSummary) ReusedSharePercent() (float64, bool) {
	return percentOf(s.ReusedBytes, s.SelectedBytes)
}

func percentOf(part, whole int64) (float64, bool) {
	if whole <= 0 {
		return 0, false
	}
	return float64(part) / float64(whole) * 100, true
}

// EstimatedTokens views a byte total from this summary as tokens, using the
// baseline's own bytes-per-token heuristic.
//
// It is a free function taking the byte total rather than a method per field
// so that no call site can mistake the result for a measured token count: the
// caller names the estimate every time it asks for one. AO does not run any
// provider's tokenizer — see projectmemory.EstimateMethod.
func EstimatedTokens(bytes int64) int64 {
	return baseline.EstimateTokensFromBytes(bytes)
}
