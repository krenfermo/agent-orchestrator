package sqlite

import (
	"database/sql"
	"testing"
)

// Migration 0170 adds the cache-creation lifetime to model_usage_events.
//
// The whole point of the column pair is a distinction a DEFAULT would have
// destroyed: NULL means "the lifetime of these writes was never observed", and
// 0 would mean "no short-lived cache was created". A row written before this
// migration has to say the first thing. If it said the second, every legacy row
// would assert that its cache creation was short-lived -- which is the exact
// claim, on the exact tokens, that this whole checkpoint exists to stop AO
// making, and it would be wrong on 97.9% of the corpus.

// seedUsageEvent writes one pre-0170 event and returns its id.
func seedUsageEvent(t *testing.T, db *sql.DB, key string, write int64) int64 {
	t.Helper()
	if _, err := db.Exec(`INSERT OR IGNORE INTO projects (id, path, registered_at) VALUES ('p1', '/tmp/p1', CURRENT_TIMESTAMP)`); err != nil {
		t.Fatalf("seed project: %v", err)
	}
	if _, err := db.Exec(`INSERT OR IGNORE INTO sessions (id, project_id, num, kind, activity_state, activity_last_at, is_terminated, created_at, updated_at)
		VALUES ('s1', 'p1', 1, 'worker', 'active', CURRENT_TIMESTAMP, 0, CURRENT_TIMESTAMP, CURRENT_TIMESTAMP)`); err != nil {
		t.Fatalf("seed session: %v", err)
	}
	var sessions int
	if err := db.QueryRow(`SELECT COUNT(*) FROM sessions WHERE id = 's1'`).Scan(&sessions); err != nil || sessions != 1 {
		t.Fatalf("session seed did not land (count=%d, err=%v)", sessions, err)
	}
	if _, err := db.Exec(`INSERT OR IGNORE INTO usage_bindings (id, subject_kind, subject_id, session_id, harness, native_root_id, state, updated_at)
		VALUES (1, 'session', 's1', 's1', 'claude-code', 'root-1', 'active', CURRENT_TIMESTAMP)`); err != nil {
		t.Fatalf("seed binding: %v", err)
	}
	res, err := db.Exec(`INSERT INTO model_usage_events
		(binding_id, model_id, input_tokens, uncached_input_tokens, cache_read_tokens, cache_write_tokens, output_tokens, source_event_key, recorded_at)
		VALUES (1, 'claude-opus-5', ?, 0, 0, ?, 10, ?, CURRENT_TIMESTAMP)`, write, write, key)
	if err != nil {
		t.Fatalf("seed event: %v", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		t.Fatalf("last id: %v", err)
	}
	return id
}

func TestMigration0170LeavesHistoricalRowsNull(t *testing.T) {
	db := openMigrationDB(t)
	upTo(t, db, 169)
	id := seedUsageEvent(t, db, "legacy-1", 2193)

	upTo(t, db, 170)

	var five, hour sql.NullInt64
	var total int64
	if err := db.QueryRow(`SELECT cache_write_5m_tokens, cache_write_1h_tokens, cache_write_tokens
		FROM model_usage_events WHERE id = ?`, id).Scan(&five, &hour, &total); err != nil {
		t.Fatalf("read back: %v", err)
	}
	if five.Valid || hour.Valid {
		t.Fatalf("a pre-0170 row must stay NULL, got 5m=%v 1h=%v", five, hour)
	}
	if total != 2193 {
		t.Fatalf("the total must be untouched, got %d", total)
	}
	// And NULL must not be reachable as a zero by anything that sums it.
	var summed sql.NullInt64
	if err := db.QueryRow(`SELECT SUM(cache_write_5m_tokens) FROM model_usage_events`).Scan(&summed); err != nil {
		t.Fatalf("sum: %v", err)
	}
	if summed.Valid {
		t.Fatalf("summing only NULLs must stay NULL, got %v", summed)
	}
}

func TestMigration0170IsStructuralOnly(t *testing.T) {
	db := openMigrationDB(t)
	upTo(t, db, 169)
	id := seedUsageEvent(t, db, "untouched-1", 5240)

	var before struct {
		model           string
		in, read, write int64
		out             int64
		key             string
	}
	row := db.QueryRow(`SELECT model_id, input_tokens, cache_read_tokens, cache_write_tokens, output_tokens, source_event_key
		FROM model_usage_events WHERE id = ?`, id)
	if err := row.Scan(&before.model, &before.in, &before.read, &before.write, &before.out, &before.key); err != nil {
		t.Fatalf("before: %v", err)
	}

	upTo(t, db, 170)

	var after = before
	row = db.QueryRow(`SELECT model_id, input_tokens, cache_read_tokens, cache_write_tokens, output_tokens, source_event_key
		FROM model_usage_events WHERE id = ?`, id)
	if err := row.Scan(&after.model, &after.in, &after.read, &after.write, &after.out, &after.key); err != nil {
		t.Fatalf("after: %v", err)
	}
	if after != before {
		t.Fatalf("the migration rewrote data: %+v -> %+v", before, after)
	}
}

func TestMigration0170KeepsTheDatabaseWhole(t *testing.T) {
	db := openMigrationDB(t)
	upTo(t, db, 169)
	seedUsageEvent(t, db, "integrity-1", 900)
	upTo(t, db, 170)

	var integrity string
	if err := db.QueryRow(`PRAGMA integrity_check`).Scan(&integrity); err != nil {
		t.Fatalf("integrity_check: %v", err)
	}
	if integrity != "ok" {
		t.Fatalf("integrity_check = %q", integrity)
	}
	rows, err := db.Query(`PRAGMA foreign_key_check`)
	if err != nil {
		t.Fatalf("foreign_key_check: %v", err)
	}
	defer rows.Close()
	if rows.Next() {
		t.Fatal("foreign_key_check reported a violation")
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("foreign_key_check rows: %v", err)
	}
	// The view must have been rebuilt with the new columns, or every ledger
	// read would still be blind to them.
	var five, hour sql.NullInt64
	if err := db.QueryRow(`SELECT cache_write_5m_tokens, cache_write_1h_tokens
		FROM usage_event_attribution LIMIT 1`).Scan(&five, &hour); err != nil {
		t.Fatalf("view does not project the lifetime: %v", err)
	}
}

func TestMigration0170StoresAndReadsBackEveryLifetimeShape(t *testing.T) {
	db := openMigrationDB(t)
	upTo(t, db, 170)
	seedUsageEvent(t, db, "seed", 0)

	for _, tc := range []struct {
		name             string
		five, hour       sql.NullInt64
		write            int64
		wantFive, wantHr sql.NullInt64
	}{
		{"short lived", sql.NullInt64{Int64: 900, Valid: true}, sql.NullInt64{Valid: true}, 900,
			sql.NullInt64{Int64: 900, Valid: true}, sql.NullInt64{Valid: true}},
		{"long lived", sql.NullInt64{Valid: true}, sql.NullInt64{Int64: 2326, Valid: true}, 2326,
			sql.NullInt64{Valid: true}, sql.NullInt64{Int64: 2326, Valid: true}},
		{"both", sql.NullInt64{Int64: 400, Valid: true}, sql.NullInt64{Int64: 600, Valid: true}, 1000,
			sql.NullInt64{Int64: 400, Valid: true}, sql.NullInt64{Int64: 600, Valid: true}},
		{"lifetime not observed", sql.NullInt64{}, sql.NullInt64{}, 5240, sql.NullInt64{}, sql.NullInt64{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := db.Exec(`INSERT INTO model_usage_events
				(binding_id, model_id, input_tokens, uncached_input_tokens, cache_read_tokens,
				 cache_write_tokens, output_tokens, source_event_key, recorded_at,
				 cache_write_5m_tokens, cache_write_1h_tokens)
				VALUES (1, 'claude-opus-5', ?, 0, 0, ?, 5, ?, CURRENT_TIMESTAMP, ?, ?)`,
				tc.write, tc.write, tc.name, tc.five, tc.hour); err != nil {
				t.Fatalf("insert: %v", err)
			}
			var five, hour sql.NullInt64
			if err := db.QueryRow(`SELECT cache_write_5m_tokens, cache_write_1h_tokens
				FROM model_usage_events WHERE source_event_key = ?`, tc.name).Scan(&five, &hour); err != nil {
				t.Fatalf("read back: %v", err)
			}
			if five != tc.wantFive || hour != tc.wantHr {
				t.Fatalf("got 5m=%v 1h=%v, want 5m=%v 1h=%v", five, hour, tc.wantFive, tc.wantHr)
			}
		})
	}

	// The aggregate's third column is the quantity whose lifetime was never
	// observed -- and it must count the legacy row's 5,240 and nothing else.
	var sum5, sum1h, unknown int64
	if err := db.QueryRow(`
		SELECT COALESCE(SUM(cache_write_5m_tokens), 0),
		       COALESCE(SUM(cache_write_1h_tokens), 0),
		       COALESCE(SUM(CASE WHEN cache_write_5m_tokens IS NULL THEN cache_write_tokens ELSE 0 END), 0)
		FROM model_usage_events`).Scan(&sum5, &sum1h, &unknown); err != nil {
		t.Fatalf("aggregate: %v", err)
	}
	if sum5 != 1300 || sum1h != 2926 || unknown != 5240 {
		t.Fatalf("aggregate = 5m %d / 1h %d / unknown %d, want 1300 / 2926 / 5240", sum5, sum1h, unknown)
	}
}

func TestMigration0170RollsBack(t *testing.T) {
	db := openMigrationDB(t)
	upTo(t, db, 170)
	id := seedUsageEvent(t, db, "rollback-1", 900)
	if _, err := db.Exec(`UPDATE model_usage_events SET cache_write_1h_tokens = 900, cache_write_5m_tokens = 0 WHERE id = ?`, id); err != nil {
		t.Fatalf("populate: %v", err)
	}

	if err := downTo(t, db, 169); err != nil {
		t.Fatalf("down: %v", err)
	}

	var col string
	err := db.QueryRow(`SELECT name FROM pragma_table_info('model_usage_events') WHERE name = 'cache_write_1h_tokens'`).Scan(&col)
	if err == nil {
		t.Fatal("the column survived the down migration")
	}
	// The row and its total are still there, and the view is back to its 0169
	// shape -- a rollback loses the lifetime, never the spend.
	var write int64
	if err := db.QueryRow(`SELECT cache_write_tokens FROM model_usage_events WHERE id = ?`, id).Scan(&write); err != nil {
		t.Fatalf("row lost on rollback: %v", err)
	}
	if write != 900 {
		t.Fatalf("cache_write_tokens = %d, want 900", write)
	}
	var one int64
	if err := db.QueryRow(`SELECT COUNT(*) FROM usage_event_attribution`).Scan(&one); err != nil {
		t.Fatalf("view not restored: %v", err)
	}
}

// TestMigration0170LifetimeTermsPartitionTheTotal pins the property the
// aggregates depend on and that the integration review found was not yet true.
//
// The three figures every aggregate returns -- 5m, 1h, and lifetime-unknown --
// must PARTITION cache_write_tokens: every token in exactly one of them, for
// every shape a row can be in. The half-written shapes below (one column set,
// the other NULL) are unreachable through the write path, which writes both or
// neither. They are exactly what a backfill could produce, and the first
// version of these queries both overstated the known lifetime and double
// counted the row against the unknown bucket.
func TestMigration0170LifetimeTermsPartitionTheTotal(t *testing.T) {
	db := openMigrationDB(t)
	upTo(t, db, 170)
	seedUsageEvent(t, db, "seed-legacy", 1000) // pre-0170 shape: NULL pair

	ins := func(key string, total int64, five, hour sql.NullInt64) {
		if _, err := db.Exec(`INSERT INTO model_usage_events
			(binding_id, model_id, input_tokens, uncached_input_tokens, cache_read_tokens,
			 cache_write_tokens, output_tokens, source_event_key, recorded_at,
			 cache_write_5m_tokens, cache_write_1h_tokens)
			VALUES (1,'claude-opus-5',?,0,0,?,1,?,CURRENT_TIMESTAMP,?,?)`,
			total, total, key, five, hour); err != nil {
			t.Fatalf("insert %s: %v", key, err)
		}
	}
	n := func(v int64) sql.NullInt64 { return sql.NullInt64{Int64: v, Valid: true} }
	ins("a-1h", 2193, n(0), n(2193))
	ins("b-5m", 900, n(900), n(0))
	ins("c-mix", 1000, n(400), n(600))
	ins("d-zero-write", 0, n(0), n(0))
	ins("e-null", 5240, sql.NullInt64{}, sql.NullInt64{})
	// Adversarial: one column set and the other NULL (only reachable by a
	// hand-written UPDATE or a future backfill bug).
	ins("f-half", 700, n(700), sql.NullInt64{})
	ins("g-half2", 300, sql.NullInt64{}, n(300))

	var total, sum5, sum1h, unknown int64
	if err := db.QueryRow(`
		SELECT COALESCE(SUM(cache_write_tokens),0),
		       COALESCE(SUM(CASE WHEN cache_write_5m_tokens IS NOT NULL AND cache_write_1h_tokens IS NOT NULL
		                         THEN cache_write_5m_tokens ELSE 0 END),0),
		       COALESCE(SUM(CASE WHEN cache_write_5m_tokens IS NOT NULL AND cache_write_1h_tokens IS NOT NULL
		                         THEN cache_write_1h_tokens ELSE 0 END),0),
		       COALESCE(SUM(CASE WHEN cache_write_5m_tokens IS NULL
		                           OR cache_write_1h_tokens IS NULL
		                         THEN cache_write_tokens ELSE 0 END),0)
		FROM model_usage_events`).Scan(&total, &sum5, &sum1h, &unknown); err != nil {
		t.Fatalf("aggregate: %v", err)
	}
	t.Logf("total=%d  5m=%d  1h=%d  unknown=%d  sum-of-parts=%d", total, sum5, sum1h, unknown, sum5+sum1h+unknown)
	if sum5+sum1h+unknown != total {
		t.Errorf("INVARIANT BROKEN: parts %d != total %d", sum5+sum1h+unknown, total)
	}
	// The view projects the raw columns -- guarding belongs in the aggregate,
	// not the projection -- so it is compared with the same guarded expression.
	var vt, v5, v1, vu int64
	if err := db.QueryRow(`
		SELECT COALESCE(SUM(cache_write_tokens),0),
		       COALESCE(SUM(CASE WHEN cache_write_5m_tokens IS NOT NULL AND cache_write_1h_tokens IS NOT NULL
		                         THEN cache_write_5m_tokens ELSE 0 END),0),
		       COALESCE(SUM(CASE WHEN cache_write_5m_tokens IS NOT NULL AND cache_write_1h_tokens IS NOT NULL
		                         THEN cache_write_1h_tokens ELSE 0 END),0),
		       COALESCE(SUM(CASE WHEN cache_write_5m_tokens IS NULL OR cache_write_1h_tokens IS NULL
		                         THEN cache_write_tokens ELSE 0 END),0)
		FROM usage_event_attribution`).Scan(&vt, &v5, &v1, &vu); err != nil {
		t.Fatalf("view: %v", err)
	}
	if vt != total || v5 != sum5 || v1 != sum1h || vu != unknown {
		t.Errorf("view disagrees with the table: %d/%d/%d/%d vs %d/%d/%d/%d",
			vt, v5, v1, vu, total, sum5, sum1h, unknown)
	}
}
