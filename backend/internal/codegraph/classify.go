package codegraph

import (
	"bytes"
	"path"
	"strings"
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

// deniedBaseNames are files whose CONTENT is a secret by convention. They are
// refused before a read, not filtered after one: the point of the rule is that
// the bytes never enter the process, so a later bug cannot leak what was never
// loaded.
//
// This is belt-and-braces with the extractor set (which claims only source
// extensions), and it is the braces on purpose: a repository with a
// `secrets.py`, an `env.ts` config module, or a `credentials.go` would
// otherwise be admitted by extension alone.
var deniedBaseNames = map[string]bool{
	".env": true, ".envrc": true, ".netrc": true, ".npmrc": true, ".pypirc": true,
	"credentials": true, "credentials.json": true, "id_rsa": true, "id_dsa": true,
	"id_ecdsa": true, "id_ed25519": true, "secrets.json": true, "secrets.yaml": true,
	"secrets.yml": true, ".htpasswd": true, "kubeconfig": true,
}

// deniedExtensions are file extensions that only ever hold key material.
var deniedExtensions = map[string]bool{
	".pem": true, ".key": true, ".p12": true, ".pfx": true, ".jks": true,
	".keystore": true, ".crt": true, ".cer": true, ".der": true, ".asc": true,
	".gpg": true, ".kdbx": true,
}

// deniedNamePrefixes catch the dotted variants of the env file: ".env.local",
// ".env.production", ".env.staging".
//
// It deliberately does NOT include a "secret"/"credentials" prefix. A
// `secrets.go` or a `credentials.ts` is source code that HANDLES secrets, not
// a file that contains one, and refusing to index it would blind the graph to
// exactly the code a reviewer most wants to find. The data files that do hold
// values are named explicitly in deniedBaseNames.
var deniedNamePrefixes = []string{".env."}

// DeniedPath reports whether a project-relative path must never be read. It is
// the security exclusion of section 28 of the brief: a graph fact may record
// that a configuration KEY exists (see extract_config.go); nothing here ever
// opens the file that holds its value.
func DeniedPath(rel string) bool {
	base := strings.ToLower(path.Base(strings.ReplaceAll(rel, "\\", "/")))
	if base == "" {
		return false
	}
	if deniedBaseNames[base] {
		return true
	}
	if deniedExtensions[strings.ToLower(path.Ext(base))] {
		return true
	}
	for _, prefix := range deniedNamePrefixes {
		if strings.HasPrefix(base, prefix) {
			return true
		}
	}
	return false
}
