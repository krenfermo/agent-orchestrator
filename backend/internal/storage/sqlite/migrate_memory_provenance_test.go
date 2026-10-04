package sqlite

import (
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	sqlitestore "github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/store"
)

// migrate_memory_provenance_test.go — the durable half of P2-D (§29).
//
// Everything here is against real SQLite at the real migrated schema, because
// the three properties being checked are properties of the SCHEMA and not of
// any Go code: a legacy row survives with no provenance, a partial unique index
// collapses a duplicate boundary onto one row, and the authority axis moves
// independently of the drift state.

// seedMemoryProvenanceDB migrates to head and seeds the one project and run
// every case below needs.
func seedMemoryProvenanceDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+filepath.Join(t.TempDir(), "ao.db")+pragmas)
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := migrate(db); err != nil {
		t.Fatalf("migrate to head: %v", err)
	}
	if _, err := db.Exec(
		`INSERT INTO projects (id, path, registered_at) VALUES ('p', '/tmp/p', CURRENT_TIMESTAMP)`,
	); err != nil {
		t.Fatalf("seed project: %v", err)
	}
	if _, err := db.Exec(`
		INSERT INTO workflow_runs (id, project_id, objective, state, policy_version, policy_snapshot,
			created_at, updated_at)
		VALUES ('wf-1', 'p', 'objective', 'running', 'v1', '{}', CURRENT_TIMESTAMP, CURRENT_TIMESTAMP)`,
	); err != nil {
		t.Fatalf("seed run: %v", err)
	}
	return db
}

func integratedBoundary(taskID, sourceSHA, targetSHA string, generation int64) domain.WorkflowMutationProvenance {
	rec := domain.WorkflowMutationProvenance{
		ID:                        "wmp-" + taskID + "-" + targetSHA,
		WorkflowRunID:             "wf-1",
		TaskID:                    taskID,
		ProjectID:                 "p",
		Boundary:                  domain.BoundaryIntegrated,
		Placement:                 domain.MutationPlacementIsolatedWorktree,
		Generation:                generation,
		HeadSHA:                   sourceSHA,
		IntegrationTargetRef:      "refs/heads/main",
		IntegrationTargetAfterSHA: targetSHA,
		IntegrationMethod:         domain.IntegrationFastForward,
		RepoIdentity:              domain.RepoIdentity("root_abc"),
		CreatedAt:                 time.Date(2026, 8, 31, 12, 0, 0, 0, time.UTC),
	}
	rec.IdempotencyKey = domain.MutationIdempotencyKey(
		rec.WorkflowRunID, rec.TaskID, rec.Boundary, rec.Generation, rec.HeadSHA, rec.IntegrationTargetAfterSHA)
	return rec
}

