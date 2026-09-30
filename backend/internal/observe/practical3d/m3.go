package practical3d

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"path/filepath"
	"sort"
	"strings"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/observe/usage"

	_ "modernc.org/sqlite" // read-only access to a position's AO database
)

// M3ParserVersion is instrument.exploration_parser_version for real runs.
const M3ParserVersion = "ao.3d-practical.m3.3c-proxy.v1"

// aoRole maps a Practical role to the role AO's control plane records in
// usage_attribution_windows.
func aoRole(r Role) string {
	switch r {
	case RoleRepair:
		return "fix_worker"
	case RolePlanner:
		return "planner"
	}
	return string(r)
}

// M3Input identifies the measured role of one finished position.
type M3Input struct {
	DataDir      string
	RunID        string
	MeasuredRole Role
	Proxy        []ProxyObservation
	// ToolResults are the tool outcomes the proxy saw sent back to the
	// provider (tool_use id -> is_error).
	ToolResults map[string]bool
	// ProjectRoots are the position's working copy and worktrees: a
	// provider target inside them must be a project row in 3C.
	ProjectRoots []string
}

// M3Evidence is the derived exploration before the milestone.
type M3Evidence struct {
	Calls        int64  `json:"exploration_calls"`
	Files        int64  `json:"distinct_files"`
	Milestone    string `json:"milestone"`
	Subject      string `json:"subject"`
	Sources      int    `json:"sources"`
	Observations int    `json:"observations"`
}

type m3Source struct {
	id                                  int64
	root, kind, subagent, nativeSession string
	byteOffset                          int64
	harness                             string // claude-code or codex
}

type m3Obs struct {
	source   int64
	eventKey string
	ordinal  int64
	op       domain.ToolOp
	tool     string
	scope    domain.ToolPathScope
	path     string
	obsKey   string
	resultOK bool // the wire carried the tool's result back, and not as an error
	call     int
	toolIdx  int // position of the tool_use in the provider's message
}

var observedToolNames = map[string]bool{"Read": true, "NotebookRead": true, "Grep": true, "Glob": true, "LS": true, "Bash": true, "Edit": true, "MultiEdit": true, "Write": true, "NotebookEdit": true}

