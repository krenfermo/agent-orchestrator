package sqlite

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/pressly/goose/v3"
)

// migrate_rebuild_fk_safety_test.go — the systemic guard for the class of
// incident that migration 0160 caused.
//
// THE INCIDENT. SQLite cannot widen a CHECK in place, so a column-constraint
// change forces a table rebuild: create the new shape, copy, DROP the old,
// rename. The first version of 0160 copied that recipe from 0096-0102
// verbatim. Foreign keys are enforced while goose runs, and SQLite treats
// DROP TABLE as deleting every row of the dropped table for foreign-key
// purposes, so `DROP TABLE workflow_attempts` raised
// SQLITE_CONSTRAINT_FOREIGNKEY (787) against the 249 child rows a real
// ~/.ao/data/ao.db held, and the daemon exited 1 on boot.
//
// WHY THE 0160 TEST DID NOT CATCH IT, and why a per-migration test is not
// enough on its own. Every migration test in this package seeds the table
// being migrated. The pre-fix 0160 test seeded an attempt with NOTHING
// REFERENCING IT, so the migration ran against exactly the one database shape
// where the bug is invisible. That is not a lapse specific to 0160: a fresh
// database has no rows at all, so a rebuild that orphans its children passes
// every check that does not deliberately create a child row first. The author
// of the next rebuild has no reason to know which tables acquired an incoming
// foreign key since the recipe was written — 0133 gave workflow_attempts two
// more, four migrations before 0160 needed them.
//
// So this file removes the need to know. It derives the incoming-foreign-key
// inventory from the schema itself at the point each migration runs, requires
// every table rebuild to be declared, and then EXERCISES each one against a
// database that actually holds the referencing rows.
//
// THE RULE these tests encode. A migration that drops a table with incoming
// foreign keys must do one of two things:
//
//  1. Park and restore the references — copy each child's (id, fk value) to a
//     scratch table, NULL the column, rebuild, put the values back. 0119, 0130
//     and the corrected 0160 do this. It keeps foreign keys enforced
//     throughout, so a genuine referential error is still caught.
//
//  2. Disable enforcement in a way that actually takes effect — `PRAGMA
//     foreign_keys=OFF` TOGETHER WITH `-- +goose NO TRANSACTION`. 0028, 0080,
//     0115, 0116, 0150 and 0152 do this.
//
// The trap is the combination that looks like (2) and is not: `PRAGMA
// foreign_keys=OFF` inside goose's default transaction, where SQLite silently
// ignores it. TestForeignKeysOffRequiresNoTransaction refuses that outright,
// because it fails as if no one had written the pragma at all.
//
// Doing NEITHER is what 0096-0102 did and what 0160 inherited. Those five are
// recorded below as outcomeOrphansChildren — they are merged and must not be
// edited, but they are the recipe that caused the incident and nothing should
// copy them again.

type rebuildHandling string

const (
	// handlingPark parks each incoming reference and restores it after the
	// rebuild, with foreign keys enforced throughout.
	handlingPark rebuildHandling = "park-and-restore"
	// handlingPragmaOff turns enforcement off for real, which requires the
	// migration to opt out of goose's transaction.
	handlingPragmaOff rebuildHandling = "foreign_keys=OFF + NO TRANSACTION"
	// handlingNone is the unsafe recipe: neither of the above.
	handlingNone rebuildHandling = "none"
)

type rebuildOutcome string

const (
	// outcomePreserved: the migration applies with referencing rows present
	// and every reference still points at its parent afterwards.
	outcomePreserved rebuildOutcome = "preserved"
	// outcomeOrphansChildren: the migration FAILS when a child row references
	// the table it drops. Recorded only for already-merged migrations that
	// cannot be edited. A new rebuild must never be registered this way.
	outcomeOrphansChildren rebuildOutcome = "orphans-its-children"
)

