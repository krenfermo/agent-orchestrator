package practical3d

import (
	"bytes"
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/observe/usage"
)

// TaskSpecSchema is the schema of the content-addressed task manifest
// artifact of a real run (manifest tasks[].task_manifest_sha256).
const TaskSpecSchema = "ao.3d-practical.task.v1"

// TaskSpec is what AO is asked to do for one task: a bounded AO task run.
type TaskSpec struct {
	Schema             string          `json:"schema"`
	TaskID             string          `json:"task_id"`
	Objective          string          `json:"objective"`
	ReviewDepth        string          `json:"review_depth"`
	WriteIntent        string          `json:"write_intent"`
	AcceptanceCriteria []string        `json:"acceptance_criteria"`
	Verification       json.RawMessage `json:"verification"`
	OracleTask         string          `json:"oracle_task"`
}

// ParseTaskSpec strictly decodes a task manifest artifact.
func ParseTaskSpec(raw []byte) (TaskSpec, error) {
	var t TaskSpec
	if err := strictUnmarshal(raw, &t); err != nil {
		return t, err
	}
	if t.Schema != TaskSpecSchema || t.TaskID == "" || strings.TrimSpace(t.Objective) == "" || t.OracleTask == "" || len(t.Verification) == 0 {
		return t, errors.New("task spec incomplete")
	}
	switch t.ReviewDepth {
	case "none", "light", "deep":
	default:
		return t, fmt.Errorf("review_depth %q", t.ReviewDepth)
	}
	return t, nil
}

// AORealConfig wires the real AO position executor.
type AORealConfig struct {
	AOBinary       string // frozen `ao` CLI binary (also exec'd by agents: must live under ToolsRO)
	RealClaude     string // real Claude Code executable
	RealCodex      string // real Codex CLI executable (reviewer)
	CodexModel     string // frozen Codex model
	OpenAIUpstream string // ChatGPT backend origin for Codex, e.g. https://chatgpt.com
	// AccountRefs is the frozen per-provider account reference map
	// (anthropic, openai) whose digest is provider.account_ref_sha256.
	AccountRefs    map[string]string
	ShimExecutable string // this harness binary; invoked as `claude`/`codex` it is the launch shim
	AOSrc          string // AO source tree (denied to agents)
	ToolsRO        string // read-only tools directory agents may exec from
	OracleDir      string // Q4 oracle script and hidden tests (denied to agents)
	Upstream       string // provider API origin, e.g. https://api.anthropic.com
	FixtureRepo    string // git repository holding the frozen fixture commit
	WebRoot        string // compiled (or placeholder) web assets for `ao server`
	ModelEnv       map[string]string
	DaemonTimeout  time.Duration
	RunTimeout     time.Duration
	SettleTimeout  time.Duration
	Log            io.Writer
}

// AORealExecutor runs one Practical position as a real AO task run: its own
// AO daemon and data dir, its agents confined by the Practical sandbox and
// reaching the provider only through the position's ProviderProxy.
type AORealExecutor struct {
	Cfg       AORealConfig
	Artifacts ArtifactResolver
}

// Preflight returns no representations: a real executor proves its initial
// cells by calibration (CalibrateInitial) instead.
func (e *AORealExecutor) Preflight(context.Context, Manifest) ([]CellRepresentation, error) {
	return nil, nil
}

func (e *AORealExecutor) logf(format string, args ...any) {
	if e.Cfg.Log != nil {
		_, _ = fmt.Fprintf(e.Cfg.Log, "[ao-real] "+format+"\n", args...)
	}
}

func (e *AORealExecutor) spec(m Manifest, task string) (TaskSpec, error) {
	for _, t := range m.Tasks {
		if t.TaskID != task {
			continue
		}
		raw, err := e.Artifacts.ReadDigest(t.TaskManifestSHA256)
		if err != nil {
			return TaskSpec{}, err
		}
		if sha256Hex(raw) != t.TaskManifestSHA256 {
			return TaskSpec{}, errors.New("task manifest digest mismatch")
		}
		s, err := ParseTaskSpec(raw)
		if err == nil && s.TaskID != task {
			err = errors.New("task spec names another task")
		}
		return s, err
	}
	return TaskSpec{}, fmt.Errorf("task %s not in manifest", task)
}

