package domain

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
	"time"
)

// task_knowledge.go — the shared-task-knowledge vocabulary (P2-C).
//
// P2-A gave AO a durable, bounded, provenance-carrying fact (ProjectMemoryItem)
// and P2-B made every role receive one. What neither settled is the question
// P2-C exists to answer: **when one task learns something, which later task may
// be told, and for how long does it stay true?**
//
// The answer is deliberately built ON the P2-A item rather than beside it.
// There is no second table, no parallel knowledge row and no second identity
// scheme, because a shared fact and a derived fact need exactly the same three
// things — an identity, a provenance and a generation fence — and a second
// model would mean two answers to "is this still true".
//
// What P2-C adds is a small, bounded LIFECYCLE carried in the item's existing
// Metadata map:
//
//   - A **status**: a decision is active, superseded or invalidated; a risk or
//     follow-up is open, resolved or obsolete. Nothing is ever deleted to
//     change a status, so an audit can always reconstruct what was believed at
//     any point.
//   - A **subject**: the stable identity of *what the fact is about*, separate
//     from the task that happened to state it. Two tasks that decide the same
//     thing address the same subject, which is what makes supersession
//     possible and what stops the same decision accumulating once per task.
//   - A **share scope**: whether the fact may reach only its own task, the
//     tasks downstream of it in one workflow, or the project. This is the
//     sibling-safety rule made explicit, rather than inferred from the origin.
//
// Metadata is the right home for all three. It is already bounded
// (MaxProjectMemoryMetadata), already normalised, already round-trips through
// storage, and already participates in the item's content hash — so a status
// change is a real content change with a real UpdatedAt, and two writers that
// disagree about a status cannot both silently win.
//
// The mapping from the P2-C knowledge vocabulary onto the P2-A item types is
// stated once, here, so nothing has to guess it:
//
//	task_result             -> MemoryTypeTaskResult
//	decision                -> MemoryTypeDecision
//	known_risk              -> MemoryTypeKnownRisk, KnowledgeKindRisk
//	follow_up               -> MemoryTypeKnownRisk, KnowledgeKindFollowUp
//	convention_discovered   -> MemoryTypeConvention
//	architecture_observation-> MemoryTypeArchitecture
//	dependency_observation  -> MemoryTypeDependency
//	verification_fact       -> MemoryTypeTaskResult (the Verified-by line)
//
// Nothing here persists reasoning or a transcript, and there is nowhere to put
// one: every field below is a status, an identity or a reference.

// Metadata keys carrying the shared-knowledge lifecycle.
//
// They are short because MaxProjectMemoryMetadata bounds the map to sixteen
// entries and a task fact already spends three of them.
const (
	// MetaKnowledgeStatus is the lifecycle status (see KnowledgeStatus).
	MetaKnowledgeStatus = "status"
	// MetaKnowledgeKind distinguishes a risk from a follow-up within
	// MemoryTypeKnownRisk.
	MetaKnowledgeKind = "kind"
	// MetaKnowledgeSubject is the stable identity of what the fact is about.
	MetaKnowledgeSubject = "subject"
	// MetaKnowledgeSupersededBy names the item id that replaced this one.
	MetaKnowledgeSupersededBy = "supersededBy"
	// MetaKnowledgeSupersedes names the item id this one replaced.
	MetaKnowledgeSupersedes = "supersedes"
	// MetaKnowledgeResolvedBy names the task ref that resolved a risk.
	MetaKnowledgeResolvedBy = "resolvedBy"
	// MetaKnowledgeConflictsWith names an item that asserts the incompatible
	// opposite of this one and that AO could not order.
	MetaKnowledgeConflictsWith = "conflictsWith"
	// MetaKnowledgeShare is the sharing scope (see KnowledgeShare).
	MetaKnowledgeShare = "share"
	// MetaKnowledgeTask is the task that produced the fact. It predates P2-C
	// and is named here so both halves address the same key.
	MetaKnowledgeTask = "task"
	// MetaKnowledgeRun is the workflow run the producing task belonged to,
	// when there was one. It is what scopes workflow-local sharing.
	MetaKnowledgeRun = "run"
	// MetaKnowledgeIntegrated records whether the producing work was part of
	// the repository's integrated state. It predates P2-C.
	MetaKnowledgeIntegrated = "integrated"
	// MetaKnowledgeAggregate marks a compaction aggregate and names how many
	// facts it stands for.
	MetaKnowledgeAggregate = "aggregate"
)