type tableRebuild struct {
	version int64
	table   string
	// children is the checked-in inventory of incoming foreign keys as they
	// exist in the schema immediately BEFORE this migration runs, formatted
	// "child_table.column ON DELETE <action>". The delete action decides the
	// failure mode: NO ACTION raises 787 and stops the daemon; CASCADE
	// silently deletes the child rows instead.
	children []string
	handling rebuildHandling
	outcome  rebuildOutcome
	// downNote, when set, explains why the Down direction is not exercised.
	downNote string
}

// knownTableRebuilds is the declared inventory. TestTableRebuildInventoryIsComplete
// derives the same set from the migrations and fails when they disagree, so a
// new rebuild cannot land without being added here deliberately.
var knownTableRebuilds = []tableRebuild{
	{
		version: 28, table: "projects",
		children: []string{
			"change_log.project_id ON DELETE NO ACTION",
			"notifications.project_id ON DELETE CASCADE",
			"review.project_id ON DELETE NO ACTION",
			"sessions.project_id ON DELETE NO ACTION",
			"shell_terminals.project_id ON DELETE CASCADE",
			"worker_idle_events.project_id ON DELETE CASCADE",
			"workspace_repos.project_id ON DELETE CASCADE",
		},
		handling: handlingPragmaOff, outcome: outcomePreserved,
	},
	{
		version: 80, table: "review",
		children: []string{"review_run.review_id ON DELETE CASCADE"},
		handling: handlingPragmaOff, outcome: outcomePreserved,
	},
	// 0096-0102: the unsafe recipe. Each drops workflow_attempts while
	// workflow_checkpoints.attempt_id references it, with no parking and no
	// effective pragma. They are merged and frozen; they are here so the
	// inventory tells the truth and so nothing copies them again.
	{
		version: 96, table: "workflow_attempts",
		children: []string{"workflow_checkpoints.attempt_id ON DELETE NO ACTION"},
		handling: handlingNone, outcome: outcomeOrphansChildren,
	},
	{
		version: 97, table: "workflow_attempts",
		children: []string{"workflow_checkpoints.attempt_id ON DELETE NO ACTION"},
		handling: handlingNone, outcome: outcomeOrphansChildren,
	},
	{
		version: 99, table: "workflow_attempts",
		children: []string{"workflow_checkpoints.attempt_id ON DELETE NO ACTION"},
		handling: handlingNone, outcome: outcomeOrphansChildren,
	},
	{
		version: 100, table: "workflow_attempts",
		children: []string{"workflow_checkpoints.attempt_id ON DELETE NO ACTION"},
		handling: handlingNone, outcome: outcomeOrphansChildren,
	},
	{
		version: 102, table: "workflow_attempts",
		children: []string{"workflow_checkpoints.attempt_id ON DELETE NO ACTION"},
		handling: handlingNone, outcome: outcomeOrphansChildren,
	},
	{
		version: 115, table: "provider_profiles",
		children: []string{"agent_health_events.provider_profile_id ON DELETE NO ACTION"},
		handling: handlingPragmaOff, outcome: outcomePreserved,
	},
	{
		version: 116, table: "users",
		children: []string{
			"agent_health_events.user_id ON DELETE NO ACTION",
			"auth_sessions.user_id ON DELETE NO ACTION",
			"provider_profiles.user_id ON DELETE NO ACTION",
			"user_execution_policies.user_id ON DELETE NO ACTION",
		},
		handling: handlingPragmaOff, outcome: outcomePreserved,
	},
	{
		version: 119, table: "workflow_tasks",
		children: []string{
			"workflow_task_dependencies.depends_on_task_id ON DELETE CASCADE",
			"workflow_task_dependencies.workflow_task_id ON DELETE CASCADE",
		},
		handling: handlingPark, outcome: outcomePreserved,
	},
	{
		version: 130, table: "workflow_tasks",
		children: []string{
			"workflow_task_dependencies.depends_on_task_id ON DELETE CASCADE",
			"workflow_task_dependencies.workflow_task_id ON DELETE CASCADE",
			"workflow_task_relationships.related_task_id ON DELETE CASCADE",
			"workflow_task_relationships.task_id ON DELETE CASCADE",
			"workflow_task_worktrees.task_id ON DELETE CASCADE",
		},
		handling: handlingPark, outcome: outcomePreserved,
	},
	{
		version: 150, table: "usage_bindings",
		children: []string{
			"model_usage_events.binding_id ON DELETE CASCADE",
			"usage_sources.binding_id ON DELETE CASCADE",
		},
		handling: handlingPragmaOff, outcome: outcomePreserved,
		downNote: "0150 is deliberately irreversible: its Down raises rather than discard non-session usage bindings.",
	},
	{
		version: 152, table: "users",
		children: []string{
			"agent_health_events.user_id ON DELETE NO ACTION",
			"auth_sessions.user_id ON DELETE NO ACTION",
			"external_identities.user_id ON DELETE NO ACTION",
			"oidc_login_flows.authenticated_user_id ON DELETE NO ACTION",
			"provider_profiles.user_id ON DELETE NO ACTION",
			"user_execution_policies.user_id ON DELETE NO ACTION",
		},
		handling: handlingPragmaOff, outcome: outcomePreserved,
	},
	{
		version: 160, table: "workflow_attempts",
		children: []string{
			"workflow_checkpoints.attempt_id ON DELETE NO ACTION",
			"workflow_dispatch_checkpoints.attempt_id ON DELETE NO ACTION",
			"workflow_mutation_provenance.attempt_id ON DELETE NO ACTION",
		},
		handling: handlingPark, outcome: outcomePreserved,
	},
	{
		// 0166 widens skill_registries.trust_policy to accept 'official'.
		//
		// This is the case the guard exists for. 0165 rebuilt the SAME table
		// for free, because it created both child tables AFTER the rebuild;
		// four months later those children exist, and both cascade. Silently
		// cascading skill_registry_revocations away would not be ordinary data
		// loss -- those are the rows that block installs of withdrawn
		// releases, so losing them re-enables exactly what revocation exists
		// to prevent, and nothing would have said so.
		version: 166, table: "skill_registries",
		children: []string{
			"skill_registry_revocations.registry_id ON DELETE CASCADE",
			"skill_registry_status.registry_id ON DELETE CASCADE",
		},
		handling: handlingPragmaOff, outcome: outcomePreserved,
	},
}

