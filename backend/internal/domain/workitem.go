package domain

import (
	"errors"
	"fmt"
	"strings"
	"time"
)

// workitem.go — P4-E's vocabulary: an external work-management item, and the
// durable link between it and AO's own work.
//
// WHY THIS IS NOT ports.Tracker. AO already has an issue-tracker abstraction
// (ports.Tracker, domain.Issue, TrackerID) and P4-E deliberately does not
// widen it. Three differences make them different ports rather than one:
//
//   - Tracker is READ-ONLY by contract: Get, List, Preflight. Its own doc
//     comment says richer per-provider behaviour belongs behind a separate
//     port, and the GitHub adapter states outright that it does not write. The
//     whole of P4-E is a write surface — link, create, transition, comment.
//   - Tracker is REPOSITORY-shaped. A TrackerID is "owner/repo#123" and a
//     TrackerRepo is "owner/repo". A Plane work item lives in a workspace and
//     a project that have no repository at all, and forcing it into that shape
//     would mean inventing a repo for something that has none.
//   - Tracker exists to HYDRATE A PROMPT and to enumerate intake candidates.
//     Work-item integration exists to keep an external plan and AO's execution
//     in step. A shared interface would have to be the union of both, which
//     means every existing adapter grows methods it does not implement.
//
// So Plane is a WORK-MANAGEMENT provider: a third category, named as such.
// What is deliberately REUSED rather than reinvented is everything below the
// port — NormalizedIssueState (the same five words), the httpkit HTTP
// plumbing both tracker adapters already share, the authorization triple, the
// notification manager, and the project-scoped tenancy model. Nothing here is
// a second copy of something AO has.
//
// AO REMAINS CANONICAL. Nothing in this file lets an external system decide
// AO's execution state. The mapping is one-way for truth (AO state → external
// state) and advisory-only in the other direction (external state is recorded,
// never applied to a run).

// WorkItemProvider identifies a work-management provider implementation.
type WorkItemProvider string

// The work-management providers this build implements.
const (
	// WorkItemProviderPlane is Plane (plane.so, and self-hosted Plane).
	WorkItemProviderPlane WorkItemProvider = "plane"
)

// ValidWorkItemProvider reports whether p is a provider this build implements.
func ValidWorkItemProvider(p WorkItemProvider) bool {
	return p == WorkItemProviderPlane
}

// ErrWorkItemInvalid is the sentinel every work-item validation failure wraps.
var ErrWorkItemInvalid = errors.New("domain: invalid work item")

// WorkItemRef names one external work item, in the provider's own terms.
//
// Both identifiers are carried because they answer different questions and
// neither can be derived from the other without a network call. ID is the
// provider's stable primary key and is what every API call addresses; Key is
// the human-readable reference a person types and reads ("PROJ-123"). A link
// that stored only the human key would break when a project is renamed; one
// that stored only the UUID could not be rendered without a fetch.
type WorkItemRef struct {
	Provider WorkItemProvider `json:"provider"`
	// Workspace is the provider's workspace identifier — for Plane, the
	// workspace slug that every API path is scoped by.
	Workspace string `json:"workspace"`
	// Project is the provider's project identifier (a UUID for Plane).
	Project string `json:"project"`
	// ID is the work item's stable provider-side identifier (a UUID for
	// Plane).
	ID string `json:"id"`
	// Key is the human-readable reference, e.g. "PROJ-123". It may be empty
	// when the provider did not return enough to build one; nothing depends
	// on it beyond display.
	Key string `json:"key,omitempty"`
}

// Validate rejects a reference that cannot address anything.
func (r WorkItemRef) Validate() error {
	if !ValidWorkItemProvider(r.Provider) {
		return fmt.Errorf("%w: unknown provider %q", ErrWorkItemInvalid, r.Provider)
	}
	if strings.TrimSpace(r.Workspace) == "" {
		return fmt.Errorf("%w: workspace is required", ErrWorkItemInvalid)
	}
	if strings.TrimSpace(r.Project) == "" {
		return fmt.Errorf("%w: project is required", ErrWorkItemInvalid)
	}
	if strings.TrimSpace(r.ID) == "" {
		return fmt.Errorf("%w: item id is required", ErrWorkItemInvalid)
	}
	return nil
}

// WorkItemStateGroup is the provider-neutral bucket an external state belongs
// to.
//
// It is a SECOND vocabulary beside NormalizedIssueState rather than a
// replacement, because the two answer different questions. NormalizedIssueState
// is what AO's tracker lane already speaks and is what a reader is shown.
// The group is what a WRITE has to name: Plane projects define their own
// states ("Ready for QA", "Blocked"), and the only stable thing across two
// installations is the group each state belongs to. Transitioning by group and
// resolving a concrete state within it is what makes status sync work against
// a workspace whose state names nobody told AO about.
//
// The values are Plane's own state groups, verbatim, because they are the
// contract AO writes against. A second provider with different groups would
// map onto these at its adapter, exactly as the tracker adapters map onto
// NormalizedIssueState.
type WorkItemStateGroup string

