package usage

import (
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	usagesvc "github.com/aoagents/agent-orchestrator/backend/internal/service/usage"
)

// exploration_test.go -- Frente 3 / 3C: tool observations from transcripts.
// The fixtures below mirror the shapes of real Claude Code and Codex
// transcripts (field names verified against transcripts on disk); the
// sentinels are strings that must NEVER reach an observation.

const (
	sentinelContent = "SENTINEL-FILE-CONTENT-do-not-store"
	sentinelCommand = "SENTINEL-COMMAND-do-not-store"
	sentinelPattern = "SENTINEL-PATTERN-do-not-store"
	sentinelPrompt  = "SENTINEL-PROMPT-do-not-store"
	sentinelSecret  = "AKIAIOSFODNN7EXAMPLE"
)

func TestExplorationScopeClassifiesPaths(t *testing.T) {
	root := t.TempDir()
	harness := filepath.Join(t.TempDir(), "claude-home")
	scope := newExplorationScope(root, filepath.Join(harness, "projects", "slug", "s.jsonl"), domain.UsageSourceClaudeMain)
	resolvedRoot, _ := filepath.EvalSymlinks(root)
	tests := []struct {
		name      string
		raw, base string
		scope     domain.ToolPathScope
		path      string
	}{
		{"absolute inside", filepath.Join(root, "src", "a.go"), "", domain.ToolPathProject, "src/a.go"},
		{"resolved spelling of the root", filepath.Join(resolvedRoot, "src", "a.go"), "", domain.ToolPathProject, "src/a.go"},
		{"root itself", root, "", domain.ToolPathProject, "."},
		{"relative with trusted base", "pkg/b.go", filepath.Join(root, "sub"), domain.ToolPathProject, "sub/pkg/b.go"},
		{"relative without base", "pkg/b.go", "", domain.ToolPathUnresolved, ""},
		{"dot-dot normalised inside", filepath.Join(root, "x", "..", "y.go"), "", domain.ToolPathProject, "y.go"},
		{"traversal escapes", "../../etc/passwd", root, domain.ToolPathOutside, ""},
		{"absolute outside", "/etc/hosts", "", domain.ToolPathOutside, ""},
		{"harness home", filepath.Join(harness, "settings.json"), "", domain.ToolPathOutsideHarness, ""},
		{"home relative", "~/.aws/credentials", "", domain.ToolPathOutside, ""},
		{"env file", filepath.Join(root, ".env"), "", domain.ToolPathSecret, ""},
		{"env variant", filepath.Join(root, "config", ".env.production"), "", domain.ToolPathSecret, ""},
		{"credential dir", filepath.Join(root, "secrets", "prod.yaml"), "", domain.ToolPathSecret, ""},
		{"private key", filepath.Join(root, "deploy", "id_ed25519"), "", domain.ToolPathSecret, ""},
		{"pem", filepath.Join(root, "certs", "server.pem"), "", domain.ToolPathSecret, ""},
		{"credential-shaped name", filepath.Join(root, sentinelSecret, "x.txt"), "", domain.ToolPathSecret, ""},
		{"excluded vcs", filepath.Join(root, ".git", "config"), "", domain.ToolPathExcluded, ""},
		{"excluded deps", filepath.Join(root, "node_modules", "x", "i.js"), "", domain.ToolPathExcluded, ""},
		{"excluded agent worktree", filepath.Join(root, ".claude", "worktrees", "w", "a.go"), "", domain.ToolPathExcluded, ""},
		{"code that handles secrets is not a secret", filepath.Join(root, "internal", "secrets.go"), "", domain.ToolPathProject, "internal/secrets.go"},
		{"empty", "", "", domain.ToolPathUnresolved, ""},
		{"nul byte", root + "/a\x00b", "", domain.ToolPathUnresolved, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			gotScope, gotPath := scope.classify(tc.raw, tc.base)
			if gotScope != tc.scope || gotPath != tc.path {
				t.Fatalf("classify(%q, %q) = %q %q, want %q %q", tc.raw, tc.base, gotScope, gotPath, tc.scope, tc.path)
			}
		})
	}
	t.Run("no known root never claims inside or outside", func(t *testing.T) {
		unknown := newExplorationScope("", "/tmp/x.jsonl", domain.UsageSourceClaudeMain)
		if s, p := unknown.classify("/etc/hosts", ""); s != domain.ToolPathUnresolved || p != "" {
			t.Fatalf("without a root = %q %q, want unresolved", s, p)
		}
	})
	t.Run("an untrusted cwd is not a base", func(t *testing.T) {
		if base := scope.baseFor("/"); base != "" {
			t.Fatalf("cwd outside the root accepted as base: %q", base)
		}
	})
}