// TestTableRebuildInventoryIsComplete walks the migrations in order, and at
// each step reads the incoming-foreign-key graph from the live schema BEFORE
// applying the next migration. Any migration that drops a table which already
// exists and already has children must be declared in knownTableRebuilds with
// exactly those children.
//
// This is the part that would have spoken up while 0160 was being written: the
// three references workflow_attempts had acquired by 0159 are read from the
// database, not from the author's memory of the recipe.
func TestTableRebuildInventoryIsComplete(t *testing.T) {
	db := openRebuildScanDB(t)

	declared := map[string]tableRebuild{}
	for _, r := range knownTableRebuilds {
		declared[rebuildKey(r.version, r.table)] = r
	}
	seen := map[string]bool{}

	for _, m := range allMigrations(t) {
		incoming := incomingForeignKeys(t, db)
		for _, dropped := range droppedTables(m.up) {
			refs := incoming[dropped]
			if len(refs) == 0 {
				continue
			}
			key := rebuildKey(m.version, dropped)
			seen[key] = true
			got := formatChildren(refs)
			r, ok := declared[key]
			if !ok {
				t.Errorf(`%s rebuilds %q, which %d table(s) reference.

This migration drops a table that other rows point at. SQLite treats DROP TABLE
as deleting every row of it, so with foreign keys enforced this either fails
with SQLITE_CONSTRAINT_FOREIGNKEY (787) and stops the daemon on boot, or
silently deletes the referencing rows where the reference is ON DELETE CASCADE.

Incoming references at this point in the schema:
    %s

Do ONE of these in the migration, then add it to knownTableRebuilds:
  * park and restore  — copy each child's (primary key, fk value) into a
    scratch table, set the column NULL, rebuild, restore the values, drop the
    scratch table. Keeps enforcement on. See 0160 for the worked example.
  * PRAGMA foreign_keys=OFF *and* '-- +goose NO TRANSACTION' — the pragma is
    silently ignored inside goose's transaction, so one without the other is
    the same as writing nothing.

TestRegisteredTableRebuildsPreserveIncomingReferences will then run this
migration against a database that actually holds those referencing rows.`,
					m.name, dropped, len(refs), strings.Join(got, "\n    "))
				continue
			}
			if !equalStrings(got, r.children) {
				t.Errorf(`%s rebuilds %q and its declared incoming references are out of date.

  declared: %v
  actual:   %v

Update the knownTableRebuilds entry, and make sure the migration itself handles
every reference in the actual list.`, m.name, dropped, r.children, got)
			}
		}
		applyMigration(t, db, m.version)
	}

	for key, r := range declared {
		if !seen[key] {
			t.Errorf("knownTableRebuilds declares %s, but no migration drops that table with incoming references; remove the stale entry", key)
		}
		_ = r
	}
}

