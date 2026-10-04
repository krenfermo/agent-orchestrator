package contextrouter

import (
	baseline "github.com/aoagents/agent-orchestrator/backend/internal/observe/projectmemory"
)

// Origin says where a section's content came from, which is the distinction a
// context-savings claim rests on.
//
// A payload that is half the size of the unrouted one but was assembled by
// reading twice as much of the repository has not saved anything; it has moved
// the cost. Splitting the routed payload by origin is what makes the two cases
// distinguishable in the evidence rather than in an argument about it.
type Origin string

const (
	// OriginRead is content read for this dispatch: the caller's own context
	// documents, the current git diff, the task statement.
	OriginRead Origin = "read"
	// OriginIndexed is content reused from a store AO had already built — the
	// code graph index and durable project memory. It is what a dispatch got
	// without re-reading the repository for it.
	OriginIndexed Origin = "indexed"
)

// Origin classifies a section kind. Graph and memory sections are served from
// AO's own stores; everything else is content this dispatch read.
func (k SectionKind) Origin() Origin {
	switch k {
	case SectionGraph, SectionMemory:
		return OriginIndexed
	default:
		return OriginRead
	}
}

// Sizes measures a set of sections in bytes and estimated tokens, split by
// origin. The byte figures are measured; the token figures are estimated from
// them with the same heuristic the baseline harness uses, which is why a
// routed payload and a baseline payload can be compared at all.
type Sizes struct {
	// Sections is how many blocks were counted.
	Sections int `json:"sections"`
	// Bytes and EstimatedTokens size all of them.
	Bytes           int `json:"bytes"`
	EstimatedTokens int `json:"estimatedTokens"`
	// IndexedBytes and IndexedEstimatedTokens size the blocks served from AO's
	// own stores; ReadBytes and ReadEstimatedTokens size the rest. The two
	// byte figures sum to Bytes.
	IndexedBytes           int `json:"indexedBytes"`
	IndexedEstimatedTokens int `json:"indexedEstimatedTokens"`
	ReadBytes              int `json:"readBytes"`
	ReadEstimatedTokens    int `json:"readEstimatedTokens"`
	// Truncated counts blocks the packer had to cut. It is zero for a
	// candidate set, which is measured before any packing happens.
	Truncated int `json:"truncated"`
}

// sizeCandidates measures the candidate set at full length: each section's
// SourceBytes, which is the content it was built from before any retrieval cap
// cut it. That is what the dispatch would have been sent unrouted.
func sizeCandidates(sections []Section) Sizes {
	return sizeSections(sections, func(s Section) int { return s.SourceBytes })
}

// sizePacked measures the payload as sent.
func sizePacked(sections []Section) Sizes {
	return sizeSections(sections, func(s Section) int { return s.Bytes })
}

// sizeSections measures a set of sections with size. Token figures are derived
// per origin group from that group's byte total rather than summed per
// section, so the parts and the whole round the same way.
func sizeSections(sections []Section, size func(Section) int) Sizes {
	out := Sizes{Sections: len(sections)}
	for _, section := range sections {
		bytes := size(section)
		out.Bytes += bytes
		if section.Truncated {
			out.Truncated++
		}
		switch section.Kind.Origin() {
		case OriginIndexed:
			out.IndexedBytes += bytes
		default:
			out.ReadBytes += bytes
		}
	}
	out.EstimatedTokens = estimateTokensFromBytes(out.Bytes)
	out.IndexedEstimatedTokens = estimateTokensFromBytes(out.IndexedBytes)
	out.ReadEstimatedTokens = estimateTokensFromBytes(out.ReadBytes)
	return out
}

// estimateTokensFromBytes sizes a byte count the way the baseline harness
// does, so every figure this package reports is comparable with a recorded
// baseline one.
func estimateTokensFromBytes(bytes int) int {
	return int(baseline.EstimateTokensFromBytes(int64(bytes)))
}

// BaselineRouting turns the selection into the routing block an evidence
// record carries (see observe/projectmemory.RoutingMetrics).
//
// The conversion lives here rather than in the baseline package because the
// dependency already runs this way: the router sizes its payload with the
// baseline's own token heuristic, and reversing it would make the baseline
// schema depend on the router it is supposed to be able to measure without.
func (s Selection) BaselineRouting() baseline.RoutingMetrics {
	return baseline.RoutingSelected(baseline.RoutingSelection{
		Role:           string(s.Role),
		Tier:           string(s.Tier),
		Sections:       s.Selected.Sections,
		Dropped:        len(s.Dropped),
		Truncated:      s.Selected.Truncated,
		PotentialBytes: int64(s.Considered.Bytes),
		SelectedBytes:  int64(s.Selected.Bytes),
		ReusedBytes:    int64(s.Selected.IndexedBytes),
		NewBytes:       int64(s.Selected.ReadBytes),
		LimitTokens:    s.Limit,
		HardCapTokens:  s.Budget.HardCapTokens,
		Notes:          s.Notes,
	})
}
