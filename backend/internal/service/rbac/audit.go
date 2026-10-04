package rbac

import (
	"context"
	"log/slog"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

// Authorization-change audit event names. Stable strings: an operator greps
// for these, so they are part of the contract even though no code branches on
// them.
const (
	EventUserCreated          = "ao.authz.user.created"
	EventUserEnabled          = "ao.authz.user.enabled"
	EventUserDisabled         = "ao.authz.user.disabled"
	EventUserRoleChanged      = "ao.authz.user.role_changed"
	EventTeamCreated          = "ao.authz.team.created"
	EventTeamUpdated          = "ao.authz.team.updated"
	EventTeamDeleted          = "ao.authz.team.deleted"
	EventTeamMemberAdded      = "ao.authz.team.member_added"
	EventTeamMemberRemoved    = "ao.authz.team.member_removed"
	EventProjectAccessGranted = "ao.authz.project.access_granted"
	EventProjectAccessRevoked = "ao.authz.project.access_revoked"

	// P4-C organization events.
	EventTenantCreated         = "ao.authz.tenant.created"
	EventTenantUpdated         = "ao.authz.tenant.updated"
	EventTenantMemberAdded     = "ao.authz.tenant.member_added"
	EventTenantMemberRemoved   = "ao.authz.tenant.member_removed"
	EventProjectTenantAssigned = "ao.authz.project.tenant_assigned"
	EventTeamTenantAssigned    = "ao.authz.team.tenant_assigned"
)

// Event is one authorization change, recorded with the actor who made it and
// the target it applied to.
//
// What is recorded is bounded on purpose: ids, roles and status values. No
// password, no session token, no provider token, and no email local part --
// the same discipline httpd/controllers.AuthAudit follows for the identity
// events, applied to the authorization ones.
type Event struct {
	Name  string
	Actor domain.Principal
	// TargetKind is "user", "team", "tenant" or "project".
	TargetKind string
	// TargetID is the object the change was made to.
	TargetID string
	// SubjectID is the second party of a two-party change (the member added
	// to a team, the subject granted access to a project).
	SubjectID string
	// Detail carries the small, allowlisted specifics of the change.
	Detail map[string]any
}

// Audit consumes authorization-change events. Implemented by LogAudit in
// production; NoopAudit keeps the service usable in tests and in daemons wired
// without telemetry.
type Audit interface {
	Record(ctx context.Context, ev Event)
}

// NoopAudit discards events.
type NoopAudit struct{}

// Record implements Audit.
func (NoopAudit) Record(context.Context, Event) {}

// LogAudit writes events to the daemon log and, when wired, to the telemetry
// sink.
type LogAudit struct {
	Log  *slog.Logger
	Sink ports.EventSink
}

// Record implements Audit.
func (a LogAudit) Record(ctx context.Context, ev Event) {
	attrs := []any{"event", ev.Name}
	payload := map[string]any{}
	if ev.Actor.User.ID != "" {
		attrs = append(attrs, "actorUserId", string(ev.Actor.User.ID))
		payload["actorUserId"] = string(ev.Actor.User.ID)
	}
	if ev.Actor.AuthMethod != "" {
		attrs = append(attrs, "actorAuthMethod", string(ev.Actor.AuthMethod))
		payload["actorAuthMethod"] = string(ev.Actor.AuthMethod)
	}
	if ev.TargetKind != "" {
		attrs = append(attrs, "targetKind", ev.TargetKind)
		payload["targetKind"] = ev.TargetKind
	}
	if ev.TargetID != "" {
		attrs = append(attrs, "targetId", ev.TargetID)
		payload["targetId"] = ev.TargetID
	}
	if ev.SubjectID != "" {
		attrs = append(attrs, "subjectId", ev.SubjectID)
		payload["subjectId"] = ev.SubjectID
	}
	for k, v := range ev.Detail {
		attrs = append(attrs, k, v)
		payload[k] = v
	}
	if a.Log != nil {
		a.Log.Info("authz audit", attrs...)
	}
	if a.Sink == nil {
		return
	}
	a.Sink.Emit(ctx, ports.TelemetryEvent{
		Name:       ev.Name,
		Source:     "authz",
		OccurredAt: time.Now().UTC(),
		Level:      ports.TelemetryLevelInfo,
		Payload:    payload,
	})
}