// TestRegisteredTableRebuildsPreserveIncomingReferences is the behavioural half:
// it runs each declared rebuild against a database that holds a row in the
// table being dropped AND a row in every table that references it, which is
// precisely the database shape the pre-fix 0160 test did not build.
//
// The fixture rows are synthetic and deliberately not domain-valid: CHECK
// enforcement is relaxed while they are created and while the migration runs,
// because the property under test is referential, not semantic. Foreign key
// enforcement, which is the thing that actually decides this, is left ON
// exactly as production has it.
func TestRegisteredTableRebuildsPreserveIncomingReferences(t *testing.T) {
	for _, r := range knownTableRebuilds {
		t.Run(fmt.Sprintf("%04d_%s", r.version, r.table), func(t *testing.T) {
			db := openRebuildScanDB(t)
			applyMigration(t, db, r.version-1)

			refs := incomingForeignKeys(t, db)[r.table]
			if len(refs) == 0 {
				t.Fatalf("no incoming references to %s at version %d; the inventory is stale", r.table, r.version-1)
			}

			// Build the fixture with enforcement relaxed, so a synthetic row
			// only has to satisfy NOT NULL and type, not the domain's CHECKs.
			execPragmas(t, db, "PRAGMA foreign_keys=OFF", "PRAGMA ignore_check_constraints=ON")
			seeded := map[string]string{}
			parentPK := seedSyntheticRow(t, db, r.table, seeded, 0)
			for _, ref := range refs {
				seedSyntheticRow(t, db, ref.child, seeded, 0)
				if _, err := db.Exec(
					fmt.Sprintf(`UPDATE "%s" SET "%s" = ?`, ref.child, ref.column), parentPK,
				); err != nil {
					t.Fatalf("point %s.%s at the seeded %s row: %v", ref.child, ref.column, r.table, err)
				}
			}
			// Production enforcement for the migration itself.
			execPragmas(t, db, "PRAGMA foreign_keys=ON")

			err := migrateTo(db, r.version)

			if r.outcome == outcomeOrphansChildren {
				if err == nil {
					t.Fatalf("migration %04d is recorded as orphaning its children but applied cleanly; if it was made safe, update its knownTableRebuilds outcome to %q", r.version, outcomePreserved)
				}
				if !strings.Contains(err.Error(), "FOREIGN KEY constraint failed") {
					t.Fatalf("migration %04d failed for a reason other than the recorded orphaned references: %v", r.version, err)
				}
				return
			}
			if err != nil {
				t.Fatalf(`migration %04d could not be applied to a database holding the rows that reference %q: %v

This is the 0160 failure mode. Park each incoming reference and restore it
after the rebuild, or opt the migration out of goose's transaction so
PRAGMA foreign_keys=OFF actually takes effect.`, r.version, r.table, err)
			}
			assertReferencesIntact(t, db, refs, parentPK, "after Up")

			if r.downNote != "" {
				return
			}
			if err := migrateDownTo(db, r.version-1); err != nil {
				t.Fatalf("migration %04d could not be rolled back with referencing rows present: %v", r.version, err)
			}
			assertReferencesIntact(t, db, refs, parentPK, "after Down")
		})
	}
}