// DeriveM3 computes (c, f) of 06 §2 from AO's 3C tool observations of the
// measured role, before its milestone: the first edit (worker, A/B/D) or the
// structured verdict submission (reviewer, C). The agent cannot declare or
// override it: 3C rows are cross-checked against the messages and tool_use
// blocks the AO provider proxy saw on the wire, and ordered by the proxy's
// call_index. Incomplete coverage, a missing milestone, or any observation
// the proxy cannot vouch for is an error (MALFORMED_RESULT).
func DeriveM3(ctx context.Context, in M3Input) (M3Evidence, error) {
	var ev M3Evidence
	db, err := sql.Open("sqlite", "file:"+url.PathEscape(in.DataDir+"/ao.db")+"?mode=ro&_pragma=busy_timeout(5000)")
	if err != nil {
		return ev, err
	}
	defer func() { _ = db.Close() }()
	role := aoRole(in.MeasuredRole)
	var kind, subject string
	n := 0
	scanErr := func() error {
		rows, err := db.QueryContext(ctx, `SELECT DISTINCT subject_kind, session_id FROM usage_attribution_windows WHERE workflow_run_id = ? AND role = ?`, in.RunID, role)
		if err != nil {
			return err
		}
		defer func() { _ = rows.Close() }()
		for rows.Next() {
			n++
			if err := rows.Scan(&kind, &subject); err != nil {
				return err
			}
		}
		return rows.Err()
	}()
	if scanErr != nil {
		return ev, fmt.Errorf("read role windows: %w", scanErr)
	}
	if n != 1 {
		return ev, fmt.Errorf("measured role %s has %d AO subjects in run %s, want exactly 1", role, n, in.RunID)
	}
	ev.Subject = kind + ":" + subject
	sources, err := m3Sources(ctx, db, kind, subject)
	if err != nil {
		return ev, err
	}
	ev.Sources = len(sources)
	// Every proxied message of this subject, keyed by AO's own event key.
	keyed := map[string]ProxyObservation{}
	for _, p := range in.Proxy {
		if p.Subject != ev.Subject || p.MessageID == "" {
			continue
		}
		for _, s := range sources {
			if s.harness != "claude-code" {
				continue // Codex rows carry no message key; they bind by call id
			}
			keyed[usage.ClaudeMessageEventKey(s.root, domain.UsageSourceKind(s.kind), s.subagent, s.nativeSession, p.MessageID)] = p
		}
	}
	obs, err := m3Observations(ctx, db, kind, subject, role, in.RunID)
	if err != nil {
		return ev, err
	}
	ev.Observations = len(obs)
	// Every 3C row must be one tool_use block the provider actually returned
	// (bound by its tool_use id), with the op 3C's own classifier derives
	// from the provider's input and the path the provider named. Rows the
	// agent adds or rewrites in its (agent-writable) transcript match none.
	type wireTool struct {
		p   ProxyObservation
		tu  ProxyToolUse
		idx int
	}
	wire := map[string]wireTool{}
	for _, p := range in.Proxy {
		if p.Subject != ev.Subject {
			continue
		}
		for idx, tu := range p.ToolUses {
			for _, s := range sources {
				key := usage.ClaudeToolObservationKey(s.root, domain.UsageSourceKind(s.kind), s.subagent, s.nativeSession, tu.ID)
				if s.harness == "codex" {
					key = usage.CodexToolObservationKey(s.root, s.nativeSession, tu.ID)
				}
				wire[key] = wireTool{p, tu, idx}
			}
		}
	}
	harnessOf := map[int64]string{}
	for _, s := range sources {
		harnessOf[s.id] = s.harness
	}
	seen := map[string]bool{}
	for i := range obs {
		codex := harnessOf[obs[i].source] == "codex"
		w, ok := wire[obs[i].obsKey]
		if !ok {
			return ev, fmt.Errorf("tool observation %d of %s is not a tool call the provider returned (forged or unobserved)", i, ev.Subject)
		}
		p := w.p
		if !codex {
			// Claude rows also name their message: it must be the one the
			// provider returned the tool_use in.
			mp, ok := keyed[obs[i].eventKey]
			if !ok {
				return ev, fmt.Errorf("tool observation %d of %s has no proxy-observed message (forged or unobserved)", i, ev.Subject)
			}
			if w.p.MessageID != mp.MessageID {
				return ev, fmt.Errorf("tool observation %d of %s is not a tool_use the provider returned in that message (forged)", i, ev.Subject)
			}
		}
		if seen[obs[i].obsKey] {
			return ev, fmt.Errorf("tool observation %d of %s duplicates a tool_use", i, ev.Subject)
		}
		seen[obs[i].obsKey] = true
		want := usage.ClaudeToolOp(w.tu.Name, w.tu.Command)
		if codex {
			// A Codex call whose command neither side can read (code-mode
			// JavaScript `exec`) hides what it explored: M3 is not
			// observable, and is never reported as zero exploration.
			if w.tu.Command == "" && w.tu.Name == "exec" {
				return ev, fmt.Errorf("tool observation %d of %s: Codex code-mode call %s is opaque to 3C; M3 not observable", i, ev.Subject, w.tu.ID)
			}
			want = usage.CodexToolOp(w.tu.Name, w.tu.Command)
		}
		if obs[i].op != want || (!codex && observedToolNames[w.tu.Name] && obs[i].tool != w.tu.Name) {
			return ev, fmt.Errorf("tool observation %d of %s: 3C %s/%s differs from the provider's %s/%s", i, ev.Subject, obs[i].tool, obs[i].op, w.tu.Name, want)
		}
		if !targetMatches(w.tu.Target, obs[i].scope, obs[i].path, in.ProjectRoots) {
			return ev, fmt.Errorf("tool observation %d of %s: 3C path %q is not the provider's target %q", i, ev.Subject, obs[i].path, w.tu.Target)
		}
		obs[i].call, obs[i].toolIdx = p.CallIndex, w.idx
		// Success is the wire's account (the transcript's is agent-writable).
		isErr, known := in.ToolResults[w.tu.ID]
		obs[i].resultOK = known && !isErr
	}
	// Every tool_use the proxy saw for the measured role must be observed.
	codexSubject := false
	for _, s := range sources {
		codexSubject = codexSubject || s.harness == "codex"
	}
	for key, w := range wire {
		if w.p.Role == in.MeasuredRole && (codexSubject || observedToolNames[w.tu.Name]) && !seen[key] {
			// A key is computed per source; only the source that carries
			// the message must hold it.
			carried := false
			for k2, w2 := range wire {
				if w2.tu.ID == w.tu.ID && seen[k2] {
					carried = true
					break
				}
			}
			if !carried {
				return ev, fmt.Errorf("message at call %d: proxy saw %s tool use %s, 3C never observed it", w.p.CallIndex, w.tu.Name, w.tu.ID)
			}
		}
	}
	sort.SliceStable(obs, func(i, j int) bool {
		if obs[i].call != obs[j].call {
			return obs[i].call < obs[j].call
		}
		// Within one message, the provider's own tool_use order: the
		// transcript's ordinal is agent-writable.
		return obs[i].toolIdx < obs[j].toolIdx
	})
	var before []m3Obs
	switch in.MeasuredRole {
	case RoleReviewer:
		call, submits := verdictCall(in.Proxy, ev.Subject)
		if call == 0 {
			return ev, fmt.Errorf("reviewer structured verdict (ao review submit) was never observed")
		}
		// The submission must have reached AO: a command that merely
		// mentions `ao review submit` is not a verdict.
		var verdicts int
		if err := db.QueryRowContext(ctx, `SELECT count(*) FROM review_run WHERE verdict <> ''`).Scan(&verdicts); err != nil {
			return ev, fmt.Errorf("read review verdicts: %w", err)
		}
		if verdicts == 0 {
			return ev, fmt.Errorf("AO recorded no review verdict for the reviewer's submission")
		}
		// Each submit invocation must be a verdict AO recorded: an earlier
		// `ao review submit --help` (or a failed attempt) would otherwise
		// become the milestone of a later real submission.
		if submits != verdicts {
			return ev, fmt.Errorf("reviewer ran %d ao review submit commands but AO recorded %d verdicts", submits, verdicts)
		}
		ev.Milestone = fmt.Sprintf("verdict_submission@call_%d", call)
		for _, o := range obs {
			if o.call < call || (o.call == call && !shellTools[o.tool]) {
				before = append(before, o)
			}
		}
	default:
		found := false
		for _, o := range obs {
			// Only an edit of a project file is the milestone: a write to
			// a scratch path outside the working copy is not "the first
			// edit" and cannot end the exploration window.
			// ...and one that succeeded: a deliberately failing Edit changes
			// nothing and cannot end the window.
			if (o.op == domain.ToolOpEdit || o.op == domain.ToolOpCommandEdit) && o.scope == domain.ToolPathProject && o.path != "" && o.resultOK {
				found = true
				ev.Milestone = fmt.Sprintf("first_edit@call_%d", o.call)
				break
			}
			before = append(before, o)
		}
		if !found {
			return ev, fmt.Errorf("worker first edit was never observed")
		}
	}
	files := map[string]bool{}
	for _, o := range before {
		if !o.op.IsExploration() {
			continue
		}
		ev.Calls++
		if o.scope == domain.ToolPathProject && o.path != "" {
			files[o.path] = true
		}
	}
	ev.Files = int64(len(files))
	return ev, nil
}

