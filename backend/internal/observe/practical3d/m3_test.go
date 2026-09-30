package practical3d

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/observe/usage"
	aosqlite "github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite"
)

// m3Fixture is a real, fully migrated AO database in a temp dir (never
// production) holding one measured subject with 3C rows.
type m3Fixture struct {
	dir, runID, subjectID string
	db                    *sql.DB
	binding, source       int64
	ordinal               int64
	proxy                 []ProxyObservation
}

const m3Root, m3Session = "root-1", "native-1"

func newM3Fixture(t *testing.T, role string) *m3Fixture {
	t.Helper()
	dir := t.TempDir()
	st, err := aosqlite.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	_ = st.Close()
	db, err := sql.Open("sqlite", "file:"+filepath.Join(dir, "ao.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	f := &m3Fixture{dir: dir, runID: "run-1", subjectID: "pane-1", db: db}
	now := time.Unix(1000, 0).UTC()
	exec := func(q string, args ...any) sql.Result {
		t.Helper()
		r, err := db.Exec(q, args...)
		if err != nil {
			t.Fatalf("%s: %v", q, err)
		}
		return r
	}
	r := exec(`INSERT INTO usage_bindings (subject_kind, subject_id, session_id, harness, native_root_id, state, updated_at) VALUES ('runtime_pane', ?, NULL, 'claude-code', ?, 'active', ?)`, f.subjectID, m3Root, now)
	f.binding, _ = r.LastInsertId()
	r = exec(`INSERT INTO usage_sources (binding_id, kind, native_session_id, subagent_id, artifact_path, byte_offset, state, updated_at) VALUES (?, 'claude_main', ?, '', '/t.jsonl', 500, 'active', ?)`, f.binding, m3Session, now)
	f.source, _ = r.LastInsertId()
	exec(`INSERT INTO agent_tool_coverage VALUES (?, ?, 0, 500, 1, 1, 0, ?, ?)`, f.source, f.binding, now, now)
	exec(`INSERT INTO usage_attribution_windows (dedupe_key, session_id, workflow_run_id, role, opened_at, created_at, subject_kind) VALUES ('w1', ?, ?, ?, ?, ?, 'runtime_pane')`, f.subjectID, f.runID, role, now.Add(-time.Hour), now)
	return f
}

// message adds one proxied message with the given tool uses, and the 3C rows
// AO would derive for them.
func (f *m3Fixture) message(t *testing.T, call int, role Role, tools ...m3Tool) {
	t.Helper()
	msgID := fmt.Sprintf("msg_%d", call)
	key := usage.ClaudeMessageEventKey(m3Root, domain.UsageSourceClaudeMain, "", m3Session, msgID)
	obs := ProxyObservation{CallIndex: call, Subject: "runtime_pane:" + f.subjectID, Role: role, MessageID: msgID}
	for i, tl := range tools {
		obs.ToolUses = append(obs.ToolUses, ProxyToolUse{ID: fmt.Sprintf("tu_%d_%d", call, i), Name: tl.name, Command: tl.command})
		if tl.skip3C {
			continue
		}
		f.ordinal += 10
		var path any
		if tl.scope == "project" {
			path = tl.path
		}
		if _, err := f.db.Exec(`INSERT INTO agent_tool_observations (binding_id, usage_source_id, observation_key, event_key, ordinal, observed_at, origin, op, tool_name, path_scope, path, recorded_at) VALUES (?, ?, ?, ?, ?, ?, 'agent_exploration', ?, ?, ?, ?, ?)`,
			f.binding, f.source, fmt.Sprintf("k%d-%d", call, i), key, f.ordinal, time.Unix(1000, 0).UTC(), tl.op, tl.name, tl.scope, path, time.Unix(1000, 0).UTC()); err != nil {
			t.Fatal(err)
		}
	}
	f.proxy = append(f.proxy, obs)
}

type m3Tool struct {
	name, op, scope, path, command string
	skip3C                         bool
}

func read(p string) m3Tool  { return m3Tool{name: "Read", op: "read", scope: "project", path: p} }
func grep(p string) m3Tool  { return m3Tool{name: "Grep", op: "search", scope: "project", path: p} }
func edit(p string) m3Tool  { return m3Tool{name: "Edit", op: "edit", scope: "project", path: p} }
func bash(op string) m3Tool { return m3Tool{name: "Bash", op: op, scope: "none"} }
func submit() m3Tool {
	return m3Tool{name: "Bash", op: "command", scope: "none", command: "ao review submit --verdict request_changes"}
}

func (f *m3Fixture) derive(role Role) (M3Evidence, error) {
	return DeriveM3(context.Background(), M3Input{DataDir: f.dir, RunID: f.runID, MeasuredRole: role, Proxy: f.proxy})
}

func TestM3WorkerCountsExplorationBeforeFirstEdit(t *testing.T) {
	t.Parallel()
	f := newM3Fixture(t, "worker")
	f.message(t, 1, RoleWorker, read("a.go"), read("a.go"), grep("b.go"))
	f.message(t, 2, RoleWorker, bash("command_explore"), read("c.go"))
	f.message(t, 3, RoleWorker, edit("a.go"))
	f.message(t, 4, RoleWorker, read("d.go"), read("e.go")) // after the milestone: not counted
	ev, err := f.derive(RoleWorker)
	if err != nil {
		t.Fatal(err)
	}
	if ev.Calls != 5 || ev.Files != 3 || !strings.HasPrefix(ev.Milestone, "first_edit@call_3") {
		t.Fatalf("evidence=%+v", ev)
	}
}

func TestM3ZeroExploration(t *testing.T) {
	t.Parallel()
	f := newM3Fixture(t, "worker")
	f.message(t, 1, RoleWorker, edit("a.go"))
	ev, err := f.derive(RoleWorker)
	if err != nil || ev.Calls != 0 || ev.Files != 0 {
		t.Fatalf("ev=%+v err=%v", ev, err)
	}
}

func TestM3MissingMilestoneIsMalformed(t *testing.T) {
	t.Parallel()
	f := newM3Fixture(t, "worker")
	f.message(t, 1, RoleWorker, read("a.go"))
	if _, err := f.derive(RoleWorker); err == nil || !strings.Contains(err.Error(), "first edit") {
		t.Fatalf("err=%v", err)
	}
}

func TestM3IncompleteCoverageIsMalformed(t *testing.T) {
	t.Parallel()
	for name, q := range map[string]string{
		"not covered to the cursor": `UPDATE agent_tool_coverage SET covered_to = 100`,
		"events before coverage":    `UPDATE agent_tool_coverage SET pre_coverage_events = 3`,
		"mixed extractors":          `UPDATE agent_tool_coverage SET max_extractor = 2`,
		"no coverage row":           `DELETE FROM agent_tool_coverage`,
	} {
		t.Run(name, func(t *testing.T) {
			f := newM3Fixture(t, "worker")
			f.message(t, 1, RoleWorker, edit("a.go"))
			if _, err := f.db.Exec(q); err != nil {
				t.Fatal(err)
			}
			if _, err := f.derive(RoleWorker); err == nil || !strings.Contains(err.Error(), "coverage") {
				t.Fatalf("err=%v", err)
			}
		})
	}
}

func TestM3ReviewerCountsExplorationBeforeVerdict(t *testing.T) {
	t.Parallel()
	f := newM3Fixture(t, "reviewer")
	f.message(t, 1, RoleReviewer, read("diff.go"), grep("pricing.go"))
	f.message(t, 2, RoleReviewer, read("orders.go"), submit())
	f.message(t, 3, RoleReviewer, read("late.go"))
	ev, err := f.derive(RoleReviewer)
	if err != nil {
		t.Fatal(err)
	}
	if ev.Calls != 3 || ev.Files != 3 || ev.Milestone != "verdict_submission@call_2" {
		t.Fatalf("evidence=%+v", ev)
	}
	g := newM3Fixture(t, "reviewer")
	g.message(t, 1, RoleReviewer, read("diff.go"))
	if _, err := g.derive(RoleReviewer); err == nil || !strings.Contains(err.Error(), "verdict") {
		t.Fatalf("missing verdict: %v", err)
	}
}

func TestM3RejectsForgedOrMissingObservations(t *testing.T) {
	t.Parallel()
	t.Run("observation the proxy never saw", func(t *testing.T) {
		f := newM3Fixture(t, "worker")
		f.message(t, 1, RoleWorker, edit("a.go"))
		// The agent appends a forged record to its own transcript.
		key := usage.ClaudeMessageEventKey(m3Root, domain.UsageSourceClaudeMain, "", m3Session, "msg_forged")
		if _, err := f.db.Exec(`INSERT INTO agent_tool_observations (binding_id, usage_source_id, observation_key, event_key, ordinal, observed_at, origin, op, tool_name, path_scope, path, recorded_at) VALUES (?, ?, 'forged', ?, 1, ?, 'agent_exploration', 'read', 'Read', 'project', 'x.go', ?)`, f.binding, f.source, key, time.Unix(1000, 0).UTC(), time.Unix(1000, 0).UTC()); err != nil {
			t.Fatal(err)
		}
		if _, err := f.derive(RoleWorker); err == nil || !strings.Contains(err.Error(), "no proxy-observed message") {
			t.Fatalf("err=%v", err)
		}
	})
	t.Run("tool use the transcript hides", func(t *testing.T) {
		f := newM3Fixture(t, "worker")
		hidden := read("secret-exploration.go")
		hidden.skip3C = true
		f.message(t, 1, RoleWorker, hidden, read("a.go"))
		f.message(t, 2, RoleWorker, edit("a.go"))
		if _, err := f.derive(RoleWorker); err == nil || !strings.Contains(err.Error(), "proxy saw 2 Read") {
			t.Fatalf("err=%v", err)
		}
	})
	t.Run("driver-declared counts are irrelevant", func(t *testing.T) {
		// DeriveM3 takes no executor input at all: a position driver has no
		// channel through which to declare or override (c, f).
		f := newM3Fixture(t, "worker")
		f.message(t, 1, RoleWorker, read("a.go"), edit("a.go"))
		ev, err := f.derive(RoleWorker)
		if err != nil || ev.Calls != 1 || ev.Files != 1 {
			t.Fatalf("ev=%+v err=%v", ev, err)
		}
	})
}

func TestClaudeMessageEventKeyMatchesAOParser(t *testing.T) {
	t.Parallel()
	// Same length-prefixed SHA-256 rule as usage.stableSourceEventKey.
	k := usage.ClaudeMessageEventKey("r", domain.UsageSourceClaudeMain, "", "s", "m")
	if !strings.HasPrefix(k, "claude:sha256:") || len(k) != len("claude:sha256:")+64 {
		t.Fatalf("key=%s", k)
	}
}