// TestForeignKeysOffRequiresNoTransaction refuses the trap that reads as a
// safe rebuild and is not. goose wraps a migration in a transaction unless it
// is marked otherwise, and SQLite silently ignores PRAGMA foreign_keys inside
// one — no error, no warning, enforcement simply stays on. A migration that
// relies on the pragma without opting out of the transaction is therefore
// exactly as exposed as one that never wrote it.
func TestForeignKeysOffRequiresNoTransaction(t *testing.T) {
	pragmaOff := regexp.MustCompile(`(?i)PRAGMA\s+foreign_keys\s*=\s*OFF`)
	for _, m := range allMigrations(t) {
		if !pragmaOff.MatchString(m.body) {
			continue
		}
		if !strings.Contains(m.body, "NO TRANSACTION") {
			t.Errorf(`%s sets PRAGMA foreign_keys=OFF but is not marked '-- +goose NO TRANSACTION'.

goose runs it inside a transaction, where SQLite ignores that pragma without
reporting anything, so the migration runs with enforcement fully ON. Either add
the marker, or park and restore the references instead.`, m.name)
		}
	}
}

// --- migration corpus -------------------------------------------------------

type migrationFile struct {
	version int64
	name    string
	body    string // whole file
	up      string // Up section, comments stripped
}

var (
	migrationVersionRe = regexp.MustCompile(`^(\d+)_`)
	dropTableRe        = regexp.MustCompile(`(?i)\bDROP\s+TABLE\s+(?:IF\s+EXISTS\s+)?["'` + "`" + `\[]?([A-Za-z0-9_]+)`)
	createTableRe      = regexp.MustCompile(`(?i)\bCREATE\s+(?:TEMP\s+|TEMPORARY\s+)?TABLE\s+(?:IF\s+NOT\s+EXISTS\s+)?["'` + "`" + `\[]?([A-Za-z0-9_]+)`)
)

func allMigrations(t *testing.T) []migrationFile {
	t.Helper()
	entries, err := migrationsFS.ReadDir("migrations")
	if err != nil {
		t.Fatalf("read migrations: %v", err)
	}
	var out []migrationFile
	for _, e := range entries {
		match := migrationVersionRe.FindStringSubmatch(e.Name())
		if match == nil {
			continue
		}
		version, err := strconv.ParseInt(match[1], 10, 64)
		if err != nil {
			t.Fatalf("parse version from %s: %v", e.Name(), err)
		}
		body, err := migrationsFS.ReadFile("migrations/" + e.Name())
		if err != nil {
			t.Fatalf("read %s: %v", e.Name(), err)
		}
		out = append(out, migrationFile{
			version: version,
			name:    e.Name(),
			body:    string(body),
			up:      stripSQLComments(upSection(string(body))),
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].version < out[j].version })
	return out
}

func upSection(body string) string {
	start := strings.Index(body, "-- +goose Up")
	if start < 0 {
		return body
	}
	rest := body[start:]
	if end := strings.Index(rest, "-- +goose Down"); end >= 0 {
		return rest[:end]
	}
	return rest
}

