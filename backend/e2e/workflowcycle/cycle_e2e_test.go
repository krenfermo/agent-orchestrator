//go:build !windows

package workflowcycle

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// cycle_e2e_test.go states the Frente 1 contract of the whole workflow cycle as
// what an operator could observe from outside AO: the HTTP API, the tmux server,
// the git repository and the agents' own traces. Nothing here reads AO's
// internals, and nothing is arranged so that the current implementation passes:
// every assertion is a property of the system a person relies on.

const projectID = "cycle-e2e"

const deliverable = "cycle-e2e-deliverable\n"

func (s *scratch) registerProject() {
	s.t.Helper()
	s.mustDo(http.MethodPost, "/api/v1/projects", map[string]any{"path": s.repo, "projectId": projectID, "name": projectID})
}

func (s *scratch) createRun(req map[string]any) string {
	s.t.Helper()
	out := s.mustDo(http.MethodPost, "/api/v1/projects/"+projectID+"/workflows", req)
	id := str(dig(out, "workflow", "run", "id"))
	if id == "" {
		b, _ := json.MarshalIndent(out, "", "  ")
		s.t.Fatalf("create run returned no run id: %s", b)
	}
	return id
}

func (s *scratch) runDetail(runID string) map[string]any {
	s.t.Helper()
	return s.mustDo(http.MethodGet, "/api/v1/workflows/"+runID, nil)
}

func stepOf(detail map[string]any, kind string) map[string]any {
	steps, _ := dig(detail, "workflow", "steps").([]any)
	for _, st := range steps {
		if m, ok := st.(map[string]any); ok && str(m["kind"]) == kind {
			return m
		}
	}
	return nil
}

func attemptsOf(step map[string]any) []map[string]any {
	raw, _ := step["attempts"].([]any)
	out := make([]map[string]any, 0, len(raw))
	for _, a := range raw {
		if m, ok := a.(map[string]any); ok {
			out = append(out, m)
		}
	}
	return out
}

func signature(detail map[string]any) string {
	sig := str(dig(detail, "workflow", "run", "state"))
	steps, _ := dig(detail, "workflow", "steps").([]any)
	for _, st := range steps {
		sig += " " + str(dig(st, "kind")) + ":" + str(dig(st, "state"))
	}
	return sig
}

func terminalState(state string) bool {
	return state == "completed" || state == "failed" || state == "cancelled"
}

// drive advances a run the way an operator does -- GET to observe, continue
// when the run is waiting for its next step -- until it is terminal or parks on
// attention. It returns the final detail and every distinct state signature
// observed, in order.
func (s *scratch) drive(runID string, timeout time.Duration) (map[string]any, []string) {
	s.t.Helper()
	var seen []string
	deadline := time.Now().Add(timeout)
	for {
		d := s.runDetail(runID)
		if sig := signature(d); len(seen) == 0 || seen[len(seen)-1] != sig {
			seen = append(seen, sig)
			s.t.Logf("%s  %s", time.Now().Format("15:04:05.000"), sig)
		}
		state := str(dig(d, "workflow", "run", "state"))
		if terminalState(state) || state == "needs_attention" {
			return d, seen
		}
		if time.Now().After(deadline) {
			b, _ := json.MarshalIndent(d, "", "  ")
			s.t.Fatalf("run %s did not settle within %s; last state %q:\n%.6000s", runID, timeout, state, b)
		}
		if state == "waiting" {
			if code, body, err := s.do(http.MethodPost, "/api/v1/workflows/"+runID+"/continue", map[string]any{}); err != nil || code >= 300 {
				s.t.Logf("continue: %d %v %s", code, err, body)
			}
		}
		time.Sleep(500 * time.Millisecond)
	}
}

// ---- scenario 1: the whole cycle -----------------------------------------------

