package main

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/observe/practical3d"
)

// miniCommand is the technical mini-E2E. It exercises manifest → schedule →
// scratch position → attempt lifecycle → telemetry → oracle → ledger →
// decide through the real CLI drivers (this binary re-invoked as
// `technical-driver`), an isolated git clone per position and the real
// teardown scan. It uses a technical-fixture manifest with a fresh random
// seed, executes only the first scheduled pair and blocks the other 38, so it
// can never produce official evidence: the official experiment stays UNSTARTED.
func miniCommand(args []string, out io.Writer) error {
	fs := flag.NewFlagSet("mini-e2e", flag.ContinueOnError)
	fs.SetOutput(out)
	base := fs.String("dir", "", "new directory below ~/.ao/scratch/frente3 (default: timestamped under 3d-practical-mini)")
	positions := fs.Int("positions", 2, "positions to execute (the first scheduled OFF/ASSISTED pair by default)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	scratch, err := practical3d.ScratchRoot()
	if err != nil {
		return err
	}
	if *base == "" {
		*base = filepath.Join(scratch, "3d-practical-mini", time.Now().UTC().Format("20060102T150405Z"))
	}
	return runMini(*base, *positions, false, out)
}

func runMini(base string, positions int, allowTemp bool, out io.Writer) error {
	if positions < 1 || positions > 4 {
		return errors.New("--positions must be 1..4; the mini-E2E never runs the official batch")
	}
	if _, err := practical3d.ValidateRunRoot(filepath.Join(base, "run"), allowTemp); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(base), 0o700); err != nil {
		return err
	}
	if err := os.Mkdir(base, 0o700); err != nil {
		return err
	}
	self, err := os.Executable()
	if err != nil {
		return err
	}
	selfBytes, err := os.ReadFile(self)
	if err != nil {
		return err
	}
	seed := make([]byte, 32)
	if _, err := rand.Read(seed); err != nil {
		return err
	}
	m, env, art, err := practical3d.NewTechnicalFixture(hex.EncodeToString(seed))
	if err != nil {
		return err
	}
	fixtureRepo := filepath.Join(base, "fixture-repo")
	commit, err := createTechnicalRepo(fixtureRepo)
	if err != nil {
		return err
	}
	m.FixtureCommit = commit
	subtree, err := practical3d.FixtureSubtreeSHA256(context.Background(), fixtureRepo, commit)
	if err != nil {
		return err
	}
	for i := range m.Tasks {
		m.Tasks[i].FixtureSubtreeSHA256 = subtree
	}
	m.Q4Oracle.RunnerImageOrBinarySHA256 = sha256Hex(selfBytes)
	if err := practical3d.ValidateManifest(m); err != nil {
		return err
	}
	artifactRoot := filepath.Join(base, "artifacts")
	if err := art.Write(artifactRoot); err != nil {
		return err
	}
	canonical, err := practical3d.CanonicalManifest(m)
	if err != nil {
		return err
	}
	manifestCopy := filepath.Join(base, "technical-manifest.json")
	if err := os.WriteFile(manifestCopy, canonical, 0o400); err != nil {
		return err
	}
	driverArgs := func(role string) []string {
		return []string{"technical-driver", role, manifestCopy, filepath.Join(artifactRoot, art.AttachmentRef)}
	}
	label := "TECHNICAL-MINI-E2E (not official evidence)"
	res, err := practical3d.Run(context.Background(), m, practical3d.RunnerOptions{
		Root:              filepath.Join(base, "run"),
		Metadata:          practical3d.EnvelopeMetadata{HumanLabel: &label},
		Environment:       practical3d.StaticEnvironmentObserver{Inputs: env},
		Artifacts:         practical3d.DirArtifactResolver{Root: artifactRoot},
		Transport:         commandTransport{path: self, args: driverArgs("provider")},
		Executor:          commandExecutor{path: self, args: driverArgs("position")},
		Oracle:            commandOracle{path: self, args: driverArgs("oracle"), manifest: m},
		Workspaces:        practical3d.GitWorkspaceManager{FixtureRepo: fixtureRepo},
		MiniE2E:           true,
		MiniPositions:     positions,
		AllowExplicitTemp: allowTemp,
	})
	if err != nil {
		return err
	}
	// Independent re-decision from the files on disk must agree.
	redecided := filepath.Join(base, "redecide-report.json")
	var buf strings.Builder
	if err := decideCommand([]string{"--run-root", res.Root, "--report", redecided}, &buf); err != nil {
		return err
	}
	events, err := practical3d.ReadLedger(filepath.Join(res.Root, "ledger.jsonl"))
	if err != nil {
		return err
	}
	counts := map[practical3d.EventType]int{}
	for _, e := range events {
		counts[e.Type]++
	}
	states := map[practical3d.TerminalState]int{}
	for _, p := range res.Report.Positions {
		states[p.State]++
	}
	if states[practical3d.StateCompleted] != positions || states[practical3d.StateBlocked] != 40-positions || res.Report.Verdict != "NO_GO" || counts[practical3d.EventSampleStart] != positions {
		return fmt.Errorf("mini-e2e unexpected outcome: states=%v verdict=%s counts=%v", states, res.Report.Verdict, counts)
	}
	if !strings.HasPrefix(buf.String(), res.Report.Verdict+" "+res.Report.ReasonCode) {
		return fmt.Errorf("re-decision disagrees: %q vs %s %s", buf.String(), res.Report.Verdict, res.Report.ReasonCode)
	}
	_, _ = fmt.Fprintf(out, "MINI_E2E_OK official_experiment=UNSTARTED technical_experiment_id=%s\n", res.Report.ExperimentID)
	_, _ = fmt.Fprintf(out, "executed_positions=%d blocked=%d verdict=%s reason=%s lineage_valid=%t\n", states[practical3d.StateCompleted], states[practical3d.StateBlocked], res.Report.Verdict, res.Report.ReasonCode, res.Report.LineageValid)
	_, _ = fmt.Fprintf(out, "events=%v\n", counts)
	for _, p := range res.Report.Positions[:positions] {
		_, _ = fmt.Fprintf(out, "position %d %s %s state=%s M1u=%d M2=%d M3=%.2f Q1=%t Q4=%t cached=%d\n", p.Position.PositionIndex, p.Position.TaskID, p.Position.Arm, p.State, p.Metrics.M1U, p.Metrics.M2, p.Metrics.M3, p.Metrics.Q1, p.Metrics.Q4, p.Diagnostics.CachedInputTokens)
	}
	_, _ = fmt.Fprintf(out, "run_root=%s\n", res.Root)
	return nil
}