func TestCommandOpClassifiesByLeadingProgramOnly(t *testing.T) {
	tests := map[string]domain.ToolOp{
		"rg -n foo src":                      domain.ToolOpCommandExplore,
		"cd sub && cat a.go":                 domain.ToolOpCommandExplore,
		"FOO=1 grep -r x .":                  domain.ToolOpCommandExplore,
		"sed -n '1,20p' a.go":                domain.ToolOpCommandExplore,
		"sed -i 's/a/b/' a.go":               domain.ToolOpCommandEdit,
		"git log --oneline -5":               domain.ToolOpCommandExplore,
		"git -C x show HEAD":                 domain.ToolOpCommandExplore,
		"git commit -m x":                    domain.ToolOpCommand,
		"cat > out.txt <<EOF":                domain.ToolOpCommandEdit,
		"go run ./gen | tee out.go":          domain.ToolOpCommandEdit,
		"git apply fix.patch":                domain.ToolOpCommandEdit,
		"rg foo 2>/dev/null | head":          domain.ToolOpCommandExplore,
		"find . -name '*.tmp' -delete":       domain.ToolOpCommand,
		"go test ./...":                      domain.ToolOpCommand,
		"/usr/bin/ls -la":                    domain.ToolOpCommandExplore,
		"":                                   domain.ToolOpCommand,
		"npm run build && cat dist/index.js": domain.ToolOpCommand,
	}
	for command, want := range tests {
		if got := commandOp(command); got != want {
			t.Errorf("commandOp(%q) = %q, want %q", command, got, want)
		}
	}
}