func verdictCall(proxy []ProxyObservation, subject string) (first, submits int) {
	for _, p := range proxy {
		if p.Subject != subject {
			continue
		}
		for _, tu := range p.ToolUses {
			if shellTools[tu.Name] && isReviewSubmit(tu.Command) {
				submits++
				if first == 0 || p.CallIndex < first {
					first = p.CallIndex
				}
			}
		}
	}
	return first, submits
}

// isReviewSubmit reports whether a Bash command is an invocation of
// `ao review submit` (optionally by path), not a command that merely
// mentions it (echo, printf, grep, a comment...).
// targetMatches reports whether a 3C project path is the provider's raw
// target: the same relative path, or an absolute path ending in it. Paths
// outside the project carry none in 3C and are not compared.
func targetMatches(target string, scope domain.ToolPathScope, path string, roots []string) bool {
	raw := strings.TrimSpace(target)
	t := filepath.ToSlash(filepath.Clean(raw))
	within := func(p string) bool {
		for _, root := range roots {
			for _, r := range []string{root, resolvedPath(root)} {
				if rel, err := filepath.Rel(r, filepath.Clean(p)); err == nil && rel != ".." && !strings.HasPrefix(rel, "../") {
					return true
				}
			}
		}
		return false
	}
	if scope == domain.ToolPathProject && path != "" {
		// An absolute provider target must itself lie in the project: a
		// suffix match alone would let a rewritten row claim
		// /elsewhere/src/app.go as the project's src/app.go.
		if filepath.IsAbs(raw) && !within(raw) {
			return false
		}
		if path == "." {
			return raw == "" || filepath.IsAbs(raw) || t == "."
		}
		return t == path || strings.HasSuffix(t, "/"+path)
	}
	if raw == "" {
		return true
	}
	// A target the provider placed inside the project must not appear in 3C
	// as outside/unresolved (a rewritten row would drop it from coverage);
	// secret and excluded project paths are counted but never named.
	inProject := !filepath.IsAbs(raw) && t != ".." && !strings.HasPrefix(t, "../")
	if filepath.IsAbs(raw) && within(raw) {
		inProject = true
	}
	if inProject {
		return scope == domain.ToolPathSecret || scope == domain.ToolPathExcluded
	}
	return true
}