func TestWorkflowCycleCompletesThroughRealTmux(t *testing.T) {
	s := newScratch(t)
	s.initRepo()
	baseSHA := s.git(s.repo, "rev-parse", "HEAD")
	s.startDaemon()
	s.registerProject()
	watch := s.watchTmux()

	runID := s.createRun(map[string]any{
		"objective":          "Add src/cycle.txt containing exactly 'cycle-e2e-deliverable' and a src/cycle.go constant.",
		"strategy":           "task",
		"reviewDepth":        "light",
		"placement":          "isolated_worktree",
		"writeIntent":        "mutating",
		"acceptanceCriteria": []string{"src/cycle.txt contains exactly cycle-e2e-deliverable", "src/cycle.go declares the Cycle constant"},
		"verification": map[string]any{
			"commands": []map[string]any{{"command": "grep", "args": []string{"-q", "cycle-e2e-deliverable", "src/cycle.txt"}, "timeoutSeconds": 30, "requiredExitCode": 0}},
			"files": []map[string]any{
				{"path": "src/cycle.txt", "exists": true, "exactContent": deliverable},
				{"path": "src/cycle.go", "exists": true},
			},
		},
	})
	s.mustDo(http.MethodPost, "/api/v1/workflows/"+runID+"/start", map[string]any{})

	final, history := s.drive(runID, 3*time.Minute)
	completedAt := time.Now()

	// ---- C8: the terminal state ----
	if state := str(dig(final, "workflow", "run", "state")); state != "completed" {
		b, _ := json.MarshalIndent(final, "", "  ")
		t.Fatalf("run ended %q, want completed:\n%.8000s", state, b)
	}
	for _, sig := range history {
		if strings.HasPrefix(sig, "needs_attention") || strings.HasPrefix(sig, "failed") {
			t.Fatalf("run passed through %q on its way to completed", sig)
		}
	}

	work, review, verify, fix := stepOf(final, "work"), stepOf(final, "review"), stepOf(final, "verify"), stepOf(final, "fix")
	if work == nil || review == nil || verify == nil {
		t.Fatalf("run detail is missing a step: work=%v review=%v verify=%v", work != nil, review != nil, verify != nil)
	}

	// ---- C5/C6/C7: the transitions, in order ----
	// work must complete no later than review, and review no later than verify.
	orderOK := func(before, after string) bool {
		bi, ai := -1, -1
		for i, sig := range history {
			if bi < 0 && strings.Contains(sig, before+":completed") {
				bi = i
			}
			if ai < 0 && strings.Contains(sig, after+":completed") {
				ai = i
			}
		}
		return bi >= 0 && ai >= 0 && bi <= ai
	}
	if !orderOK("work", "review") || !orderOK("review", "verify") {
		t.Fatalf("steps did not complete in work -> review -> verify order: %v", history)
	}
	for _, st := range []map[string]any{work, review, verify} {
		if str(st["state"]) != "completed" {
			t.Fatalf("%s step is %q, want completed", str(st["kind"]), str(st["state"]))
		}
	}
	if fix != nil && str(fix["state"]) != "pending" {
		t.Fatalf("an approved review opened a fix cycle: fix step %q", str(fix["state"]))
	}

	// ---- C2/C9: exactly one worker attempt, exactly one reviewer ----
	workAttempts := attemptsOf(work)
	if len(workAttempts) != 1 {
		t.Fatalf("work step has %d attempts, want exactly 1: %+v", len(workAttempts), workAttempts)
	}
	if o := str(workAttempts[0]["outcome"]); o != "succeeded" {
		t.Fatalf("worker attempt outcome %q, want succeeded", o)
	}
	workerSession := str(work["sessionId"])
	if workerSession == "" {
		t.Fatal("the completed work step records no session")
	}
	if v := str(review["verdict"]); v != "approved" {
		t.Fatalf("review verdict %q, want approved", v)
	}
	reviewRunID := str(review["reviewRunId"])
	if reviewRunID == "" {
		t.Fatal("the completed review step records no review run")
	}
	verifyAttempts := attemptsOf(verify)
	if len(verifyAttempts) != 1 || str(verifyAttempts[0]["outcome"]) != "succeeded" {
		t.Fatalf("verify attempts = %+v, want exactly one that succeeded", verifyAttempts)
	}

	// ---- C3/C4: the agents really ran, inside the real tmux, where AO said ----
	workers, reviewers := s.traces("worker"), s.traces("reviewer")
	if len(workers) != 1 {
		t.Fatalf("the worker agent ran %d times, want exactly once: %v", len(workers), workers)
	}
	if len(reviewers) != 1 {
		t.Fatalf("the reviewer agent ran %d times, want exactly once: %v", len(reviewers), reviewers)
	}
	w, r := workers[0], reviewers[0]
	for role, tr := range map[string]agentTrace{"worker": w, "reviewer": r} {
		if !strings.HasPrefix(tr["argv0"], s.binDir+"/") {
			t.Fatalf("%s ran %q, not the scratch shim under %s", role, tr["argv0"], s.binDir)
		}
		if !strings.Contains(tr["tmux"], "/"+s.socket+",") {
			t.Fatalf("%s was not inside the scratch tmux server: TMUX=%q", role, tr["tmux"])
		}
		if tr["tmux_session"] == "" || tr["tmux_pane"] == "" {
			t.Fatalf("%s trace has no tmux session/pane: %v", role, tr)
		}
	}
	if w["ao_session_id"] != workerSession || w["tmux_session"] != workerSession {
		t.Fatalf("worker ran as session %q in tmux session %q, want %q for both", w["ao_session_id"], w["tmux_session"], workerSession)
	}
	worktree := w["pwd"]
	if !strings.HasPrefix(worktree, filepath.Join(s.dataDir, "worktrees")+"/") {
		t.Fatalf("worker ran in %q, not an isolated worktree under the scratch data dir", worktree)
	}
	if w["turn"] != "done" {
		t.Fatalf("worker trace did not reach the end of its turn: %v", w)
	}
	if r["review_run_id"] != reviewRunID || r["review_submit"] != "ok" {
		t.Fatalf("reviewer submitted for run %q (%s), want %q ok", r["review_run_id"], r["review_submit"], reviewRunID)
	}
	if r["ao_review_worker_session_id"] != workerSession {
		t.Fatalf("reviewer reviewed worker %q, want %q", r["ao_review_worker_session_id"], workerSession)
	}
	if r["pwd"] != worktree {
		t.Fatalf("reviewer ran in %q, want the worker's worktree %q", r["pwd"], worktree)
	}

	// ---- C2: ownership stamps in the real tmux server ----
	seen := watch.Stop()
	if len(seen) != 2 {
		t.Fatalf("tmux saw %d sessions over the run, want exactly 2 (one worker, one reviewer): %+v", len(seen), seen)
	}
	workerTmux, ok := seen[workerSession]
	if !ok {
		t.Fatalf("no tmux session named %q was ever observed: %+v", workerSession, seen)
	}
	if want := "ao-session:" + workerSession + ":" + w["ao_runtime_launch_id"]; workerTmux.Env["AO_SESSION_OWNER"] != want {
		t.Fatalf("worker tmux owner = %q, want %q", workerTmux.Env["AO_SESSION_OWNER"], want)
	}
	var reviewerTmux tmuxSession
	for name, ts := range seen {
		if name != workerSession {
			reviewerTmux = ts
		}
	}
	if !strings.HasPrefix(reviewerTmux.Env["AO_SESSION_OWNER"], "ao-reviewer:") || reviewerTmux.Name != r["tmux_session"] {
		t.Fatalf("reviewer tmux session %q owner %q, want the traced reviewer session with an ao-reviewer token", reviewerTmux.Name, reviewerTmux.Env["AO_SESSION_OWNER"])
	}
	for _, ts := range []tmuxSession{workerTmux, reviewerTmux} {
		if ts.Env["AO_INSTALLATION_ID"] != s.installationID || ts.Env["AO_DAEMON_INSTANCE_ID"] != s.instanceID {
			t.Fatalf("tmux session %s is stamped installation %q / daemon %q, want %q / %q",
				ts.Name, ts.Env["AO_INSTALLATION_ID"], ts.Env["AO_DAEMON_INSTANCE_ID"], s.installationID, s.instanceID)
		}
	}
	if !strings.Contains(workerTmux.StartCommand, filepath.Join(s.binDir, "claude")) {
		t.Fatalf("worker pane did not launch the scratch agent: %.300s", workerTmux.StartCommand)
	}

	// ---- C7/C8: the deliverable is verified AND preserved in git ----
	branch := str(work["branch"])
	if !strings.HasPrefix(branch, "ao/") {
		t.Fatalf("work branch %q is not an AO branch", branch)
	}
	head := s.git(worktree, "rev-parse", "HEAD")
	if want := str(verify["headSha"]); want == "" || head != want {
		t.Fatalf("worktree HEAD %s, verify recorded %q", head, want)
	}
	if !strings.Contains(str(verify["nextAction"]), head) {
		t.Fatalf("verify did not record the local commit %s: %q", head, str(verify["nextAction"]))
	}
	if got := s.git(worktree, "rev-parse", "--abbrev-ref", "HEAD"); got != branch {
		t.Fatalf("worktree is on %q, want %q", got, branch)
	}
	if parent := s.git(worktree, "rev-parse", "HEAD^"); parent != baseSHA {
		t.Fatalf("the preserved commit's parent is %s, want the base %s", parent, baseSHA)
	}
	if got := s.git(worktree, "show", "HEAD:src/cycle.txt") + "\n"; got != deliverable {
		t.Fatalf("committed src/cycle.txt = %q, want %q", got, deliverable)
	}
	_ = s.git(worktree, "cat-file", "-e", "HEAD:src/cycle.go")
	if dirty := s.git(worktree, "status", "--porcelain"); dirty != "" {
		t.Fatalf("the worktree still has uncommitted changes after completion:\n%s", dirty)
	}
	// The person's own checkout is untouched: AO worked in its worktree.
	if got := s.git(s.repo, "rev-parse", "HEAD"); got != baseSHA {
		t.Fatalf("the project checkout moved from %s to %s", baseSHA, got)
	}
	if dirty := s.git(s.repo, "status", "--porcelain"); dirty != "" {
		t.Fatalf("the project checkout was modified:\n%s", dirty)
	}
	if _, err := os.Stat(filepath.Join(s.repo, "src", "cycle.txt")); !os.IsNotExist(err) {
		t.Fatalf("the deliverable leaked into the project checkout (stat err %v)", err)
	}

	// ---- C10: durable state and runtime agree ----
	// completeRun reclaims the worker's runtime behind the completion CAS, so
	// the worker session must be gone from tmux and durably terminated.
	waitFor(t, 15*time.Second, "the completed run's worker runtime to be reclaimed", func() bool {
		for _, ts := range s.tmuxSessions() {
			if ts.Name == workerSession {
				return false
			}
		}
		return true
	})
	sess := s.mustDo(http.MethodGet, "/api/v1/sessions/"+workerSession, nil)
	if dig(sess, "session", "isTerminated") != true && dig(sess, "isTerminated") != true {
		b, _ := json.MarshalIndent(sess, "", "  ")
		t.Fatalf("worker runtime is gone but its session is not durably terminated:\n%.3000s", b)
	}

	// ---- C9: no orphan runtime survives ----
	// Anything still on the server must be AO-owned and finished, and AO's own
	// runtime GC must reclaim it. KNOWN GAP (reported, not hidden): completion
	// reclaims the worker immediately but a workflow reviewer's pane is left to
	// the GC sweep, which runs every 15 minutes. The sweep is triggered here
	// instead of waited for; before the tmux inventory fix it could not see the
	// reviewer at all and this assertion failed.
	for _, ts := range s.tmuxSessions() {
		if !strings.HasPrefix(ts.Env["AO_SESSION_OWNER"], "ao-reviewer:") {
			t.Fatalf("a non-reviewer runtime outlived the completed run: %+v", ts)
		}
		t.Logf("reviewer pane %s still alive %s after completion (reclaimed only by the GC sweep)", ts.Name, time.Since(completedAt).Round(time.Millisecond))
	}
	dry := s.mustDo(http.MethodPost, "/api/v1/runtime/gc", map[string]any{"dryRun": true})
	if n := len(s.tmuxSessions()); n > 0 {
		if c, _ := dry["candidates"].(float64); int(c) != n {
			b, _ := json.MarshalIndent(dry, "", "  ")
			t.Fatalf("%d runtime(s) outlived the run but GC proposes %v candidates:\n%s", n, dry["candidates"], b)
		}
	}
	s.mustDo(http.MethodPost, "/api/v1/runtime/gc", map[string]any{"dryRun": false})
	if left := s.tmuxSessions(); len(left) != 0 {
		t.Fatalf("tmux sessions survived the completed run and AO's GC sweep: %+v", left)
	}
	if final := str(dig(s.runDetail(runID), "workflow", "run", "state")); final != "completed" {
		t.Fatalf("run state changed to %q after runtime cleanup", final)
	}
}

