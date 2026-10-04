// Package projectmemory is AO's durable project memory: the small, long-lived
// facts about a project that are worth keeping between runs, together with the
// provenance that says where each one came from and whether it can still be
// believed.
//
// It is the store side of the project-memory work. The measurement side
// already exists in internal/observe/projectmemory, which records per-dispatch
// baseline evidence; this package READS that evidence as one memory source
// (see BaselineReader) rather than re-implementing or altering how it is
// recorded.
//
// Three properties are load-bearing:
//
//   - Durability with one home. Items live under AO's data dir (~/.ao/data,
//     or AO_DATA_DIR), never beside a checkout and never in an OS-default
//     application-data location — the hard rule in AGENTS.md.
//   - Idempotent ingestion. An item is addressed by a stable identity and
//     carries a hash of its content, so re-ingesting an unchanged fact leaves
//     the stored row byte-for-byte alone: no duplicate, no touched UpdatedAt.
//   - Provenance strong enough to invalidate. Every item names the commit it
//     was derived at and, where it is about a file, that file's hash. When the
//     commit is no longer reachable from HEAD or the file's content moved, the
//     item is marked stale rather than silently served as current — see
//     StaleCheck.
package projectmemory

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"
)

// ItemSchemaVersion identifies the shape of a stored memory item. It is
// written into every project file so a later schema change can recognise, and
// refuse to misread, what an older AO wrote.
const ItemSchemaVersion = 1

// ErrItemInvalid is the sentinel every item-level validation failure wraps.
var ErrItemInvalid = errors.New("projectmemory: invalid memory item")

// ItemType is what kind of fact an item holds. It is an open vocabulary — a
// later source can introduce its own type without a schema change — but the
// values below are the ones AO writes today.
type ItemType string

// Item types written by this package.
const (
	// TypeBaselineDispatch is one agent dispatch as the baseline harness
	// measured it: what its scope made available versus what AO actually sent.
	TypeBaselineDispatch ItemType = "baseline-dispatch"
	// TypeFileUsage is one file a dispatch read, and how much of it. It is the
	// per-file detail behind a TypeBaselineDispatch item.
	TypeFileUsage ItemType = "file-usage"
	// TypeNote is a free-form fact recorded by a caller that is not one of the
	// structured sources.
	TypeNote ItemType = "note"
)

// SourceKind names where an item came from. Like ItemType it is open, and the
// values below are what AO writes today.
type SourceKind string

// Source kinds written by this package.
const (
	// SourceBaselineEvidence is an item derived from a per-dispatch evidence
	// record written by internal/observe/projectmemory.
	SourceBaselineEvidence SourceKind = "baseline-evidence"
	// SourceManual is an item supplied directly by a caller.
	SourceManual SourceKind = "manual"
)

// Source is an item's evidence: where the fact came from and what it was
// derived from, in enough detail to check later whether it still holds.
type Source struct {
	// Kind names the producer (see SourceKind).
	Kind SourceKind `json:"kind"`
	// Ref is the producer's own stable identifier for this fact — an evidence
	// record id, a document id. Together with project, scope and type it is
	// the item's identity, which is what lets a later ingestion of the SAME
	// fact update the stored row instead of appending a second one.
	//
	// A source with no stable ref leaves this empty; such an item is
	// identified by its content hash instead.
	Ref string `json:"ref,omitempty"`
	// Path is the project-relative file this item is about, when it is about
	// one. It is the anchor for file-hash staleness.
	Path string `json:"path,omitempty"`
	// FileHash is the content hash of Path as it stood when the item was
	// derived. An item whose file no longer hashes to this is stale: the fact
	// was read off a version of the file that is no longer there.
	FileHash string `json:"fileHash,omitempty"`
	// Detail is a short human-readable note about the derivation (which
	// harness, which schema version). It never carries metrics.
	Detail string `json:"detail,omitempty"`
}

// MemoryItem is one durable fact about one project.
type MemoryItem struct {
	// ID is the deterministic identity hash (see Identity). It is derived, not
	// supplied: a caller that sets it has it overwritten on upsert, so two
	// ingestions of the same fact can never disagree about which row they are.
	ID string `json:"id"`
	// Project is the project this fact belongs to. Every read and write is
	// scoped by it, and each project's items live in their own file, so two
	// projects can never observe each other's memory.
	Project string `json:"project"`
	// Scope narrows the fact within the project: a module, a directory, a
	// dispatch role. Empty means project-wide.
	Scope string `json:"scope,omitempty"`
	// Type is what kind of fact this is (see ItemType).
	Type ItemType `json:"type"`
	// Content is the fact itself, as text an agent can be handed directly.
	Content string `json:"content"`
	// Source is the evidence behind Content (see Source).
	Source Source `json:"source"`
	// CreatedAt is when this item first entered the store; it survives every
	// later update of the same identity.
	CreatedAt time.Time `json:"createdAt"`
	// UpdatedAt is when its content or provenance last actually changed. An
	// upsert that changes nothing does not move it — that is the whole point
	// of hashing the content.
	UpdatedAt time.Time `json:"updatedAt"`
	// SourceCommit is the commit the fact was derived at. It is what makes
	// invalidation possible: a fact derived at a commit that is no longer
	// reachable from HEAD describes a history the project no longer has.
	SourceCommit string `json:"sourceCommit,omitempty"`
	// Confidence is how much weight the fact deserves, in [0,1]. It is set by
	// the source from what it could actually measure, never as a flat default
	// that would make a guess look like an observation.
	Confidence float64 `json:"confidence"`
	// Stale reports that the fact's provenance no longer holds. A stale item
	// is kept, not deleted: knowing that a fact went stale (and why) is itself
	// information, and re-deriving it is cheaper from the old row than from
	// nothing.
	Stale bool `json:"stale"`
	// StaleReason says why, and is empty exactly when Stale is false.
	StaleReason string `json:"staleReason,omitempty"`
	// ContentHash is the hash the idempotent upsert compares. It covers the
	// content and the fields that describe what the content is about.
	ContentHash string `json:"contentHash"`
}