// KnowledgeStatus is where one shared fact is in its lifecycle.
//
// Decisions and risks share one enum rather than having two, because the three
// meaningful states are the same in both: it still holds, something replaced
// it, or it stopped applying. The words differ in prose only.
type KnowledgeStatus string

// Knowledge statuses.
const (
	// KnowledgeActive is a decision that still governs, or a risk still open.
	// It is the only status normal retrieval serves.
	KnowledgeActive KnowledgeStatus = "active"
	// KnowledgeSuperseded is a decision a later decision replaced. It is kept
	// forever: "we used to do X, then decided Y" is the one thing a deleted
	// decision can never tell anyone.
	KnowledgeSuperseded KnowledgeStatus = "superseded"
	// KnowledgeResolved is a risk or follow-up a later task closed.
	KnowledgeResolved KnowledgeStatus = "resolved"
	// KnowledgeObsolete is a fact whose subject stopped existing — the module
	// was deleted, the dependency was dropped. It was never replaced; it
	// simply no longer applies.
	KnowledgeObsolete KnowledgeStatus = "obsolete"
	// KnowledgeConflicting is a fact AO could not order against an
	// incompatible peer. It is deliberately NOT served as current knowledge:
	// it reaches a Planner as a stated conflict, and nobody else.
	KnowledgeConflicting KnowledgeStatus = "conflicting"
)

// Current reports whether a fact in this status may be served as something AO
// currently vouches for.
//
// Only KnowledgeActive may. In particular KnowledgeConflicting may not: an
// unresolved contradiction is information about the project's memory, not
// information about the project, and handing it to a Worker as fact would be
// choosing a side silently.
func (s KnowledgeStatus) Current() bool { return s == "" || s == KnowledgeActive }

// Valid reports whether the status is one this build writes.
func (s KnowledgeStatus) Valid() bool {
	switch s {
	case KnowledgeActive, KnowledgeSuperseded, KnowledgeResolved,
		KnowledgeObsolete, KnowledgeConflicting:
		return true
	default:
		return false
	}
}

// KnowledgeKind distinguishes the two things MemoryTypeKnownRisk carries.
type KnowledgeKind string

// Knowledge kinds within MemoryTypeKnownRisk.
const (
	// KnowledgeKindRisk is something that may go wrong.
	KnowledgeKindRisk KnowledgeKind = "risk"
	// KnowledgeKindFollowUp is work deliberately left undone.
	KnowledgeKindFollowUp KnowledgeKind = "follow_up"
)

// KnowledgeShare is how far one fact may travel.
//
// This is the sibling-safety rule (P2-C §15) written down as a field instead
// of inferred. Origin says where a fact CAME from; share says who may READ it,
// and the two are not the same question: a task-local fact from a verified
// task that a dependent task explicitly waits on may be shared with that
// dependent task, while a task-local fact from a task still running may not
// reach anyone.
type KnowledgeShare string

// Knowledge sharing scopes, narrowest first.
const (
	// ShareTask is readable only by the task that produced it. It is the
	// default, and the only safe default: a fact whose sharing was never
	// decided must not travel.
	ShareTask KnowledgeShare = "task"
	// ShareWorkflow is readable by tasks downstream of the producer within the
	// same workflow run, and by nobody else. It is what lets Task 2 use Task
	// 1's verified result without waiting for the parent workflow to finish,
	// while a sibling that does not depend on Task 1 still sees nothing.
	ShareWorkflow KnowledgeShare = "workflow"
	// ShareCanonical is project knowledge. Only integrated work reaches it.
	ShareCanonical KnowledgeShare = "canonical"
)

// Valid reports whether the sharing scope is one this build writes.
func (s KnowledgeShare) Valid() bool {
	switch s {
	case ShareTask, ShareWorkflow, ShareCanonical:
		return true
	default:
		return false
	}
}

