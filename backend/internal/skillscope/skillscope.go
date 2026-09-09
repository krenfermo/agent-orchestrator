// Package skillscope is the one definition of "which run may do this".
//
// Secrets (phase 5) and network egress (phase 7) both need to answer the same
// question — which tenant, project, package version and mode a permission
// belongs to — and both need the same answer: every field, no wildcard, exact
// match. Two copies of that rule would drift, and the drift would be a grant
// that one subsystem honours and the other does not.
package skillscope

import (
	"errors"
	"fmt"
	"strings"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

// ErrInvalid marks a partially-specified scope.
var ErrInvalid = errors.New("skillscope: invalid")

// Scope is what a grant is bound to. Every field narrows; none is optional,
// because a grant missing one of them answers "who may do this" with
// "somebody, somewhere, running something".
type Scope struct {
	// TenantID and ProjectID bind the grant to one organization's project.
	TenantID  domain.TenantID
	ProjectID domain.ProjectID
	// SkillID and Version bind it to the exact package. A newer version is a
	// different package with a different manifest, so it needs its own grant.
	SkillID string
	Version string
	// ModeID binds it to one mode. A package granted network in its dependency
	// mode does not get it in its pentest mode.
	ModeID string
}

// Validate rejects a partially-specified scope.
func (s Scope) Validate() error {
	fields := []struct {
		name  string
		value string
	}{
		{"tenantId", string(s.TenantID)},
		{"projectId", string(s.ProjectID)},
		{"skillId", s.SkillID},
		{"version", s.Version},
		{"modeId", s.ModeID},
	}
	missing := make([]string, 0, len(fields))
	for _, f := range fields {
		if strings.TrimSpace(f.value) == "" {
			missing = append(missing, f.name)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("%w: scope is missing %s; a grant without every field is a grant to "+
			"somebody, somewhere, running something", ErrInvalid, strings.Join(missing, ", "))
	}
	// A wildcard is refused OUTRIGHT rather than left to fail at Matches.
	//
	// Matches is exact equality, so a scope holding "*" would simply never
	// match anything and the grant would be dead rather than dangerous. That is
	// the wrong failure: somebody who wrote "*" meant "all of them", and a
	// grant that silently means "none of them" teaches them the syntax works.
	// The next person to read the row sees a wildcard grant that appears to
	// exist. Refusing at the boundary keeps "what may this scope do" answerable
	// by reading the row.
	for _, f := range fields {
		if strings.ContainsAny(f.value, "*?") {
			return fmt.Errorf("%w: %s is %q; a scope has no wildcards, and one that appeared to "+
				"have them would be a grant nobody can enumerate", ErrInvalid, f.name, f.value)
		}
	}
	return nil
}

// Matches reports whether this scope is exactly the other. There is no
// wildcard, and adding one would be adding a way to write a grant nobody can
// reason about.
func (s Scope) Matches(other Scope) bool {
	return s.TenantID == other.TenantID &&
		s.ProjectID == other.ProjectID &&
		s.SkillID == other.SkillID &&
		s.Version == other.Version &&
		s.ModeID == other.ModeID
}

// String renders the scope for an audit line. It contains no secret material.
func (s Scope) String() string {
	return fmt.Sprintf("%s/%s %s@%s#%s", s.TenantID, s.ProjectID, s.SkillID, s.Version, s.ModeID)
}
