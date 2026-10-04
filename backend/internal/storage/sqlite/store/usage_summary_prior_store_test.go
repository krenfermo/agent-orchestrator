package store_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite"
)

// usage_summary_prior_store_test.go -- P7.2B2.1: the candidate list for the
// cross-session summary prior, against a real schema.
//
// The query returns ONE column on purpose, so what it must get right is which
// sessions qualify: a session that compacted AND carries a harness rollup, once,
// however many sources say so -- and nothing whose parser state is unreadable.

func TestListCompactedSessionsForSummaryPriorSelectsOnlyCompleteEvidence(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	now := time.Unix(1700000000, 0).UTC()

	const rollup = `"harness_totals":{"models":[{"model_id":"claude-opus-5[1m]","output_tokens":47453}]}`
	source := func(sess domain.SessionRecord, root, path, state string) {
		t.Helper()
		binding := mustUpsertUsageBinding(t, s, sess, now, domain.UsageBindingRecord{
			NativeRootID: root, InitialModelID: "claude-opus-5", State: domain.UsageBindingActive,
		})
		src := mustInsertUsageSource(t, s, now, domain.UsageSourceRecord{
			BindingID: binding.ID, Kind: domain.UsageSourceClaudeMain,
			ArtifactPath: path, State: domain.UsageSourceActive,
		})
		stmt := fmt.Sprintf(`UPDATE usage_sources SET parser_state_json = '%s' WHERE id = %d`, state, src.ID)
		if err := s.ExecForCacheTTLMaintenanceTest(ctx, stmt); err != nil {
			t.Fatalf("set parser state: %v", err)
		}
	}

	complete := seedUsageSession(t, s, domain.HarnessClaudeCode)
	source(complete, "root-complete", "/t/complete.jsonl", `{"version":1,"claude":{"compaction_count":2,`+rollup+`}}`)
	// A second qualifying source for the SAME session: still one candidate.
	source(complete, "root-complete-2", "/t/complete-2.jsonl", `{"version":1,"claude":{"compaction_count":1,`+rollup+`}}`)

	noRollup := seedUsageSession(t, s, domain.HarnessClaudeCode)
	source(noRollup, "root-no-rollup", "/t/no-rollup.jsonl", `{"version":1,"claude":{"compaction_count":1}}`)

	neverCompacted := seedUsageSession(t, s, domain.HarnessClaudeCode)
	source(neverCompacted, "root-never", "/t/never.jsonl", `{"version":1,"claude":{`+rollup+`}}`)

	unreadable := seedUsageSession(t, s, domain.HarnessClaudeCode)
	source(unreadable, "root-bad", "/t/bad.jsonl", `{not json`)

	got, err := s.ListCompactedSessionsForSummaryPrior(ctx, 64)
	if err != nil {
		t.Fatalf("ListCompactedSessionsForSummaryPrior: %v", err)
	}
	if len(got) != 1 || got[0] != complete.ID {
		t.Fatalf("candidates = %v, want exactly [%s]", got, complete.ID)
	}
}

func TestListCompactedSessionsForSummaryPriorHonoursItsLimit(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	now := time.Unix(1700000000, 0).UTC()
	state := `{"version":1,"claude":{"compaction_count":1,"harness_totals":{"models":[{"model_id":"m","output_tokens":1}]}}}`

	var ids []domain.SessionID
	for i := 0; i < 3; i++ {
		sess := seedUsageSession(t, s, domain.HarnessClaudeCode)
		binding := mustUpsertUsageBinding(t, s, sess, now, domain.UsageBindingRecord{
			NativeRootID: fmt.Sprintf("root-%d", i), State: domain.UsageBindingActive,
		})
		src := mustInsertUsageSource(t, s, now, domain.UsageSourceRecord{
			BindingID: binding.ID, Kind: domain.UsageSourceClaudeMain,
			ArtifactPath: fmt.Sprintf("/t/%d.jsonl", i), State: domain.UsageSourceActive,
		})
		mustSetParserState(t, s, src.ID, state)
		ids = append(ids, sess.ID)
	}

	all, err := s.ListCompactedSessionsForSummaryPrior(ctx, 10)
	if err != nil || len(all) != 3 {
		t.Fatalf("all = %v / %v, want 3", all, err)
	}
	for i := 1; i < len(all); i++ {
		if all[i-1] >= all[i] {
			t.Errorf("candidates not in deterministic session order: %v", all)
		}
	}
	two, err := s.ListCompactedSessionsForSummaryPrior(ctx, 2)
	if err != nil || len(two) != 2 {
		t.Fatalf("limited = %v / %v, want 2", two, err)
	}
	none, err := s.ListCompactedSessionsForSummaryPrior(ctx, 0)
	if err != nil || none != nil {
		t.Errorf("limit 0 = %v / %v, want nil / nil", none, err)
	}
	_ = ids
}

func mustSetParserState(t *testing.T, s *sqlite.Store, sourceID int64, state string) {
	t.Helper()
	stmt := fmt.Sprintf(`UPDATE usage_sources SET parser_state_json = '%s' WHERE id = %d`, state, sourceID)
	if err := s.ExecForCacheTTLMaintenanceTest(context.Background(), stmt); err != nil {
		t.Fatalf("set parser state: %v", err)
	}
}
