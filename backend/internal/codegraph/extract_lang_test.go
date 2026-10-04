package codegraph

import (
	"strings"
	"testing"
)

// extract_lang_test.go — the per-language proof.
//
// Section 43 of the brief: a language is not "supported" because an extractor
// is registered for it. It is supported when a test shows real symbols and
// real relations coming out of real source. Each fixture below is written the
// way the language is actually written -- with comments, docstrings, nested
// scopes and strings that contain code -- because those are what a scanner
// gets wrong, and a fixture without them proves nothing.

func edgeTargets(edges []Edge, kind EdgeKind, from string) []string {
	var out []string
	for _, edge := range edges {
		if edge.Kind == kind && (from == "" || edge.From == from) {
			out = append(out, edge.To)
		}
	}
	return out
}

func hasTarget(targets []string, want string) bool {
	for _, t := range targets {
		if t == want {
			return true
		}
	}
	return false
}

const goServiceFixture = `package api

import (
	"net/http"
	"os"

	"github.com/go-chi/chi/v5"
)

// Store is what the service persists through.
type Store interface {
	// Delete removes one record.
	Delete(id string) error
}

// Base carries the shared fields.
type Base struct{ ID string }

// Service applies the export permission rules. It is the only place that
// decides them.
type Service struct {
	Base
	store Store
}

var _ Store = (*Service)(nil)

// Delete removes a record if the caller may.
func (s *Service) Delete(id string) error {
	if os.Getenv("AO_ALLOW_DELETE") == "" {
		return nil
	}
	return s.store.Delete(id)
}

// Routes installs the HTTP surface.
func Routes(r chi.Router, s *Service) {
	r.Delete("/api/records/{id}", s.Delete)
	r.Get("/api/records", listRecords)
	// A computed pattern is deliberately not recorded.
	r.Post(buildPath(), s.Delete)
}

func listRecords(http.ResponseWriter, *http.Request) {}

func buildPath() string { return "/api/records" }
`

func TestGoExtractorProvesStructureRoutesAndConfig(t *testing.T) {
	extraction, err := goExtractor{}.Extract("internal/api/service.go", []byte(goServiceFixture))
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	const file = "internal/api/service.go"

	iface, ok := findSymbol(extraction.Symbols, "Store")
	if !ok || iface.Kind != SymbolInterface {
		t.Fatalf("interface not recorded as an interface: %+v", iface)
	}
	if !strings.Contains(iface.Summary, "what the service persists through") {
		t.Fatalf("interface summary lost its doc sentence: %q", iface.Summary)
	}
	if _, ok := findSymbol(extraction.Symbols, "Store.Delete"); !ok {
		t.Fatalf("interface method set missing from %+v", extraction.Symbols)
	}

	svc, ok := findSymbol(extraction.Symbols, "Service")
	if !ok || svc.Signature != "struct" {
		t.Fatalf("struct symbol = %+v", svc)
	}
	if !strings.HasPrefix(svc.Summary, "type Service struct — Service applies the export permission rules.") {
		t.Fatalf("struct summary = %q", svc.Summary)
	}

	del, ok := findSymbol(extraction.Symbols, "Service.Delete")
	if !ok {
		t.Fatal("method missing")
	}
	if del.Signature != "(id string) error" {
		t.Fatalf("signature = %q", del.Signature)
	}
	if !strings.Contains(del.Summary, "[reads config]") {
		t.Fatalf("statically evident side effect missing from summary %q", del.Summary)
	}
	if del.EndLine <= del.Line {
		t.Fatalf("declaration span = %d..%d", del.Line, del.EndLine)
	}

	for _, want := range []Edge{
		{Kind: EdgeEmbeds, From: file + "#type:Service", To: "Base"},
		{Kind: EdgeImplements, From: file + "#type:Service", To: "Store"},
		{Kind: EdgeConfigures, From: file + "#method:Service.Delete", To: "AO_ALLOW_DELETE"},
		{Kind: EdgeRoutesTo, From: file + "#endpoint:DELETE /api/records/{id}", To: "s.Delete"},
		{Kind: EdgeRoutesTo, From: file + "#endpoint:GET /api/records", To: "listRecords"},
		{Kind: EdgeReferences, From: file + "#method:Service.Delete", To: "error"},
	} {
		if want.To == "error" {
			// `error` is a builtin and must NOT be a reference edge.
			if hasEdge(extraction.Edges, want) {
				t.Fatalf("builtin type recorded as a reference: %+v", want)
			}
			continue
		}
		if !hasEdge(extraction.Edges, want) {
			t.Fatalf("edge %+v missing from %+v", want, extraction.Edges)
		}
	}

	if _, ok := findSymbol(extraction.Symbols, "AO_ALLOW_DELETE"); !ok {
		t.Fatal("configuration key not recorded as a symbol")
	}
	for _, sym := range extraction.Symbols {
		if sym.Kind == SymbolEndpoint && strings.Contains(sym.Name, "buildPath") {
			t.Fatalf("a computed route pattern was invented: %+v", sym)
		}
	}
	if got := len(edgeTargets(extraction.Edges, EdgeRoutesTo, "")); got != 2 {
		t.Fatalf("routes recorded = %d, want exactly the two with literal patterns", got)
	}
}