func claudeFixture(root string) []string {
	abs := func(rel string) string { return filepath.Join(root, rel) }
	j := func(v any) string { b, _ := json.Marshal(v); return string(b) }
	assistant := func(id, uuid, ts string, sidechain bool, blocks ...map[string]any) string {
		return j(map[string]any{
			"type": "assistant", "uuid": uuid, "timestamp": ts, "cwd": root, "isSidechain": sidechain,
			"message": map[string]any{
				"id": id, "model": "claude-opus-5", "stop_reason": "tool_use",
				"usage":   map[string]any{"input_tokens": 10, "cache_creation_input_tokens": 0, "cache_read_input_tokens": 90, "output_tokens": 5},
				"content": blocks,
			},
		})
	}
	tool := func(id, name string, input map[string]any) map[string]any {
		return map[string]any{"type": "tool_use", "id": id, "name": name, "input": input}
	}
	result := func(uuid, toolID string, content string, tur any) string {
		return j(map[string]any{
			"type": "user", "uuid": uuid, "timestamp": "2026-09-24T10:00:30Z", "cwd": root,
			"message":       map[string]any{"role": "user", "content": []map[string]any{{"type": "tool_result", "tool_use_id": toolID, "content": content}}},
			"toolUseResult": tur,
		})
	}
	return []string{
		// AO's prompt.
		j(map[string]any{"type": "user", "uuid": "u0", "timestamp": "2026-09-24T10:00:00Z", "cwd": root,
			"message": map[string]any{"role": "user", "content": sentinelPrompt + " fix the bug"}}),
		// Harness-injected instruction file.
		j(map[string]any{"type": "attachment", "uuid": "att1", "timestamp": "2026-09-24T10:00:00Z", "cwd": root,
			"attachment": map[string]any{"type": "nested_memory", "path": abs("CLAUDE.md"), "content": map[string]any{"c": sentinelContent}}}),
		// Thinking first, then the calls in later records of the same message.
		assistant("m1", "a1", "2026-09-24T10:00:10Z", false, map[string]any{"type": "thinking", "thinking": "hmm"}),
		assistant("m1", "a2", "2026-09-24T10:00:10Z", false,
			tool("t-read-1", "Read", map[string]any{"file_path": abs("src/app.go")}),
			tool("t-grep", "Grep", map[string]any{"pattern": sentinelPattern, "path": abs("src")}),
		),
		result("r1", "t-read-1", sentinelContent+"\n1\tpackage main", map[string]any{"type": "text", "file": map[string]any{"filePath": abs("src/app.go"), "content": sentinelContent, "numLines": 42}}),
		result("r2", "t-grep", "src/app.go", map[string]any{"mode": "files_with_matches", "numFiles": 3, "filenames": []string{"a"}}),
		assistant("m2", "a3", "2026-09-24T10:01:00Z", false,
			tool("t-read-2", "Read", map[string]any{"file_path": abs("src/app.go")}),
			tool("t-env", "Read", map[string]any{"file_path": abs(".env")}),
			tool("t-out", "Read", map[string]any{"file_path": "/etc/hosts"}),
			tool("t-glob", "Glob", map[string]any{"pattern": "**/*.go"}),
			tool("t-bash", "Bash", map[string]any{"command": "rg " + sentinelCommand + " src"}),
		),
		assistant("m3", "a4", "2026-09-24T10:02:00Z", false,
			tool("t-edit", "Edit", map[string]any{"file_path": abs("src/app.go"), "old_string": sentinelContent, "new_string": "y"}),
		),
		// A subagent's call seen in the main transcript: not this agent's.
		assistant("m4", "a5", "2026-09-24T10:02:30Z", true,
			tool("t-side", "Read", map[string]any{"file_path": abs("side.go")}),
		),
		// A tool whose path argument is not a string costs that field only.
		assistant("m5", "a6", "2026-09-24T10:03:00Z", false,
			tool("t-weird", "LS", map[string]any{"path": map[string]any{"nested": true}}),
		),
	}
}

func claudeSource(root string) domain.UsageSourceContext {
	return domain.UsageSourceContext{
		Source: domain.UsageSourceRecord{
			Kind: domain.UsageSourceClaudeMain, NativeSessionID: "native-1",
			ArtifactPath: "/home/x/.claude/projects/slug/native-1.jsonl", ParserStateJSON: "{}",
		},
		Subject:       domain.SessionSubject("s1"),
		NativeRootID:  "native-1",
		WorkspaceRoot: root,
	}
}

func toRecords(lines []string) []jsonlRecord {
	out := make([]jsonlRecord, 0, len(lines))
	var offset int64
	for _, l := range lines {
		out = append(out, jsonlRecord{Data: []byte(l), Offset: offset})
		offset += int64(len(l)) + 1
	}
	return out
}