// SharedKnowledgeTypes are the item types that carry what ONE TASK learned, as
// opposed to what the repository is.
//
// The distinction decides a policy, so it is named rather than open-coded: a
// fact about the repository is relevant to anyone working in that repository,
// but a fact about what some earlier task did is relevant only to work that
// overlaps it. Everything in this set therefore has to EARN its place in a
// pack (see the relevance gate in the pack builder); everything outside it is
// admitted on the repository's authority alone.
func SharedKnowledgeTypes() []ProjectMemoryType {
	return []ProjectMemoryType{
		MemoryTypeTaskResult,
		MemoryTypeDecision,
		MemoryTypeKnownRisk,
	}
}

// IsSharedKnowledgeType reports whether a type carries one task's learning.
func IsSharedKnowledgeType(t ProjectMemoryType) bool {
	switch t {
	case MemoryTypeTaskResult, MemoryTypeDecision, MemoryTypeKnownRisk:
		return true
	default:
		return false
	}
}

// KnowledgeStatusOf reads one item's lifecycle status.
//
// An item written before P2-C carries no status and is reported active, which
// is the right reading: it was written as a current fact and nothing has
// replaced it. Legacy memory therefore keeps working untouched, which is the
// P2-C §22 requirement.
func KnowledgeStatusOf(i ProjectMemoryItem) KnowledgeStatus {
	raw := KnowledgeStatus(strings.TrimSpace(i.Metadata[MetaKnowledgeStatus]))
	if raw == "" {
		return KnowledgeActive
	}
	return raw
}

// KnowledgeShareOf reads how far one item may travel.
//
// The default follows the origin, and is the conservative reading in both
// directions: a canonical fact is project knowledge, and a task-local fact
// with no explicit sharing decision reaches only its own task.
func KnowledgeShareOf(i ProjectMemoryItem) KnowledgeShare {
	if raw := KnowledgeShare(strings.TrimSpace(i.Metadata[MetaKnowledgeShare])); raw.Valid() {
		return raw
	}
	if i.Origin == OriginCanonical {
		return ShareCanonical
	}
	return ShareTask
}

// KnowledgeKindOf reads whether a known-risk item is a risk or a follow-up.
func KnowledgeKindOf(i ProjectMemoryItem) KnowledgeKind {
	if raw := KnowledgeKind(strings.TrimSpace(i.Metadata[MetaKnowledgeKind])); raw != "" {
		return raw
	}
	return KnowledgeKindRisk
}

// KnowledgeRunOf reads the workflow run a fact was produced in, if any.
func KnowledgeRunOf(i ProjectMemoryItem) string {
	return strings.TrimSpace(i.Metadata[MetaKnowledgeRun])
}

// KnowledgeTaskOf reads the task that produced a fact, if any.
//
// It prefers the recorded metadata over OriginRef, because promotion clears
// OriginRef — a promoted decision is the project's, but it still knows which
// task made it.
func KnowledgeTaskOf(i ProjectMemoryItem) string {
	if task := strings.TrimSpace(i.Metadata[MetaKnowledgeTask]); task != "" {
		return task
	}
	return strings.TrimSpace(i.OriginRef)
}

// KnowledgeSubjectOf reads the stable identity of what a fact is about.
//
// Falling back to the item's own id is deliberate: a fact with no declared
// subject is about itself, so it can never be superseded by accident. Silence
// must not be read as "same subject as".
func KnowledgeSubjectOf(i ProjectMemoryItem) string {
	if s := strings.TrimSpace(i.Metadata[MetaKnowledgeSubject]); s != "" {
		return s
	}
	return i.ID
}

// KnowledgeSubject derives the subject identity of a statement.
//
// It is a hash of the statement's significant words, so "use GraphQL for the
// public API" and "Use GraphQL for the public API." address the same subject
// while "use REST for the public API" does not. That is deliberately a
// STRUCTURAL match rather than a semantic one: AO can prove two statements are
// the same text, and it cannot prove two different texts mean the same thing.
// Supersession therefore needs an explicit statement of what it replaces
// whenever the wording changed — which is the honest boundary, and is why
// SupersededSubject exists as a separate, caller-supplied signal.
//
// The scope participates so the same sentence about two different modules is
// two subjects.
func KnowledgeSubject(scope ProjectMemoryScope, scopeKey, statement string) string {
	words := significantWords(statement)
	h := sha256.New()
	h.Write([]byte(strings.ToLower(strings.TrimSpace(string(scope)))))
	h.Write([]byte{0})
	h.Write([]byte(strings.ToLower(strings.TrimSpace(scopeKey))))
	h.Write([]byte{0})
	for _, w := range words {
		h.Write([]byte(w))
		h.Write([]byte{0})
	}
	return "sub_" + hex.EncodeToString(h.Sum(nil))[:20]
}

