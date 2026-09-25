package sqlite

import (
	"testing"
)

// Migration 0175 (Frente 3 / 3C) is purely additive: a new table, two indexes
// and a view. Existing usage rows are untouched on the way up, and the Down
// removes exactly what the Up added -- so a rollback leaves the ledger as it
// was.

func TestMigration0175IsAdditiveAndReversible(t *testing.T) {
	db := openMigrationDB(t)
	upTo(t, db, 174)
	id := seedUsageEvent(t, db, "pre-3c", 100)

	upTo(t, db, 175)
	for _, name := range []string{"agent_tool_observations", "agent_tool_observation_attribution",
		"idx_agent_tool_observations_binding_order", "idx_agent_tool_observations_source"} {
		var n int
		if err := db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE name = ?`, name).Scan(&n); err != nil || n != 1 {
			t.Fatalf("%s after up: count=%d err=%v", name, n, err)
		}
	}
	var events int
	if err := db.QueryRow(`SELECT COUNT(*) FROM model_usage_events WHERE id = ?`, id).Scan(&events); err != nil || events != 1 {
		t.Fatalf("existing usage row disturbed: count=%d err=%v", events, err)
	}
	// Nothing is backfilled.
	var observations int
	if err := db.QueryRow(`SELECT COUNT(*) FROM agent_tool_observations`).Scan(&observations); err != nil || observations != 0 {
		t.Fatalf("0175 must backfill nothing: count=%d err=%v", observations, err)
	}
	// The unique key is exactly-once per binding.
	insert := `INSERT INTO agent_tool_observations (binding_id, observation_key, ordinal, origin, op, path_scope, recorded_at)
		VALUES (1, 'k1', 0, 'agent_exploration', 'read', 'none', CURRENT_TIMESTAMP)`
	if _, err := db.Exec(insert); err != nil {
		t.Fatalf("insert: %v", err)
	}
	if _, err := db.Exec(insert); err == nil {
		t.Fatal("a duplicate observation key in one binding must be refused")
	}
	// Deleting the binding cascades, so the new table never holds an orphan.
	if _, err := db.Exec(`DELETE FROM usage_bindings WHERE id = 1`); err != nil {
		t.Fatalf("delete binding: %v", err)
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM agent_tool_observations`).Scan(&observations); err != nil || observations != 0 {
		t.Fatalf("observations must cascade with their binding: count=%d err=%v", observations, err)
	}

	if err := downTo(t, db, 174); err != nil {
		t.Fatalf("down: %v", err)
	}
	for _, name := range []string{"agent_tool_observations", "agent_tool_observation_attribution",
		"idx_agent_tool_observations_binding_order", "idx_agent_tool_observations_source"} {
		var n int
		if err := db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE name = ?`, name).Scan(&n); err != nil || n != 0 {
			t.Fatalf("%s after down: count=%d err=%v", name, n, err)
		}
	}
}
