package controllers

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// agent_authority_coverage_test.go — the fence covers every session route, and
// keeps covering them.
//
// P5-A phase 2C's fence lives at AuthorizeSessionAccess, so it protects a route
// the moment that route goes through the canonical boundary. What it cannot do
// by itself is survive a new controller: the boundary reads the checker off the
// SessionScoping its caller built, so a controller that forgets to put it there
// is silently unfenced while every one of its routes still looks gated.
//
// That is not hypothetical. It is exactly what happened while this was being
// built: SessionsController -- which owns /send, /kill, /rollback and
// /switch-agent, the most sensitive writes in the system -- had the field and
// the scoping() plumbing but was never handed the dependency in api.go, so the
// fence was wired on three controllers out of five and /kill answered 200 for a
// finished attempt. A route-level test caught it; these two make it structural.

var sessionScopingLiteral = regexp.MustCompile(`SessionScoping\{[^}]*\}`)

// Every SessionScoping built in this package must carry the fence. A scoping
// without it disables the check for every route that controller serves.
func TestEverySessionScopingCarriesTheAuthorityFence(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	var bad []string
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("read %s: %v", f, err)
		}
		for _, lit := range sessionScopingLiteral.FindAllString(string(src), -1) {
			// The zero value is a scoping with nothing enforced at all, used by
			// tests and by call sites that pass their own fields; it is not a
			// controller wiring its own gate.
			if strings.TrimSpace(lit) == "SessionScoping{}" {
				continue
			}
			if !strings.Contains(lit, "AgentAuthority") {
				bad = append(bad, f+": "+strings.Join(strings.Fields(lit), " "))
			}
		}
	}
	if len(bad) > 0 {
		t.Fatalf("these SessionScoping values omit the authority fence, so every route they gate is unfenced:\n  %s",
			strings.Join(bad, "\n  "))
	}
}

// And every controller that HAS the field must actually be handed it where the
// router builds it. A field nobody populates is the same as no fence at all,
// and it is the failure this suite exists because of.
func TestEveryFencedControllerIsHandedTheDependency(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	var fenced []string
	decl := regexp.MustCompile(`type (\w+Controller) struct \{`)
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("read %s: %v", f, err)
		}
		text := string(src)
		for _, m := range decl.FindAllStringSubmatch(text, -1) {
			name := m[1]
			start := strings.Index(text, m[0])
			end := strings.Index(text[start:], "\n}\n")
			if end < 0 {
				continue
			}
			if strings.Contains(text[start:start+end], "AgentAuthority AgentAuthorityChecker") {
				fenced = append(fenced, name)
			}
		}
	}
	if len(fenced) == 0 {
		t.Fatal("no controller declares the fence; the mechanism has been removed")
	}

	api, err := os.ReadFile(filepath.Join("..", "api.go"))
	if err != nil {
		t.Fatalf("read api.go: %v", err)
	}
	text := string(api)
	var unwired []string
	for _, name := range fenced {
		marker := "&controllers." + name + "{"
		start := strings.Index(text, marker)
		if start < 0 {
			// Not built by the router at all; nothing to wire.
			continue
		}
		end := strings.Index(text[start:], "\n\t\t},")
		if end < 0 {
			t.Fatalf("could not read %s's construction in api.go", name)
		}
		if !strings.Contains(text[start:start+end], "AgentAuthority: deps.AgentAuthority") {
			unwired = append(unwired, name)
		}
	}
	if len(unwired) > 0 {
		t.Fatalf("these controllers declare the authority fence but are never handed it, so it does nothing for their routes: %s",
			strings.Join(unwired, ", "))
	}
}