func TestClaudeExplorationParsing(t *testing.T) {
	root := t.TempDir()
	result := parseRecords(claudeSource(root), toRecords(claudeFixture(root)), 1, time.Now())
	if result.err != nil {
		t.Fatal(result.err)
	}
	type key struct {
		op    domain.ToolOp
		scope domain.ToolPathScope
		path  string
	}
	var got []key
	origins := map[domain.ToolObservationOrigin]int{}
	for _, o := range result.Tools.Observations {
		if !o.Valid() {
			t.Fatalf("parser produced an invalid observation: %+v", o)
		}
		origins[o.Origin]++
		if o.Origin == domain.OriginAgentExploration {
			got = append(got, key{o.Op, o.PathScope, o.Path})
		}
		if o.Origin == domain.OriginAgentExploration && o.EventKey == "" {
			t.Fatalf("a Claude tool call must link to its billed message: %+v", o)
		}
	}
	want := []key{
		{domain.ToolOpRead, domain.ToolPathProject, "src/app.go"},
		{domain.ToolOpSearch, domain.ToolPathProject, "src"},
		{domain.ToolOpRead, domain.ToolPathProject, "src/app.go"},
		{domain.ToolOpRead, domain.ToolPathSecret, ""},
		{domain.ToolOpRead, domain.ToolPathOutside, ""},
		{domain.ToolOpList, domain.ToolPathProject, "."},
		{domain.ToolOpCommandExplore, domain.ToolPathNone, ""},
		{domain.ToolOpEdit, domain.ToolPathProject, "src/app.go"},
		{domain.ToolOpList, domain.ToolPathUnresolved, ""},
	}
	if len(got) != len(want) {
		t.Fatalf("observations = %+v, want %+v (sidechain call must be skipped)", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("observation %d = %+v, want %+v", i, got[i], want[i])
		}
	}
	if origins[domain.OriginAOContext] != 1 || origins[domain.OriginHarnessContext] != 1 {
		t.Fatalf("origins = %v, want one AO prompt and one harness injection", origins)
	}
	if len(result.Tools.Results) != 2 {
		t.Fatalf("results = %d, want 2", len(result.Tools.Results))
	}
	if r := result.Tools.Results[0]; r.ResultItems == nil || *r.ResultItems != 42 || r.ResultBytes <= 0 {
		t.Fatalf("read result = %+v, want 42 lines and a positive length", r)
	}
	if r := result.Tools.Results[1]; r.ResultItems == nil || *r.ResultItems != 3 {
		t.Fatalf("grep result = %+v, want numFiles 3", r)
	}
	// The same tool id must key the call and its result.
	if result.Tools.Results[0].Key != result.Tools.Observations[2].Key {
		t.Fatalf("result key does not match its call")
	}
	assertNoSentinels(t, result.Tools)
	// And the usage events still parse as before: three billed messages
	// with tokens, the sidechain one excluded from main.
	if len(result.Events) == 0 {
		t.Fatal("usage events disappeared")
	}
}

func assertNoSentinels(t *testing.T, facts domain.AgentToolFacts) {
	t.Helper()
	encoded, err := json.Marshal(facts)
	mustNoError(t, err)
	for _, s := range []string{sentinelContent, sentinelCommand, sentinelPattern, sentinelPrompt, sentinelSecret, "/etc/hosts", ".env", "fix the bug"} {
		if strings.Contains(string(encoded), s) {
			t.Fatalf("observation facts carry %q: %s", s, encoded)
		}
	}
}

func TestClaudeSubagentTranscriptKeepsItsSidechainCalls(t *testing.T) {
	root := t.TempDir()
	source := claudeSource(root)
	source.Source.Kind = domain.UsageSourceClaudeSubagent
	source.Source.SubagentID = "agent-1"
	result := parseRecords(source, toRecords(claudeFixture(root)), 1, time.Now())
	found := false
	for _, o := range result.Tools.Observations {
		if o.Path == "side.go" {
			found = true
		}
	}
	if !found {
		t.Fatal("a subagent transcript's own (sidechain) calls must be observed")
	}
}

