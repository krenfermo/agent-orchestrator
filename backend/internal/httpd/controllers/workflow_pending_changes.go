package controllers

import (
	"errors"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/apispec"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/envelope"
	workflowsvc "github.com/aoagents/agent-orchestrator/backend/internal/service/workflow"
	workflowcore "github.com/aoagents/agent-orchestrator/backend/internal/workflow"
)

// workflow_pending_changes.go — P3-A §17: the two routes behind
// "Hacer commit y continuar".
//
// Two routes rather than one, and the split IS the safety model: seeing what is
// pending must not require the ability to commit it. The GET runs `git status`
// and nothing else; the POST commits exactly what the GET showed, under a
// message the caller supplied, and resumes only after proving the tree is
// clean.
//
// There is no stash route and there will not be one. Moving somebody's
// uncommitted work somewhere they did not choose, to unblock a run, is the
// behaviour this flow exists to replace.

// WorkflowPendingChangeView is one uncommitted path.
type WorkflowPendingChangeView struct {
	Path string `json:"path"`
	// Status is git's own porcelain code, unmodified — the technical detail
	// beside the path, never the line a person reads.
	Status string `json:"status,omitempty"`
}

// WorkflowPendingChangesResponse is the body of
// GET /api/v1/workflows/{workflowId}/pending-changes.
type WorkflowPendingChangesResponse struct {
	// Available reports that AO could actually probe the repository. False
	// leaves dirty/changes at their zero values and means UNKNOWN, never
	// "clean": an unreadable repository is not evidence that there is nothing
	// in it.
	Available bool `json:"available"`
	// Unavailable names why AO could not answer.
	Unavailable string `json:"unavailable,omitempty"`
	RepoPath    string `json:"repoPath,omitempty"`
	Branch      string `json:"branch,omitempty"`
	HeadSHA     string `json:"headSha,omitempty"`
	Dirty       bool   `json:"dirty"`
	// Changes is every pending path, so the person committing sees exactly what
	// they are committing before they do it.
	Changes []WorkflowPendingChangeView `json:"changes,omitempty"`
	// ProposedMessage is a starting point derived from the run's objective. It
	// is expected to be edited; nothing is committed under it unless a person
	// sends it back.
	ProposedMessage string `json:"proposedMessage,omitempty"`
	// Placement, WorktreePath and Historical say WHERE this answer came from.
	//
	// Historical=true means the run's execution placement has been retired --
	// the run is over -- and this answer was reconstructed from the durable
	// record. The distinction is not cosmetic: a historical answer is readable
	// and describable, and the commit route will refuse it.
	Placement    string `json:"placement,omitempty" enum:"direct_branch,isolated_worktree"`
	WorktreePath string `json:"worktreePath,omitempty"`
	Historical   bool   `json:"historical,omitempty"`
}

// CommitPendingChangesRequest is the message the caller approved.
type CommitPendingChangesRequest struct {
	// Message is required. AO does not write a commit message on somebody's
	// behalf and then put their name on it.
	Message string `json:"message" required:"true"`
	// Authority is P3-C's optional proof from the GET /advice reading this
	// click was computed against. Omitted keeps the pre-P3-C behaviour; a
	// commit that arrives after AO started repairing the same run is refused
	// with the reason either way.
	Authority *WorkflowActionAuthorityRequest `json:"authority,omitempty"`
}

// CommitPendingChangesResponse reports what actually happened, in the order it
// happened: commit, re-probe, resume.
type CommitPendingChangesResponse struct {
	// Committed is false when the repository turned out to be clean already,
	// which is a success rather than an error.
	Committed bool   `json:"committed"`
	CommitSHA string `json:"commitSha,omitempty"`
	// Clean is the re-probe's answer. A commit that left the tree dirty does
	// NOT resume the run: it would stop on the same condition again.
	Clean bool `json:"clean"`
	// Resumed reports that the run was continued afterwards.
	Resumed bool `json:"resumed"`
	// Detail explains a clean=false or resumed=false outcome in AO's own words.
	Detail string `json:"detail,omitempty"`
}