const goTestFixture = `package api

import "testing"

func TestServiceDelete(t *testing.T) {
	s := &Service{}
	if err := s.Delete("x"); err != nil {
		t.Fatal(err)
	}
}

func TestNothingReal(t *testing.T) {
	t.Skip("placeholder")
}
`

func TestGoExtractorProvesTestCoverageFromEvidence(t *testing.T) {
	extraction, err := goExtractor{}.Extract("internal/api/service_test.go", []byte(goTestFixture))
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	const file = "internal/api/service_test.go"

	if sym, ok := findSymbol(extraction.Symbols, "TestServiceDelete"); !ok || sym.Kind != SymbolTest {
		t.Fatalf("test function not recorded as a test: %+v", sym)
	}
	if !hasEdge(extraction.Edges, Edge{Kind: EdgeTests, From: file + "#test:TestServiceDelete", To: "Delete"}) {
		t.Fatalf("tests edge missing from %+v", extraction.Edges)
	}
	// The evidence rule: a test whose name suggests a subject it never calls
	// asserts nothing.
	if got := edgeTargets(extraction.Edges, EdgeTests, file+"#test:TestNothingReal"); len(got) != 0 {
		t.Fatalf("coverage invented from a name alone: %v", got)
	}
}

const tsFixture = "import React from 'react';\n" +
	"import { load } from './store';\n" +
	"\n" +
	"/** Panel renders the memory summary. */\n" +
	"export class Panel extends Base implements Renderable {\n" +
	"  // A comment that mentions class Ghost and import 'ghost' from 'nowhere'.\n" +
	"  render(): string {\n" +
	"    const label = `class Fake extends Nothing`;\n" +
	"    return load(process.env.AO_MEMORY_MODE) + label;\n" +
	"  }\n" +
	"\n" +
	"  private hidden() {\n" +
	"    return 1;\n" +
	"  }\n" +
	"}\n" +
	"\n" +
	"export const helper = (n: number): number => {\n" +
	"  return n + 1;\n" +
	"};\n"