// The work-item state groups.
const (
	WorkItemStateBacklog   WorkItemStateGroup = "backlog"
	WorkItemStateUnstarted WorkItemStateGroup = "unstarted"
	WorkItemStateStarted   WorkItemStateGroup = "started"
	WorkItemStateCompleted WorkItemStateGroup = "completed"
	WorkItemStateCancelled WorkItemStateGroup = "cancelled"
	WorkItemStateTriage    WorkItemStateGroup = "triage"
)

// ValidWorkItemStateGroup reports whether g is a group this build recognises.
// An unrecognised group read back from a provider is preserved rather than
// coerced — see NormalizedFrom, which reports it as unknown rather than
// guessing.
func ValidWorkItemStateGroup(g WorkItemStateGroup) bool {
	switch g {
	case WorkItemStateBacklog, WorkItemStateUnstarted, WorkItemStateStarted,
		WorkItemStateCompleted, WorkItemStateCancelled, WorkItemStateTriage:
		return true
	default:
		return false
	}
}

// NormalizedFrom maps an external state group onto AO's existing cross-provider
// issue vocabulary, so a work item renders beside a GitHub or GitLab issue
// without a second set of words.
//
// It is explicit rather than string equality, which is what section 7 asks
// for: an unrecognised group returns false and is rendered as its own raw
// name, never silently bucketed into whichever normalized value happens to be
// closest.
func (g WorkItemStateGroup) NormalizedFrom() (NormalizedIssueState, bool) {
	switch g {
	case WorkItemStateBacklog, WorkItemStateUnstarted, WorkItemStateTriage:
		return IssueOpen, true
	case WorkItemStateStarted:
		return IssueInProgress, true
	case WorkItemStateCompleted:
		return IssueDone, true
	case WorkItemStateCancelled:
		return IssueCancelled, true
	default:
		return "", false
	}
}

// WorkItemState is one state a provider's project defines.
type WorkItemState struct {
	ID    string             `json:"id"`
	Name  string             `json:"name"`
	Group WorkItemStateGroup `json:"group"`
	// Default marks the state a new item lands in when none is named.
	Default bool `json:"default,omitempty"`
	// Sequence is the provider's own ordering within the project, used to pick
	// deterministically between two states of the same group.
	Sequence float64 `json:"sequence,omitempty"`
}

// WorkItemProject is one project inside a provider's workspace.
type WorkItemProject struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	// Identifier is the short prefix the provider builds human keys from
	// ("PROJ" in "PROJ-123").
	Identifier string `json:"identifier,omitempty"`
	// Description is the project's own one-liner, shown when a person is
	// choosing which project to map to.
	Description string `json:"description,omitempty"`
}

// WorkItem is the provider-neutral projection of one external work item.
//
// It carries what AO renders and what AO reconciles against, and nothing else.
// Provider-specific richness (cycles, modules, estimates, custom properties)
// stays inside the adapter: AO has no behaviour that depends on it, and
// projecting fields nobody reads is how a boundary becomes a second model of
// somebody else's product.
type WorkItem struct {
	Ref   WorkItemRef `json:"ref"`
	Title string      `json:"title"`
	// Description is the item's body as plain text, bounded by the adapter.
	// Never HTML: what AO shows is text, and passing markup through a boundary
	// that does not render it is how an injection surface appears.
	Description string `json:"description,omitempty"`
	// StateID and StateName are the provider's own state; StateGroup is the
	// portable bucket. All three are kept because a person reads the name, AO
	// writes by group, and a transition addresses the id.
	StateID    string             `json:"stateId,omitempty"`
	StateName  string             `json:"stateName,omitempty"`
	StateGroup WorkItemStateGroup `json:"stateGroup,omitempty"`
	// State is StateGroup mapped onto AO's cross-provider vocabulary, empty
	// when the group is one this build does not recognise.
	State     NormalizedIssueState `json:"state,omitempty"`
	Priority  string               `json:"priority,omitempty"`
	URL       string               `json:"url,omitempty"`
	Labels    []string             `json:"labels,omitempty"`
	Assignees []string             `json:"assignees,omitempty"`
	// ExternalSource and ExternalID are the provider's own foreign-key fields.
	// Plane supports them natively and AO writes its own ids into them, which
	// is what makes "has AO already created an item for this run" answerable
	// with one filtered read instead of a title search.
	ExternalSource string    `json:"externalSource,omitempty"`
	ExternalID     string    `json:"externalId,omitempty"`
	UpdatedAt      time.Time `json:"updatedAt,omitempty"`
}

// WorkItemLinkScope is what AO thing a link attaches to.
//
// Three scopes rather than one because they are linked for different reasons
// and have different lifetimes: a project mapping is standing configuration, a
// run link says "this execution delivers that planned work", and a task link
// is the finest granularity a plan can be tracked at.
type WorkItemLinkScope string

// The link scopes.
const (
	// WorkItemScopeProject links an AO project to an external project. It is
	// the mapping every other link is resolved against.
	WorkItemScopeProject WorkItemLinkScope = "project"
	// WorkItemScopeRun links one workflow run to one external item.
	WorkItemScopeRun WorkItemLinkScope = "run"
	// WorkItemScopeTask links one planned task to one external item.
	WorkItemScopeTask WorkItemLinkScope = "task"
)