func (c *WorkflowsController) pendingChangesSvc(w http.ResponseWriter, r *http.Request, method, route string) (workflowsvc.PendingChangesManager, bool) {
	if c.Svc == nil {
		apispec.NotImplemented(w, r, method, route)
		return nil, false
	}
	svc, ok := c.Svc.(workflowsvc.PendingChangesManager)
	if !ok {
		apispec.NotImplemented(w, r, method, route)
		return nil, false
	}
	return svc, true
}

func (c *WorkflowsController) getPendingChanges(w http.ResponseWriter, r *http.Request) {
	svc, ok := c.pendingChangesSvc(w, r, http.MethodGet, "/api/v1/workflows/{workflowId}/pending-changes")
	if !ok {
		return
	}
	out, err := svc.PendingChanges(r.Context(), chi.URLParam(r, "workflowId"))
	if err != nil {
		writeWorkflowError(w, r, err)
		return
	}
	resp := WorkflowPendingChangesResponse{
		Available: out.Available, Unavailable: out.Unavailable,
		RepoPath: out.RepoPath, Branch: out.Branch, HeadSHA: out.HeadSHA,
		Dirty: out.Dirty, ProposedMessage: out.ProposedMessage,
		Placement: string(out.Placement), WorktreePath: out.WorktreePath,
		Historical: out.Historical,
	}
	for _, ch := range out.Changes {
		resp.Changes = append(resp.Changes, WorkflowPendingChangeView{Path: ch.Path, Status: ch.Status})
	}
	envelope.WriteJSON(w, http.StatusOK, resp)
}

func (c *WorkflowsController) commitPendingChanges(w http.ResponseWriter, r *http.Request) {
	svc, ok := c.pendingChangesSvc(w, r, http.MethodPost, "/api/v1/workflows/{workflowId}/pending-changes/commit")
	if !ok {
		return
	}
	var body CommitPendingChangesRequest
	if err := decodeJSON(r, &body); err != nil {
		envelope.WriteAPIError(w, r, http.StatusBadRequest, "bad_request", "INVALID_JSON", "Invalid JSON body", nil)
		return
	}
	if strings.TrimSpace(body.Message) == "" {
		envelope.WriteAPIError(w, r, http.StatusBadRequest, "bad_request", "COMMIT_MESSAGE_REQUIRED",
			"a commit message is required; AO does not write one on somebody's behalf", nil)
		return
	}
	runID := chi.URLParam(r, "workflowId")
	// P3-C §15. commitAuthorityFor already fails this closed on the fact that
	// matters -- a run that no longer holds its branch cannot commit to it, and
	// a repair that took the branch is exactly that case. What the check below
	// adds is the SENTENCE: "AO is already repairing this run" is something a
	// person can act on, and "this run can no longer prove it is entitled to
	// commit to this branch" is a true statement that leaves them guessing why.
	//
	// It runs before the commit rather than instead of those proofs. This is the
	// highest-consequence action in the product -- it writes to somebody's real
	// repository -- so it gets both.
	if c.refuseStaleAction(w, r, runID, workflowcore.ActionCommitAndContinue, body.Authority.authority()) {
		return
	}
	out, err := svc.CommitPendingChanges(r.Context(), runID, body.Message)
	if err != nil {
		if errors.Is(err, workflowcore.ErrPendingChangesUnavailable) {
			envelope.WriteAPIError(w, r, http.StatusConflict, "conflict", "PENDING_CHANGES_UNAVAILABLE",
				"AO cannot say which repository and branch this run's work belongs to, so it will not commit anything", nil)
			return
		}
		// A refusal of AUTHORITY, not of visibility. It is its own code because
		// the two invite opposite reactions: an unavailable probe is worth
		// retrying, and "AO can see your branch and will not write to it"
		// never is. The message carries the specific clause that failed.
		if errors.Is(err, workflowcore.ErrPendingChangesNoAuthority) {
			envelope.WriteAPIError(w, r, http.StatusConflict, "conflict", "PENDING_CHANGES_NO_AUTHORITY",
				err.Error(), nil)
			return
		}
		writeWorkflowError(w, r, err)
		return
	}
	envelope.WriteJSON(w, http.StatusOK, CommitPendingChangesResponse{
		Committed: out.Committed, CommitSHA: out.CommitSHA,
		Clean: out.Clean, Resumed: out.Resumed, Detail: out.Detail,
	})
}
