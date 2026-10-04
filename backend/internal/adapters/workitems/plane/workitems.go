package plane

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

// workitems.go — the ports.WorkItems implementation.

// Preflight verifies the credential by doing the cheapest real thing: listing
// the workspace's projects.
//
// A token that merely EXISTS proves nothing, and Plane has no "who am I"
// endpoint on the v1 API. Listing projects proves the key is valid AND that it
// can see the workspace it is configured for, which is the pair of facts a
// person pressing "test connection" is actually asking about.
func (c *Client) Preflight(ctx context.Context) (ports.WorkItemsIdentity, error) {
	projects, err := c.ListProjects(ctx)
	if err != nil {
		return ports.WorkItemsIdentity{}, err
	}
	return ports.WorkItemsIdentity{
		Provider:  domain.WorkItemProviderPlane,
		Workspace: c.workspace,
		Projects:  len(projects),
	}, nil
}

type planeProject struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Identifier  string `json:"identifier"`
	Description string `json:"description"`
}

// ListProjects enumerates the workspace's projects.
func (c *Client) ListProjects(ctx context.Context) ([]domain.WorkItemProject, error) {
	var out []domain.WorkItemProject
	path := "/workspaces/" + url.PathEscape(c.workspace) + "/projects/"
	err := c.paginate(ctx, "list_projects", path, nil, func(raw json.RawMessage) error {
		var page []planeProject
		if err := json.Unmarshal(raw, &page); err != nil {
			return &ports.WorkItemsError{Op: "list_projects", Kind: ports.WorkItemsErrUnavailable,
				Message: "Plane returned projects AO could not decode", Err: err}
		}
		for _, p := range page {
			out = append(out, domain.WorkItemProject{
				ID: p.ID, Name: p.Name, Identifier: p.Identifier,
				Description: truncate(p.Description, 240),
			})
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

type planeState struct {
	ID       string  `json:"id"`
	Name     string  `json:"name"`
	Group    string  `json:"group"`
	Default  bool    `json:"default"`
	Sequence float64 `json:"sequence"`
}

// ListStates returns the states one project defines.
func (c *Client) ListStates(ctx context.Context, projectID string) ([]domain.WorkItemState, error) {
	if strings.TrimSpace(projectID) == "" {
		return nil, &ports.WorkItemsError{Op: "list_states", Kind: ports.WorkItemsErrInvalid,
			Message: "no Plane project id was given"}
	}
	var out []domain.WorkItemState
	path := "/workspaces/" + url.PathEscape(c.workspace) + "/projects/" + url.PathEscape(projectID) + "/states/"
	err := c.paginate(ctx, "list_states", path, nil, func(raw json.RawMessage) error {
		var page []planeState
		if err := json.Unmarshal(raw, &page); err != nil {
			return &ports.WorkItemsError{Op: "list_states", Kind: ports.WorkItemsErrUnavailable,
				Message: "Plane returned states AO could not decode", Err: err}
		}
		for _, s := range page {
			out = append(out, domain.WorkItemState{
				ID: s.ID, Name: s.Name,
				Group:   domain.WorkItemStateGroup(strings.ToLower(strings.TrimSpace(s.Group))),
				Default: s.Default, Sequence: s.Sequence,
			})
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// planeIssue is the subset of Plane's work-item serializer AO projects from.
//
// It is deliberately partial. The serializer returns the whole model minus two
// fields, and mapping all of it would make this adapter a second model of
// Plane's product; what is decoded here is what AO renders or reconciles
// against, and nothing else.
type planeIssue struct {
	ID              string   `json:"id"`
	Name            string   `json:"name"`
	Description     string   `json:"description_stripped"`
	DescriptionHTML string   `json:"description_html"`
	State           string   `json:"state"`
	Priority        string   `json:"priority"`
	SequenceID      int      `json:"sequence_id"`
	Project         string   `json:"project"`
	Workspace       string   `json:"workspace"`
	Labels          []string `json:"labels"`
	Assignees       []string `json:"assignees"`
	ExternalSource  string   `json:"external_source"`
	ExternalID      string   `json:"external_id"`
	UpdatedAt       string   `json:"updated_at"`
}

// Get reads one work item.
func (c *Client) Get(ctx context.Context, ref domain.WorkItemRef) (domain.WorkItem, error) {
	if err := ref.Validate(); err != nil {
		return domain.WorkItem{}, &ports.WorkItemsError{Op: "get", Kind: ports.WorkItemsErrInvalid,
			Message: err.Error(), Err: err}
	}
	var issue planeIssue
	path := "/workspaces/" + url.PathEscape(ref.Workspace) +
		"/projects/" + url.PathEscape(ref.Project) +
		"/work-items/" + url.PathEscape(ref.ID) + "/"
	if err := c.do(ctx, "get", http.MethodGet, path, nil, nil, &issue); err != nil {
		return domain.WorkItem{}, err
	}
	return c.project(ctx, issue, ref.Workspace)
}

// identifierPattern matches a human work-item reference, "PROJ-123".
//
// Plane project identifiers are short uppercase prefixes; the pattern is
// deliberately permissive about case and length and strict about shape, so a
// person pasting "proj-123" is understood and a person pasting a sentence is
// not mistaken for one.
var identifierPattern = regexp.MustCompile(`^([A-Za-z][A-Za-z0-9_]{0,19})-(\d{1,10})$`)

// Resolve finds an item from what a person actually has: a human key
// ("PROJ-123") or a Plane URL they copied from the address bar.
func (c *Client) Resolve(ctx context.Context, reference string) (domain.WorkItem, error) {
	reference = strings.TrimSpace(reference)
	if reference == "" {
		return domain.WorkItem{}, &ports.WorkItemsError{Op: "resolve", Kind: ports.WorkItemsErrInvalid,
			Message: "no work item reference was given"}
	}
	// A pasted browser URL. Plane's own app URLs carry the workspace, the
	// project UUID and the item UUID, which is everything a direct read needs.
	if ws, project, id, ok := parseIssueURL(reference); ok {
		if ws != "" && !strings.EqualFold(ws, c.workspace) {
			return domain.WorkItem{}, &ports.WorkItemsError{Op: "resolve", Kind: ports.WorkItemsErrInvalid,
				Message: fmt.Sprintf("that work item is in workspace %q, but this project is connected to %q", ws, c.workspace)}
		}
		return c.Get(ctx, domain.WorkItemRef{
			Provider: domain.WorkItemProviderPlane, Workspace: c.workspace,
			Project: project, ID: id,
		})
	}
	if !identifierPattern.MatchString(reference) {
		return domain.WorkItem{}, &ports.WorkItemsError{Op: "resolve", Kind: ports.WorkItemsErrInvalid,
			Message: `expected a work item reference like "PROJ-123" or a Plane work item URL`}
	}
	var issue planeIssue
	path := "/workspaces/" + url.PathEscape(c.workspace) + "/work-items/" + url.PathEscape(strings.ToUpper(reference)) + "/"
	if err := c.do(ctx, "resolve", http.MethodGet, path, nil, nil, &issue); err != nil {
		return domain.WorkItem{}, err
	}
	return c.project(ctx, issue, c.workspace)
}

// parseIssueURL extracts workspace/project/item from a Plane app URL of the
// shape /<workspace>/projects/<project-uuid>/issues/<issue-uuid>.
func parseIssueURL(raw string) (workspace, project, id string, ok bool) {
	if !strings.HasPrefix(raw, "http://") && !strings.HasPrefix(raw, "https://") {
		return "", "", "", false
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "", "", "", false
	}
	segs := strings.Split(strings.Trim(u.Path, "/"), "/")
	for i := 0; i+1 < len(segs); i++ {
		// Both the app's "issues" and the API's "work-items" spelling appear
		// in URLs people paste, depending on where they copied from.
		if segs[i] != "issues" && segs[i] != "work-items" {
			continue
		}
		id = segs[i+1]
		for j := 0; j+1 < i; j++ {
			if segs[j] == "projects" {
				project = segs[j+1]
				if j > 0 {
					workspace = segs[0]
				}
				break
			}
		}
		if project != "" && id != "" {
			return workspace, project, id, true
		}
	}
	return "", "", "", false
}

// FindByExternalID asks Plane whether AO has already created an item for one
// of its own ids.
//
// This is Plane's own filter (`?external_id=&external_source=`), not a title
// search — §5's "do not infer links from titles alone" is satisfied by there
// being no code path here that looks at a title at all.
func (c *Client) FindByExternalID(ctx context.Context, projectID, externalID string) (domain.WorkItem, bool, error) {
	if strings.TrimSpace(projectID) == "" || strings.TrimSpace(externalID) == "" {
		return domain.WorkItem{}, false, &ports.WorkItemsError{Op: "find_external",
			Kind: ports.WorkItemsErrInvalid, Message: "a project id and an external id are both required"}
	}
	query := url.Values{}
	query.Set("external_id", externalID)
	query.Set("external_source", domain.WorkItemExternalSource)

	var envelope pageEnvelope
	path := "/workspaces/" + url.PathEscape(c.workspace) + "/projects/" + url.PathEscape(projectID) + "/work-items/"
	if err := c.do(ctx, "find_external", http.MethodGet, path, query, nil, &envelope); err != nil {
		return domain.WorkItem{}, false, err
	}
	var page []planeIssue
	if len(envelope.Results) > 0 {
		if err := json.Unmarshal(envelope.Results, &page); err != nil {
			return domain.WorkItem{}, false, &ports.WorkItemsError{Op: "find_external",
				Kind: ports.WorkItemsErrUnavailable, Message: "Plane returned work items AO could not decode", Err: err}
		}
	}
	if len(page) == 0 {
		return domain.WorkItem{}, false, nil
	}
	item, err := c.project(ctx, page[0], c.workspace)
	if err != nil {
		return domain.WorkItem{}, false, err
	}
	return item, true, nil
}

// createBody is the request AO sends. Only the fields AO actually sets are
// present: a create that posted the whole serializer would be asserting
// defaults for things AO has no opinion about.
type createBody struct {
	Name            string   `json:"name"`
	DescriptionHTML string   `json:"description_html,omitempty"`
	State           string   `json:"state,omitempty"`
	Labels          []string `json:"labels,omitempty"`
	ExternalSource  string   `json:"external_source"`
	ExternalID      string   `json:"external_id"`
}

// Create makes a work item, and is safe to retry.
//
// Plane refuses a duplicate (external_source, external_id) with 409 and puts
// the existing item's id in the body. AO treats that as success and reads the
// existing item back — which is what makes "create an item for this run" an
// idempotent operation without AO keeping a record of having tried.
func (c *Client) Create(ctx context.Context, req ports.WorkItemCreateRequest) (domain.WorkItem, error) {
	if strings.TrimSpace(req.ProjectID) == "" {
		return domain.WorkItem{}, &ports.WorkItemsError{Op: "create", Kind: ports.WorkItemsErrInvalid,
			Message: "no Plane project id was given"}
	}
	if strings.TrimSpace(req.Title) == "" {
		return domain.WorkItem{}, &ports.WorkItemsError{Op: "create", Kind: ports.WorkItemsErrInvalid,
			Message: "a work item needs a title"}
	}
	if strings.TrimSpace(req.ExternalID) == "" {
		return domain.WorkItem{}, &ports.WorkItemsError{Op: "create", Kind: ports.WorkItemsErrInvalid,
			Message: "a created work item must carry an external id, or AO cannot find it again"}
	}

	body := createBody{
		Name:           truncate(req.Title, 250),
		ExternalSource: domain.WorkItemExternalSource,
		ExternalID:     req.ExternalID,
	}
	if req.Description != "" {
		body.DescriptionHTML = toHTML(req.Description)
	}
	if req.StateGroup != "" {
		stateID, err := c.stateIDForGroup(ctx, req.ProjectID, req.StateGroup)
		if err == nil {
			body.State = stateID
		}
		// A group with no matching state is not a reason to refuse to create
		// the item: the item in the project's default state is far more useful
		// than no item at all.
	}
	if len(req.Labels) > 0 {
		if ids, err := c.labelIDs(ctx, req.ProjectID, req.Labels); err == nil {
			body.Labels = ids
		}
	}

	path := "/workspaces/" + url.PathEscape(c.workspace) + "/projects/" + url.PathEscape(req.ProjectID) + "/work-items/"
	var issue planeIssue
	err := c.do(ctx, "create", http.MethodPost, path, nil, body, &issue)
	if err != nil {
		var wErr *ports.WorkItemsError
		if errors.As(err, &wErr) && wErr.Status == http.StatusConflict {
			// The item already exists for this external id. That is the
			// idempotent outcome, not a failure.
			existing, found, findErr := c.FindByExternalID(ctx, req.ProjectID, req.ExternalID)
			if findErr == nil && found {
				return existing, nil
			}
		}
		return domain.WorkItem{}, err
	}
	return c.project(ctx, issue, c.workspace)
}

// Transition moves an item into a state group.
func (c *Client) Transition(ctx context.Context, ref domain.WorkItemRef, group domain.WorkItemStateGroup) error {
	if err := ref.Validate(); err != nil {
		return &ports.WorkItemsError{Op: "transition", Kind: ports.WorkItemsErrInvalid,
			Message: err.Error(), Err: err}
	}
	if !domain.ValidWorkItemStateGroup(group) {
		return &ports.WorkItemsError{Op: "transition", Kind: ports.WorkItemsErrInvalid,
			Message: "unknown target state group " + string(group)}
	}

	// Already there? Do nothing. A sync that fires twice must not churn the
	// item's activity feed, and Plane records every state write as an activity
	// a person sees.
	current, err := c.Get(ctx, ref)
	if err != nil {
		return err
	}
	if current.StateGroup == group {
		return nil
	}

	stateID, err := c.stateIDForGroup(ctx, ref.Project, group)
	if err != nil {
		return err
	}
	path := "/workspaces/" + url.PathEscape(ref.Workspace) +
		"/projects/" + url.PathEscape(ref.Project) +
		"/work-items/" + url.PathEscape(ref.ID) + "/"
	return c.do(ctx, "transition", http.MethodPatch, path, nil,
		map[string]string{"state": stateID}, nil)
}

// stateIDForGroup picks the concrete state AO writes for a group.
//
// A project can have several states in one group ("In Progress", "In Review"
// are both `started`). The choice is deterministic — the project's default
// within the group if it has one, else the lowest sequence, else the first by
// name — so two AO instances driving the same project agree, and so a person
// can predict where AO will put things.
func (c *Client) stateIDForGroup(ctx context.Context, projectID string, group domain.WorkItemStateGroup) (string, error) {
	states, err := c.ListStates(ctx, projectID)
	if err != nil {
		return "", err
	}
	var candidates []domain.WorkItemState
	for _, s := range states {
		if s.Group == group {
			candidates = append(candidates, s)
		}
	}
	if len(candidates) == 0 {
		return "", &ports.WorkItemsError{Op: "transition", Kind: ports.WorkItemsErrInvalid,
			Message: fmt.Sprintf("this Plane project defines no %q state", group)}
	}
	sort.Slice(candidates, func(i, j int) bool {
		if candidates[i].Default != candidates[j].Default {
			return candidates[i].Default
		}
		if candidates[i].Sequence != candidates[j].Sequence {
			return candidates[i].Sequence < candidates[j].Sequence
		}
		return candidates[i].Name < candidates[j].Name
	})
	return candidates[0].ID, nil
}

type planeLabel struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// labelIDs resolves label names to ids, silently dropping names the project
// does not have.
//
// AO never CREATES a label: minting taxonomy in somebody else's planning tool
// is not a side effect that linking an item should have. A label AO was asked
// for and the project does not define is simply not applied.
func (c *Client) labelIDs(ctx context.Context, projectID string, names []string) ([]string, error) {
	want := map[string]bool{}
	for _, n := range names {
		if n = strings.ToLower(strings.TrimSpace(n)); n != "" {
			want[n] = true
		}
	}
	if len(want) == 0 {
		return nil, nil
	}
	var out []string
	path := "/workspaces/" + url.PathEscape(c.workspace) + "/projects/" + url.PathEscape(projectID) + "/labels/"
	err := c.paginate(ctx, "list_labels", path, nil, func(raw json.RawMessage) error {
		var page []planeLabel
		if err := json.Unmarshal(raw, &page); err != nil {
			return &ports.WorkItemsError{Op: "list_labels", Kind: ports.WorkItemsErrUnavailable,
				Message: "Plane returned labels AO could not decode", Err: err}
		}
		for _, l := range page {
			if want[strings.ToLower(strings.TrimSpace(l.Name))] {
				out = append(out, l.ID)
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// commentBody is a progress note. The dedupe key travels in Plane's own
// comment external-id fields.
type commentBody struct {
	CommentHTML    string `json:"comment_html"`
	ExternalSource string `json:"external_source,omitempty"`
	ExternalID     string `json:"external_id,omitempty"`
}

// Comment posts a progress note.
//
// A 409 from a duplicate external id is success: the note AO wanted posted is
// already there, which is exactly what the dedupe key is for.
func (c *Client) Comment(ctx context.Context, ref domain.WorkItemRef, body, dedupeKey string) error {
	if err := ref.Validate(); err != nil {
		return &ports.WorkItemsError{Op: "comment", Kind: ports.WorkItemsErrInvalid,
			Message: err.Error(), Err: err}
	}
	if strings.TrimSpace(body) == "" {
		return &ports.WorkItemsError{Op: "comment", Kind: ports.WorkItemsErrInvalid,
			Message: "an empty comment is not worth posting"}
	}
	payload := commentBody{CommentHTML: toHTML(body)}
	if dedupeKey != "" {
		payload.ExternalSource = domain.WorkItemExternalSource
		payload.ExternalID = dedupeKey
	}
	path := "/workspaces/" + url.PathEscape(ref.Workspace) +
		"/projects/" + url.PathEscape(ref.Project) +
		"/work-items/" + url.PathEscape(ref.ID) + "/comments/"
	err := c.do(ctx, "comment", http.MethodPost, path, nil, payload, nil)
	if err != nil {
		var wErr *ports.WorkItemsError
		if errors.As(err, &wErr) && wErr.Status == http.StatusConflict {
			return nil
		}
	}
	return err
}

// project maps Plane's shape onto AO's, resolving the state UUID to a group.
//
// The state lookup is why this takes a context: a work item carries its state
// as a bare UUID, and the group — the only portable thing about it — lives on
// the state. One extra read per projection is the honest cost of not inventing
// a state vocabulary.
func (c *Client) project(ctx context.Context, issue planeIssue, workspace string) (domain.WorkItem, error) {
	ref := domain.WorkItemRef{
		Provider:  domain.WorkItemProviderPlane,
		Workspace: workspace,
		Project:   issue.Project,
		ID:        issue.ID,
	}
	item := domain.WorkItem{
		Ref:            ref,
		Title:          issue.Name,
		Description:    truncate(plainText(issue.Description, issue.DescriptionHTML), maxDescription),
		StateID:        issue.State,
		Priority:       issue.Priority,
		Labels:         issue.Labels,
		Assignees:      issue.Assignees,
		ExternalSource: issue.ExternalSource,
		ExternalID:     issue.ExternalID,
	}
	if ts, err := time.Parse(time.RFC3339, issue.UpdatedAt); err == nil {
		item.UpdatedAt = ts.UTC()
	}
	if issue.State != "" && issue.Project != "" {
		if states, err := c.ListStates(ctx, issue.Project); err == nil {
			for _, s := range states {
				if s.ID == issue.State {
					item.StateName = s.Name
					item.StateGroup = s.Group
					break
				}
			}
		}
		// A state AO could not resolve leaves StateGroup empty, which reads
		// downstream as "unknown" rather than as any particular state. That is
		// the fail-closed direction: an unknown planning state must never
		// render as "done".
	}
	if state, ok := item.StateGroup.NormalizedFrom(); ok {
		item.State = state
	}
	if issue.SequenceID > 0 {
		if ident := c.projectIdentifier(ctx, issue.Project); ident != "" {
			item.Ref.Key = ident + "-" + strconv.Itoa(issue.SequenceID)
		}
	}
	item.URL = c.itemURL(item.Ref)
	return item, nil
}

// projectIdentifier looks up a project's short prefix for building human keys.
// A failure yields "", which costs the item its display key and nothing else.
func (c *Client) projectIdentifier(ctx context.Context, projectID string) string {
	projects, err := c.ListProjects(ctx)
	if err != nil {
		return ""
	}
	for _, p := range projects {
		if p.ID == projectID {
			return p.Identifier
		}
	}
	return ""
}

// itemURL builds the browser URL for an item.
//
// It is derived from the API origin, which is right for a self-hosted Plane
// (one origin serves both) and wrong for Plane Cloud, whose app lives at
// app.plane.so while the API lives at api.plane.so — so that one case is
// translated explicitly rather than producing a link that 404s.
func (c *Client) itemURL(ref domain.WorkItemRef) string {
	if ref.Project == "" || ref.ID == "" {
		return ""
	}
	// A no-op when the substring is absent, which is the self-hosted case.
	origin := strings.Replace(c.base, "//api.plane.so", "//app.plane.so", 1)
	return origin + "/" + ref.Workspace + "/projects/" + ref.Project + "/issues/" + ref.ID
}

// plainText prefers the stripped text Plane already computed, and falls back to
// crudely stripping the HTML body when it is absent.
func plainText(stripped, html string) string {
	if s := strings.TrimSpace(stripped); s != "" {
		return s
	}
	return strings.TrimSpace(stripTags(html))
}

var tagPattern = regexp.MustCompile(`<[^>]*>`)

func stripTags(s string) string {
	s = strings.ReplaceAll(s, "</p>", "\n")
	s = strings.ReplaceAll(s, "<br>", "\n")
	s = strings.ReplaceAll(s, "<br/>", "\n")
	return strings.TrimSpace(tagPattern.ReplaceAllString(s, ""))
}

// toHTML wraps plain text in the minimal markup Plane's editor expects,
// escaping first.
//
// Escaping is the point: AO writes commit subjects, task titles and stop
// reasons into these bodies, and any of them can contain a character that
// would otherwise close a tag. Nothing AO sends is markup.
func toHTML(text string) string {
	var b strings.Builder
	for _, line := range strings.Split(strings.TrimSpace(text), "\n") {
		b.WriteString("<p>")
		b.WriteString(escapeHTML(line))
		b.WriteString("</p>")
	}
	if b.Len() == 0 {
		return "<p></p>"
	}
	return b.String()
}

func escapeHTML(s string) string {
	r := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;", "'", "&#39;")
	return r.Replace(s)
}
