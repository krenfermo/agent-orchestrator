// Command ao3dpractical runs the frozen 3D-PRACTICAL harness
// (docs/frente3/3d-practical.md, 06-benchmark-plan.md). It never opens AO's
// database, refuses run roots outside ~/.ao/scratch/frente3, and refuses an
// inherited AO_DATA_DIR/AO_RUN_FILE that points at production state.
package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/observe/practical3d"
)

func main() {
	// Invoked as `claude`/`codex` from a position daemon's PATH, this binary
	// is the Practical launch shim (before any other processing).
	if name := filepath.Base(os.Args[0]); name == "claude" || name == "codex" {
		os.Exit(practical3d.RunShim(name, os.Args[1:], os.Stderr, syscall.Exec))
	}
	if err := run(os.Args[1:], os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "ao3dpractical:", err)
		os.Exit(1)
	}
}

const usage = "usage: ao3dpractical freeze|validate|schedule|run|decide|mini-e2e [flags]"

func run(args []string, out io.Writer) error {
	if len(args) == 0 {
		return errors.New(usage)
	}
	if err := practical3d.RefuseProductionEnvironment(); err != nil {
		return err
	}
	switch args[0] {
	case "freeze":
		return freezeCommand(args[1:], out)
	case "validate":
		return validateCommand(args[1:], out)
	case "schedule":
		return scheduleCommand(args[1:], out)
	case "run":
		return runCommand(args[1:], out)
	case "decide":
		return decideCommand(args[1:], out)
	case "mini-e2e":
		return miniCommand(args[1:], out)
	case "calibrate":
		return calibrateCommand(args[1:], out)
	case "mini-e2e-real":
		return miniRealCommand(args[1:], out)
	case "technical-driver":
		return technicalDriver(args[1:], os.Stdin, out)
	default:
		return fmt.Errorf("unknown command %q; %s", args[0], usage)
	}
}

// freezeCommand turns a complete draft manifest into a frozen envelope. The
// only fields it fills are the ones that must not be chosen by hand: the
// 32-byte root seed (OS CSPRNG, drawn exactly once), the schedule regenerated
// from it, and the constant schema digest. Everything else must already be
// explicit in the draft; the result is validated before it is written.
func freezeCommand(args []string, out io.Writer) error {
	fs := flag.NewFlagSet("freeze", flag.ContinueOnError)
	fs.SetOutput(out)
	draftPath := fs.String("draft", "", "draft manifest with empty randomization.seed_hex and schedule")
	outPath := fs.String("out", "", "new envelope path (must not exist)")
	label := fs.String("label", "", "human label (metadata only; not part of identity)")
	version := fs.String("version", "", "human version (metadata only; not part of identity)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *draftPath == "" || *outPath == "" {
		return errors.New("--draft and --out are required")
	}
	raw, err := os.ReadFile(*draftPath)
	if err != nil {
		return err
	}
	if err := practical3d.RefuseProductionPath(*outPath); err != nil {
		return err
	}
	m, err := practical3d.DecodeDraftManifest(raw)
	if err != nil {
		return fmt.Errorf("draft: %w", err)
	}
	if m.Randomization.SeedHex != "" || len(m.Randomization.Schedule) != 0 {
		return errors.New("draft must not carry a seed or schedule; the seed is drawn once at freeze")
	}
	seed := make([]byte, 32)
	if _, err := rand.Read(seed); err != nil {
		return err
	}
	m.Randomization.SeedHex = hex.EncodeToString(seed)
	m.ManifestSchemaSHA256 = practical3d.ExpectedManifestSchemaSHA256
	if m.Randomization.Schedule, err = practical3d.GenerateSchedule(m.Randomization); err != nil {
		return err
	}
	canonical, err := practical3d.CanonicalManifest(m)
	if err != nil {
		return err
	}
	if _, err := practical3d.DecodeManifest(canonical); err != nil {
		return err
	}
	id := sha256Hex(canonical)
	env := practical3d.Envelope{ExperimentID: id, ManifestSHA256: id, Manifest: canonical, Metadata: metadata(*label, *version)}
	b, err := json.Marshal(env)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(*outPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o400)
	if err != nil {
		return err
	}
	if _, err = f.Write(append(b, '\n')); err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	_, _ = fmt.Fprintf(out, "FROZEN experiment_id=%s positions=%d envelope=%s\n", id, len(m.Randomization.Schedule), *outPath)
	return nil
}