func codexFixture(root string) []string {
	j := func(v any) string { b, _ := json.Marshal(v); return string(b) }
	env := func(ts, typ string, payload map[string]any) string {
		return j(map[string]any{"timestamp": ts, "type": typ, "payload": payload})
	}
	args, _ := json.Marshal(map[string]any{"cmd": "rg -n " + sentinelPattern + " src", "workdir": root})
	return []string{
		env("2026-09-24T10:00:00Z", "session_meta", map[string]any{"id": "codex-root", "cwd": root, "base_instructions": map[string]any{"text": "You are Codex. " + sentinelContent}}),
		env("2026-09-24T10:00:00Z", "turn_context", map[string]any{"model": "gpt-5.6", "cwd": root}),
		env("2026-09-24T10:00:01Z", "response_item", map[string]any{"type": "message", "role": "developer", "content": []map[string]any{{"type": "input_text", "text": "dev instructions"}}}),
		env("2026-09-24T10:00:01Z", "response_item", map[string]any{"type": "message", "role": "user", "content": []map[string]any{{"type": "input_text", "text": "# AGENTS.md instructions " + sentinelContent}}}),
		env("2026-09-24T10:00:01Z", "event_msg", map[string]any{"type": "user_message", "message": sentinelPrompt}),
		env("2026-09-24T10:00:02Z", "response_item", map[string]any{"type": "custom_tool_call", "name": "exec", "call_id": "call-1", "input": "const r = await tools.exec_command({cmd:\"cat " + sentinelCommand + "\"});"}),
		env("2026-09-24T10:00:03Z", "response_item", map[string]any{"type": "custom_tool_call_output", "call_id": "call-1", "output": []map[string]any{{"type": "input_text", "text": "Script completed\n" + sentinelContent}}}),
		env("2026-09-24T10:00:04Z", "response_item", map[string]any{"type": "function_call", "name": "exec_command", "call_id": "call-2", "arguments": string(args)}),
		env("2026-09-24T10:00:05Z", "response_item", map[string]any{"type": "function_call_output", "call_id": "call-2", "output": "src/a.go:1:" + sentinelContent}),
		env("2026-09-24T10:00:06Z", "event_msg", map[string]any{"type": "patch_apply_end", "call_id": "call-3", "success": true,
			"changes": map[string]any{filepath.Join(root, "src", "a.go"): map[string]any{"update": map[string]any{"unified_diff": sentinelContent}}, filepath.Join(root, ".env"): map[string]any{}}}),
		string(codexTokenLine("2026-09-24T10:00:07Z", 100, 60, 0, 20, 5)),
	}
}

func TestCodexExplorationParsing(t *testing.T) {
	root := t.TempDir()
	source := domain.UsageSourceContext{
		Source: domain.UsageSourceRecord{
			Kind: domain.UsageSourceCodexRollout, NativeSessionID: "codex-root",
			ArtifactPath: "/home/x/.codex/sessions/2026/09/24/rollout.jsonl", ParserStateJSON: "{}",
		},
		NativeRootID: "codex-root", WorkspaceRoot: root,
	}
	result := parseRecords(source, toRecords(codexFixture(root)), 1, time.Now())
	if result.err != nil {
		t.Fatal(result.err)
	}
	counts := map[string]int{}
	for _, o := range result.Tools.Observations {
		if !o.Valid() {
			t.Fatalf("invalid observation %+v", o)
		}
		counts[string(o.Origin)+"/"+string(o.Op)+"/"+o.ToolName+"/"+string(o.PathScope)+"/"+o.Path]++
	}
	want := map[string]int{
		"harness_context/injected/base_instructions/none/":        1,
		"harness_context/injected/developer_message/none/":        1,
		"ao_context/prompt/user_message/none/":                    1,
		"agent_exploration/command/exec/none/":                    1, // code mode: not parsed
		"agent_exploration/command_explore/exec_command/none/":    1,
		"agent_exploration/edit/patch_apply_end/project/src/a.go": 1,
		"agent_exploration/edit/patch_apply_end/secret/":          1,
	}
	if len(counts) != len(want) {
		t.Fatalf("observations = %v, want %v", counts, want)
	}
	for k, n := range want {
		if counts[k] != n {
			t.Fatalf("observation %q = %d, want %d (all: %v)", k, counts[k], n, counts)
		}
	}
	if len(result.Tools.Results) != 2 || result.Tools.Results[0].ResultBytes <= 0 {
		t.Fatalf("results = %+v, want two measured outputs", result.Tools.Results)
	}
	if len(result.Events) != 1 {
		t.Fatalf("codex usage events = %d, want 1 (unchanged by 3C)", len(result.Events))
	}
	assertNoSentinels(t, result.Tools)
}