func resolvedPath(p string) string {
	if r, err := filepath.EvalSymlinks(p); err == nil {
		return r
	}
	return p
}

func isReviewSubmit(command string) bool {
	// AO's reviewer instructions pipe the verdict JSON into the command
	// (`printf '...' | ao review submit --session S --reviews -`), so any
	// pipeline/list segment may be the invocation; a segment is one only if
	// it starts with the program.
	for _, seg := range strings.FieldsFunc(command, func(r rune) bool { return r == '|' || r == ';' || r == '&' || r == '\n' }) {
		f := strings.Fields(strings.TrimSpace(seg))
		if len(f) < 3 || (f[0] != "ao" && !strings.HasSuffix(f[0], "/ao")) || f[1] != "review" || f[2] != "submit" {
			continue
		}
		help := false
		for _, a := range f[3:] {
			if a == "--help" || a == "-h" || a == "help" {
				help = true
			}
		}
		if !help {
			return true
		}
	}
	return false
}

// shellTools are the tool names under which the harnesses run commands
// (Claude Code's Bash; Codex's function tools).
var shellTools = map[string]bool{"Bash": true, "exec_command": true, "shell": true, "local_shell": true, "container.exec": true, "exec": true}

func m3Sources(ctx context.Context, db *sql.DB, kind, subject string) ([]m3Source, error) {
	rows, err := db.QueryContext(ctx, `SELECT s.id, b.native_root_id, s.kind, s.subagent_id, s.native_session_id, s.byte_offset, b.harness,
		COALESCE(c.covered_from, -1), COALESCE(c.covered_to, -1), COALESCE(c.min_extractor, 0), COALESCE(c.max_extractor, 0), COALESCE(c.pre_coverage_events, 1)
		FROM usage_bindings b JOIN usage_sources s ON s.binding_id = b.id
		LEFT JOIN agent_tool_coverage c ON c.usage_source_id = s.id
		WHERE b.subject_kind = ? AND b.subject_id = ?`, kind, subject)
	if err != nil {
		return nil, fmt.Errorf("read usage sources: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []m3Source
	for rows.Next() {
		var s m3Source
		var harness string
		var from, to, minX, maxX, pre int64
		if err := rows.Scan(&s.id, &s.root, &s.kind, &s.subagent, &s.nativeSession, &s.byteOffset, &harness, &from, &to, &minX, &maxX, &pre); err != nil {
			return nil, err
		}
		if harness != "claude-code" && harness != "codex" {
			return nil, fmt.Errorf("source %d harness %q has no Practical M3 binding", s.id, harness)
		}
		s.harness = harness
		if from < 0 || pre != 0 || to < s.byteOffset || minX != maxX || minX == 0 {
			return nil, fmt.Errorf("3C coverage of source %d is incomplete (from=%d to=%d offset=%d pre=%d extractors=%d..%d)", s.id, from, to, s.byteOffset, pre, minX, maxX)
		}
		out = append(out, s)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("subject %s:%s has no transcript source", kind, subject)
	}
	return out, rows.Err()
}

func m3Observations(ctx context.Context, db *sql.DB, kind, subject, role, runID string) ([]m3Obs, error) {
	rows, err := db.QueryContext(ctx, `SELECT a.usage_source_id, a.event_key, a.ordinal, a.op, a.tool_name, a.path_scope, COALESCE(a.path, ''), a.observation_key
		FROM agent_tool_observation_attribution a JOIN usage_attribution_windows w ON w.id = a.window_id
		WHERE a.subject_kind = ? AND a.subject_id = ? AND w.role = ? AND w.workflow_run_id = ? AND a.origin = 'agent_exploration'
		ORDER BY a.usage_source_id, a.ordinal`, kind, subject, role, runID)
	if err != nil {
		return nil, fmt.Errorf("read tool observations: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []m3Obs
	for rows.Next() {
		var o m3Obs
		var src sql.NullInt64
		var op, scope string
		if err := rows.Scan(&src, &o.eventKey, &o.ordinal, &op, &o.tool, &scope, &o.path, &o.obsKey); err != nil {
			return nil, err
		}
		o.source, o.op, o.scope = src.Int64, domain.ToolOp(op), domain.ToolPathScope(scope)
		if !o.op.Valid() || !o.scope.Valid() {
			return nil, fmt.Errorf("observation with unknown op/scope %q/%q", op, scope)
		}
		out = append(out, o)
	}
	return out, rows.Err()
}