func metadata(label, version string) practical3d.EnvelopeMetadata {
	var md practical3d.EnvelopeMetadata
	if label != "" {
		md.HumanLabel = &label
	}
	if version != "" {
		md.HumanVersion = &version
	}
	return md
}

func validateCommand(args []string, out io.Writer) error {
	fs := flag.NewFlagSet("validate", flag.ContinueOnError)
	fs.SetOutput(out)
	path := fs.String("envelope", "", "frozen envelope path")
	if err := fs.Parse(args); err != nil {
		return err
	}
	env, m, err := practical3d.ReadEnvelope(*path)
	if err != nil {
		return err
	}
	_, _ = fmt.Fprintf(out, "VALID experiment_id=%s positions=%d\n", env.ExperimentID, len(m.Randomization.Schedule))
	return nil
}

func scheduleCommand(args []string, out io.Writer) error {
	fs := flag.NewFlagSet("schedule", flag.ContinueOnError)
	fs.SetOutput(out)
	path := fs.String("envelope", "", "frozen envelope path")
	if err := fs.Parse(args); err != nil {
		return err
	}
	_, m, err := practical3d.ReadEnvelope(*path)
	if err != nil {
		return err
	}
	for _, p := range m.Randomization.Schedule {
		_, _ = fmt.Fprintf(out, "%02d %s pair=%d %-8s order=%s/%s sample=%s\n", p.PositionIndex, p.TaskID, p.PairIndex, p.Arm, p.PairOrder[0], p.PairOrder[1], p.SampleID)
	}
	return nil
}

// allowTempDecide lets in-package tests decide runs below os.TempDir().
var allowTempDecide = false

func decideCommand(args []string, out io.Writer) error {
	fs := flag.NewFlagSet("decide", flag.ContinueOnError)
	fs.SetOutput(out)
	runRoot := fs.String("run-root", "", "run directory containing envelope.json and ledger.jsonl")
	reportPath := fs.String("report", "", "new report path (must not exist)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *runRoot == "" || *reportPath == "" {
		return errors.New("--run-root and --report are required")
	}
	if err := practical3d.RefuseProductionPath(*reportPath); err != nil {
		return err
	}
	report, err := practical3d.DecideRun(*runRoot, allowTempDecide, time.Now().UTC())
	if err != nil {
		return err
	}
	if err := practical3d.WriteReport(*reportPath, report); err != nil {
		return err
	}
	_, _ = fmt.Fprintf(out, "%s %s %s\nreport: %s\n", report.Verdict, report.ReasonCode, report.Reason, *reportPath)
	return nil
}

type stringList []string

func (s *stringList) String() string     { return strings.Join(*s, ",") }
func (s *stringList) Set(v string) error { *s = append(*s, v); return nil }

