package ports

import (
	"errors"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

// ErrSessionNotFound reports an observation for an unknown session id.
var ErrSessionNotFound = errors.New("session not found")

// ErrProviderProfileRequired reports that a workflow's owner has no
// enabled ProviderProfile for the harness a launch needs (Checkpoint
// 8P-B.1). Like ErrChatAuthRequired, this is a configuration/security
// condition, never auto-retried and never treated as capacity/transient
// failure -- see workflow/failure_classifier.go.
var ErrProviderProfileRequired = errors.New("provider profile required: connect this provider in Settings → Agents & Models")

// SpawnConfig is the request to start a new session: which project/issue, which
// agent harness, and the branch/prompt the agent launches with.
type SpawnConfig struct {
	ProjectID domain.ProjectID
	IssueID   domain.IssueID
	// TrackerProvider is the issue-tracker provider hint from the CLI's
	// --tracker-provider flag (defaults to "github"). It is used as a fallback
	// when the project's SCM origin cannot be classified by the configured
	// SCM provider. When the SCM origin resolves successfully, the resolved
	// provider takes precedence over this hint.
	TrackerProvider domain.TrackerProvider
	// IssueContext is optional pre-fetched tracker context for the task prompt.
	// Standing rules stay in SystemPrompt; issue facts belong to the user task.
	IssueContext string
	Kind         domain.SessionKind
	Harness      domain.AgentHarness
	Branch       string
	// BaseRef overrides the project's default branch as the base a new
	// session's worktree is created from. Empty keeps the existing
	// project-default-branch behavior. Used by the master workflow
	// (Checkpoint 8M.1) to base task N+1's worktree on the integration state
	// containing every previously completed dependency task, instead of the
	// project's default branch — the mechanism that lets task N+1 physically
	// see task N's verified code. Accepts anything git can resolve as a ref
	// (a branch name or a full ref path like refs/ao/workflows/<id>/integration).
	BaseRef string
	Prompt  string
	// AgentConfig overrides the resolved project/role agent config for this
	// single spawn. Empty fields keep the project defaults.
	AgentConfig AgentConfig

	// RequestedMode is the caller's explicit session mode, or empty to let the
	// daemon resolve its default. It is validated and persisted before any
	// controller launches. A later explicit interface transition may replace that
	// controller while preserving the AO session. An unsupported explicit request
	// fails the spawn rather than falling back to the other mode.
	RequestedMode domain.SessionMode

	// DisplayName is the user-facing sidebar label. Empty falls back to the
	// session id in the read model (e.g. orchestrator sessions).
	DisplayName string
	// Attachments are files pasted or dropped into the task brief. They are
	// written into the session worktree and referenced by path in the prompt so
	// the agent can read them (CLI agents receive the prompt as text and cannot
	// consume inline binary data). Any file type is accepted except for
	// explicitly blocked types (e.g., SVG for security reasons).
	Attachments []SpawnAttachment

	// RuntimeEnv overrides subprocess env vars for the launched provider
	// process (Checkpoint 8P-B.1) -- e.g. HOME/CLAUDE_CONFIG_DIR/CODEX_HOME
	// pinned to the workflow owner's isolated runtime-home. Nil preserves
	// pre-8P-B.1 behavior exactly (daemon's own real environment). Applied
	// last, after every other env source, so it always wins.
	RuntimeEnv map[string]string

	// Owner is the resolved workflow-run owner, when known (Checkpoint
	// 8P-B.1). Empty means unresolved/unowned; best-effort stamped onto the
	// created session for later ownership checks, never trusted as-is from
	// a client -- callers must resolve it from durable ownership data.
	Owner domain.UserID

	// WorkflowRunID names the autonomous run this spawn belongs to, when it
	// belongs to one (Checkpoint 8P-E.14). It exists for exactly one decision:
	// in direct-branch mode the session manager acquires the repository+branch
	// execution lock for an ordinary task, but a workflow's worker must not,
	// because its run already owns that lock and the session would otherwise
	// contend with the run that spawned it.
	//
	// Daemon-internal: it is set by the workflow coordinator from durable run
	// state and is deliberately absent from the HTTP spawn DTO, so no client
	// can present itself as a workflow to bypass task branch locking.
	WorkflowRunID string
}

// SpawnAttachment is a single file attached to a spawn request. Data holds the
// already-decoded bytes; the manager derives the on-disk filename from the
// MIME type.
type SpawnAttachment struct {
	// Ext is the file extension (including the leading dot, e.g. ".png")
	// inferred from the attachment's declared MIME type, or ".bin" for unknown types.
	Ext  string
	Data []byte
}
