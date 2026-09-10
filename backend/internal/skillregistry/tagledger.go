package skillregistry

import (
	"fmt"
	"strings"
	"time"
)

// tagledger.go -- remembering where a tag pointed, so that it moving is a
// FACT rather than something nobody noticed.
//
// # The attack, in three lines
//
//	Monday:   v1.2.3 -> commit A. Somebody reads the code and installs it.
//	Thursday: v1.2.3 -> commit B. One force-push; the tag name is unchanged.
//	Friday:   everybody who reinstalls "the same version" gets B.
//
// Nothing in git prevents it, no forge notifies anybody, and a registry client
// that resolves a tag fresh every time will follow it without a word -- which
// is the worst of both worlds, because it looks like immutability and is not.
//
// # What AO does instead
//
// It writes down, per (registry, owner, repository, tag), the commit it saw.
// Every later resolution compares. A disagreement is a MOVED TAG, and a moved
// tag is:
//
//	SHOWN            in the marketplace, on the row, permanently
//	AUDITED          as external_tag_moved, once, with both SHAs
//	NEVER FOLLOWED   silently: an install of a moved tag requires somebody to
//	                 say so explicitly
//	NEVER RETROACTIVE an existing install keeps the commit it was installed
//	                 from, and its provenance is not rewritten
//
// # Why the existing install is not "corrected"
//
// Because it is not wrong. The bytes on this host came from commit A, AO
// verified them against A's digests, and that record is true forever. Updating
// it to say B would be AO changing history because somebody else changed a
// pointer -- and it would erase the single most useful piece of evidence in an
// incident: what was actually installed, and when.

// TagObservation is one remembered (tag -> commit) mapping.
type TagObservation struct {
	RegistryID string `json:"registryId"`
	Owner      string `json:"owner"`
	Repository string `json:"repository"`
	Tag        string `json:"tag"`
	// Commit is the SHA the tag pointed at when AO last looked and accepted it.
	Commit string `json:"commit"`
	// FirstObservedAt and LastObservedAt bracket how long AO has seen this
	// mapping. The first is what an incident review reads.
	FirstObservedAt time.Time `json:"firstObservedAt"`
	LastObservedAt  time.Time `json:"lastObservedAt"`
	// MovedFromCommit and MovedAt record that this tag USED to point somewhere
	// else. They are kept on the row rather than replaced, because "this tag
	// moved once in March" is exactly the fact somebody needs six months later
	// and exactly the one an overwrite destroys.
	MovedFromCommit string     `json:"movedFromCommit,omitempty"`
	MovedAt         *time.Time `json:"movedAt,omitzero"`
}

// Key is the identity of one observation.
func (o TagObservation) Key() string {
	return strings.ToLower(o.Owner + "/" + o.Repository + "#" + o.Tag)
}

// Moved reports whether AO has ever seen this tag point somewhere else.
func (o TagObservation) Moved() bool { return o.MovedFromCommit != "" }

// TagObservationOf builds the observation a resolved release implies.
func TagObservationOf(rel Release, at time.Time) (TagObservation, bool) {
	src := rel.Source
	if !src.Declared() || src.Tag == "" || src.Commit == "" {
		// A release with no tag is pinned to a commit and nothing else, which
		// is the immutable case with nothing to remember: there is no name for
		// anybody to move.
		return TagObservation{}, false
	}
	return TagObservation{
		RegistryID:      rel.RegistryID,
		Owner:           src.Owner,
		Repository:      src.Repository,
		Tag:             src.Tag,
		Commit:          src.Commit,
		FirstObservedAt: at,
		LastObservedAt:  at,
	}, true
}

// TagVerdict is what comparing a fresh resolution against the ledger concluded.
type TagVerdict string

const (
	// TagFirstSeen means AO had no record of this tag. It is not suspicious:
	// every tag is new once.
	TagFirstSeen TagVerdict = "first-seen"
	// TagStable means the tag points where it pointed last time.
	TagStable TagVerdict = "stable"
	// TagMoved means the tag now points at a different commit. It blocks a
	// silent install and it never rewrites what is already installed.
	TagMoved TagVerdict = "moved"
)

// CompareTag decides what a fresh resolution means against what AO recorded.
//
// It returns the verdict and the row to persist. The row for a MOVED tag
// carries both SHAs, so the ledger holds the evidence rather than only the new
// value -- an overwrite would leave "this tag moved" unprovable ten minutes
// later.
func CompareTag(
	fresh TagObservation, previous TagObservation, known bool, at time.Time,
) (TagVerdict, TagObservation) {
	if !known {
		fresh.FirstObservedAt, fresh.LastObservedAt = at, at
		return TagFirstSeen, fresh
	}
	updated := previous
	updated.LastObservedAt = at
	if previous.Commit == fresh.Commit {
		return TagStable, updated
	}
	moved := at
	updated.MovedFromCommit = previous.Commit
	updated.Commit = fresh.Commit
	updated.MovedAt = &moved
	return TagMoved, updated
}

// DescribeTagMove is the sentence a refusal and an audit line both use.
//
// Both SHAs in full, because deciding two commits are different needs every
// character and this is precisely the moment somebody is going to compare them
// against a repository page.
func DescribeTagMove(o TagObservation) string {
	if !o.Moved() {
		return ""
	}
	when := ""
	if o.MovedAt != nil {
		when = " on " + o.MovedAt.UTC().Format(time.RFC3339)
	}
	return fmt.Sprintf("tag %s in %s/%s moved%s: AO recorded it at commit %s and it now points at "+
		"%s. A tag is a name somebody can re-point; the commit is the release. Nothing already "+
		"installed was changed, and its provenance still records the commit it came from.",
		o.Tag, o.Owner, o.Repository, when, o.MovedFromCommit, o.Commit)
}

// MovedTagRefusal is what an install is told when it would silently follow a
// moved tag.
const MovedTagRefusal = "This release's tag now points at a different commit than the one AO " +
	"recorded. AO will not install it as if nothing had happened: re-read what changed, and if you " +
	"mean to take the new commit, say so explicitly."