// runCommand executes the official frozen schedule. It is the only command
// that can start official positions, and it runs them through the same real
// AO executor, oracle and workspaces as mini-e2e-real (realRunnerOptions):
// there is no second implementation. --plan prints what it would execute and
// stops before any position starts.
func runCommand(args []string, out io.Writer) error {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	fs.SetOutput(out)
	envelopePath := fs.String("envelope", "", "frozen envelope path")
	root := fs.String("run-root", "", "new run directory below ~/.ao/scratch/frente3")
	artifacts := fs.String("artifact-root", "", "root of content-addressed artifacts (<root>/sha256/<digest>) and treatment refs")
	plan := fs.Bool("plan", false, "print the frozen schedule and the executor that would run it; start nothing")
	rf := addRealFlags(fs)
	if err := fs.Parse(args); err != nil {
		return err
	}
	env, m, err := practical3d.ReadEnvelope(*envelopePath)
	if err != nil {
		return err
	}
	if *artifacts == "" || *root == "" {
		return errors.New("--run-root and --artifact-root are required")
	}
	primary, helper, codex, err := practical3d.ManifestModels(m)
	if err != nil {
		return err
	}
	cfg, creds, err := rf.executorConfig(realModels{primary: primary, helper: helper, codex: codex}, out)
	if err != nil {
		return err
	}
	executor := &practical3d.AORealExecutor{Cfg: cfg}
	observer := practical3d.LiveEnvironmentObserver{Expected: m.ExecutionEnvironment.Inputs, AOBinaryPath: cfg.AOBinary}
	opts := realRunnerOptions(m, rf, executor, observer, practical3d.DirArtifactResolver{Root: *artifacts}, *root, env.Metadata)
	if *plan {
		return printPlan(out, m, opts)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	// The accounts the injected credentials select must be the frozen ones.
	accounts, err := liveAccountRefs(ctx, rf, creds, helper, *root+".account")
	if err != nil {
		return err
	}
	if ref, err := practical3d.AccountRefSet(accounts); err != nil || ref != m.Provider.AccountRefSHA256 {
		return errors.New("the operator's provider accounts differ from the frozen account reference")
	}
	executor.Cfg.AccountRefs = accounts
	res, err := practical3d.Run(ctx, m, opts)
	if err != nil {
		return err
	}
	_, _ = fmt.Fprintf(out, "%s %s %s\nreport: %s\n", res.Report.Verdict, res.Report.ReasonCode, res.Report.Reason, filepath.Join(res.Root, "report.json"))
	return nil
}

// printPlan shows the frozen schedule and the concrete executor, oracle and
// workspace types the official run would use, without starting anything.
func printPlan(out io.Writer, m practical3d.Manifest, opts practical3d.RunnerOptions) error {
	_, _ = fmt.Fprintf(out, "executor=%T oracle=%T workspaces=%T transport=%T\n", opts.Executor, opts.Oracle, opts.Workspaces, opts.Transport)
	for _, p := range m.Randomization.Schedule {
		_, _ = fmt.Fprintf(out, "position %d task=%s arm=%s sample=%s\n", p.PositionIndex, p.TaskID, p.Arm, p.SampleID)
	}
	_, _ = fmt.Fprintf(out, "positions=%d (plan only: nothing started)\n", len(m.Randomization.Schedule))
	return nil
}

func verifyListedBinary(path string, versions []practical3d.VersionInput) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	digest := sha256Hex(b)
	for _, v := range versions {
		if v.BinarySHA256 == digest {
			return nil
		}
	}
	return fmt.Errorf("driver %s digest %s is not in the frozen execution environment", path, digest)
}

// positionEnv is the complete environment of a position/oracle driver: no
// provider credential, and every state path inside the position root.
func positionEnv(w practical3d.PositionWorkspace, marker string) []string {
	return []string{"PATH=" + os.Getenv("PATH"), "HOME=" + w.RuntimeHome, "AO_DATA_DIR=" + w.AODataDir, "AO_RUN_FILE=" + filepath.Join(w.AODataDir, "running.json"), "TMPDIR=" + w.Root, marker + "=1"}
}

type commandTransport struct {
	path     string
	args     []string
	envNames []string
}

func (t commandTransport) Do(ctx context.Context, request practical3d.TransportRequest) (practical3d.ProviderResponse, error) {
	cmd := exec.CommandContext(ctx, t.path, t.args...)
	inGroup(cmd)
	cmd.Dir = request.Workspace.Root
	cmd.Stdin = bytes.NewReader(request.CanonicalRequest)
	env := positionEnv(request.Workspace, "AO_3D_PRACTICAL_PROVIDER")
	env, err := withEnv(env, t.envNames)
	if err != nil {
		return practical3d.ProviderResponse{}, err
	}
	cmd.Env = env
	raw, err := cmd.Output()
	_ = killGroup(cmd)
	if err != nil {
		return practical3d.ProviderResponse{}, err
	}
	var response practical3d.ProviderResponse
	if err := decodeStrict(raw, &response); err != nil {
		return practical3d.ProviderResponse{}, err
	}
	return response, nil
}

// driverMessage is one JSONL line of the position-driver protocol:
//
//	runner → driver: {"type":"preflight","manifest":…} | {"type":"start","context":…} | {"type":"call_result",…}
//	driver → runner: {"type":"preflight_result","representations":[…]} | {"type":"call","request":…} | {"type":"result","result":…}
type driverMessage struct {
	Type            string                           `json:"type"`
	Manifest        *practical3d.Manifest            `json:"manifest,omitempty"`
	Context         *practical3d.PositionContext     `json:"context,omitempty"`
	Request         *practical3d.ProviderRequest     `json:"request,omitempty"`
	Response        *practical3d.ProviderResponse    `json:"response,omitempty"`
	Result          *practical3d.ExecutionResult     `json:"result,omitempty"`
	Representations []practical3d.CellRepresentation `json:"representations,omitempty"`
	Error           string                           `json:"error,omitempty"`
	TerminalState   practical3d.TerminalState        `json:"terminal_state,omitempty"`
}

type commandExecutor struct {
	path     string
	args     []string
	envNames []string
}