// significantWords reduces a statement to the sorted set of words that carry
// its meaning, so punctuation, ordering and filler cannot make one statement
// look like two.
//
// The stop list is small on purpose. A large one would start deciding that two
// statements mean the same thing, which is exactly the judgement this function
// must not make.
func significantWords(statement string) []string {
	stop := map[string]struct{}{
		"the": {}, "a": {}, "an": {}, "we": {}, "to": {}, "of": {}, "for": {},
		"and": {}, "or": {}, "in": {}, "on": {}, "is": {}, "are": {}, "be": {},
		"will": {}, "should": {}, "must": {}, "this": {}, "that": {}, "it": {},
	}
	fields := strings.FieldsFunc(strings.ToLower(statement), func(r rune) bool {
		return (r < 'a' || r > 'z') && (r < '0' || r > '9') && r != '_' && r != '-' && r != '/' && r != '.'
	})
	seen := make(map[string]struct{}, len(fields))
	out := make([]string, 0, len(fields))
	for _, f := range fields {
		f = strings.Trim(f, "-_./")
		if f == "" {
			continue
		}
		if _, skip := stop[f]; skip {
			continue
		}
		if _, dup := seen[f]; dup {
			continue
		}
		seen[f] = struct{}{}
		out = append(out, f)
	}
	sort.Strings(out)
	return out
}

// WithKnowledgeMetadata returns the item with one metadata key set, without
// mutating the caller's map.
//
// Copying rather than writing through is not politeness: memory items are
// passed by value and a shared map would let a status write on one copy change
// a fact another caller is still reading.
func WithKnowledgeMetadata(i ProjectMemoryItem, key, value string) ProjectMemoryItem {
	meta := make(map[string]string, len(i.Metadata)+1)
	for k, v := range i.Metadata {
		meta[k] = v
	}
	if strings.TrimSpace(value) == "" {
		delete(meta, key)
	} else {
		meta[key] = value
	}
	i.Metadata = meta
	return i
}

// Knowledge relation kinds added by P2-C.
//
// They join the P2-A kinds rather than replacing any: the graph's vocabulary
// is open, and a backend that does not know these labels stores them as it
// stores the others.
const (
	// RelationProduced is task -> knowledge item. It is the provenance edge
	// that makes "what did we learn from this task" one traversal.
	RelationProduced ProjectMemoryRelationKind = "produced"
	// RelationSupersedes is decision -> decision, newest first. Following it
	// backwards reconstructs what the project used to believe.
	RelationSupersedes ProjectMemoryRelationKind = "supersedes"
	// RelationResolvedBy is risk -> task. A resolved risk keeps its edge, so
	// "who closed this" survives the risk leaving normal retrieval.
	RelationResolvedBy ProjectMemoryRelationKind = "resolved_by"
	// RelationFollowsUp is task -> task or knowledge -> knowledge.
	RelationFollowsUp ProjectMemoryRelationKind = "follows_up"
	// RelationConcerns is risk -> module.
	RelationConcerns ProjectMemoryRelationKind = "concerns"
	// RelationConflictsWith is knowledge -> knowledge, for a contradiction AO
	// could not order. It is symmetric in meaning and written in both
	// directions, so a traversal from either side finds the other.
	RelationConflictsWith ProjectMemoryRelationKind = "conflicts_with"
)

// NodeKnowledge is a memory item as a graph endpoint, addressed by its item id.
//
// It is distinct from NodeDecision — which names a decision by subject — for
// the same reason an item id is distinct from a key: an edge that must survive
// supersession has to name the exact row, not the current answer.
const NodeKnowledge ProjectMemoryNodeKind = "knowledge"

