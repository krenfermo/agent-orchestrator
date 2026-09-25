package codegraph

import (
	"bytes"
	"path"
	"strings"

	"github.com/aoagents/agent-orchestrator/backend/internal/repoaccess"
)

// classify.go — what a file IS, and whether AO is allowed to read it at all.
//
// Two decisions live here because both have to be made identically by the
// walker (which decides what to visit) and by the per-file sync path (which
// decides what to keep). Splitting them would let a path the walker refused
// enter the graph through an incremental update naming it directly, which is
// exactly the hole a diff-driven indexer would otherwise have.

// FileRole is what kind of file an entry describes.
type FileRole string

// The file roles the native indexer assigns.
const (
	// RoleSource is ordinary implementation code.
	RoleSource FileRole = "source"
	// RoleTest is a test file. Tests are the highest-value nodes in the graph
	// for verification: "what covers this symbol" is a question an agent asks
	// before it changes anything.
	RoleTest FileRole = "test"
	// RoleMigration is a schema migration.
	RoleMigration FileRole = "migration"
	// RoleQuery is a named-query file (sqlc and friends).
	RoleQuery FileRole = "query"
	// RoleGenerated is code produced by a generator. It is indexed rather than
	// skipped -- generated code is frequently the API authority, and an agent
	// asking "where does this DTO come from" needs the answer -- but it is
	// marked, so retrieval can prefer a hand-written definition over a
	// thousand generated ones.
	RoleGenerated FileRole = "generated"
)

// ClassifyFile assigns a role to a project-relative path. Content is consulted
// only for the generated-code marker, which is a convention (`Code generated
// ... DO NOT EDIT.`) rather than something a path can express; pass nil when
// the bytes are not at hand and the path-based verdict is enough.
func ClassifyFile(rel string, src []byte) FileRole {
	rel = strings.ToLower(rel)
	base := path.Base(rel)

	if isMigrationPath(rel) {
		return RoleMigration
	}
	if strings.HasSuffix(base, ".sql") {
		return RoleQuery
	}
	// A generated test is still a test: what a caller wants to know about it
	// is that it is coverage, not that a tool wrote it.
	if isTestPath(rel, base) {
		return RoleTest
	}
	if isGeneratedPath(rel, base) || hasGeneratedMarker(src) {
		return RoleGenerated
	}
	return RoleSource
}

func isMigrationPath(rel string) bool {
	if !strings.HasSuffix(rel, ".sql") {
		return false
	}
	for _, seg := range strings.Split(rel, "/") {
		if seg == "migrations" || seg == "migration" {
			return true
		}
	}
	return false
}

func isTestPath(rel, base string) bool {
	switch {
	case strings.HasSuffix(base, "_test.go"):
		return true
	case strings.HasSuffix(base, ".test.ts"), strings.HasSuffix(base, ".test.tsx"),
		strings.HasSuffix(base, ".test.js"), strings.HasSuffix(base, ".test.jsx"),
		strings.HasSuffix(base, ".spec.ts"), strings.HasSuffix(base, ".spec.tsx"),
		strings.HasSuffix(base, ".spec.js"), strings.HasSuffix(base, ".spec.jsx"):
		return true
	case strings.HasPrefix(base, "test_") && strings.HasSuffix(base, ".py"):
		return true
	case strings.HasSuffix(base, "_test.py"):
		return true
	}
	for _, seg := range strings.Split(rel, "/") {
		if seg == "__tests__" || seg == "tests" || seg == "testdata" {
			return true
		}
	}
	return false
}

func isGeneratedPath(rel, base string) bool {
	switch {
	case strings.HasSuffix(base, ".pb.go"), strings.HasSuffix(base, "_pb.go"),
		strings.HasSuffix(base, ".gen.go"), strings.HasSuffix(base, "_gen.go"),
		strings.HasSuffix(base, ".generated.ts"), strings.HasSuffix(base, ".gen.ts"),
		strings.HasSuffix(base, "_pb2.py"):
		return true
	}
	for _, seg := range strings.Split(rel, "/") {
		if seg == "gen" || seg == "generated" || seg == "__generated__" {
			return true
		}
	}
	return false
}

// generatedMarkerScan is how far into a file the DO-NOT-EDIT banner is looked
// for. Every generator that writes one writes it in the first few lines.
const generatedMarkerScan = 1024

func hasGeneratedMarker(src []byte) bool {
	if len(src) == 0 {
		return false
	}
	head := src
	if len(head) > generatedMarkerScan {
		head = head[:generatedMarkerScan]
	}
	lowered := bytes.ToLower(head)
	return bytes.Contains(lowered, []byte("code generated")) &&
		bytes.Contains(lowered, []byte("do not edit"))
}

// DeniedPath reports whether a project-relative path must never be read. It is
// the security exclusion of section 28 of the brief: a graph fact may record
// that a configuration KEY exists (from the code that reads it); nothing here
// ever opens the file that holds its value.
//
// Frente 3 / 3B: the list itself now lives in repoaccess, shared with project
// memory, so the two indexers can no longer disagree about what a secret is.
func DeniedPath(rel string) bool {
	return repoaccess.IsSecretPath(rel)
}
