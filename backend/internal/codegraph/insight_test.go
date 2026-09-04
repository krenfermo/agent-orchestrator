package codegraph

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
)

// insight_test.go — the architecture summary and task retrieval, proven on a
// fixture that has a real shape: an HTTP layer over a service over a store
// over SQL, with tests and a frontend beside it.
//
// The fixture matters as much as the assertions. A summary or a retrieval that
// works on two files proves nothing; what has to hold is that the ranking
// picks the authorization path out of a layered project when the only input is
// the sentence a user would actually type.

// newLayeredProject writes a checkout shaped like the projects AO is meant to
// remember: api -> service -> store -> schema, with tests and a renderer.
func newLayeredProject(t *testing.T) string {
	t.Helper()
	root := filepath.Join(t.TempDir(), "medusa")

	writeFile(t, root, "cmd/api/main.go", `package main

import "example.com/medusa/internal/api"

func main() { api.Serve() }
`)

	writeFile(t, root, "internal/api/routes.go", `package api

import (
	"net/http"

	"github.com/go-chi/chi/v5"

	"example.com/medusa/internal/service"
)

// Serve installs the HTTP surface.
func Serve() {}

// Routes registers every record route.
func Routes(r chi.Router, s *service.Records) {
	r.Delete("/api/records/{id}", deleteRecord)
	r.Post("/api/records/export", exportRecords)
}

func deleteRecord(w http.ResponseWriter, req *http.Request) {}

// exportRecords is the export endpoint handler.
func exportRecords(w http.ResponseWriter, req *http.Request) {}
`)

	writeFile(t, root, "internal/service/records.go", `package service

import "example.com/medusa/internal/store"

// Role names a principal's role.
type Role string

// Supervisor may approve and, from this change on, export.
const Supervisor Role = "supervisor"

// Records is the record service.
type Records struct{ store *store.Records }

// MayExport decides whether a role may export records. It is the permission
// evaluator, and the only place the answer is decided.
func (r *Records) MayExport(role Role) bool {
	return role == Supervisor
}

// Export writes an export record.
func (r *Records) Export(role Role, id string) error {
	if !r.MayExport(role) {
		return nil
	}
	return r.store.InsertExport(id)
}
`)

	writeFile(t, root, "internal/service/records_test.go", `package service

import "testing"

func TestRecordsMayExport(t *testing.T) {
	r := &Records{}
	if !r.MayExport(Supervisor) {
		t.Fatal("supervisor should be allowed to export")
	}
}
`)

	writeFile(t, root, "internal/store/records.go", `package store

// Records persists records.
type Records struct{}

// InsertExport records one export.
func (r *Records) InsertExport(id string) error { return nil }
`)

	writeFile(t, root, "internal/storage/migrations/0001_records.sql", `-- +goose Up
CREATE TABLE records (id TEXT PRIMARY KEY);
CREATE TABLE record_exports (id TEXT PRIMARY KEY, record_id TEXT REFERENCES records(id));
`)

	writeFile(t, root, "internal/storage/queries/records.sql", `-- name: InsertExport :execrows
-- Record that a record was exported.
INSERT INTO record_exports (id, record_id) VALUES (?, ?);

-- name: ListRecords :many
SELECT * FROM records;
`)

	writeFile(t, root, "web/src/export-panel.tsx", `import { useState } from "react";
import { api } from "./client";

/** ExportPanel offers the export action to supervisors. */
export function ExportPanel() {
  return api.post("/api/records/export");
}
`)
	writeFile(t, root, "web/src/client.ts", `export const api = { post: (p: string) => p };
`)
	return root
}

func indexedGraph(t *testing.T, root string) *Graph {
	t.Helper()
	indexer := newIndexer(t)
	if _, err := indexer.Index(context.Background(), IndexRequest{ProjectRoot: root, Commit: "abc123def456789"}); err != nil {
		t.Fatalf("Index: %v", err)
	}
	canonical, err := CanonicalRoot(root)
	if err != nil {
		t.Fatalf("CanonicalRoot: %v", err)
	}
	graph, found, err := indexer.store.Load(canonical)
	if err != nil || !found {
		t.Fatalf("Load: found=%v err=%v", found, err)
	}
	return graph
}