// normalized trims the free-text fields, fills in the derived ones
// (ContentHash, ID) and clears a stale reason that no longer applies. It never
// invents a project, a type, or content — those are the caller's to get right,
// and Validate rejects an item that lacks them.
func (i MemoryItem) normalized() MemoryItem {
	i.Project = strings.TrimSpace(i.Project)
	i.Scope = strings.TrimSpace(i.Scope)
	i.Type = ItemType(strings.TrimSpace(string(i.Type)))
	i.Content = strings.TrimSpace(i.Content)
	i.SourceCommit = strings.TrimSpace(i.SourceCommit)
	i.Source.Kind = SourceKind(strings.TrimSpace(string(i.Source.Kind)))
	i.Source.Ref = strings.TrimSpace(i.Source.Ref)
	i.Source.Path = strings.TrimSpace(i.Source.Path)
	i.Source.FileHash = strings.TrimSpace(i.Source.FileHash)
	i.Source.Detail = strings.TrimSpace(i.Source.Detail)
	if i.Source.Kind == "" {
		i.Source.Kind = SourceManual
	}
	if !i.Stale {
		i.StaleReason = ""
	}
	i.ContentHash = i.contentHash()
	i.ID = i.Identity()
	return i
}

// contentHash covers exactly what "the same fact" means: the project it
// belongs to, what it is about (scope, type), and the text itself.
//
// Provenance is deliberately NOT hashed. A fact re-derived at a newer commit
// is the same fact, and the upsert has to be able to see that its content did
// not move while still refreshing the commit it was last confirmed at.
func (i MemoryItem) contentHash() string {
	h := sha256.New()
	for _, part := range []string{
		fmt.Sprintf("v%d", ItemSchemaVersion),
		i.Project,
		i.Scope,
		string(i.Type),
		i.Content,
	} {
		h.Write([]byte(part))
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))
}

// Identity is the key an upsert addresses. It is the project, scope and type
// plus the source's stable ref when there is one, and the content hash when
// there is not.
//
// The split matters. An item from a source that can name the same fact again
// (an evidence record id) must UPDATE when its content changes, or memory
// would accumulate one row per re-measurement of one dispatch. An item with no
// such ref has nothing but its content to be identified by, so a changed
// content is a different item — the honest answer when the source cannot say
// otherwise.
//
// Project is always part of the key, so the same content ingested for two
// projects yields two distinct items rather than one shared row.
func (i MemoryItem) Identity() string {
	h := sha256.New()
	parts := []string{
		fmt.Sprintf("v%d", ItemSchemaVersion),
		i.Project,
		i.Scope,
		string(i.Type),
		string(i.Source.Kind),
	}
	if i.Source.Ref != "" {
		parts = append(parts, "ref", i.Source.Ref)
	} else {
		parts = append(parts, "content", i.contentHash())
	}
	for _, part := range parts {
		h.Write([]byte(part))
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))[:32]
}

// Validate checks the fields an item cannot be stored without. It runs before
// every write, so an item that is on disk is one that names its project, says
// what kind of fact it is, carries content, and labels its confidence
// honestly.
func (i MemoryItem) Validate() error {
	if strings.TrimSpace(i.Project) == "" {
		return fmt.Errorf("%w: project is required", ErrItemInvalid)
	}
	if strings.TrimSpace(string(i.Type)) == "" {
		return fmt.Errorf("%w: type is required", ErrItemInvalid)
	}
	if strings.TrimSpace(i.Content) == "" {
		return fmt.Errorf("%w: content is required", ErrItemInvalid)
	}
	if strings.TrimSpace(string(i.Source.Kind)) == "" {
		return fmt.Errorf("%w: source kind is required", ErrItemInvalid)
	}
	if i.Confidence < 0 || i.Confidence > 1 {
		return fmt.Errorf("%w: confidence %v is outside [0,1]", ErrItemInvalid, i.Confidence)
	}
	if i.Stale && strings.TrimSpace(i.StaleReason) == "" {
		return fmt.Errorf("%w: a stale item must say why", ErrItemInvalid)
	}
	return nil
}

// sameFactAs reports whether other carries the same content and the same
// provenance as i — the test the upsert uses to decide that a write would
// change nothing. Timestamps, the derived id, and the staleness annotation are
// excluded: the first two are not facts about the content, and the third is
// recomputed by StaleCheck rather than supplied by an ingestion.
func (i MemoryItem) sameFactAs(other MemoryItem) bool {
	return i.ContentHash == other.ContentHash &&
		i.SourceCommit == other.SourceCommit &&
		i.Confidence == other.Confidence &&
		i.Source == other.Source
}