func TestCodexStructuredItemsParsing(t *testing.T) {
	root := t.TempDir()
	j := func(v any) string { b, _ := json.Marshal(v); return string(b) }
	item := func(it map[string]any) string {
		return j(map[string]any{"timestamp": "2026-09-24T10:00:02Z", "type": "event_msg", "payload": map[string]any{"type": "item_completed", "item": it}})
	}
	lines := []string{
		item(map[string]any{"type": "UserMessage", "id": "u1", "content": []map[string]any{{"type": "text", "text": sentinelPrompt}}}),
		item(map[string]any{"type": "CommandExecution", "id": "c1", "cwd": root, "command": []string{"bash", "-lc", sentinelCommand},
			"aggregated_output": sentinelContent,
			"parsed_cmd": []map[string]any{
				{"type": "read", "cmd": sentinelCommand, "name": "money.go", "path": "internal/money/money.go"},
				{"type": "search", "cmd": sentinelCommand, "query": sentinelPattern, "path": "internal"},
				{"type": "list_files", "cmd": "ls", "path": "."},
				{"type": "read", "cmd": "cat .env", "name": ".env", "path": ".env"},
				{"type": "unknown", "cmd": "go test ./..."},
			}}),
		item(map[string]any{"type": "FileChange", "id": "f1", "changes": map[string]any{
			filepath.Join(root, "internal", "money", "money.go"): map[string]any{"type": "update", "unified_diff": sentinelContent}}}),
	}
	source := domain.UsageSourceContext{
		Source:       domain.UsageSourceRecord{Kind: domain.UsageSourceCodexRollout, NativeSessionID: "r", ParserStateJSON: "{}", ArtifactPath: "/h/.codex/sessions/2026/09/24/x.jsonl"},
		NativeRootID: "r", WorkspaceRoot: root,
	}
	result := parseRecords(source, toRecords(lines), 1, time.Now())
	got := map[string]int{}
	for _, o := range result.Tools.Observations {
		if !o.Valid() {
			t.Fatalf("invalid %+v", o)
		}
		got[string(o.Op)+"/"+o.ToolName+"/"+string(o.PathScope)+"/"+o.Path]++
	}
	want := map[string]int{
		"command/codex_parsed_cmd/none/":                         1,
		"prompt/user_message/none/":                              1,
		"read/codex_parsed_cmd/project/internal/money/money.go":  1,
		"search/codex_parsed_cmd/project/internal":               1,
		"list/codex_parsed_cmd/project/.":                        1,
		"read/codex_parsed_cmd/secret/":                          1,
		"edit/codex_file_change/project/internal/money/money.go": 1,
	}
	if len(got) != len(want) {
		t.Fatalf("observations = %v, want %v", got, want)
	}
	for k, n := range want {
		if got[k] != n {
			t.Fatalf("%q = %d, want %d (all %v)", k, got[k], n, got)
		}
	}
	assertNoSentinels(t, result.Tools)
}

