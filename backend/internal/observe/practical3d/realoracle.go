package practical3d

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
)

// HiddenTestManifest is the content-addressed description of one task's
// hidden oracle tests (Q4_oracle.task_oracles[].hidden_test_manifest_sha256).
type HiddenTestManifest struct {
	Schema string       `json:"schema"`
	Task   string       `json:"task"`
	Files  []HiddenFile `json:"files"`
}

// HiddenFile is one hidden test file and its digest.
type HiddenFile struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
}

// HiddenManifestSchema versions HiddenTestManifest.
const HiddenManifestSchema = "ao.3d-practical.hidden-tests.v1"

// BuildHiddenManifest digests <hiddenDir>/<task>/* deterministically.
func BuildHiddenManifest(hiddenDir, task string) ([]byte, error) {
	dir := filepath.Join(hiddenDir, task)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	m := HiddenTestManifest{Schema: HiddenManifestSchema, Task: task}
	for _, e := range entries {
		if !e.Type().IsRegular() {
			return nil, fmt.Errorf("hidden oracle %s/%s is not a regular file", task, e.Name())
		}
		b, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			return nil, err
		}
		m.Files = append(m.Files, HiddenFile{Path: e.Name(), SHA256: sha256Hex(b)})
	}
	sort.Slice(m.Files, func(i, j int) bool { return m.Files[i].Path < m.Files[j].Path })
	return CanonicalJSON(m)
}

// RealOracle evaluates Q1 (visible verification) and Q4 (hidden tests via
// the frozen oracle script) on the worker's final commit, in a fresh clone
// outside every agent's reach. It runs in the trusted supervisor, never
// inside the position sandbox.
type RealOracle struct {
	Script    string   // oracle.sh; its bytes are Q4_oracle.command_sha256
	HiddenDir string   // hidden tests, <dir>/<task>/*
	Verify    []string // Q1 command; SHA-256 of strings.Join(Verify, " ") is Q1_oracle.verify_command_sha256
	Manifest  Manifest
	Exec      *AORealExecutor
}

// Evaluate implements OracleRunner. Both oracle steps compile and run the
// worker's commit, i.e. agent-authored code: they run confined by the oracle
// sandbox (no network, no private state, no credentials) with a scrubbed
// environment, on a staged copy of the oracle whose bytes are verified before
// each step.
func (o RealOracle) Evaluate(ctx context.Context, pc PositionContext, _ ExecutionResult) (OracleResult, error) {
	var res OracleResult
	if sha256Hex([]byte(strings.Join(o.Verify, " "))) != o.Manifest.Q1Oracle.VerifyCommandSHA256 {
		return res, errors.New("verify command differs from the frozen Q1 oracle")
	}
	spec, err := o.Exec.spec(o.Manifest, pc.Position.TaskID)
	if err != nil {
		return res, err
	}
	var want string
	for _, t := range o.Manifest.Q4Oracle.TaskOracles {
		if t.TaskID == pc.Position.TaskID {
			want = t.HiddenTestManifestSHA256
		}
	}
	stage, err := stageOracle(o, spec.OracleTask, want)
	if stage != nil {
		defer stage.cleanup()
	}
	if err != nil {
		return res, err
	}
	commit, err := finalWorkerCommit(ctx, pc.Workspace)
	if err != nil {
		return res, err
	}
	clone := filepath.Join(stage.root, "clone")
	if out, err := exec.CommandContext(ctx, "git", "clone", "--quiet", "--no-hardlinks", pc.Workspace.WorkingCopy, clone).CombinedOutput(); err != nil {
		return res, fmt.Errorf("clone final tree: %w: %s", err, out)
	}
	if out, err := exec.CommandContext(ctx, "git", "-C", clone, "checkout", "--quiet", "--detach", commit).CombinedOutput(); err != nil {
		return res, fmt.Errorf("checkout final commit: %w: %s", err, out)
	}
	// The clone must not name the position it came from.
	if out, err := exec.CommandContext(ctx, "git", "-C", clone, "remote", "remove", "origin").CombinedOutput(); err != nil {
		return res, fmt.Errorf("detach clone: %w: %s", err, out)
	}
	q1, err := stage.command(ctx, o, false, clone, o.Verify...)
	if err != nil {
		return res, err
	}
	q1out, q1err := q1.CombinedOutput()
	res.Q1ExitCode = exitCode(q1err)
	// Q1 could not reach the staged oracle; verify the bytes Q4 will run.
	if err := stage.verify(o.Manifest, spec.OracleTask, want); err != nil {
		return res, err
	}
	q4, err := stage.command(ctx, o, true, stage.root, "/bin/bash", stage.script, spec.OracleTask, clone)
	if err != nil {
		return res, err
	}
	q4out, q4err := q4.CombinedOutput()
	lines := strings.Split(strings.TrimSpace(string(q4out)), "\n")
	last := lines[len(lines)-1]
	res.Q4Passed = q4err == nil && last == "ORACLE "+spec.OracleTask+" PASS"
	if err := stage.verify(o.Manifest, spec.OracleTask, want); err != nil {
		return res, fmt.Errorf("after Q4: %w", err)
	}
	raw, _ := json.Marshal(map[string]any{"final_commit": commit, "q1_exit": res.Q1ExitCode, "q1_output_tail": tail(string(q1out), 4000), "q4_last_line": last, "q4_output_tail": tail(string(q4out), 4000), "sandbox": "oracle.sb"})
	_ = writeExclusive(filepath.Join(pc.Workspace.Root, "oracle-evidence.json"), raw)
	res.Q1VerifyCommandSHA256 = o.Manifest.Q1Oracle.VerifyCommandSHA256
	res.Q4TaskOracleSHA256 = want
	res.Q4CommandSHA256 = o.Manifest.Q4Oracle.CommandSHA256
	return res, nil
}