// positionRig is everything one real position (or calibration) runs with.
type positionRig struct {
	e          *AORealExecutor
	ctlDir     string
	daemonPort int
	// gateway is the agents' only route to the daemon (gatewayPort), found
	// through gwRunFile (AO_RUN_FILE).
	gateway     *DaemonGateway
	gatewayPort int
	gwRunFile   string
	proxyPort   int
	socket      string
	home        string
	tmp         string
	dataDir     string
	runFile     string
	work        string
	daemon      *exec.Cmd
	project     string
	runID       string
}

func freePort() (int, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer func() { _ = ln.Close() }()
	addr, ok := ln.Addr().(*net.TCPAddr)
	if !ok {
		return 0, errors.New("loopback listener has no TCP address")
	}
	return addr.Port, nil
}

func randHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func (e *AORealExecutor) newRig(w PositionWorkspace) (*positionRig, error) {
	ctl, err := os.MkdirTemp("/private/tmp", "ao3dp-")
	if err != nil {
		return nil, err
	}
	r := &positionRig{e: e, ctlDir: ctl, home: w.RuntimeHome, tmp: filepath.Join(w.Root, "tmp"), dataDir: w.AODataDir, runFile: filepath.Join(w.AODataDir, "running.json"), work: w.WorkingCopy, socket: "ao3dp-" + randHex(6)}
	for _, d := range []string{r.tmp, filepath.Join(r.home, "Library"), filepath.Join(r.home, ".claude")} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			return nil, err
		}
	}
	realHome, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	// Claude auth lives in the login keychain; the provider home is fresh.
	if err := os.Symlink(filepath.Join(realHome, "Library", "Keychains"), filepath.Join(r.home, "Library", "Keychains")); err != nil && !os.IsExist(err) {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Join(r.home, "codex-home"), 0o700); err != nil {
		return nil, err
	}
	// Codex authenticates with the operator's ChatGPT login, linked read-only.
	if err := os.Symlink(filepath.Join(realHome, ".codex", "auth.json"), filepath.Join(r.home, "codex-home", "auth.json")); err != nil && !os.IsExist(err) {
		return nil, err
	}
	if err := os.WriteFile(filepath.Join(r.home, ".claude.json"), []byte(`{"hasCompletedOnboarding":true,"officialMarketplaceAutoInstallAttempted":true}`), 0o600); err != nil {
		return nil, err
	}
	// Same neutral commit identity in every position (a fresh HOME has none).
	if err := os.WriteFile(filepath.Join(r.home, ".gitconfig"), []byte("[user]\n\tname = practical\n\temail = practical@example.invalid\n[commit]\n\tgpgsign = false\n"), 0o600); err != nil {
		return nil, err
	}
	if r.daemonPort, err = freePort(); err != nil {
		return nil, err
	}
	r.gateway = &DaemonGateway{DaemonPort: r.daemonPort}
	if r.gatewayPort, err = r.gateway.Start(); err != nil {
		return nil, err
	}
	r.gwRunFile = filepath.Join(r.home, "ao-running.json")
	return r, nil
}

func (r *positionRig) cleanup() {
	if r.gateway != nil {
		r.gateway.Close()
	}
	_ = exec.Command("tmux", "-L", r.socket, "kill-server").Run()
	_ = os.RemoveAll(r.ctlDir)
}

func (r *positionRig) writeShim(cfg ShimConfig) error {
	bin := filepath.Join(r.ctlDir, "bin")
	if err := os.MkdirAll(bin, 0o700); err != nil {
		return err
	}
	for _, name := range []string{"claude", "codex"} {
		if err := os.Symlink(r.e.Cfg.ShimExecutable, filepath.Join(bin, name)); err != nil {
			return err
		}
	}
	raw, err := json.Marshal(cfg)
	if err != nil {
		return err
	}
	return writeExclusive(filepath.Join(r.ctlDir, "shim.json"), raw)
}