// reservedEnv are the per-position variables no allowlisted or provider env
// entry may override.
var reservedEnv = map[string]bool{"PATH": true, "HOME": true, "AO_DATA_DIR": true, "AO_RUN_FILE": true, "TMPDIR": true}

// allowlistedEnv are the environment variables whose effective values the
// frozen environment digest attests; drivers receive exactly those values.
func allowlistedEnv(m practical3d.Manifest) []string {
	in := m.ExecutionEnvironment.Inputs
	names := make([]string, 0, len(in.EffectiveEnvironmentConfigAllowlist)+len(in.AdditionalLocalConfiguration))
	for _, c := range m.ExecutionEnvironment.Inputs.EffectiveEnvironmentConfigAllowlist {
		names = append(names, c.Name)
	}
	for _, c := range m.ExecutionEnvironment.Inputs.AdditionalLocalConfiguration {
		names = append(names, c.Name)
	}
	return names
}

func withEnv(env, names []string) ([]string, error) {
	for _, name := range names {
		if name == "PATH" {
			continue // positionEnv already passes the attested PATH
		}
		if name == "" || strings.ContainsAny(name, "=\x00") || reservedEnv[name] || strings.HasPrefix(name, "AO_3D_PRACTICAL") {
			return nil, fmt.Errorf("env name %q is invalid or reserved", name)
		}
		value, ok := os.LookupEnv(name)
		if !ok {
			return nil, fmt.Errorf("env %q is unset", name)
		}
		env = append(env, name+"="+value)
	}
	return env, nil
}

type driverSession struct {
	cmd     *exec.Cmd
	stdin   io.WriteCloser
	enc     *json.Encoder
	scanner *bufio.Scanner
	done    bool
}

func (e commandExecutor) start(ctx context.Context, dir string, env []string, stderrPath string) (*driverSession, error) {
	cmd := exec.CommandContext(ctx, e.path, e.args...)
	inGroup(cmd)
	cmd.Dir = dir
	cmd.Env = env
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	stderr, err := os.OpenFile(stderrPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return nil, err
	}
	cmd.Stderr = stderr
	if err := cmd.Start(); err != nil {
		_ = stderr.Close()
		return nil, err
	}
	_ = stderr.Close()
	s := bufio.NewScanner(stdout)
	s.Buffer(make([]byte, 64*1024), 64*1024*1024)
	return &driverSession{cmd: cmd, stdin: stdin, enc: json.NewEncoder(stdin), scanner: s}, nil
}

// close tears the driver's whole process group down and reaps it.
func (s *driverSession) close() error {
	_ = s.stdin.Close()
	_ = killGroup(s.cmd)
	err := s.cmd.Wait()
	if s.done {
		return nil
	}
	return err
}

func (s *driverSession) next() (driverMessage, error) {
	if !s.scanner.Scan() {
		if err := s.scanner.Err(); err != nil {
			return driverMessage{}, err
		}
		return driverMessage{}, errors.New("driver exited without a final message")
	}
	var msg driverMessage
	if err := decodeStrict(s.scanner.Bytes(), &msg); err != nil {
		return driverMessage{}, err
	}
	return msg, nil
}

func (e commandExecutor) Preflight(ctx context.Context, m practical3d.Manifest) ([]practical3d.CellRepresentation, error) {
	scratch, err := practical3d.ScratchRoot()
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(scratch, 0o700); err != nil {
		return nil, err
	}
	dir, err := os.MkdirTemp(scratch, "ao3dpractical-preflight-")
	if err != nil {
		return nil, err
	}
	defer func() { _ = os.RemoveAll(dir) }()
	w := practical3d.PositionWorkspace{Root: dir, AODataDir: filepath.Join(dir, "ao-data"), RuntimeHome: filepath.Join(dir, "home")}
	for _, d := range []string{w.AODataDir, w.RuntimeHome} {
		if err := os.Mkdir(d, 0o700); err != nil {
			return nil, err
		}
	}
	env, err := withEnv(positionEnv(w, "AO_3D_PRACTICAL_PREFLIGHT"), e.envNames)
	if err != nil {
		return nil, err
	}
	s, err := e.start(ctx, dir, env, filepath.Join(dir, "stderr"))
	if err != nil {
		return nil, err
	}
	defer func() { _ = s.close() }()
	if err := s.enc.Encode(driverMessage{Type: "preflight", Manifest: &m}); err != nil {
		return nil, err
	}
	msg, err := s.next()
	if err != nil {
		return nil, err
	}
	if msg.Type != "preflight_result" {
		return nil, fmt.Errorf("unexpected preflight reply %q", msg.Type)
	}
	s.done = true
	return msg.Representations, nil
}

