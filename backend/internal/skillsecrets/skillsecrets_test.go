package skillsecrets

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"
)

const synthetic = "SYNTHETIC-VALUE-NEVER-A-REAL-CREDENTIAL"

// The control that does the most work: a value cannot become text by any of
// the routes Go code normally takes. Every one of these is a path a real leak
// has taken in some codebase.
func TestSecretValue_RefusesToRenderItself(t *testing.T) {
	v := NewSecretValue(synthetic)
	// Held as `any` so the %s case exercises fmt's own verb handling rather
	// than being folded into a direct String() call by the compiler -- which
	// is the path a real accidental leak would take.
	var asAny any = v

	renders := map[string]string{
		"String":     v.String(),
		"%v":         fmt.Sprintf("%v", v),
		"%s":         fmt.Sprintf("%s", asAny),
		"%q":         fmt.Sprintf("%q", v),
		"%+v":        fmt.Sprintf("%+v", v),
		"%#v":        fmt.Sprintf("%#v", v),
		"%v pointer": fmt.Sprintf("%v", &v),
		"Sprint":     fmt.Sprint(v),
		"error":      fmt.Errorf("failed for %v", v).Error(),
	}
	for how, got := range renders {
		if strings.Contains(got, synthetic) {
			t.Fatalf("%s leaked the value: %s", how, got)
		}
		if !strings.Contains(got, Redacted) {
			t.Fatalf("%s did not redact: %s", how, got)
		}
	}

	// Inside a struct, which is how it will actually travel.
	type carrier struct {
		Name  string
		Value SecretValue
	}
	c := carrier{Name: "API_TOKEN", Value: v}
	for how, got := range map[string]string{
		"struct %v":  fmt.Sprintf("%v", c),
		"struct %+v": fmt.Sprintf("%+v", c),
	} {
		if strings.Contains(got, synthetic) {
			t.Fatalf("%s leaked the value: %s", how, got)
		}
	}

	// JSON, which is how a report or an API response would carry it.
	body, err := json.Marshal(c)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(body), synthetic) {
		t.Fatalf("JSON leaked the value: %s", body)
	}
	if !strings.Contains(string(body), Redacted) {
		t.Fatalf("JSON did not redact: %s", body)
	}

	// And a structured logger, which is the most likely accidental route.
	var buf bytes.Buffer
	slog.New(slog.NewJSONHandler(&buf, nil)).Info("delivering", "secret", v, "carrier", c)
	if strings.Contains(buf.String(), synthetic) {
		t.Fatalf("slog leaked the value: %s", buf.String())
	}

	// Reveal is the one door, and it works.
	if v.Reveal() != synthetic {
		t.Fatal("Reveal did not return the plaintext")
	}
}

// A value must never arrive from a request body: it is registered through the
// one path that seals it.
func TestSecretValue_RefusesToBeDecoded(t *testing.T) {
	var v SecretValue
	if err := json.Unmarshal([]byte(`"whatever"`), &v); err == nil {
		t.Fatal("a secret value was decoded from JSON")
	}
	if !v.IsZero() {
		t.Fatal("a refused decode left a value behind")
	}
}

func TestParseRef(t *testing.T) {
	for _, ok := range []string{"API_TOKEN", "SENTRY_DSN", "A_B_C_9"} {
		if _, err := ParseRef(ok); err != nil {
			t.Fatalf("ParseRef(%q): %v", ok, err)
		}
	}
	for _, bad := range []string{"", "ab", "lower_case", "WITH-DASH", "9LEADING", "WITH SPACE",
		"WITH/SLASH", "../ESCAPE", strings.Repeat("A", 65)} {
		if _, err := ParseRef(bad); err == nil {
			t.Fatalf("ParseRef(%q) was accepted", bad)
		}
	}
}

func fullScope() Scope {
	return Scope{
		TenantID: "tenant-a", ProjectID: "medusa",
		SkillID: "security-audit", Version: "0.1.0", ModeID: "dependencies",
	}
}

// A grant missing any scope field is a grant to somebody, somewhere, running
// something.
func TestScope_RejectsAPartialScope(t *testing.T) {
	if err := fullScope().Validate(); err != nil {
		t.Fatalf("a full scope was rejected: %v", err)
	}
	mutations := map[string]func(*Scope){
		"tenantId":  func(s *Scope) { s.TenantID = "" },
		"projectId": func(s *Scope) { s.ProjectID = "" },
		"skillId":   func(s *Scope) { s.SkillID = "" },
		"version":   func(s *Scope) { s.Version = "" },
		"modeId":    func(s *Scope) { s.ModeID = "" },
	}
	for field, mutate := range mutations {
		s := fullScope()
		mutate(&s)
		err := s.Validate()
		if err == nil || !strings.Contains(err.Error(), field) {
			t.Fatalf("missing %s: err = %v", field, err)
		}
	}
}