func (r *positionRig) shimConfig(profile string, capture bool) ShimConfig {
	realHome, _ := os.UserHomeDir()
	aoHome := filepath.Join(realHome, ".ao")
	env := map[string]string{}
	for k, v := range r.e.Cfg.ModelEnv {
		env[k] = v
	}
	// Agents (and their hooks) find the daemon only through the gateway.
	env["AO_RUN_FILE"] = r.gwRunFile
	codexArgs := []string{"-c", `model_provider="p3d"`, "-c", `model_providers.p3d.name="p3d"`, "-c", `model_providers.p3d.base_url="{BASE}"`,
		"-c", `model_providers.p3d.wire_api="responses"`, "-c", "model_providers.p3d.requires_openai_auth=true", "-c", "model_providers.p3d.supports_websockets=false"}
	if r.e.Cfg.CodexModel != "" {
		codexArgs = append(codexArgs, "-c", `model="`+r.e.Cfg.CodexModel+`"`)
	}
	cfg := ShimConfig{RealClaude: r.e.Cfg.RealClaude, RealCodex: r.e.Cfg.RealCodex, CodexArgs: codexArgs, Profile: profile, ControlSocket: filepath.Join(r.ctlDir, "ctl.sock"), ExtraEnv: env,
		ClaudeArgs: []string{"--strict-mcp-config", "--disallowedTools=RemoteTrigger,SendMessage,ListAgents,WebFetch,WebSearch,Task,Agent"},
		LaunchLog:  filepath.Join(r.ctlDir, "launches.jsonl"),
		Sandbox: SandboxParams{AOHome: aoHome, RealHome: realHome, AOSrc: r.e.Cfg.AOSrc, PrivateCtl: r.ctlDir, ToolsRO: r.e.Cfg.ToolsRO, OracleDir: r.e.Cfg.OracleDir,
			PosWork: r.work, PosWorktrees: filepath.Join(r.dataDir, "worktrees"), PosHome: r.home, PosTmp: r.tmp, PosRunFile: r.runFile, PosPrompts: filepath.Join(r.dataDir, "prompts"), PosHookBin: filepath.Join(r.dataDir, "hook-bin"),
			ProxyPort: r.proxyPort, DaemonPort: r.gatewayPort}}
	if capture {
		cfg.Capture = filepath.Join(r.ctlDir, "captures")
	}
	return cfg
}

// startDaemon starts the position's own AO daemon: scratch data dir, own
// tmux socket, own provider home (shared with its agents, so AO's workspace
// trust record and transcript discovery see the same HOME), PATH resolving
// `claude`/`codex` to the launch shim, and the arm switch.
func (r *positionRig) startDaemon(ctx context.Context, arm Arm, task string) error {
	if err := RefuseProductionPath(r.dataDir); err != nil {
		return err
	}
	env := []string{"HOME=" + r.home, "USER=" + os.Getenv("USER"), "LOGNAME=" + os.Getenv("LOGNAME"), "SHELL=/bin/zsh", "LANG=en_US.UTF-8", "TERM=xterm-256color",
		"TMPDIR=" + r.tmp + "/",
		"PATH=" + filepath.Join(r.ctlDir, "bin") + ":" + filepath.Dir(r.e.Cfg.AOBinary) + ":/opt/homebrew/bin:/usr/local/bin:/usr/local/go/bin:/usr/bin:/bin:/usr/sbin:/sbin",
		"AO_DATA_DIR=" + r.dataDir, "AO_RUN_FILE=" + r.runFile, "AO_TRUSTED_LOCAL_MODE=on", "AO_AUTH_MODE=trusted_local",
		"AO_TMUX_SOCKET=" + r.socket, "AO_MEMORY_EXTERNAL=off", "AO_MEMORY_ROLES=" + memoryRolesFor(task), "CODEX_HOME=" + filepath.Join(r.home, "codex-home"), "AO_3DP_SHIM_CONFIG=" + filepath.Join(r.ctlDir, "shim.json"),
		"DISABLE_AUTOUPDATER=1", "CLAUDE_CODE_DISABLE_OFFICIAL_MARKETPLACE_AUTOINSTALL=1", "CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1", "ENABLE_CLAUDEAI_MCP_SERVERS=0", "GOTOOLCHAIN=local"}
	if arm == ArmAssisted {
		env = append(env, "AO_MEMORY_MODE=assisted")
	} else {
		env = append(env, "AO_MEMORY_MODE=off")
	}
	logf, err := os.OpenFile(filepath.Join(filepath.Dir(r.dataDir), "daemon.log"), os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	cmd := exec.Command(r.e.Cfg.AOBinary, "server", "--data-dir", r.dataDir, "--port", fmt.Sprint(r.daemonPort), "--web-root", r.e.Cfg.WebRoot)
	cmd.Env, cmd.Stdout, cmd.Stderr = env, logf, logf
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		_ = logf.Close()
		return err
	}
	r.daemon = cmd
	go func() { _ = cmd.Wait(); _ = logf.Close() }()
	deadline := time.Now().Add(r.e.Cfg.DaemonTimeout)
	for time.Now().Before(deadline) {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		resp, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d/healthz", r.daemonPort))
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				if err := WriteGatewayRunFile(r.runFile, r.gwRunFile, r.gatewayPort); err == nil {
					return nil
				}
			}
		}
		time.Sleep(time.Second)
	}
	return errors.New("AO daemon did not become healthy")
}

