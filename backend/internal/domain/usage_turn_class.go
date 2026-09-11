package domain

// usage_turn_class.go -- what each provider call DID, beside what it cost.
//
// The ledger can already say what a call was FOR: every event resolves to an
// attribution window carrying a role and a repair cycle, so "reviewer",
// "fix_worker cycle 2" and "planner" are answered facts. What it could not say
// is what the model actually spent the turn doing -- and that is the half of
// the question anybody asks first when a one-field defect costs 193 calls.
//
// A turn class is derived from ONE thing: the content blocks of the billed
// assistant message the usage parser is already reading, reduced to their block
// type and, for a tool call, the tool's NAME. Nothing else is read and nothing
// else is kept. No command text, no arguments, no paths, no message body ever
// reaches this type or the column behind it. That restraint is the point: the
// P5 dynamics reader named "excessive polling" and "the same suite run again"
// as underivable because detecting them looked like it required a durable
// ledger of command text. It does not. It requires knowing that a call's only
// act was to check on something it had already started, and the tool name says
// that by itself.
//
// HOW THIS MAPS TO THE USUAL VOCABULARY. "Verification", "review", "recovery"
// and "finalization" are ROLES, not turn shapes: a review turn and a work turn
// can both be a single Bash call, and telling them apart from the transcript
// alone would be a guess. AO already stores that axis on the attribution
// window, so the two compose -- role says what the call was for, class says
// what it did, and a reader wanting "how many of the reviewer's calls were
// reads" gets it from the pair without either axis inventing the other.

// TurnClass is the closed vocabulary of turn shapes. Closed for the same
// reason every other reason vocabulary in AO is closed: a class no UI can
// translate and no test can pin is not telemetry, it is noise.
type TurnClass string

// TurnClass values.
const (
	// TurnUnclassified is the absence of a class, never a class. It is what a
	// row written before migration 0169 carries, what a Codex rollout carries
	// (its envelope does not expose per-call tool blocks), and what a
	// malformed record carries. A read model reports its share; it never
	// folds it into a real class, because unknown is not zero.
	TurnUnclassified TurnClass = ""
	// TurnEdit -- the call mutated the workspace. The only class that changes
	// anything durable by itself.
	TurnEdit TurnClass = "edit"
	// TurnCommand -- the call ran something (a build, a suite, a query, a
	// shell pipeline). Work, and also the class whose RESULT does most of the
	// growing.
	TurnCommand TurnClass = "command"
	// TurnRead -- the call read or searched without mutating and without
	// running anything.
	TurnRead TurnClass = "read"
	// TurnWait -- the call's only act was to check on work it had already
	// started elsewhere. This is the polling shape, and the one turn class
	// that is pure overhead by construction: the model paid a whole context
	// re-read to ask "is it done yet".
	TurnWait TurnClass = "wait"
	// TurnSubagent -- the call delegated into a separate context. One turn
	// here stands in for a conversation that never entered this one.
	TurnSubagent TurnClass = "subagent"
	// TurnPlan -- the call recorded plan or task state and did nothing else.
	TurnPlan TurnClass = "plan"
	// TurnMessage -- the call spoke and invoked nothing. The last one is the
	// result; the ones before it are the recapitulation habit that Checkpoint
	// P7's output discipline exists to bound.
	TurnMessage TurnClass = "message"
	// TurnMixed -- the call did more than one kind of thing at once. It is
	// listed last because it is the only class that is GOOD NEWS: a mixed turn
	// is several turns that did not happen, and collapsing it into whichever
	// class happened to come first would hide exactly the behaviour worth
	// encouraging.
	TurnMixed TurnClass = "mixed"
)

// Valid reports whether a class is part of the closed vocabulary. The empty
// string is valid: it is the honest "not classified", and rejecting it would
// force a writer to invent one.
func (c TurnClass) Valid() bool {
	switch c {
	case TurnUnclassified, TurnEdit, TurnCommand, TurnRead, TurnWait,
		TurnSubagent, TurnPlan, TurnMessage, TurnMixed:
		return true
	default:
		return false
	}
}

// Coordination reports whether a class is overhead rather than work.
//
// The split is deliberately conservative in the direction that UNDERSTATES
// overhead: a command that turned out to be a no-op still counts as work,
// because AO cannot see that it was. Only the three shapes that are overhead
// by construction count as coordination -- a check on something already
// started, a plan-state write, and a turn that said something and did nothing.
func (c TurnClass) Coordination() bool {
	switch c {
	case TurnWait, TurnPlan, TurnMessage:
		return true
	default:
		return false
	}
}

// TurnMix is a fold of turn classes over some scope: a run, a step, a session.
//
// It is a COUNT of calls, never a sum of tokens. Two calls of the same class
// can differ tenfold in cost, so a mix answers "what was the model doing" and
// the trajectory beside it answers "what did that cost"; multiplying them
// together is the reader's business and not this type's.
type TurnMix struct {
	// Counts is the per-class call count. A class with no calls is absent
	// rather than present-and-zero, so a caller iterating it never renders a
	// row for something that did not happen.
	Counts map[TurnClass]int64
	// Classified and Unclassified partition the scope's calls. Their sum is
	// the scope's ProviderCalls, and Unclassified > 0 makes every share below
	// a share OF THE CLASSIFIED CALLS ONLY, which a UI must say.
	Classified   int64
	Unclassified int64
}

// AddTurn folds one call's class into the mix.
func (m *TurnMix) AddTurn(c TurnClass) {
	if c == TurnUnclassified || !c.Valid() {
		m.Unclassified++
		return
	}
	if m.Counts == nil {
		m.Counts = map[TurnClass]int64{}
	}
	m.Counts[c]++
	m.Classified++
}

// Count returns one class's call count.
func (m TurnMix) Count(c TurnClass) int64 { return m.Counts[c] }

// CoordinationCalls is how many classified calls were overhead by
// construction. See TurnClass.Coordination for what that deliberately
// excludes.
func (m TurnMix) CoordinationCalls() int64 {
	var n int64
	for class, count := range m.Counts {
		if class.Coordination() {
			n += count
		}
	}
	return n
}

// WorkCalls is the classified remainder: every call that did something to the
// workspace, ran something, read something, or delegated.
func (m TurnMix) WorkCalls() int64 { return m.Classified - m.CoordinationCalls() }

// CoordinationShare is the percentage of CLASSIFIED calls that were
// coordination, and whether the question is answerable at all. A scope with no
// classified call has no share -- reporting 0% for "AO could not classify any
// of these" is the exact misreport this second return value prevents.
func (m TurnMix) CoordinationShare() (int, bool) {
	if m.Classified <= 0 {
		return 0, false
	}
	return int(m.CoordinationCalls() * 100 / m.Classified), true
}