func TestTypeScriptExtractorMasksNonCodeAndQualifiesMethods(t *testing.T) {
	extraction, err := tsExtractor{language: "typescript", extensions: []string{".ts"}}.
		Extract("web/panel.ts", []byte(tsFixture))
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	const file = "web/panel.ts"

	for _, ghost := range []string{"Ghost", "Fake"} {
		if _, ok := findSymbol(extraction.Symbols, ghost); ok {
			t.Fatalf("%q was read out of a comment or a template literal", ghost)
		}
	}
	if hasTarget(edgeTargets(extraction.Edges, EdgeImport, file), "nowhere") {
		t.Fatal("an import written inside a comment was recorded")
	}
	for _, want := range []string{"react", "./store"} {
		if !hasTarget(edgeTargets(extraction.Edges, EdgeImport, file), want) {
			t.Fatalf("import %q missing from %v", want, edgeTargets(extraction.Edges, EdgeImport, file))
		}
	}

	render, ok := findSymbol(extraction.Symbols, "Panel.render")
	if !ok || render.Kind != SymbolMethod {
		t.Fatalf("class method not qualified by its class: %+v", extraction.Symbols)
	}
	if _, ok := findSymbol(extraction.Symbols, "Panel.hidden"); !ok {
		t.Fatal("private method missing")
	}
	panel, _ := findSymbol(extraction.Symbols, "Panel")
	if !strings.Contains(panel.Summary, "Panel renders the memory summary.") {
		t.Fatalf("JSDoc lost: %q", panel.Summary)
	}

	for _, want := range []Edge{
		{Kind: EdgeEmbeds, From: file + "#type:Panel", To: "Base"},
		{Kind: EdgeImplements, From: file + "#type:Panel", To: "Renderable"},
		{Kind: EdgeConfigures, From: file + "#method:Panel.render", To: "AO_MEMORY_MODE"},
		{Kind: EdgeCall, From: file + "#method:Panel.render", To: "load"},
	} {
		if !hasEdge(extraction.Edges, want) {
			t.Fatalf("edge %+v missing from %+v", want, extraction.Edges)
		}
	}
	if _, ok := findSymbol(extraction.Symbols, "helper"); !ok {
		t.Fatal("exported arrow function missing")
	}
}

const pyFixture = `"""Module docstring mentioning def ghost and class Ghost."""

import os
from app import store


class Repo(Base):
    """Repo stores records. It is the only writer."""

    def delete(self, key):
        """Delete removes one record."""
        return os.environ["AO_DB_PATH"] + store.delete(key)


def load(path):
    """Load reads a record from disk."""
    return open(path).read()


MAX = 10
`

func TestPythonExtractorMasksDocstringsAndQualifiesMethods(t *testing.T) {
	extraction, err := pyExtractor{}.Extract("app/repo.py", []byte(pyFixture))
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	const file = "app/repo.py"

	for _, ghost := range []string{"ghost", "Ghost"} {
		if _, ok := findSymbol(extraction.Symbols, ghost); ok {
			t.Fatalf("%q was read out of a docstring", ghost)
		}
	}
	repo, ok := findSymbol(extraction.Symbols, "Repo")
	if !ok || !strings.Contains(repo.Summary, "Repo stores records.") {
		t.Fatalf("class docstring not attached: %+v", repo)
	}
	del, ok := findSymbol(extraction.Symbols, "Repo.delete")
	if !ok || del.Kind != SymbolMethod {
		t.Fatalf("method not qualified: %+v", extraction.Symbols)
	}
	if !strings.Contains(del.Summary, "Delete removes one record.") {
		t.Fatalf("method docstring not attached: %q", del.Summary)
	}
	if _, ok := findSymbol(extraction.Symbols, "load"); !ok {
		t.Fatal("module function missing")
	}
	if sym, ok := findSymbol(extraction.Symbols, "MAX"); !ok || sym.Kind != SymbolConstant {
		t.Fatalf("module constant missing: %+v", sym)
	}

	for _, want := range []Edge{
		{Kind: EdgeEmbeds, From: file + "#type:Repo", To: "Base"},
		{Kind: EdgeConfigures, From: file + "#method:Repo.delete", To: "AO_DB_PATH"},
		{Kind: EdgeImport, From: file, To: "os"},
		{Kind: EdgeImport, From: file, To: "app"},
	} {
		if !hasEdge(extraction.Edges, want) {
			t.Fatalf("edge %+v missing from %+v", want, extraction.Edges)
		}
	}
}

const sqlMigrationFixture = `-- +goose Up
CREATE TABLE records (
    id   TEXT PRIMARY KEY,
    team TEXT NOT NULL REFERENCES teams(id)
);
CREATE INDEX idx_records_team ON records (team);
`