// ValidWorkItemLinkScope reports whether s is a scope this build persists.
func ValidWorkItemLinkScope(s WorkItemLinkScope) bool {
	switch s {
	case WorkItemScopeProject, WorkItemScopeRun, WorkItemScopeTask:
		return true
	default:
		return false
	}
}

// WorkItemLinkOrigin says how a link came to exist.
//
// It is recorded because it decides what AO may do with the item. A link AO
// made by creating the item is one AO may keep updating; a link a person made
// to an item that already existed is one AO comments on but never re-titles.
type WorkItemLinkOrigin string

// The link origins.
const (
	// WorkItemLinkManual is a link a person made to an item that already
	// existed.
	WorkItemLinkManual WorkItemLinkOrigin = "manual"
	// WorkItemLinkCreated is a link AO made by creating the item itself.
	WorkItemLinkCreated WorkItemLinkOrigin = "created"
)

// ValidWorkItemLinkOrigin reports whether o is an origin this build persists.
func ValidWorkItemLinkOrigin(o WorkItemLinkOrigin) bool {
	return o == WorkItemLinkManual || o == WorkItemLinkCreated
}

// WorkItemLink is the durable association between AO work and an external item.
//
// It stores IDENTIFIERS ONLY. Titles, states and descriptions are read from the
// provider on demand and cached for display, never treated as truth: a link
// that carried a copy of the title would be a second, stale answer to "what is
// this item called". The one exception is LastSeen*, which is explicitly a
// cache with a timestamp saying how old it is.
//
// Tenancy is NOT a column here, matching the P4-C rule: a link's tenancy is
// its project's tenancy, and a denormalized copy that can drift from the
// authority it copies is a second thing to keep true rather than a safety
// property.
type WorkItemLink struct {
	ID        string            `json:"id"`
	ProjectID ProjectID         `json:"projectId"`
	Scope     WorkItemLinkScope `json:"scope"`
	// ScopeID is the workflow run id or planned task id the link attaches to,
	// and is empty exactly when Scope is WorkItemScopeProject.
	ScopeID string             `json:"scopeId,omitempty"`
	Ref     WorkItemRef        `json:"ref"`
	Origin  WorkItemLinkOrigin `json:"origin"`
	// SyncEnabled is whether AO may push its execution state to this item. A
	// link is useful without it — "these two things are about each other" is
	// worth recording on its own — so it is a separate switch rather than an
	// implied consequence of linking.
	SyncEnabled bool `json:"syncEnabled"`
	// LastSeenTitle, LastSeenState and LastSeenAt are a display cache. They
	// are what the UI renders when the provider is unreachable, labelled with
	// their age so nobody mistakes them for current.
	LastSeenTitle string             `json:"lastSeenTitle,omitempty"`
	LastSeenState WorkItemStateGroup `json:"lastSeenState,omitempty"`
	LastSeenAt    time.Time          `json:"lastSeenAt,omitempty"`
	// CreatedBy is the account that made the link, for the audit trail.
	CreatedBy string    `json:"createdBy,omitempty"`
	CreatedAt time.Time `json:"createdAt"`
	UpdatedAt time.Time `json:"updatedAt"`
}

// Validate rejects a link that cannot be reconciled later.
func (l WorkItemLink) Validate() error {
	if strings.TrimSpace(string(l.ProjectID)) == "" {
		return fmt.Errorf("%w: project is required", ErrWorkItemInvalid)
	}
	if !ValidWorkItemLinkScope(l.Scope) {
		return fmt.Errorf("%w: unknown link scope %q", ErrWorkItemInvalid, l.Scope)
	}
	if l.Scope == WorkItemScopeProject && strings.TrimSpace(l.ScopeID) != "" {
		return fmt.Errorf("%w: a project-scoped link names no scope id", ErrWorkItemInvalid)
	}
	if l.Scope != WorkItemScopeProject && strings.TrimSpace(l.ScopeID) == "" {
		return fmt.Errorf("%w: a %s link must name what it attaches to", ErrWorkItemInvalid, l.Scope)
	}
	if !ValidWorkItemLinkOrigin(l.Origin) {
		return fmt.Errorf("%w: unknown link origin %q", ErrWorkItemInvalid, l.Origin)
	}
	return l.Ref.Validate()
}

// ExternalIDFor is the value AO writes into the provider's own external-id
// field for one link.
//
// It is derived from the link's scope and target rather than chosen, so two
// AO instances reconciling the same run derive the same key and the provider's
// own upsert-by-external-id does the deduplication for us. That is why AO does
// not need its own "did I already create this" table.
func ExternalIDFor(scope WorkItemLinkScope, scopeID string) string {
	if scope == WorkItemScopeProject {
		return ""
	}
	return string(scope) + ":" + strings.TrimSpace(scopeID)
}

// WorkItemExternalSource is the value AO writes into the provider's
// external-source field. It identifies AO as the writer, so a person looking at
// an item in Plane can tell which system created it, and so AO's own filtered
// reads never match items some other integration wrote.
const WorkItemExternalSource = "agent-orchestrator"