func createTechnicalRepo(dir string) (string, error) {
	if err := os.Mkdir(dir, 0o700); err != nil {
		return "", err
	}
	if err := os.WriteFile(filepath.Join(dir, "fixture.go"), practical3d.TechnicalReviewedFile, 0o600); err != nil {
		return "", err
	}
	git := func(args ...string) (string, error) {
		cmd := exec.Command("git", append([]string{"-C", dir, "-c", "user.name=fixture", "-c", "user.email=fixture@example.invalid", "-c", "commit.gpgsign=false"}, args...)...)
		cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1")
		raw, err := cmd.CombinedOutput()
		if err != nil {
			return "", fmt.Errorf("git %v: %w: %s", args, err, raw)
		}
		return strings.TrimSpace(string(raw)), nil
	}
	for _, a := range [][]string{{"init", "--quiet"}, {"add", "fixture.go"}, {"commit", "--quiet", "-m", "technical fixture"}} {
		if _, err := git(a...); err != nil {
			return "", err
		}
	}
	return git("rev-parse", "HEAD")
}

// technicalDriver implements the three driver protocols for the mini-E2E:
// position (JSONL), provider (one canonical request on stdin → one response)
// and oracle. Arguments: <role> <technical-manifest.json> <attachment>.
func technicalDriver(args []string, in io.Reader, out io.Writer) error {
	if len(args) != 3 {
		return errors.New("technical-driver <position|provider|oracle> <manifest> <attachment>")
	}
	raw, err := os.ReadFile(args[1])
	if err != nil {
		return err
	}
	m, err := practical3d.DecodeManifest(raw)
	if err != nil {
		return err
	}
	if m.Provider.ProviderID != practical3d.TechnicalFixtureProviderID {
		return errors.New("technical-driver only serves technical-fixture manifests")
	}
	attachment, err := os.ReadFile(args[2])
	if err != nil {
		return err
	}
	switch args[0] {
	case "position":
		return technicalPosition(m, attachment, in, out)
	case "provider":
		_ = os.Setenv("AO_3D_TECHNICAL_ACCOUNT", m.Provider.AccountRefSHA256)
		return technicalProvider(in, out)
	case "oracle":
		return technicalOracle(m, in, out)
	}
	return fmt.Errorf("unknown technical driver %q", args[0])
}