// TestMutationProvenanceIsExactlyOncePerBoundary is the duplicate-callback and
// restart case (P2-D §29), and it is a property of the partial unique index
// rather than of any caller's bookkeeping.
//
// The two writes below are what a harness retrying a completion callback, or a
// daemon that died between observing the integration and writing the row, both
// produce: the same boundary, described twice, with DIFFERENT row ids because
// each attempt minted its own. Exactly one row must survive, and the second
// caller must be handed the surviving row rather than a zero value it would
// read as "no provenance exists".
func TestMutationProvenanceIsExactlyOncePerBoundary(t *testing.T) {
	db := seedMemoryProvenanceDB(t)
	store := sqlitestore.NewStore(db, db)
	ctx := t.Context()

	first, err := store.RecordWorkflowMutationProvenance(ctx, integratedBoundary("task-1", "src-1", "tgt-1", 1))
	if err != nil {
		t.Fatalf("record first: %v", err)
	}
	duplicate := integratedBoundary("task-1", "src-1", "tgt-1", 1)
	duplicate.ID = "wmp-a-second-attempt-at-the-same-moment"
	second, err := store.RecordWorkflowMutationProvenance(ctx, duplicate)
	if err != nil {
		t.Fatalf("record duplicate: %v", err)
	}

	if second.ID != first.ID {
		t.Fatalf("a duplicate boundary produced a second row %s beside %s", second.ID, first.ID)
	}
	rows, err := store.ListWorkflowMutationProvenanceByTask(ctx, "task-1")
	if err != nil {
		t.Fatalf("list by task: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("%d provenance rows for one boundary, want exactly 1", len(rows))
	}
	if rows[0].IntegrationTargetAfterSHA != "tgt-1" || rows[0].IntegrationMethod != domain.IntegrationFastForward {
		t.Fatalf("the surviving row lost its integration evidence: %+v", rows[0])
	}
}

// TestMutationProvenanceSeparatesDistinctBoundaries is the other half of the
// same property: collapsing must not go too far.
//
// A re-integration onto a moved target is a DIFFERENT moment from the first
// one, and a boundary of a different kind for the same task is a different
// moment again. Both must produce their own rows, or the exactly-once
// guarantee would be hiding real history.
func TestMutationProvenanceSeparatesDistinctBoundaries(t *testing.T) {
	db := seedMemoryProvenanceDB(t)
	store := sqlitestore.NewStore(db, db)
	ctx := t.Context()

	if _, err := store.RecordWorkflowMutationProvenance(ctx, integratedBoundary("task-1", "src-1", "tgt-1", 1)); err != nil {
		t.Fatalf("record first integration: %v", err)
	}
	if _, err := store.RecordWorkflowMutationProvenance(ctx, integratedBoundary("task-1", "src-1", "tgt-2", 2)); err != nil {
		t.Fatalf("record re-integration: %v", err)
	}
	verified := integratedBoundary("task-1", "src-1", "", 1)
	verified.ID = "wmp-verified"
	verified.Boundary = domain.BoundaryVerified
	verified.IntegrationTargetAfterSHA = ""
	verified.IdempotencyKey = domain.MutationIdempotencyKey("wf-1", "task-1", domain.BoundaryVerified, 1, "src-1", "")
	if _, err := store.RecordWorkflowMutationProvenance(ctx, verified); err != nil {
		t.Fatalf("record verified boundary: %v", err)
	}

	rows, err := store.ListWorkflowMutationProvenanceByTask(ctx, "task-1")
	if err != nil {
		t.Fatalf("list by task: %v", err)
	}
	if len(rows) != 3 {
		t.Fatalf("%d rows, want 3 distinct boundaries", len(rows))
	}

	// Newest generation wins, which is what pins a promotion to the LAST
	// integration AO observed rather than the first.
	latest, found, err := store.GetLatestWorkflowMutationProvenanceByTaskBoundary(ctx, "task-1", domain.BoundaryIntegrated)
	if err != nil || !found {
		t.Fatalf("latest integrated boundary: found=%v err=%v", found, err)
	}
	if latest.IntegrationTargetAfterSHA != "tgt-2" {
		t.Fatalf("latest integration is %s, want the re-integration tgt-2", latest.IntegrationTargetAfterSHA)
	}
}

// TestBlankIdempotencyKeysDoNotCollide guards the partial index's WHERE clause.
//
// A writer that honestly cannot identify the boundary leaves the key empty,
// and two such writers are describing two observations AO cannot prove are the
// same moment. Collapsing them would be inventing an identity, which is the
// one thing this table may never do.
func TestBlankIdempotencyKeysDoNotCollide(t *testing.T) {
	db := seedMemoryProvenanceDB(t)
	store := sqlitestore.NewStore(db, db)
	ctx := t.Context()

	for i, id := range []string{"wmp-blank-a", "wmp-blank-b"} {
		rec := integratedBoundary("task-blank", "src", "tgt", int64(i))
		rec.ID = id
		rec.IdempotencyKey = ""
		if _, err := store.RecordWorkflowMutationProvenance(ctx, rec); err != nil {
			t.Fatalf("record %s: %v", id, err)
		}
	}
	rows, err := store.ListWorkflowMutationProvenanceByTask(ctx, "task-blank")
	if err != nil {
		t.Fatalf("list by task: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("%d rows for two unidentifiable observations, want 2", len(rows))
	}
}

// TestMemoryAuthorityMovesIndependentlyOfState is the schema half of the
// two-axis model.
//
// A fact whose files nobody touched (state stays valid) can lose its licence,
// and moving the licence must not disturb the drift state, the content, or the
// generation fence. If the two axes were one column this test could not be
// written at all.
func TestMemoryAuthorityMovesIndependentlyOfState(t *testing.T) {
	db := seedMemoryProvenanceDB(t)
	store := sqlitestore.NewStore(db, db)
	ctx := t.Context()
	now := time.Date(2026, 8, 31, 12, 0, 0, 0, time.UTC)

	if err := store.EnsureProjectMemoryRepo(ctx, "p", "repo-1", "/tmp/p", now); err != nil {
		t.Fatalf("ensure repo: %v", err)
	}
	item := domain.ProjectMemoryItem{
		Key: domain.ProjectMemoryKey{
			ProjectID: "p", RepoID: "repo-1",
			Type: domain.MemoryTypeDecision, Scope: domain.MemoryScopeRepository, Key: "transport",
		},
		Summary:        "the API speaks GraphQL",
		SourceCommit:   "c1",
		ProvenanceKind: domain.ProvenanceTaskOutcome,
		RepoIdentity:   domain.RepoIdentity("root_abc"),
	}.Normalized()
	if _, err := store.PutProjectMemoryItem(ctx, item, now); err != nil {
		t.Fatalf("put item: %v", err)
	}

	applied, err := store.SetProjectMemoryItemAuthority(ctx, item.ID, item.Generation,
		domain.AuthorityUnprovable,
		domain.MemoryAuthorityReason(domain.ReasonPromotionUnprovable, "no integration on record"), now)
	if err != nil || !applied {
		t.Fatalf("set authority: applied=%v err=%v", applied, err)
	}

	got, found, err := store.GetProjectMemoryItem(ctx, item.ID)
	if err != nil || !found {
		t.Fatalf("read back: found=%v err=%v", found, err)
	}
	if got.State != domain.MemoryStateValid {
		t.Fatalf("state = %q, want valid: withholding a licence must not touch the drift axis", got.State)
	}
	if got.Authority != domain.AuthorityUnprovable {
		t.Fatalf("authority = %q, want unprovable", got.Authority)
	}
	if got.Servable() {
		t.Fatal("an unprovable fact is still being served")
	}
	if domain.MemoryAuthorityReasonClass(got.AuthorityReason) != domain.ReasonPromotionUnprovable {
		t.Fatalf("reason class = %q, want %q",
			domain.MemoryAuthorityReasonClass(got.AuthorityReason), domain.ReasonPromotionUnprovable)
	}
	if got.Summary != item.Summary {
		t.Fatal("withholding a licence rewrote the fact's content")
	}

	// RECONFIRMATION IS NOT REVALIDATION (P2-D section 19). A pass that finds
	// this fact's content unchanged refreshes its provenance and its
	// generation, and must NOT hand back the licence: "the evidence still
	// looks the same" was never the reason the fact was withheld. Only a
	// promotion or a real re-derivation can re-establish one.
	reconfirm := got
	reconfirm.Generation = 5
	reconfirm.Authority = domain.AuthorityAuthoritative
	reconfirm = reconfirm.Normalized()
	if _, err := store.PutProjectMemoryItem(ctx, reconfirm, now); err != nil {
		t.Fatalf("reconfirm at a newer generation: %v", err)
	}
	stillWithheld, _, err := store.GetProjectMemoryItem(ctx, item.ID)
	if err != nil {
		t.Fatalf("read back after reconfirmation: %v", err)
	}
	if stillWithheld.Authority != domain.AuthorityUnprovable {
		t.Fatalf("authority = %q after a mere reconfirmation, want it to stay unprovable",
			stillWithheld.Authority)
	}
	if stillWithheld.Generation != 5 {
		t.Fatalf("generation = %d after reconfirmation, want the fence to have moved to 5",
			stillWithheld.Generation)
	}

	// A real REBUILD -- new content, new generation -- does re-establish it,
	// because the fact was derived again rather than merely re-observed.
	rebuilt := stillWithheld
	rebuilt.Summary = "the API speaks gRPC"
	rebuilt.Generation = 6
	rebuilt.Authority = domain.AuthorityAuthoritative
	rebuilt = rebuilt.Normalized()
	if _, err := store.PutProjectMemoryItem(ctx, rebuilt, now); err != nil {
		t.Fatalf("rebuild at a newer generation: %v", err)
	}

	// And now the stale-validator case: a pass that woke up carrying the OLD
	// generation must not be able to withhold the rebuilt fact. It changes
	// nothing and says so, rather than winning silently.
	stale, err := store.SetProjectMemoryItemAuthority(ctx, item.ID, 0,
		domain.AuthorityUnprovable, domain.MemoryAuthorityReason(domain.ReasonGenerationStale, "late pass"), now)
	if err != nil {
		t.Fatalf("stale authority write: %v", err)
	}
	if stale {
		t.Fatal("a validator carrying an older generation withheld a rebuilt fact")
	}
	after, _, err := store.GetProjectMemoryItem(ctx, item.ID)
	if err != nil {
		t.Fatalf("read back after stale write: %v", err)
	}
	if after.Authority != domain.AuthorityAuthoritative || !after.Servable() {
		t.Fatalf("authority = %q after a refused stale write, want the rebuilt fact still served", after.Authority)
	}
}

// TestLegacyMemoryRowsAreWithheldNotDeleted is P2-D §21.
//
// A row an upgraded install already had carries no provenance kind, because
// the column did not exist when it was written. It must be withheld — not
// served, not deleted, and not given a fabricated provenance — and it must be
// classified as LEGACY rather than as broken, because the two want different
// operator responses.
func TestLegacyMemoryRowsAreWithheldNotDeleted(t *testing.T) {
	db := seedMemoryProvenanceDB(t)
	store := sqlitestore.NewStore(db, db)
	ctx := t.Context()
	now := time.Date(2026, 8, 31, 12, 0, 0, 0, time.UTC)

	if err := store.EnsureProjectMemoryRepo(ctx, "p", "repo-1", "/tmp/p", now); err != nil {
		t.Fatalf("ensure repo: %v", err)
	}
	legacy := domain.ProjectMemoryItem{
		Key: domain.ProjectMemoryKey{
			ProjectID: "p", RepoID: "repo-1",
			Type: domain.MemoryTypeConvention, Scope: domain.MemoryScopeRepository, Key: "naming",
		},
		Summary: "table-driven tests everywhere",
		// No ProvenanceKind and no RepoIdentity: exactly a pre-0146 row.
	}.Normalized()
	if _, err := store.PutProjectMemoryItem(ctx, legacy, now); err != nil {
		t.Fatalf("put legacy item: %v", err)
	}

	n, err := store.MarkLegacyProjectMemoryItemsUnprovable(ctx, "p", "repo-1",
		domain.MemoryAuthorityReason(domain.ReasonLegacyNoProvenance, "predates provenance recording"), now)
	if err != nil {
		t.Fatalf("classify legacy: %v", err)
	}
	if n != 1 {
		t.Fatalf("%d rows classified, want 1", n)
	}

	got, found, err := store.GetProjectMemoryItem(ctx, legacy.ID)
	if err != nil || !found {
		t.Fatalf("the legacy row was deleted rather than withheld: found=%v err=%v", found, err)
	}
	if got.Authority != domain.AuthorityLegacyUnprovable {
		t.Fatalf("authority = %q, want legacy_unprovable", got.Authority)
	}
	if got.Servable() {
		t.Fatal("a legacy row with no provenance is still being served")
	}
	if got.ProvenanceKind != "" {
		t.Fatalf("provenance kind was fabricated as %q", got.ProvenanceKind)
	}

	// Idempotent, and it never reclassifies a row a later pass ruled on. A
	// second sweep after something proved (or disproved) the row must leave
	// that verdict alone.
	if _, err := store.SetProjectMemoryItemAuthority(ctx, legacy.ID, legacy.Generation,
		domain.AuthorityAuthoritative, "", now); err != nil {
		t.Fatalf("re-establish: %v", err)
	}
	again, err := store.MarkLegacyProjectMemoryItemsUnprovable(ctx, "p", "repo-1", "reason", now)
	if err != nil {
		t.Fatalf("second sweep: %v", err)
	}
	if again != 1 {
		// It IS reclassified here, because the row was put back to the default
		// authority with its provenance still empty -- which is honestly still
		// a legacy row. What the guard prevents is reclassifying a row already
		// sitting at a non-default authority; that is the case below.
		t.Fatalf("%d rows on the second sweep, want 1", again)
	}
	third, err := store.MarkLegacyProjectMemoryItemsUnprovable(ctx, "p", "repo-1", "reason", now)
	if err != nil {
		t.Fatalf("third sweep: %v", err)
	}
	if third != 0 {
		t.Fatalf("%d rows reclassified when all were already withheld, want 0", third)
	}
}

// TestRepoIdentityChangeWithholdsEverythingAndNeverGuesses is P2-D §9's
// dangerous case, at the schema.
//
// A different repository checked out where an old one was must not inherit the
// old one's knowledge. And the complement matters just as much: a day on which
// AO cannot read the identity at all must change NOTHING, because turning "I
// could not tell today" into "this project's memory is gone" would be the
// worst possible reading of a missing fact.
func TestRepoIdentityChangeWithholdsEverythingAndNeverGuesses(t *testing.T) {
	db := seedMemoryProvenanceDB(t)
	store := sqlitestore.NewStore(db, db)
	ctx := t.Context()
	now := time.Date(2026, 8, 31, 12, 0, 0, 0, time.UTC)

	if err := store.EnsureProjectMemoryRepo(ctx, "p", "repo-1", "/tmp/p", now); err != nil {
		t.Fatalf("ensure repo: %v", err)
	}
	mk := func(key string, identity domain.RepoIdentity) domain.ProjectMemoryItem {
		return domain.ProjectMemoryItem{
			Key: domain.ProjectMemoryKey{
				ProjectID: "p", RepoID: "repo-1",
				Type: domain.MemoryTypeModule, Scope: domain.MemoryScopeModule, Key: key,
			},
			Summary:        "module " + key,
			ProvenanceKind: domain.ProvenanceRepoDerivation,
			RepoIdentity:   identity,
			SourceCommit:   "c1",
		}.Normalized()
	}
	old := mk("a", domain.RepoIdentity("remote_old"))
	unidentified := mk("b", "")
	current := mk("c", domain.RepoIdentity("remote_new"))
	for _, item := range []domain.ProjectMemoryItem{old, unidentified, current} {
		if _, err := store.PutProjectMemoryItem(ctx, item, now); err != nil {
			t.Fatalf("put %s: %v", item.Key.Key, err)
		}
	}

	// An unreadable identity changes nothing at all.
	none, err := store.MarkProjectMemoryItemsUnprovableByRepoIdentity(ctx, "p", "repo-1", "", "reason", now)
	if err != nil {
		t.Fatalf("sweep with unknown identity: %v", err)
	}
	if none != 0 {
		t.Fatalf("%d facts withheld on a day AO could not identify the repository, want 0", none)
	}

	n, err := store.MarkProjectMemoryItemsUnprovableByRepoIdentity(ctx, "p", "repo-1",
		domain.RepoIdentity("remote_new"),
		domain.MemoryAuthorityReason(domain.ReasonRepoIdentityChanged, "a different repository now sits here"), now)
	if err != nil {
		t.Fatalf("identity sweep: %v", err)
	}
	if n != 2 {
		t.Fatalf("%d facts withheld, want 2 (the one from the old repository and the one AO could never place)", n)
	}

	for _, tc := range []struct {
		item domain.ProjectMemoryItem
		want domain.MemoryAuthority
	}{
		{old, domain.AuthorityUnprovable},
		{unidentified, domain.AuthorityUnprovable},
		{current, domain.AuthorityAuthoritative},
	} {
		got, found, err := store.GetProjectMemoryItem(ctx, tc.item.ID)
		if err != nil || !found {
			t.Fatalf("read %s: found=%v err=%v", tc.item.Key.Key, found, err)
		}
		if got.Authority != tc.want {
			t.Fatalf("%s authority = %q, want %q", tc.item.Key.Key, got.Authority, tc.want)
		}
	}
}