func (r *positionRig) stopDaemon() {
	if r.daemon == nil || r.daemon.Process == nil {
		return
	}
	req, _ := http.NewRequest(http.MethodPost, fmt.Sprintf("http://127.0.0.1:%d/shutdown", r.daemonPort), strings.NewReader("{}"))
	req.Header.Set("Content-Type", "application/json")
	if resp, err := (&http.Client{Timeout: 5 * time.Second}).Do(req); err == nil {
		_ = resp.Body.Close()
	}
	for i := 0; i < 60; i++ {
		if syscall.Kill(r.daemon.Process.Pid, 0) != nil {
			break
		}
		time.Sleep(time.Second)
	}
	_ = syscall.Kill(-r.daemon.Process.Pid, syscall.SIGKILL)
	_ = exec.Command("tmux", "-L", r.socket, "kill-server").Run()
}

func (r *positionRig) api(ctx context.Context, method, path string, body any, timeout time.Duration) (map[string]any, error) {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		rd = bytes.NewReader(b)
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, method, fmt.Sprintf("http://127.0.0.1:%d/api/v1%s", r.daemonPort, path), rd)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if resp.StatusCode >= 300 {
		return nil, fmt.Errorf("%s %s: %s: %s", method, path, resp.Status, strings.TrimSpace(string(raw)))
	}
	out := map[string]any{}
	if len(bytes.TrimSpace(raw)) > 0 {
		if err := json.Unmarshal(raw, &out); err != nil {
			return nil, fmt.Errorf("%s %s: %w", method, path, err)
		}
	}
	return out, nil
}

// startTaskRun registers the fixture as a project, rebuilds Project Memory
// (both arms: identical steps; only the arm switch decides use) and creates
// the bounded AO task run.
func (r *positionRig) startTaskRun(ctx context.Context, spec TaskSpec) error {
	_, _ = r.api(ctx, http.MethodPost, "/auth/register", map[string]string{"displayName": "practical", "email": "practical@example.invalid", "password": "practical-" + randHex(8)}, 30*time.Second)
	// A fixed project id: AO derives session/branch names and the Project
	// Memory relevance keywords from the prompt, so a random id would make
	// the ASSISTED attachment differ between positions of the same task.
	r.project = "practical-" + strings.ToLower(spec.TaskID)
	// AO's bootstrap execution policy (no stored policy) already routes the
	// worker to Claude Code and, for high-risk tasks, the reviewer to Codex
	// (cross-provider review independence); the manifest freezes that flow.
	if _, err := r.api(ctx, http.MethodPost, "/projects", map[string]string{"path": r.work, "projectId": r.project}, 60*time.Second); err != nil {
		return err
	}
	if _, err := r.api(ctx, http.MethodPost, "/projects/"+r.project+"/memory/rebuild", map[string]any{}, 300*time.Second); err != nil {
		return err
	}
	body := map[string]any{"projectId": r.project, "objective": spec.Objective, "strategy": "task", "approvalPolicy": "automatic", "autonomous": true,
		"writeIntent": spec.WriteIntent, "acceptanceCriteria": spec.AcceptanceCriteria, "verification": spec.Verification, "reviewDepth": spec.ReviewDepth}
	created, err := r.api(ctx, http.MethodPost, "/projects/"+r.project+"/workflows", body, 60*time.Second)
	if err != nil {
		return err
	}
	r.runID = findRunID(created)
	if r.runID == "" {
		return errors.New("workflow creation returned no run id")
	}
	return nil
}