// There is no wildcard. Every field must match exactly, which is what makes
// the tenant, project, version and mode negative cases hold.
func TestScope_MatchesIsExact(t *testing.T) {
	base := fullScope()
	if !base.Matches(fullScope()) {
		t.Fatal("a scope did not match itself")
	}
	for name, mutate := range map[string]func(*Scope){
		"other tenant":  func(s *Scope) { s.TenantID = "tenant-b" },
		"other project": func(s *Scope) { s.ProjectID = "poseidon" },
		"other skill":   func(s *Scope) { s.SkillID = "other-skill" },
		"other version": func(s *Scope) { s.Version = "0.2.0" },
		"other mode":    func(s *Scope) { s.ModeID = "static-code" },
	} {
		other := fullScope()
		mutate(&other)
		if base.Matches(other) {
			t.Fatalf("%s matched", name)
		}
	}
}

func TestGrant_ActiveHonoursExpiryAndRevocation(t *testing.T) {
	now := time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC)
	live := Grant{
		ID: "g1", Ref: "API_TOKEN", Scope: fullScope(), GrantedBy: "admin",
		GrantedAt: now.Add(-time.Hour), ExpiresAt: now.Add(time.Hour),
	}
	if !live.Active(now) {
		t.Fatal("a live grant read as inactive")
	}

	expired := live
	expired.ExpiresAt = now.Add(-time.Minute)
	if expired.Active(now) {
		t.Fatal("an expired grant read as active")
	}

	revokedAt := now.Add(-time.Minute)
	revoked := live
	revoked.RevokedAt = &revokedAt
	if revoked.Active(now) {
		t.Fatal("a revoked grant read as active")
	}

	// Revocation in the future has not happened yet.
	future := now.Add(time.Minute)
	pending := live
	pending.RevokedAt = &future
	if !pending.Active(now) {
		t.Fatal("a future revocation was applied early")
	}
}

func TestGrant_Validate(t *testing.T) {
	now := time.Now().UTC()
	base := Grant{
		ID: "g1", Ref: "API_TOKEN", Scope: fullScope(), GrantedBy: "admin",
		GrantedAt: now, ExpiresAt: now.Add(time.Hour),
	}
	if err := base.Validate(); err != nil {
		t.Fatalf("a valid grant was rejected: %v", err)
	}
	for name, mutate := range map[string]func(*Grant){
		"bad ref":                   func(g *Grant) { g.Ref = "lower" },
		"no approver":               func(g *Grant) { g.GrantedBy = "" },
		"no expiry":                 func(g *Grant) { g.ExpiresAt = time.Time{} },
		"expires before it is made": func(g *Grant) { g.ExpiresAt = g.GrantedAt.Add(-time.Hour) },
		"partial scope":             func(g *Grant) { g.Scope.ModeID = "" },
	} {
		g := base
		mutate(&g)
		if err := g.Validate(); err == nil {
			t.Fatalf("%s was accepted", name)
		}
	}
}

func TestLease_UsableAndValidate(t *testing.T) {
	now := time.Now().UTC()
	base := Lease{
		ID: "l1", Scope: fullScope(), RunID: "run-1", AttemptID: "attempt-1",
		Refs: []Ref{"API_TOKEN"}, IssuedAt: now, ExpiresAt: now.Add(time.Minute),
	}
	if err := base.Validate(); err != nil {
		t.Fatalf("a valid lease was rejected: %v", err)
	}
	if !base.Usable(now) {
		t.Fatal("a fresh lease was unusable")
	}

	consumedAt := now
	spent := base
	spent.ConsumedAt = &consumedAt
	if spent.Usable(now) {
		t.Fatal("a consumed lease was reusable")
	}
	if base.Usable(now.Add(2 * time.Minute)) {
		t.Fatal("an expired lease was usable")
	}

	for name, mutate := range map[string]func(*Lease){
		"no run":                      func(l *Lease) { l.RunID = "" },
		"no attempt":                  func(l *Lease) { l.AttemptID = "" },
		"no refs":                     func(l *Lease) { l.Refs = nil },
		"expires before it is issued": func(l *Lease) { l.ExpiresAt = l.IssuedAt.Add(-time.Second) },
	} {
		l := base
		mutate(&l)
		if err := l.Validate(); err == nil {
			t.Fatalf("%s was accepted", name)
		}
	}
}
