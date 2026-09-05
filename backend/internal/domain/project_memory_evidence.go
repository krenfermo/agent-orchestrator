package domain

// project_memory_evidence.go — P4-H's evidence class: how strong the claim
// behind a fact is, as a first-class axis.
//
// AO already records two things about a fact's provenance, and neither
// answers this question:
//
//	ProvenanceKind   which PROOF applies (a repo derivation, a task outcome,
//	                 a workflow row, a legacy write). It says which validator
//	                 to run.
//	MemoryAuthority  whether that proof currently HOLDS.
//
// What was missing is what the P4-H brief asks for directly: never present an
// inference as a confirmed fact. Two items can both be repo derivations with
// intact authority and still be very different claims — one is a sentence
// quoted verbatim out of AGENTS.md, the other is AO's own reading of a
// directory census. A reader who cannot tell those apart will act on the
// second as if somebody had written it down.
//
// So the class travels on the row rather than being re-guessed per surface.
// It is set by the PRODUCER, which is the only party that knows whether it
// copied something or concluded it, and it is rendered wherever a fact is
// shown.
//
// The empty class is legal and means "this row does not say". Every row
// written before P4-H has it, and inventing a class for those would be the
// same fabrication this axis exists to prevent.

// MemoryEvidenceClass is how strong the claim behind one durable fact is:
// something AO read, something it concluded, something a person stated, or
// something a verified workflow established.
type MemoryEvidenceClass string

// The evidence classes, from strongest claim to weakest.
const (
	// EvidenceWorkflowVerified is knowledge a workflow produced AND that
	// passed review/verification before promotion. It is the strongest class
	// because something actually ran and something actually checked it: a
	// build command in this class is one AO watched succeed.
	EvidenceWorkflowVerified MemoryEvidenceClass = "workflow_verified"
	// EvidenceUserProvided is a fact a person stated. AO does not get to
	// overwrite it from a derivation; a derivation that contradicts it raises
	// a conflict instead (see ConflictOutcome).
	EvidenceUserProvided MemoryEvidenceClass = "user_provided"
	// EvidenceObserved is content AO read directly and is repeating: an
	// excerpt of an instruction file, a declared manifest field, a table name
	// out of a migration. The claim is "the repository says this", and it is
	// checkable by opening the named file.
	EvidenceObserved MemoryEvidenceClass = "observed"
	// EvidenceDerived is AO's own conclusion from evidence it observed — a
	// module census, an architecture summary, "authorisation is decided in
	// these three files". It is the class that must never be rendered as if
	// somebody had written it down, and it is why this axis exists.
	EvidenceDerived MemoryEvidenceClass = "derived"
)

// Valid reports whether the class is one this build writes. The empty class is
// NOT valid — it is the absence of a claim, which callers test for directly
// rather than by asking whether it is a class.
func (c MemoryEvidenceClass) Valid() bool {
	switch c {
	case EvidenceWorkflowVerified, EvidenceUserProvided, EvidenceObserved, EvidenceDerived:
		return true
	default:
		return false
	}
}

// Inferred reports whether the class describes AO's own conclusion rather than
// something it copied or was told. A surface that renders a fact must mark
// these, and a surface that lets memory REPLACE a source must refuse them.
func (c MemoryEvidenceClass) Inferred() bool { return c == EvidenceDerived }

// Rank orders the classes by how much weight a fact deserves when two of them
// answer the same question. It is used for conflict resolution: a
// workflow-verified fact supersedes a derived one about the same subject, and
// a derived one never supersedes anything stronger.
//
// An unknown or unset class ranks LOWEST — below derived — so a row that does
// not say what it is can never win an argument with one that does.
func (c MemoryEvidenceClass) Rank() int {
	switch c {
	case EvidenceWorkflowVerified:
		return 4
	case EvidenceUserProvided:
		return 3
	case EvidenceObserved:
		return 2
	case EvidenceDerived:
		return 1
	default:
		return 0
	}
}

// WithEvidenceClass returns a copy of the item labelled with how strong its
// claim is.
//
// It exists as a builder rather than as a parameter on every constructor
// because the class is a property of the DERIVATION, not of the item's shape:
// the same summary/content pair is `observed` when AO quoted it and `derived`
// when AO concluded it, and the producer is the only party that knows which.
// Threading it through every constructor signature would put the answer in the
// hands of callers who do not have it.
func (i ProjectMemoryItem) WithEvidenceClass(c MemoryEvidenceClass) ProjectMemoryItem {
	i.EvidenceClass = c
	return i
}