func findRunID(v map[string]any) string {
	wf, _ := v["workflow"].(map[string]any)
	if wf == nil {
		wf = v
	}
	if run, ok := wf["run"].(map[string]any); ok {
		if id, ok := run["id"].(string); ok {
			return id
		}
	}
	id, _ := wf["id"].(string)
	return id
}

var terminalRunStates = map[string]bool{"completed": true, "failed": true, "cancelled": true, "canceled": true, "needs_attention": true, "blocked": true, "abandoned": true}

// waitRun polls the run until a terminal state or the context ends.
func (r *positionRig) waitRun(ctx context.Context) (string, map[string]any) {
	var last map[string]any
	for {
		res, err := r.api(ctx, http.MethodGet, "/workflows/"+r.runID, nil, 30*time.Second)
		if err == nil {
			last = res
			wf, _ := res["workflow"].(map[string]any)
			if wf == nil {
				wf = res
			}
			run, _ := wf["run"].(map[string]any)
			if run == nil {
				run = wf
			}
			state, _ := run["state"].(string)
			next, _ := wf["nextAction"].(string)
			if next == "" {
				next, _ = run["nextAction"].(string)
			}
			if terminalRunStates[state] || next == "human_attention" || next == "needs_attention" {
				return state, last
			}
		}
		select {
		case <-ctx.Done():
			return "timeout", last
		case <-time.After(5 * time.Second):
		}
	}
}

// waitSettled waits until AO's exploration read model reports complete tool
// coverage and stops changing, so 3C has ingested every transcript byte.
func (r *positionRig) waitSettled(ctx context.Context) bool {
	deadline := time.Now().Add(r.e.Cfg.SettleTimeout)
	prev, stable := "", 0
	for time.Now().Before(deadline) {
		res, err := r.api(ctx, http.MethodGet, "/workflows/"+r.runID+"/exploration", nil, 30*time.Second)
		if err == nil {
			totals, _ := res["totals"].(map[string]any)
			cov, _ := totals["toolCoverage"].(map[string]any)
			complete, _ := cov["complete"].(bool)
			sig, _ := json.Marshal(res["agents"])
			if complete && string(sig) == prev {
				stable++
				if stable >= 3 {
					return true
				}
			} else {
				stable = 0
			}
			prev = string(sig)
		}
		time.Sleep(5 * time.Second)
	}
	return false
}

