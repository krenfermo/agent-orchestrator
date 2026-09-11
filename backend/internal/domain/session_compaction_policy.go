package domain

import "time"

// session_compaction_policy.go -- the per-run opt-in for Checkpoint P7's
// compaction, and the provenance that makes it auditable.
//
// WorkflowPolicy.SessionCompactionEnabled has existed since P7 and has been
// unreachable: default false, no API/CLI field that could set it, and no
// inheritance rule carrying it to the child runs where all of an objective's
// real work happens. The knob was therefore only ever true inside a test.
//
// What was missing is not a second boolean. It is the RECORD of who set the
// one that exists. A bare bool has no state distinguishing "a person asked for
// this run to compact" from "this snapshot was written by a binary that had
// never heard of compaction", and without that distinction two things are
// impossible: saying honestly in an audit why a run compacted, and refusing to
// propagate a value nobody chose. Both are what this file adds.

// SessionCompactionPolicyVersion identifies the shape of a frozen
// session-compaction decision.
const SessionCompactionPolicyVersion = "session-compaction/v1"

// SessionCompactionSource names how a run's SessionCompactionEnabled came to
// hold its value.
type SessionCompactionSource string

const (
	// SessionCompactionExplicit is a per-run choice a caller actually made,
	// true or false. Only an explicit record makes the value inheritable.
	SessionCompactionExplicit SessionCompactionSource = "explicit"
	// SessionCompactionInherited is a child run carrying its parent
	// objective's explicit choice, recorded with the parent it came from.
	SessionCompactionInherited SessionCompactionSource = "inherited"
)

// Valid reports whether s names a source this binary understands.
func (s SessionCompactionSource) Valid() bool {
	switch s {
	case SessionCompactionExplicit, SessionCompactionInherited:
		return true
	}
	return false
}

// SessionCompactionProvenance is the durable record of where a run's
// SessionCompactionEnabled value came from. It is evidence, never a second
// copy of the value: the value itself stays in the single field every reader
// already consults, so nothing here can disagree with what the run executes.
//
// The zero value (Source == "") is a snapshot nobody recorded a choice on --
// every run created before this existed. It is never a defect and never
// healed; such a run simply keeps whatever SessionCompactionEnabled it has
// (false, for every run any production path has ever created) and never passes
// it to a child.
type SessionCompactionProvenance struct {
	Version string                  `json:"version,omitempty"`
	Source  SessionCompactionSource `json:"source,omitempty"`
	// RequestedBy is the operator identity that asked, for an explicit
	// choice. Empty when the caller could not be identified; an unknown
	// requester is recorded as unknown rather than invented.
	RequestedBy string `json:"requestedBy,omitempty"`
	// ParentRunID is the objective a child inherited from, for an inherited
	// record.
	ParentRunID string    `json:"parentRunId,omitempty"`
	At          time.Time `json:"at,omitempty"`
}

// Recorded reports whether a real decision is behind this run's compaction
// flag, as opposed to the zero value a run created before this model carries.
//
// This is the gate inheritance and audit both read. It is deliberately NOT
// "SessionCompactionEnabled is true": an explicit false is just as much a
// decision, and a run that was explicitly told not to compact must pass that
// refusal to its children rather than letting them fall back to a default that
// happens to agree today.
func (p SessionCompactionProvenance) Recorded() bool {
	return p.Source.Valid()
}