// stripSQLComments removes whole-line `--` comments. The migrations describe
// their own DROP TABLE statements in prose — 0160's header quotes the exact
// statement that caused the incident — and a scanner that reads those as code
// would report rebuilds that do not exist.
func stripSQLComments(sql string) string {
	var kept []string
	for _, line := range strings.Split(sql, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "--") {
			continue
		}
		kept = append(kept, line)
	}
	return strings.Join(kept, "\n")
}

// droppedTables returns the tables an Up section drops, excluding scaffolding
// it created itself (the `_new` / `_old` / `_bak` tables a rebuild builds and
// tears down within the same migration).
func droppedTables(up string) []string {
	created := map[string]bool{}
	for _, m := range createTableRe.FindAllStringSubmatch(up, -1) {
		created[m[1]] = true
	}
	var out []string
	seen := map[string]bool{}
	for _, m := range dropTableRe.FindAllStringSubmatch(up, -1) {
		if created[m[1]] || seen[m[1]] {
			continue
		}
		seen[m[1]] = true
		out = append(out, m[1])
	}
	return out
}

// --- schema introspection ---------------------------------------------------

type incomingRef struct {
	child    string
	column   string
	onDelete string
}

// incomingForeignKeys maps each referenced table to the references pointing at
// it, read from the live schema. Self references are excluded: a table's
// reference to itself goes away with the table and cannot be orphaned by its
// own rebuild.
func incomingForeignKeys(t *testing.T, db *sql.DB) map[string][]incomingRef {
	t.Helper()
	out := map[string][]incomingRef{}
	for _, child := range userTables(t, db) {
		for _, ref := range tableForeignKeys(t, db, child) {
			out[ref.parent] = append(out[ref.parent], ref.incomingRef)
		}
	}
	return out
}

type parentedRef struct {
	incomingRef
	parent string
}

// tableForeignKeys reads one table's outgoing references. It is its own
// function so the rows handle can be deferred rather than closed by hand on
// each path out of the loop.
func tableForeignKeys(t *testing.T, db *sql.DB, child string) []parentedRef {
	t.Helper()
	rows, err := db.Query("PRAGMA foreign_key_list(" + child + ")")
	if err != nil {
		t.Fatalf("foreign_key_list(%s): %v", child, err)
	}
	defer func() { _ = rows.Close() }()

	var out []parentedRef
	for rows.Next() {
		rec := scanRowAsMap(t, rows)
		parent := fmt.Sprintf("%v", rec["table"])
		if parent == child {
			continue
		}
		out = append(out, parentedRef{
			incomingRef: incomingRef{
				child:    child,
				column:   fmt.Sprintf("%v", rec["from"]),
				onDelete: fmt.Sprintf("%v", rec["on_delete"]),
			},
			parent: parent,
		})
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate foreign_key_list(%s): %v", child, err)
	}
	return out
}

func userTables(t *testing.T, db *sql.DB) []string {
	t.Helper()
	rows, err := db.Query(`SELECT name FROM sqlite_master WHERE type='table' AND name NOT LIKE 'sqlite_%' ORDER BY name`)
	if err != nil {
		t.Fatalf("list tables: %v", err)
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatalf("scan table name: %v", err)
		}
		out = append(out, name)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate tables: %v", err)
	}
	return out
}

func scanRowAsMap(t *testing.T, rows *sql.Rows) map[string]any {
	t.Helper()
	cols, err := rows.Columns()
	if err != nil {
		t.Fatalf("columns: %v", err)
	}
	values := make([]any, len(cols))
	pointers := make([]any, len(cols))
	for i := range values {
		pointers[i] = &values[i]
	}
	if err := rows.Scan(pointers...); err != nil {
		t.Fatalf("scan: %v", err)
	}
	rec := make(map[string]any, len(cols))
	for i, c := range cols {
		rec[c] = values[i]
	}
	return rec
}

func formatChildren(refs []incomingRef) []string {
	out := make([]string, 0, len(refs))
	for _, r := range refs {
		out = append(out, fmt.Sprintf("%s.%s ON DELETE %s", r.child, r.column, r.onDelete))
	}
	sort.Strings(out)
	return out
}