// Execute runs one position for real. Agent processes are confined by the
// Practical sandbox and reach the provider only through the position proxy,
// which records every attempt into the ledger through the client. The
// exploration metric is derived afterwards from AO's 3C observations,
// cross-checked against the proxy; nothing the agents write is trusted.
func (e *AORealExecutor) Execute(ctx context.Context, pc PositionContext, c *ObservedClient) (ExecutionResult, error) {
	res := ExecutionResult{TerminalState: StateMalformedResult}
	spec, err := e.spec(c.m, pc.Position.TaskID)
	if err != nil {
		return res, err
	}
	r, err := e.newRig(pc.Workspace)
	if err != nil {
		return res, err
	}
	defer r.cleanup()
	proxy, err := NewProviderProxy(e.Cfg.Upstream, dbRoleResolver{dataDir: r.dataDir}, filepath.Join(pc.Workspace.Root, "provider-evidence"))
	if err != nil {
		return res, err
	}
	if e.Cfg.OpenAIUpstream != "" {
		if proxy.OpenAI, err = url.Parse(e.Cfg.OpenAIUpstream); err != nil {
			return res, err
		}
	}
	proxy.AccountRefs = e.Cfg.AccountRefs
	if r.proxyPort, err = proxy.Start(filepath.Join(r.ctlDir, "ctl.sock")); err != nil {
		return res, err
	}
	defer func() { _ = proxy.Close() }()
	proxy.Bind(c)
	defer proxy.Unbind()
	profile, err := WriteSandboxProfile(r.ctlDir)
	if err != nil {
		return res, err
	}
	if err := r.writeShim(r.shimConfig(profile, false)); err != nil {
		return res, err
	}
	if err := r.startDaemon(ctx, pc.Position.Arm, pc.Position.TaskID); err != nil {
		return res, err
	}
	defer r.stopDaemon()
	if err := r.startTaskRun(ctx, spec); err != nil {
		return res, err
	}
	e.logf("position %d %s/%s run %s", pc.Position.PositionIndex, pc.Position.TaskID, pc.Position.Arm, r.runID)
	state, detail := r.waitRun(ctx)
	raw, _ := json.Marshal(detail)
	_ = writeExclusive(filepath.Join(pc.Workspace.Root, "run-detail.json"), raw)
	if ctx.Err() != nil {
		return res, nil // the runner classifies the position deadline as TIMEOUT
	}
	if !r.waitSettled(ctx) {
		return res, errors.New("AO exploration telemetry did not settle (3C coverage incomplete)")
	}
	r.stopDaemon()
	proxy.Unbind()
	if rej := proxy.Rejected(); len(rej) > 0 {
		return res, fmt.Errorf("provider proxy refused %d request(s): %s", len(rej), rej[0])
	}
	// Refused daemon requests reached nothing; they are kept as evidence.
	if refused := r.gateway.Refused(); len(refused) > 0 {
		raw, _ := json.Marshal(refused)
		_ = writeExclusive(filepath.Join(pc.Workspace.Root, "daemon-gateway-refused.json"), raw)
	}
	obs := proxy.Observations()
	if err := checkNoUnobservedProviderCalls(ctx, r.dataDir, r.runID, obs); err != nil {
		return res, err
	}
	if err := checkRunContext(ctx, r.dataDir, r.runID, pc.Position.Arm, c.m, c.p.TaskID); err != nil {
		return res, err
	}
	measured := RoleWorker
	if m3, ok := m3Cap(c.m, pc.Position.TaskID); ok {
		measured = m3.Role
	}
	ev, err := DeriveM3(ctx, M3Input{DataDir: r.dataDir, RunID: r.runID, MeasuredRole: measured, Proxy: obs, ToolResults: proxy.ToolResults(), ProjectRoots: []string{r.work, filepath.Join(r.dataDir, "worktrees")}})
	evRaw, _ := json.Marshal(map[string]any{"m3": ev, "error": errString(err), "proxy_observations": obs, "run_state": state})
	_ = writeExclusive(filepath.Join(pc.Workspace.Root, "m3-evidence.json"), evRaw)
	if err != nil {
		return res, fmt.Errorf("M3: %w", err)
	}
	res.ExplorationCalls, res.DistinctFilesRead, res.MilestoneObserved = ev.Calls, ev.Files, true
	switch state {
	case "completed":
		res.TerminalState = StateCompleted
	default:
		res.TerminalState = StateFailedWorker
	}
	return res, nil
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// dbRoleResolver resolves a subject's role from the position database.
type dbRoleResolver struct{ dataDir string }

// ResolveRole reads AO's usage_attribution_windows (read-only).
func (d dbRoleResolver) ResolveRole(ctx context.Context, subject string, at time.Time) (Role, error) {
	kind, id, ok := strings.Cut(subject, ":")
	if !ok || kind == "" || id == "" {
		return "", fmt.Errorf("malformed subject %q", subject)
	}
	db, err := sql.Open("sqlite", "file:"+url.PathEscape(filepath.Join(d.dataDir, "ao.db"))+"?mode=ro&_pragma=busy_timeout(5000)")
	if err != nil {
		return "", err
	}
	defer func() { _ = db.Close() }()
	var role string
	// Timestamps are compared as instants: SQLite stores them as text, so the
	// newest window at or before `at` is chosen in Go after a bounded read.
	rows, err := db.QueryContext(ctx, `SELECT role, opened_at FROM usage_attribution_windows WHERE subject_kind = ? AND session_id = ? ORDER BY id`, kind, id)
	if err != nil {
		return "", err
	}
	defer func() { _ = rows.Close() }()
	var best time.Time
	for rows.Next() {
		var rl string
		var opened time.Time
		if err := rows.Scan(&rl, &opened); err != nil {
			return "", err
		}
		if !opened.After(at) && !opened.Before(best) {
			best, role = opened, rl
		}
	}
	if err := rows.Err(); err != nil {
		return "", err
	}
	switch role {
	case "worker":
		return RoleWorker, nil
	case "fix_worker":
		return RoleRepair, nil
	case "reviewer":
		return RoleReviewer, nil
	case "":
		return "", fmt.Errorf("no AO role window for %s at %s", subject, at.UTC().Format(time.RFC3339Nano))
	}
	return "", fmt.Errorf("AO role %q is outside the Practical role set", role)
}

// checkNoUnobservedProviderCalls fails closed if AO's own usage ledger for
// the run holds a provider message the proxy never saw: some launch reached
// the provider by a route other than the observed boundary.
func checkNoUnobservedProviderCalls(ctx context.Context, dataDir, runID string, proxy []ProxyObservation) error {
	db, err := sql.Open("sqlite", "file:"+url.PathEscape(filepath.Join(dataDir, "ao.db"))+"?mode=ro&_pragma=busy_timeout(5000)")
	if err != nil {
		return err
	}
	defer func() { _ = db.Close() }()
	seen := map[string]bool{}
	rows, err := db.QueryContext(ctx, `SELECT b.harness, b.native_root_id, COALESCE(s.kind,''), COALESCE(s.subagent_id,''), COALESCE(s.native_session_id,''), e.source_event_key
		FROM model_usage_events e JOIN usage_bindings b ON b.id = e.binding_id LEFT JOIN usage_sources s ON s.id = e.usage_source_id
		WHERE b.subject_kind || char(31) || b.subject_id IN (SELECT subject_kind || char(31) || session_id FROM usage_attribution_windows WHERE workflow_run_id = ?)`, runID)
	if err != nil {
		return fmt.Errorf("read usage ledger: %w", err)
	}
	defer func() { _ = rows.Close() }()
	type ev struct{ harness, root, kind, sub, native, key string }
	var events []ev
	for rows.Next() {
		var x ev
		if err := rows.Scan(&x.harness, &x.root, &x.kind, &x.sub, &x.native, &x.key); err != nil {
			return err
		}
		events = append(events, x)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("read usage ledger: %w", err)
	}
	for _, x := range events {
		if x.harness == "codex" {
			continue // cumulative-delta rows: checked by totals below
		}
		if x.harness != "claude-code" {
			return fmt.Errorf("AO recorded %s usage the Practical boundary cannot observe", x.harness)
		}
		for _, p := range proxy {
			if p.MessageID != "" && usage.ClaudeMessageEventKey(x.root, domain.UsageSourceKind(x.kind), x.sub, x.native, p.MessageID) == x.key {
				seen[x.key] = true
			}
		}
		if !seen[x.key] {
			return fmt.Errorf("AO usage event %s has no proxy-observed provider message", x.key)
		}
	}
	if len(events) == 0 {
		return errors.New("AO recorded no provider usage for the run")
	}
	return checkCodexTotals(ctx, db, runID, proxy)
}

// checkCodexTotals: AO meters Codex from cumulative token_count deltas, not
// per-response ids, so the join is by totals per subject: every input token
// AO recorded for a Codex subject must have crossed the proxy.
func checkCodexTotals(ctx context.Context, db *sql.DB, runID string, proxy []ProxyObservation) error {
	rows, err := db.QueryContext(ctx, `SELECT b.subject_kind || ':' || b.subject_id, COALESCE(SUM(e.input_tokens), 0)
		FROM model_usage_events e JOIN usage_bindings b ON b.id = e.binding_id
		WHERE b.harness = 'codex' AND b.subject_kind || char(31) || b.subject_id IN (SELECT subject_kind || char(31) || session_id FROM usage_attribution_windows WHERE workflow_run_id = ?)
		GROUP BY 1`, runID)
	if err != nil {
		return fmt.Errorf("read codex usage: %w", err)
	}
	defer func() { _ = rows.Close() }()
	observed := map[string]int64{}
	for _, p := range proxy {
		observed[p.Subject] += p.InputTokens
	}
	for rows.Next() {
		var subject string
		var total int64
		if err := rows.Scan(&subject, &total); err != nil {
			return err
		}
		if total > observed[subject] {
			return fmt.Errorf("AO recorded %d Codex input tokens for %s but only %d crossed the proxy", total, subject, observed[subject])
		}
	}
	return rows.Err()
}

// checkRunContext verifies AO's own record of the run's context sources and
// Project Memory provenance: external context off in both arms; a worker
// memory manifest only in ASSISTED, whose rendered pack is inside the frozen
// attachment.
func checkRunContext(ctx context.Context, dataDir, runID string, arm Arm, m Manifest, task string) error {
	db, err := sql.Open("sqlite", "file:"+url.PathEscape(filepath.Join(dataDir, "ao.db"))+"?mode=ro&_pragma=busy_timeout(5000)")
	if err != nil {
		return err
	}
	defer func() { _ = db.Close() }()
	var ext sql.NullString
	if err := db.QueryRowContext(ctx, `SELECT json_extract(policy_snapshot,'$.contextSources.externalContext') FROM workflow_runs WHERE id = ?`, runID).Scan(&ext); err != nil {
		return fmt.Errorf("read run context sources: %w", err)
	}
	if ext.String != "off" {
		return fmt.Errorf("AO externalContext is %q, want off", ext.String)
	}
	var n int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM project_memory_context_manifests WHERE workflow_run_id = ? AND pack_digest <> ''`, runID).Scan(&n); err != nil {
		return fmt.Errorf("read memory manifests: %w", err)
	}
	if arm == ArmOff && n != 0 {
		return fmt.Errorf("OFF run has %d Project Memory context manifests", n)
	}
	if arm == ArmAssisted && n == 0 {
		return errors.New("ASSISTED run has no Project Memory context manifest")
	}
	if arm != ArmAssisted {
		return nil
	}
	// Every pack AO rendered must be the frozen one, for the targeted role.
	target := measuredRoleFor(task)
	cell, ok := treatmentCell(m, task, target, CallInitial)
	if !ok || cell.ASSISTED.AttachmentPresent == nil || !*cell.ASSISTED.AttachmentPresent {
		return fmt.Errorf("task %s has no ASSISTED attachment for %s", task, target)
	}
	want := strings.TrimPrefix(cell.ASSISTED.AttachmentVersion, "ao-project-memory-pack:")
	rows, err := db.QueryContext(ctx, `SELECT role, pack_digest FROM project_memory_context_manifests WHERE workflow_run_id = ? AND pack_digest <> ''`, runID)
	if err != nil {
		return fmt.Errorf("read memory manifests: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var role, digest string
		if err := rows.Scan(&role, &digest); err != nil {
			return err
		}
		if role != aoRole(target) {
			return fmt.Errorf("ASSISTED run rendered Project Memory for AO role %q; only %s is targeted", role, target)
		}
		if digest != want {
			return fmt.Errorf("ASSISTED run rendered pack %s, frozen pack is %s", digest, want)
		}
	}
	return rows.Err()
}