// ---- scenario 2: the observable + ignored deliverable --------------------------

func TestIgnoredContractualDeliverableNeverSpawnsThroughRealTmux(t *testing.T) {
	s := newScratch(t)
	s.initRepo()
	s.startDaemon()
	s.registerProject()
	watch := s.watchTmux()

	// The audited BLOCKER shape: src/cycle.txt is observable, out/report.pdf is
	// required by Verification.Files and sits under the repository's `out/`
	// ignore rule. Both are required.
	runID := s.createRun(map[string]any{
		"objective":   "Write src/cycle.txt and the audit report out/report.pdf.",
		"strategy":    "task",
		"reviewDepth": "light",
		"placement":   "isolated_worktree",
		"writeIntent": "mutating",
		"verification": map[string]any{
			"commands": []map[string]any{{"command": "grep", "args": []string{"-q", "cycle-e2e-deliverable", "src/cycle.txt"}, "timeoutSeconds": 30, "requiredExitCode": 0}},
			"files": []map[string]any{
				{"path": "src/cycle.txt", "exists": true},
				{"path": "out/report.pdf", "exists": true},
			},
		},
	})
	code, body, err := s.do(http.MethodPost, "/api/v1/workflows/"+runID+"/start", map[string]any{})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	t.Logf("start: %d %.300s", code, body)

	final, history := s.drive(runID, time.Minute)
	if state := str(dig(final, "workflow", "run", "state")); state != "needs_attention" {
		t.Fatalf("run settled %q, want needs_attention: %v", state, history)
	}
	attempts := attemptsOf(stepOf(final, "work"))
	if len(attempts) == 0 {
		t.Fatal("the refused dispatch recorded no attempt")
	}
	if cls := str(attempts[len(attempts)-1]["errorClass"]); cls != "deliverable_not_observable" {
		t.Fatalf("work attempt error class %q, want deliverable_not_observable", cls)
	}

	// Operator presses continue: the refusal must hold, and still spawn nothing.
	_, _, _ = s.do(http.MethodPost, "/api/v1/workflows/"+runID+"/continue", map[string]any{})
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if state := str(dig(s.runDetail(runID), "workflow", "run", "state")); state == "completed" {
			t.Fatal("a run whose required deliverable git cannot see reported completed")
		}
		time.Sleep(500 * time.Millisecond)
	}
	for _, sig := range history {
		if strings.HasPrefix(sig, "completed") {
			t.Fatalf("the run passed through completed: %v", history)
		}
	}

	// Nothing was spawned: no agent ran, no tmux session ever existed.
	if n := len(s.traces("worker")) + len(s.traces("reviewer")); n != 0 {
		t.Fatalf("%d agent session(s) ran for a refused dispatch", n)
	}
	if seen := watch.Stop(); len(seen) != 0 {
		t.Fatalf("tmux sessions were created for a refused dispatch: %+v", seen)
	}
	if left := s.tmuxSessions(); len(left) != 0 {
		t.Fatalf("tmux sessions exist after a refused dispatch: %+v", left)
	}
	// And nothing was materialized: no AO branch carries a commit.
	if out := s.git(s.repo, "for-each-ref", "--format=%(refname)", "refs/heads/ao/"); out != "" {
		for _, ref := range strings.Split(out, "\n") {
			if s.git(s.repo, "rev-parse", ref) != s.git(s.repo, "rev-parse", "main") {
				t.Fatalf("AO branch %s moved for a refused dispatch", ref)
			}
		}
	}
}