// --- fixture seeding --------------------------------------------------------

type syntheticColumn struct {
	name     string
	declType string
	notNull  bool
	hasDflt  bool
	pk       int
	hidden   int
}

// seedSyntheticRow inserts one row into table, recursively seeding a row in
// every table it references first so its own foreign key columns point
// somewhere real. Rows are memoised per table, so a child seeded after its
// parent reuses the parent row this fixture already created.
//
// The caller must have relaxed foreign key and CHECK enforcement: the values
// are shaped only to satisfy NOT NULL and column type.
func seedSyntheticRow(t *testing.T, db *sql.DB, table string, seeded map[string]string, depth int) string {
	t.Helper()
	if pk, ok := seeded[table]; ok {
		return pk
	}
	if depth > 12 {
		t.Fatalf("foreign key chain deeper than 12 tables while seeding %s", table)
	}
	seeded[table] = "" // cycle guard, replaced with the real key below

	columns := syntheticColumns(t, db, table)
	fkByColumn := map[string]string{}
	for parent, refs := range incomingForeignKeys(t, db) {
		for _, r := range refs {
			if r.child == table {
				fkByColumn[r.column] = parent
			}
		}
	}

	var names, values []string
	pkValue := ""
	assignedIntegerPK := false
	for _, c := range columns {
		if c.hidden == 2 || c.hidden == 3 {
			continue // generated column: SQLite computes it
		}
		// An INTEGER PRIMARY KEY is the rowid, and some of these tables
		// already hold rows the CDC triggers wrote while earlier fixture rows
		// were inserted. Letting SQLite assign it is both collision-free and
		// closer to how the daemon writes.
		if c.pk == 1 && strings.Contains(strings.ToUpper(c.declType), "INT") {
			assignedIntegerPK = true
			continue
		}
		var value string
		switch parent, isFK := fkByColumn[c.name]; {
		case isFK:
			value = "'" + seedSyntheticRow(t, db, parent, seeded, depth+1) + "'"
		case c.pk > 0, c.notNull && !c.hasDflt:
			value = syntheticValue(table, c)
		default:
			continue // nullable or defaulted and not load-bearing here
		}
		names = append(names, `"`+c.name+`"`)
		values = append(values, value)
		if c.pk == 1 {
			pkValue = strings.Trim(value, "'")
		}
	}

	statement := fmt.Sprintf(`INSERT INTO "%s" (%s) VALUES (%s)`,
		table, strings.Join(names, ", "), strings.Join(values, ", "))
	result, err := db.Exec(statement)
	if err != nil {
		t.Fatalf("seed a synthetic %s row: %v\n%s", table, err, statement)
	}
	if assignedIntegerPK {
		rowID, err := result.LastInsertId()
		if err != nil {
			t.Fatalf("read the key SQLite assigned the synthetic %s row: %v", table, err)
		}
		pkValue = strconv.FormatInt(rowID, 10)
	}
	if pkValue == "" {
		t.Fatalf("could not determine the primary key of the synthetic %s row", table)
	}
	seeded[table] = pkValue
	return pkValue
}

func syntheticColumns(t *testing.T, db *sql.DB, table string) []syntheticColumn {
	t.Helper()
	rows, err := db.Query("PRAGMA table_xinfo(" + table + ")")
	if err != nil {
		t.Fatalf("table_xinfo(%s): %v", table, err)
	}
	defer func() { _ = rows.Close() }()
	var out []syntheticColumn
	for rows.Next() {
		var (
			cid     int
			c       syntheticColumn
			notNull int
			dflt    sql.NullString
		)
		if err := rows.Scan(&cid, &c.name, &c.declType, &notNull, &dflt, &c.pk, &c.hidden); err != nil {
			t.Fatalf("scan table_xinfo(%s): %v", table, err)
		}
		c.notNull = notNull != 0
		c.hasDflt = dflt.Valid
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate table_xinfo(%s): %v", table, err)
	}
	return out
}