func exitCode(err error) int {
	if err == nil {
		return 0
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return ee.ExitCode()
	}
	return -1
}

func tail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[len(s)-n:]
}

// finalWorkerCommit is the head of the worker session's branch in the
// position's project repository.
func finalWorkerCommit(ctx context.Context, w PositionWorkspace) (string, error) {
	db, err := sql.Open("sqlite", "file:"+url.PathEscape(filepath.Join(w.AODataDir, "ao.db"))+"?mode=ro&_pragma=busy_timeout(5000)")
	if err != nil {
		return "", err
	}
	defer func() { _ = db.Close() }()
	rows, err := db.QueryContext(ctx, `SELECT branch FROM sessions WHERE kind = 'worker' AND branch <> ''`)
	if err != nil {
		return "", err
	}
	defer func() { _ = rows.Close() }()
	var branches []string
	for rows.Next() {
		var b string
		if err := rows.Scan(&b); err != nil {
			return "", err
		}
		branches = append(branches, b)
	}
	if err := rows.Err(); err != nil {
		return "", err
	}
	if len(branches) != 1 {
		return "", fmt.Errorf("position has %d worker branches, want exactly 1", len(branches))
	}
	out, err := exec.CommandContext(ctx, "git", "-C", w.WorkingCopy, "rev-parse", "--verify", branches[0]+"^{commit}").Output()
	if err != nil {
		return "", fmt.Errorf("resolve worker branch %s: %w", branches[0], err)
	}
	return strings.TrimSpace(string(out)), nil
}

// CheckFixtureHistory fails if any hidden oracle test exists anywhere in the
// fixture repository's object database: agents read the working copy's full
// Git history (a deleted-but-committed hidden test would be a Q4 leak that
// hashing the frozen commit's tree cannot see).
func CheckFixtureHistory(ctx context.Context, repo, hiddenDir string) error {
	tasks, err := os.ReadDir(hiddenDir)
	if err != nil {
		return err
	}
	for _, t := range tasks {
		if !t.IsDir() {
			continue // notes next to the task directories
		}
		files, err := os.ReadDir(filepath.Join(hiddenDir, t.Name()))
		if err != nil {
			return err
		}
		for _, f := range files {
			p := filepath.Join(hiddenDir, t.Name(), f.Name())
			out, err := exec.CommandContext(ctx, "git", "-C", repo, "hash-object", "--", p).Output()
			if err != nil {
				return fmt.Errorf("hash hidden test %s: %w", p, err)
			}
			blob := strings.TrimSpace(string(out))
			if exec.CommandContext(ctx, "git", "-C", repo, "cat-file", "-e", blob).Run() == nil {
				return fmt.Errorf("hidden test %s/%s is present in the fixture repository's history", t.Name(), f.Name())
			}
		}
	}
	return nil
}