// MemoryContextManifest is the durable record of what one execution was told
// (P2-C §16).
//
// It carries IDENTITIES, never content. The facts themselves are in
// ProjectMemoryItem, and a manifest that copied them would both duplicate the
// store and go out of date; a manifest that names them stays correct forever
// and stays useful precisely when the items have since been superseded, which
// is the case it exists for. There is deliberately nowhere to put the rendered
// prompt: the pack digest already proves which memory was sent, and storing
// the prompt to prove it again would be storing a transcript.
type MemoryContextManifest struct {
	// ID is derived (see MemoryContextManifestID), so re-provisioning the same
	// context after a restart addresses the same row.
	ID        string
	ProjectID ProjectID
	RepoID    string
	// WorkflowRunID and TaskRef name the execution. Both may be empty: a
	// dispatch outside a workflow still has a role and still received facts.
	WorkflowRunID string
	TaskRef       string
	// Role is the pack role the context was assembled for.
	Role string
	// PackDigest identifies the exact memory. Two manifests with the same
	// digest describe the same context, whatever else differs.
	PackDigest string
	// PolicyVersion is the selection policy that produced it, so a pack that
	// would be assembled differently today is recognisable rather than
	// silently compared against one from a different policy.
	PolicyVersion int
	// Generation and IndexedCommit are the memory state it was served from.
	Generation    int64
	IndexedCommit string
	// ItemIDs are the facts, in the order the pack presented them.
	ItemIDs []string
	// SelectedBytes and EstimatedTokens are what the pack cost.
	SelectedBytes   int
	EstimatedTokens int
	CreatedAt       time.Time
	UpdatedAt       time.Time
}

// MemoryContextManifestID derives a manifest's identity.
//
// The pack digest participates, so a task re-provisioned with DIFFERENT memory
// gets a second manifest rather than overwriting the first — which is correct:
// two different premises are two observations, and a restart that changed what
// the task knows is exactly the thing an operator needs to be able to see.
func MemoryContextManifestID(projectID ProjectID, runID, taskRef, role, packDigest string) string {
	h := sha256.New()
	for _, part := range []string{
		fmt.Sprintf("v%d", ProjectMemorySchemaVersion),
		string(projectID), strings.TrimSpace(runID), strings.TrimSpace(taskRef),
		strings.TrimSpace(role), strings.TrimSpace(packDigest),
	} {
		h.Write([]byte(part))
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))[:32]
}

// Normalized fills in the manifest's derived fields and trims its parts.
func (m MemoryContextManifest) Normalized() MemoryContextManifest {
	m.RepoID = strings.TrimSpace(m.RepoID)
	m.WorkflowRunID = strings.TrimSpace(m.WorkflowRunID)
	m.TaskRef = strings.TrimSpace(m.TaskRef)
	m.Role = strings.TrimSpace(m.Role)
	m.PackDigest = strings.TrimSpace(m.PackDigest)
	ids := make([]string, 0, len(m.ItemIDs))
	for _, id := range m.ItemIDs {
		if id = strings.TrimSpace(id); id != "" {
			ids = append(ids, id)
		}
	}
	m.ItemIDs = ids
	m.ID = MemoryContextManifestID(m.ProjectID, m.WorkflowRunID, m.TaskRef, m.Role, m.PackDigest)
	return m
}

// Validate rejects a manifest that cannot be stored.
func (m MemoryContextManifest) Validate() error {
	n := m.Normalized()
	if strings.TrimSpace(string(n.ProjectID)) == "" {
		return fmt.Errorf("%w: a context manifest must name its project", ErrProjectMemoryInvalid)
	}
	if n.Role == "" {
		return fmt.Errorf("%w: a context manifest must name the role it was assembled for", ErrProjectMemoryInvalid)
	}
	if n.SelectedBytes < 0 || n.EstimatedTokens < 0 || n.Generation < 0 {
		return fmt.Errorf("%w: a context manifest cannot carry negative counters", ErrProjectMemoryInvalid)
	}
	if len(n.ItemIDs) > MaxManifestItems {
		return fmt.Errorf("%w: %d manifest items, over the %d cap",
			ErrProjectMemoryInvalid, len(n.ItemIDs), MaxManifestItems)
	}
	return nil
}

// MaxManifestItems bounds a manifest. It is comfortably above the largest pack
// any role's budget admits, so it never truncates a real manifest — it exists
// so a corrupted or hostile caller cannot write an unbounded row.
const MaxManifestItems = 512