const sqlQueryFixture = `-- name: GetRecord :one
-- Read one record with its team.
SELECT * FROM records JOIN teams ON teams.id = records.team WHERE records.id = ?;

-- name: DeleteRecord :execrows
DELETE FROM records WHERE id = ?;
`

func TestSQLExtractorReadsSchemaAndNamedQueries(t *testing.T) {
	migration, err := sqlExtractor{}.Extract("internal/storage/sqlite/migrations/0153_records.sql", []byte(sqlMigrationFixture))
	if err != nil {
		t.Fatalf("Extract migration: %v", err)
	}
	if sym, ok := findSymbol(migration.Symbols, "records"); !ok || sym.Kind != SymbolTable {
		t.Fatalf("table not declared: %+v", migration.Symbols)
	}

	queries, err := sqlExtractor{}.Extract("internal/storage/sqlite/queries/records.sql", []byte(sqlQueryFixture))
	if err != nil {
		t.Fatalf("Extract queries: %v", err)
	}
	const file = "internal/storage/sqlite/queries/records.sql"

	get, ok := findSymbol(queries.Symbols, "GetRecord")
	if !ok || get.Kind != SymbolQuery {
		t.Fatalf("named query missing: %+v", queries.Symbols)
	}
	if get.Signature != ":one" || !strings.Contains(get.Summary, "Read one record with its team.") {
		t.Fatalf("query symbol = %+v", get)
	}
	for _, want := range []Edge{
		{Kind: EdgeReadsFrom, From: file + "#query:GetRecord", To: "records"},
		{Kind: EdgeReadsFrom, From: file + "#query:GetRecord", To: "teams"},
		{Kind: EdgeWritesTo, From: file + "#query:DeleteRecord", To: "records"},
	} {
		if !hasEdge(queries.Edges, want) {
			t.Fatalf("edge %+v missing from %+v", want, queries.Edges)
		}
	}
	if hasEdge(queries.Edges, Edge{Kind: EdgeWritesTo, From: file + "#query:GetRecord", To: "records"}) {
		t.Fatal("a read-only query was recorded as a writer")
	}
}

func TestDeniedPathsAreNeverIndexable(t *testing.T) {
	for _, denied := range []string{
		".env", ".env.local", "deploy/.env.production", "certs/server.pem",
		"secrets.json", "id_rsa", "config/credentials",
	} {
		if !DeniedPath(denied) {
			t.Fatalf("DeniedPath(%q) = false, want true", denied)
		}
	}
	// Source code that HANDLES secrets is still code, and refusing it would
	// blind the graph to the code a reviewer most wants.
	for _, allowed := range []string{
		"internal/auth/secrets.go", "web/credentials.ts", "app/secret.py",
	} {
		if DeniedPath(allowed) {
			t.Fatalf("DeniedPath(%q) = true, want false", allowed)
		}
	}
}

func TestClassifyFileNamesTheRolesRetrievalDependsOn(t *testing.T) {
	cases := map[string]FileRole{
		"internal/api/service.go":                            RoleSource,
		"internal/api/service_test.go":                       RoleTest,
		"web/panel.test.tsx":                                 RoleTest,
		"app/test_repo.py":                                   RoleTest,
		"internal/storage/sqlite/migrations/0144_memory.sql": RoleMigration,
		"internal/storage/sqlite/queries/project_memory.sql": RoleQuery,
		"internal/storage/sqlite/gen/models.go":              RoleGenerated,
		"frontend/src/api/schema.gen.ts":                     RoleGenerated,
	}
	for path, want := range cases {
		if got := ClassifyFile(path, nil); got != want {
			t.Fatalf("ClassifyFile(%q) = %q, want %q", path, got, want)
		}
	}
	if got := ClassifyFile("internal/api/dto.go", []byte("// Code generated by specgen. DO NOT EDIT.\npackage api\n")); got != RoleGenerated {
		t.Fatalf("generated marker not honoured: %q", got)
	}
}