// The whole pipeline, end to end on a real file and a real database: the
// workspace root comes from AO's session record, a restart that re-reads the
// transcript from offset zero writes nothing twice, and a result that lands in
// a later chunk completes its call.
func TestIngestorPersistsToolObservationsExactlyOnceAcrossRestart(t *testing.T) {
	ctx := context.Background()
	now := time.Unix(1700000000, 0).UTC()
	root := t.TempDir()
	dataDir := t.TempDir()
	store, session := seedUsageTestSession(t, dataDir, "usage", domain.HarnessClaudeCode, domain.ActivityActive, "native-1", now)
	session.Metadata.WorkspacePath = root
	mustNoError(t, store.UpdateSession(ctx, session))
	binding := seedUsageTestBinding(t, store, session, "native-1", domain.UsageBindingActive, now)
	lines := claudeFixture(root)
	path := filepath.Join(t.TempDir(), "projects", "slug", "native-1.jsonl")
	mustNoError(t, os.MkdirAll(filepath.Dir(path), 0o700))
	// First chunk stops right after the Read call, before its result.
	mustNoError(t, os.WriteFile(path, []byte(strings.Join(lines[:4], "\n")+"\n"), 0o600))
	path = canonicalTranscriptPath(path)
	identity, err := usagesvc.SourceIdentity(ctx, path)
	mustNoError(t, err)
	source, err := store.InsertUsageSource(ctx, domain.UsageSourceRecord{
		BindingID: binding.ID, Kind: domain.UsageSourceClaudeMain, NativeSessionID: "native-1",
		ArtifactPath: path, FileIdentity: identity, State: domain.UsageSourcePending, UpdatedAt: now,
	})
	mustNoError(t, err)
	ingestor := NewIngestor(store, IngestorConfig{Clock: func() time.Time { return now }})
	ingestSourceFully(ctx, t, ingestor, source.ID)
	appendJSONLRecord(t, path, []byte(strings.Join(lines[4:], "\n")))
	ingestSourceFully(ctx, t, ingestor, source.ID)

	db, err := sql.Open("sqlite", "file:"+filepath.Join(dataDir, "ao.db"))
	mustNoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	count := func() (rows, withPath, withResult int64) {
		t.Helper()
		mustNoError(t, db.QueryRowContext(ctx, `SELECT COUNT(*), COUNT(path), COUNT(result_bytes) FROM agent_tool_observations`).Scan(&rows, &withPath, &withResult))
		return
	}
	rows, withPath, withResult := count()
	if rows == 0 || withResult < 2 {
		t.Fatalf("rows=%d results=%d, want observations with the late results completed", rows, withResult)
	}

	// Restart: a fresh ingestor and a replaced artifact (same content, new
	// inode) force a re-read from offset zero.
	replacement := path + ".next"
	content, err := os.ReadFile(path)
	mustNoError(t, err)
	mustNoError(t, os.WriteFile(replacement, content, 0o600))
	mustNoError(t, os.Rename(replacement, path))
	restarted := NewIngestor(store, IngestorConfig{Clock: func() time.Time { return now.Add(time.Minute) }})
	ingestAllWatchable(ctx, t, store, restarted)
	rows2, withPath2, withResult2 := count()
	if rows2 != rows || withPath2 != withPath || withResult2 != withResult {
		t.Fatalf("after restart rows/paths/results = %d/%d/%d, want %d/%d/%d (exactly once)", rows2, withPath2, withResult2, rows, withPath, withResult)
	}

	// Nothing but project-relative paths, and no sentinel anywhere in the table.
	dump, err := db.QueryContext(ctx, `SELECT observation_key, event_key, origin, op, tool_name, path_scope, COALESCE(path,'') FROM agent_tool_observations`)
	mustNoError(t, err)
	defer func() { _ = dump.Close() }()
	for dump.Next() {
		var cols [7]string
		mustNoError(t, dump.Scan(&cols[0], &cols[1], &cols[2], &cols[3], &cols[4], &cols[5], &cols[6]))
		all := strings.Join(cols[:], "|")
		for _, s := range []string{sentinelContent, sentinelCommand, sentinelPattern, sentinelPrompt, root, "/etc", ".env"} {
			if strings.Contains(all, s) {
				t.Fatalf("stored row carries %q: %s", s, all)
			}
		}
	}
	mustNoError(t, dump.Err())
}