func syntheticValue(table string, c syntheticColumn) string {
	switch declType := strings.ToUpper(c.declType); {
	case strings.Contains(declType, "INT"):
		return "1"
	case strings.Contains(declType, "REAL"), strings.Contains(declType, "FLOA"), strings.Contains(declType, "DOUBLE"):
		return "1.0"
	case strings.Contains(declType, "BLOB"):
		return "x'00'"
	case strings.Contains(declType, "TIME"), strings.Contains(declType, "DATE"):
		return "'2026-01-01 00:00:00'"
	default:
		return "'aofx-" + table + "-" + c.name + "'"
	}
}

func assertReferencesIntact(t *testing.T, db *sql.DB, refs []incomingRef, parentPK, when string) {
	t.Helper()
	for _, ref := range refs {
		var total, pointing int
		if err := db.QueryRow(fmt.Sprintf(`SELECT COUNT(*) FROM "%s"`, ref.child)).Scan(&total); err != nil {
			t.Fatalf("count %s %s: %v", ref.child, when, err)
		}
		if err := db.QueryRow(
			fmt.Sprintf(`SELECT COUNT(*) FROM "%s" WHERE "%s" = ?`, ref.child, ref.column), parentPK,
		).Scan(&pointing); err != nil {
			t.Fatalf("count %s.%s references %s: %v", ref.child, ref.column, when, err)
		}
		switch {
		case total == 0:
			t.Errorf("%s: every row of %s was deleted; the rebuild cascaded through %s.%s instead of preserving it",
				when, ref.child, ref.child, ref.column)
		case pointing == 0:
			t.Errorf("%s: %s.%s no longer points at the row it referenced; the rebuild dropped the reference",
				when, ref.child, ref.column)
		}
	}
	rows, err := db.Query("PRAGMA foreign_key_check")
	if err != nil {
		t.Fatalf("foreign_key_check %s: %v", when, err)
	}
	defer func() { _ = rows.Close() }()
	if rows.Next() {
		t.Errorf("%s: foreign_key_check reports a violation", when)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate foreign_key_check %s: %v", when, err)
	}
}

// --- goose plumbing ---------------------------------------------------------

func openRebuildScanDB(t *testing.T) *sql.DB {
	t.Helper()
	dsn := "file:" + filepath.Join(t.TempDir(), "ao.db") + "?_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	// One connection, so the pragmas the fixture sets stay in force.
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func execPragmas(t *testing.T, db *sql.DB, pragmas ...string) {
	t.Helper()
	for _, p := range pragmas {
		if _, err := db.Exec(p); err != nil {
			t.Fatalf("%s: %v", p, err)
		}
	}
}

func applyMigration(t *testing.T, db *sql.DB, version int64) {
	t.Helper()
	if err := migrateTo(db, version); err != nil {
		t.Fatalf("migrate to %d: %v", version, err)
	}
}

func migrateTo(db *sql.DB, version int64) error {
	gooseMu.Lock()
	defer gooseMu.Unlock()
	goose.SetBaseFS(migrationsFS)
	goose.SetLogger(goose.NopLogger())
	if err := goose.SetDialect("sqlite3"); err != nil {
		return err
	}
	return goose.UpTo(db, "migrations", version)
}

func migrateDownTo(db *sql.DB, version int64) error {
	gooseMu.Lock()
	defer gooseMu.Unlock()
	goose.SetBaseFS(migrationsFS)
	goose.SetLogger(goose.NopLogger())
	if err := goose.SetDialect("sqlite3"); err != nil {
		return err
	}
	return goose.DownTo(db, "migrations", version)
}

func rebuildKey(version int64, table string) string {
	return fmt.Sprintf("%04d/%s", version, table)
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