func technicalPosition(m practical3d.Manifest, attachment []byte, in io.Reader, out io.Writer) error {
	sc := bufio.NewScanner(in)
	sc.Buffer(make([]byte, 64*1024), 64*1024*1024)
	enc := json.NewEncoder(out)
	if !sc.Scan() {
		return errors.New("no start message")
	}
	var msg driverMessage
	if err := json.Unmarshal(sc.Bytes(), &msg); err != nil {
		return err
	}
	if msg.Type == "preflight" {
		return enc.Encode(driverMessage{Type: "preflight_result", Representations: practical3d.TechnicalPreflight(m, attachment)})
	}
	if msg.Type != "start" || msg.Context == nil {
		return fmt.Errorf("unexpected message %q", msg.Type)
	}
	p := msg.Context.Position
	role := practical3d.RoleWorker
	if p.TaskID == "C" {
		role = practical3d.RoleReviewer
	}
	// The worker edits a file in its isolated clone (the M3 milestone).
	if role == practical3d.RoleWorker {
		if err := os.WriteFile(filepath.Join(msg.Context.Workspace.WorkingCopy, "fixture.go"), []byte("package fixture\n\nfunc Allowed(role string) bool { return role == \"admin\" }\n"), 0o600); err != nil {
			return err
		}
	}
	rep := practical3d.TechnicalRepresentation(m, attachment, p.TaskID, p.Arm, role, practical3d.CallInitial)
	rep.Payload = json.RawMessage(`{"technical_fixture":true,"simulate":"retryable_first"}`)
	if err := enc.Encode(driverMessage{Type: "call", Request: &practical3d.ProviderRequest{Role: role, CallClass: practical3d.CallInitial, Representation: rep}}); err != nil {
		return err
	}
	if !sc.Scan() {
		return errors.New("no call_result")
	}
	var reply driverMessage
	if err := json.Unmarshal(sc.Bytes(), &reply); err != nil {
		return err
	}
	result := practical3d.ExecutionResult{TerminalState: practical3d.StateCompleted, ExplorationCalls: 2, DistinctFilesRead: 1, MilestoneObserved: true}
	if reply.Error != "" {
		result.TerminalState = practical3d.StateFailedWorker
	}
	if p.TaskID == "C" {
		result.Findings = []practical3d.Finding{practical3d.FindingFor(1, m.Q6Oracle.MandatoryDefects[0])}
	}
	return enc.Encode(driverMessage{Type: "result", Result: &result})
}

// technicalProvider answers RETRYABLE to the first attempt of a position and
// SUCCESS afterwards, so the mini-E2E exercises a real in-position retry.
func technicalProvider(in io.Reader, out io.Writer) error {
	req, err := io.ReadAll(in)
	if err != nil {
		return err
	}
	counter := filepath.Join(os.Getenv("TMPDIR"), "technical-provider-attempts")
	n := 0
	if b, err := os.ReadFile(counter); err == nil {
		n, _ = strconv.Atoi(strings.TrimSpace(string(b)))
	}
	n++
	if err := os.WriteFile(counter, []byte(strconv.Itoa(n)), 0o600); err != nil {
		return err
	}
	outcome := practical3d.OutcomeSuccess
	if n == 1 && strings.Contains(string(req), `"simulate":"retryable_first"`) {
		outcome = practical3d.OutcomeRetryable
	}
	uncached, cached := int64(len(req)/4), int64(n*3)
	total := uncached + cached
	return json.NewEncoder(out).Encode(practical3d.ProviderResponse{Output: json.RawMessage(`{"technical":true}`), Outcome: outcome, InputTokens: &total, CachedInputTokens: &cached, UncachedInputTokens: &uncached, ProviderMetadata: json.RawMessage(`{"account_ref_sha256":"` + os.Getenv("AO_3D_TECHNICAL_ACCOUNT") + `"}`), TerminalMetadata: json.RawMessage(`{"attempt":` + strconv.Itoa(n) + `}`)})
}

func technicalOracle(m practical3d.Manifest, in io.Reader, out io.Writer) error {
	var input oracleInput
	if err := json.NewDecoder(in).Decode(&input); err != nil {
		return err
	}
	q4 := ""
	for _, t := range m.Q4Oracle.TaskOracles {
		if t.TaskID == input.Context.Position.TaskID {
			q4 = t.HiddenTestManifestSHA256
		}
	}
	_, statErr := os.Stat(filepath.Join(input.Context.Workspace.WorkingCopy, ".git"))
	exit := 0
	if statErr != nil {
		exit = 1
	}
	return json.NewEncoder(out).Encode(practical3d.OracleResult{Q1ExitCode: exit, Q1VerifyCommandSHA256: m.Q1Oracle.VerifyCommandSHA256, Q4Passed: statErr == nil, Q4TaskOracleSHA256: q4, Q4CommandSHA256: m.Q4Oracle.CommandSHA256})
}