func (e commandExecutor) Execute(ctx context.Context, pc practical3d.PositionContext, client *practical3d.ObservedClient) (practical3d.ExecutionResult, error) {
	env, err := withEnv(positionEnv(pc.Workspace, "AO_3D_PRACTICAL"), e.envNames)
	if err != nil {
		return practical3d.ExecutionResult{}, err
	}
	s, err := e.start(ctx, pc.Workspace.WorkingCopy, env, filepath.Join(pc.Workspace.Root, "position-driver.stderr"))
	if err != nil {
		return practical3d.ExecutionResult{}, err
	}
	result, runErr := e.converse(s, pc, client)
	if closeErr := s.close(); runErr == nil && closeErr != nil {
		runErr = closeErr
	}
	return result, runErr
}

func (e commandExecutor) converse(s *driverSession, pc practical3d.PositionContext, client *practical3d.ObservedClient) (practical3d.ExecutionResult, error) {
	if err := s.enc.Encode(driverMessage{Type: "start", Context: &pc}); err != nil {
		return practical3d.ExecutionResult{}, err
	}
	for {
		msg, err := s.next()
		if err != nil {
			return practical3d.ExecutionResult{}, err
		}
		switch msg.Type {
		case "call":
			if msg.Request == nil {
				return practical3d.ExecutionResult{}, errors.New("driver call missing request")
			}
			response, callErr := client.Call(*msg.Request)
			reply := driverMessage{Type: "call_result", Response: &response}
			if callErr != nil {
				reply.Error = callErr.Error()
				var ce *practical3d.CallError
				if errors.As(callErr, &ce) {
					reply.TerminalState = ce.State
				}
			}
			if err := s.enc.Encode(reply); err != nil {
				return practical3d.ExecutionResult{}, err
			}
		case "result":
			if msg.Result == nil {
				return practical3d.ExecutionResult{}, errors.New("driver result missing result")
			}
			s.done = true
			return *msg.Result, nil
		default:
			return practical3d.ExecutionResult{}, fmt.Errorf("unknown driver message %q", msg.Type)
		}
	}
}

type commandOracle struct {
	path     string
	args     []string
	manifest practical3d.Manifest
	envNames []string
}

type oracleInput struct {
	Context   practical3d.PositionContext `json:"context"`
	Execution practical3d.ExecutionResult `json:"execution"`
	Q1        practical3d.Q1Oracle        `json:"Q1_oracle"`
	Q4        practical3d.Q4Oracle        `json:"Q4_oracle"`
}

func (o commandOracle) Evaluate(ctx context.Context, pc practical3d.PositionContext, execution practical3d.ExecutionResult) (practical3d.OracleResult, error) {
	binary, err := os.ReadFile(o.path)
	if err != nil {
		return practical3d.OracleResult{}, err
	}
	if sha256Hex(binary) != o.manifest.Q4Oracle.RunnerImageOrBinarySHA256 {
		return practical3d.OracleResult{}, errors.New("Q4 oracle runner binary digest mismatch")
	}
	raw, err := json.Marshal(oracleInput{Context: pc, Execution: execution, Q1: o.manifest.Q1Oracle, Q4: o.manifest.Q4Oracle})
	if err != nil {
		return practical3d.OracleResult{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, time.Duration(o.manifest.Q4Oracle.TimeoutSeconds)*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, o.path, o.args...)
	inGroup(cmd)
	cmd.Dir = pc.Workspace.Root
	cmd.Stdin = bytes.NewReader(raw)
	env, err := withEnv(positionEnv(pc.Workspace, "AO_3D_PRACTICAL_ORACLE"), o.envNames)
	if err != nil {
		return practical3d.OracleResult{}, err
	}
	cmd.Env = env
	output, err := cmd.Output()
	_ = killGroup(cmd)
	if err != nil {
		return practical3d.OracleResult{}, err
	}
	var result practical3d.OracleResult
	if err := decodeStrict(output, &result); err != nil {
		return practical3d.OracleResult{}, err
	}
	return result, nil
}

func decodeStrict(raw []byte, dst any) error {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return err
	}
	var extra any
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("multiple JSON values")
		}
		return err
	}
	return nil
}

func sha256Hex(b []byte) string { h := sha256.Sum256(b); return hex.EncodeToString(h[:]) }