func TestArchitectureDescribesTheProjectFromCountsAlone(t *testing.T) {
	graph := indexedGraph(t, newLayeredProject(t))
	arch := graph.Architecture()

	if arch.IndexedCommit != "abc123def456789" {
		t.Fatalf("architecture lost its provenance: %+v", arch)
	}
	if len(arch.EntryPoints) != 1 || arch.EntryPoints[0] != "cmd/api/main.go" {
		t.Fatalf("entry points = %v", arch.EntryPoints)
	}
	if arch.Endpoints != 2 {
		t.Fatalf("endpoints = %d, want the two with literal patterns", arch.Endpoints)
	}
	if arch.TableCount != 2 || !hasTarget(arch.Tables, "record_exports") {
		t.Fatalf("tables = %v", arch.Tables)
	}
	if arch.TestFiles != 1 || arch.CoveredSymbols == 0 {
		t.Fatalf("test surface = %d files, %d covered", arch.TestFiles, arch.CoveredSymbols)
	}

	byPath := map[string]ModuleFacts{}
	for _, m := range arch.Modules {
		byPath[m.Path] = m
	}
	service, ok := byPath["internal/service"]
	if !ok {
		t.Fatalf("service module missing from %+v", arch.Modules)
	}
	if !hasTarget(service.DependsOn, "internal/store") {
		t.Fatalf("service -> store dependency not resolved: %+v", service)
	}
	if byPath["internal/store"].DependedOnBy == 0 {
		t.Fatalf("store has no dependants: %+v", byPath["internal/store"])
	}

	integrations := make([]string, 0, len(arch.Integrations))
	for _, i := range arch.Integrations {
		integrations = append(integrations, i.Name)
	}
	if !hasTarget(integrations, "github.com/go-chi/chi") && !hasTarget(integrations, "github.com/go-chi/chi/v5") {
		t.Fatalf("external dependency not recorded: %v", integrations)
	}
	if hasTarget(integrations, "example.com/medusa/internal/store") {
		t.Fatal("an internal module was reported as an external integration")
	}

	rendered := arch.Render()
	if len(rendered) > MaxArchitectureBytes {
		t.Fatalf("rendered architecture is %d bytes, over the %d cap", len(rendered), MaxArchitectureBytes)
	}
	for _, want := range []string{"internal/service", "Entry points: cmd/api/main.go", "Persistence: 2 tables"} {
		if !strings.Contains(rendered, want) {
			t.Fatalf("rendered summary missing %q:\n%s", want, rendered)
		}
	}
	if rendered != graph.Architecture().Render() {
		t.Fatal("architecture rendering is not deterministic")
	}
}

func TestRetrieveFindsTheAuthorizationPathFromAnObjective(t *testing.T) {
	graph := indexedGraph(t, newLayeredProject(t))

	// The sentence a user would actually type, with no architecture explained.
	got := graph.Retrieve(RetrieveRequest{
		Terms: []string{"add export permissions to the Supervisor role"},
	})
	if got.Empty() {
		t.Fatal("retrieval found nothing")
	}

	names := make([]string, 0, len(got.Symbols))
	for _, s := range got.Symbols {
		names = append(names, s.Symbol.Name)
	}
	for _, want := range []string{"Records.MayExport", "Supervisor", "Records.Export"} {
		if !hasTarget(names, want) {
			t.Fatalf("%q missing from retrieval %v", want, names)
		}
	}
	if !hasTarget(names, "POST /api/records/export") {
		t.Fatalf("the export endpoint was not retrieved: %v", names)
	}

	testNames := make([]string, 0, len(got.Tests))
	for _, sym := range got.Tests {
		testNames = append(testNames, sym.Name)
	}
	if !hasTarget(testNames, "TestRecordsMayExport") {
		t.Fatalf("covering test missing: %v (edges: %+v)", testNames, got.Callers)
	}

	if got.ConsideredSymbols <= got.SelectedSymbols() {
		t.Fatalf("considered %d symbols but selected %d: the retrieval is not bounding anything",
			got.ConsideredSymbols, got.SelectedSymbols())
	}

	rendered := got.Render()
	if !strings.Contains(rendered, "internal/service/records.go") {
		t.Fatalf("rendered neighbourhood missing the evaluator:\n%s", rendered)
	}
}

func TestRetrieveAnchorsOnChangedFilesAndBounds(t *testing.T) {
	graph := indexedGraph(t, newLayeredProject(t))

	got := graph.Retrieve(RetrieveRequest{
		Files:      []string{"internal/service/records.go"},
		MaxSymbols: 2,
	})
	if len(got.Symbols) != 2 || !got.Truncated {
		t.Fatalf("bound not enforced: %d symbols, truncated=%v", len(got.Symbols), got.Truncated)
	}
	for _, s := range got.Symbols {
		if s.Symbol.File != "internal/service/records.go" {
			t.Fatalf("an unanchored symbol outranked the anchored ones: %+v", s)
		}
	}

	// Naming a symbol outright beats every other signal.
	named := graph.Retrieve(RetrieveRequest{Symbols: []string{"Records.MayExport"}})
	if len(named.Symbols) == 0 || named.Symbols[0].Symbol.Name != "Records.MayExport" {
		t.Fatalf("a named symbol did not rank first: %+v", named.Symbols)
	}
	if named.Symbols[0].Reason != "named by the task" {
		t.Fatalf("selection reason = %q", named.Symbols[0].Reason)
	}
}

func TestRetrieveExcludesGeneratedCodeUnlessAsked(t *testing.T) {
	root := newLayeredProject(t)
	writeFile(t, root, "internal/gen/models.go", `// Code generated by sqlc. DO NOT EDIT.
package gen

// ExportRow is a generated row type for records export.
type ExportRow struct{}
`)
	graph := indexedGraph(t, root)

	plain := graph.Retrieve(RetrieveRequest{Terms: []string{"export"}})
	for _, s := range plain.Symbols {
		if s.Symbol.File == "internal/gen/models.go" {
			t.Fatalf("generated code entered a default retrieval: %+v", s)
		}
	}

	// It is still in the graph, and still reachable when a caller wants it:
	// generated code is frequently the API authority.
	withGenerated := graph.Retrieve(RetrieveRequest{Terms: []string{"export"}, IncludeGenerated: true})
	found := false
	for _, s := range withGenerated.Symbols {
		if s.Symbol.File == "internal/gen/models.go" {
			found = true
		}
	}
	if !found {
		t.Fatalf("generated code is unreachable even on request: %+v", withGenerated.Symbols)
	}
}
