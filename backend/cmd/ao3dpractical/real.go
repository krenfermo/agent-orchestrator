package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/observe/practical3d"
)

// calibrateCommand runs one provider-free AO task run (claude launches are
// captured, never executed) and prints the worker's initial prompt facts.
func calibrateCommand(args []string, out io.Writer) error {
	fs := flag.NewFlagSet("calibrate", flag.ContinueOnError)
	fs.SetOutput(out)
	aoBinary := fs.String("ao-binary", "", "frozen ao CLI binary")
	realClaude := fs.String("real-claude", "", "real Claude Code executable")
	fixture := fs.String("fixture-repo", "", "fixture repository")
	commit := fs.String("commit", "", "fixture commit")
	specPath := fs.String("task-spec", "", "task spec JSON")
	arm := fs.String("arm", "ASSISTED", "OFF or ASSISTED")
	dir := fs.String("dir", "", "new directory below ~/.ao/scratch/frente3")
	oracleDir := fs.String("oracle-dir", "", "Q4 oracle directory (denied to agents)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if _, err := practical3d.ValidateRunRoot(*dir, false); err != nil {
		return err
	}
	raw, err := os.ReadFile(*specPath)
	if err != nil {
		return err
	}
	spec, err := practical3d.ParseTaskSpec(raw)
	if err != nil {
		return err
	}
	self, err := os.Executable()
	if err != nil {
		return err
	}
	src, _ := filepath.Abs("../..")
	e := &practical3d.AORealExecutor{Cfg: practical3d.AORealConfig{AOBinary: *aoBinary, RealClaude: *realClaude, ShimExecutable: self, AOSrc: src, OracleDir: *oracleDir, ToolsRO: filepath.Dir(*aoBinary), WebRoot: filepath.Join(filepath.Dir(*aoBinary), "webroot"),
		DaemonTimeout: 90 * time.Second, SettleTimeout: 5 * time.Minute, Log: os.Stderr}}
	res, err := e.Calibrate(context.Background(), practical3d.FixtureConfig{Repo: *fixture, Commit: *commit}, spec, practical3d.Arm(*arm), *dir)
	if err != nil {
		return err
	}
	b, _ := json.MarshalIndent(res, "", "  ")
	if err := os.WriteFile(filepath.Join(*dir, "calibration.json"), b, 0o600); err != nil {
		return err
	}
	_, _ = fmt.Fprintf(out, "CALIBRATED task=%s arm=%s prompt_bytes=%d pack_digest=%s attachment_bytes=%d\n", res.TaskID, res.Arm, len(res.Prompt), res.PackDigest, len(res.Attachment))
	if res.Arm == practical3d.ArmAssisted && res.Attachment == "" {
		return errors.New("no attachment extracted")
	}
	return nil
}

type noTransport struct{}

func (noTransport) Do(context.Context, practical3d.TransportRequest) (practical3d.ProviderResponse, error) {
	return practical3d.ProviderResponse{}, errors.New("real runs reach the provider only through the position proxy")
}

// miniRealCommand is the technical REAL mini-E2E: one A/OFF + A/ASSISTED
// pair through a real AO daemon per position, real Claude Code agents
// confined by the Practical sandbox, the AO provider proxy, 3C-derived M3
// and the real oracle. The other 38 positions are BLOCKED; the manifest's
// client id is technical, so this is never official evidence.
func miniRealCommand(args []string, out io.Writer) error {
	fs := flag.NewFlagSet("mini-e2e-real", flag.ContinueOnError)
	fs.SetOutput(out)
	rf := addRealFlags(fs)
	commit := fs.String("fixture-commit", "", "fixture commit")
	primary := fs.String("primary-model", "", "frozen primary model")
	helper := fs.String("helper-model", "", "frozen helper (small fast) model")
	codexModel := fs.String("codex-model", "", "frozen Codex model")
	if err := fs.Parse(args); err != nil {
		return err
	}
	tools, fixture, oracleDir := rf.tools, rf.fixture, rf.oracleDir
	ctx := context.Background()
	scratch, err := practical3d.ScratchRoot()
	if err != nil {
		return err
	}
	base := filepath.Join(scratch, "3d-practical-mini-real", time.Now().UTC().Format("20060102T150405Z"))
	if _, err := practical3d.ValidateRunRoot(filepath.Join(base, "run"), false); err != nil {
		return err
	}
	if err := os.MkdirAll(base, 0o700); err != nil {
		return err
	}
	cfg, creds, err := rf.executorConfig(realModels{primary: *primary, helper: *helper, codex: *codexModel}, out)
	if err != nil {
		return err
	}
	aoBin := cfg.AOBinary
	envTemplate := practical3d.EnvironmentInputs{
		RuntimeVersions:                     []practical3d.VersionInput{{Component: "go"}},
		ProviderClientCLIVersions:           []practical3d.VersionInput{{Component: "claude"}, {Component: "codex"}},
		TaskToolVersions:                    []practical3d.VersionInput{{Component: "git"}},
		RunnerInstrumentVersions:            []practical3d.VersionInput{{Component: "ao3dpractical"}},
		EffectiveEnvironmentConfigAllowlist: []practical3d.ConfigInput{},
		AdditionalLocalConfiguration:        []practical3d.ConfigInput{},
	}
	observer := practical3d.LiveEnvironmentObserver{Expected: envTemplate, AOBinaryPath: aoBin}
	env, err := observer.Observe(ctx)
	if err != nil {
		return fmt.Errorf("observe environment: %w", err)
	}
	if len(env.AOCommit) != 40 {
		return errors.New("the ao binary carries no VCS revision: build it from a clean clone")
	}
	observer.Expected = env
	specs := map[string][]byte{}
	for _, t := range []string{"A", "B", "C", "D"} {
		if specs[t], err = os.ReadFile(filepath.Join(*tools, "task-specs", t+".json")); err != nil {
			return err
		}
	}
	specA, err := practical3d.ParseTaskSpec(specs["A"])
	if err != nil {
		return err
	}
	_, _ = fmt.Fprintln(out, "[mini-real] attesting the provider accounts the injected credentials select")
	accounts, err := liveAccountRefs(ctx, rf, creds, *helper, base)
	if err != nil {
		return err
	}
	account := accounts["anthropic"]
	cfg.AccountRefs = accounts
	executor := &practical3d.AORealExecutor{Cfg: cfg}
	_, _ = fmt.Fprintln(out, "[mini-real] freezing the ASSISTED attachment by provider-free calibration")
	cal, err := executor.Calibrate(ctx, practical3d.FixtureConfig{Repo: *fixture, Commit: *commit}, specA, practical3d.ArmAssisted, filepath.Join(base, "freeze-calibration"))
	if err != nil {
		return err
	}
	subtree, err := practical3d.FixtureSubtreeSHA256(ctx, *fixture, *commit)
	if err != nil {
		return err
	}
	if err := practical3d.CheckFixtureHistory(ctx, *fixture, filepath.Join(*oracleDir, "hidden")); err != nil {
		return err
	}
	hidden := map[string][]byte{}
	for _, t := range []string{"A", "B", "C", "D"} {
		if hidden[t], err = practical3d.BuildHiddenManifest(filepath.Join(*oracleDir, "hidden"), t); err != nil {
			return err
		}
	}
	oracleScript, err := os.ReadFile(filepath.Join(*oracleDir, "oracle.sh"))
	if err != nil {
		return err
	}
	gitOut := func(args ...string) ([]byte, error) {
		return osexec("git", append([]string{"-C", *fixture}, args...)...)
	}
	target, err := gitOut("show", *commit)
	if err != nil {
		return err
	}
	const reviewPath = "internal/orders/pricing.go"
	reviewFile, err := gitOut("show", *commit+":"+reviewPath)
	if err != nil {
		return err
	}
	causal := 0
	for i, line := range strings.Split(string(reviewFile), "\n") {
		if strings.Contains(line, "code.Amount(total)") {
			causal = i + 1
			break
		}
	}
	verify := practical3d.RealVerifyCommand
	claudeVersion, codexVersion := "", ""
	for _, v := range env.ProviderClientCLIVersions {
		f := strings.Fields(v.Version)
		switch v.Component {
		case "claude":
			claudeVersion = f[0]
		case "codex":
			codexVersion = f[len(f)-1]
		}
	}
	m, blobs, err := practical3d.BuildRealMiniManifest(practical3d.RealMiniInputs{AOCommit: env.AOCommit, FixtureCommit: *commit, AccountRefSHA256: account, ClaudeVersion: claudeVersion,
		PrimaryModel: *primary, HelperModel: *helper, CodexModel: *codexModel, CodexVersion: codexVersion, AccountRefs: accounts, Env: env, TaskSpecs: specs, Attachment: []byte(cal.Attachment), AttachmentRef: "attachment-A.bin",
		OracleScript: oracleScript, HiddenManifests: hidden, VerifyCommand: strings.Join(verify, " "), FixtureSubtree: subtree,
		ReviewTarget: target, ReviewFile: reviewFile, ReviewFilePath: reviewPath, ReviewCausalLine: causal, IndexedCommit: *commit, PackDigest: cal.PackDigest})
	if err != nil {
		return err
	}
	artifacts := filepath.Join(base, "artifacts")
	if err := os.MkdirAll(filepath.Join(artifacts, "sha256"), 0o700); err != nil {
		return err
	}
	for d, b := range blobs {
		if err := os.WriteFile(filepath.Join(artifacts, "sha256", d), b, 0o400); err != nil {
			return err
		}
	}
	if err := os.WriteFile(filepath.Join(artifacts, "attachment-A.bin"), []byte(cal.Attachment), 0o400); err != nil {
		return err
	}
	resolver := practical3d.DirArtifactResolver{Root: artifacts}
	label := "TECHNICAL-MINI-E2E-REAL (not official evidence)"
	_, _ = fmt.Fprintln(out, "[mini-real] running the first A/OFF + A/ASSISTED pair for real")
	// The official `run` wiring (realRunnerOptions), limited to 2 positions.
	opts := realRunnerOptions(m, rf, executor, observer, resolver, filepath.Join(base, "run"), practical3d.EnvelopeMetadata{HumanLabel: &label})
	opts.MiniE2E, opts.MiniPositions = true, 2
	res, err := practical3d.Run(ctx, m, opts)
	if err != nil && res.Root == "" {
		return err
	}
	states := map[practical3d.TerminalState]int{}
	for _, p := range res.Report.Positions {
		states[p.State]++
	}
	_, _ = fmt.Fprintf(out, "MINI_E2E_REAL official_experiment=UNSTARTED technical_experiment_id=%s verdict=%s reason=%s lineage_valid=%t states=%v\n", res.Report.ExperimentID, res.Report.Verdict, res.Report.ReasonCode, res.Report.LineageValid, states)
	for _, p := range res.Report.Positions[:2] {
		_, _ = fmt.Fprintf(out, "position %d %s %s state=%s M1u=%d M2=%d M3=%.3f Q1=%t Q4=%t attempts=%d cached=%d errors=%v\n", p.Position.PositionIndex, p.Position.TaskID, p.Position.Arm, p.State, p.Metrics.M1U, p.Metrics.M2, p.Metrics.M3, p.Metrics.Q1, p.Metrics.Q4, p.Diagnostics.Attempts, p.Diagnostics.CachedInputTokens, p.Errors)
	}
	_, _ = fmt.Fprintf(out, "run_root=%s\n", res.Root)
	return err
}

func osexec(name string, args ...string) ([]byte, error) {
	return exec.Command(name, args...).Output()
}

// realFlags are the inputs of the one real-AO execution path, shared by the
// official `run` and `mini-e2e-real`: there is no second implementation.
type realFlags struct {
	tools, fixture, oracleDir, realClaude, realCodex, upstream, openaiUpstream, aoSrc *string
}

func addRealFlags(fs *flag.FlagSet) realFlags {
	return realFlags{
		tools:          fs.String("tools", "", "dir with ao and ao3dpractical built from a clean clone, and webroot/"),
		fixture:        fs.String("fixture-repo", "", "fixture repository"),
		oracleDir:      fs.String("oracle-dir", "", "dir with oracle.sh and hidden/ (denied to agents)"),
		realClaude:     fs.String("real-claude", "", "real Claude Code executable"),
		realCodex:      fs.String("real-codex", "", "real Codex CLI executable (reviewer)"),
		upstream:       fs.String("upstream", "https://api.anthropic.com", "Anthropic origin"),
		openaiUpstream: fs.String("openai-upstream", "https://chatgpt.com", "ChatGPT backend origin for Codex"),
		aoSrc:          fs.String("ao-src", "", "directory holding every AO checkout (denied to agents)"),
	}
}

// realModels are the frozen models the launch shim pins.
type realModels struct{ primary, helper, codex string }

// executorConfig builds the real executor's configuration: the AO binary and
// shim from the clean-built tools directory, the sandbox inputs, and the
// supervisor-side credential injector.
func (f realFlags) executorConfig(models realModels, out io.Writer) (practical3d.AORealConfig, *practical3d.OperatorCredentials, error) {
	for name, v := range map[string]string{"--tools": *f.tools, "--fixture-repo": *f.fixture, "--oracle-dir": *f.oracleDir, "--real-claude": *f.realClaude, "--real-codex": *f.realCodex, "--ao-src": *f.aoSrc} {
		if v == "" {
			return practical3d.AORealConfig{}, nil, fmt.Errorf("%s is required", name)
		}
	}
	self, err := os.Executable()
	if err != nil {
		return practical3d.AORealConfig{}, nil, err
	}
	src, err := filepath.Abs(*f.aoSrc)
	if err != nil {
		return practical3d.AORealConfig{}, nil, err
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return practical3d.AORealConfig{}, nil, err
	}
	creds := &practical3d.OperatorCredentials{CodexAuthFile: filepath.Join(home, ".codex", "auth.json")}
	modelEnv := map[string]string{"ANTHROPIC_MODEL": models.primary, "ANTHROPIC_DEFAULT_OPUS_MODEL": models.primary, "ANTHROPIC_DEFAULT_SONNET_MODEL": models.primary, "ANTHROPIC_DEFAULT_HAIKU_MODEL": models.helper, "ANTHROPIC_SMALL_FAST_MODEL": models.helper}
	return practical3d.AORealConfig{AOBinary: filepath.Join(*f.tools, "ao"), RealClaude: *f.realClaude, RealCodex: *f.realCodex, CodexModel: models.codex, OpenAIUpstream: *f.openaiUpstream,
		ShimExecutable: self, AOSrc: src, OracleDir: *f.oracleDir, ToolsRO: *f.tools, Upstream: *f.upstream, FixtureRepo: *f.fixture, WebRoot: filepath.Join(*f.tools, "webroot"),
		ModelEnv: modelEnv, DaemonTimeout: 120 * time.Second, SettleTimeout: 8 * time.Minute, Log: out, Credentials: creds}, creds, nil
}

// realRunnerOptions wires a frozen manifest to the real AO executor, the
// real oracle and the fixture workspaces: the same wiring for the 2 mini
// positions and for the official schedule.
func realRunnerOptions(m practical3d.Manifest, f realFlags, executor *practical3d.AORealExecutor, observer practical3d.LiveEnvironmentObserver, resolver practical3d.DirArtifactResolver, root string, meta practical3d.EnvelopeMetadata) practical3d.RunnerOptions {
	verify := practical3d.RealVerifyCommand
	executor.Artifacts = resolver
	return practical3d.RunnerOptions{
		Root: root, Metadata: meta,
		Environment: observer, Artifacts: resolver, Transport: noTransport{}, Executor: executor,
		Oracle:     practical3d.RealOracle{Script: filepath.Join(*f.oracleDir, "oracle.sh"), HiddenDir: filepath.Join(*f.oracleDir, "hidden"), Verify: verify, Manifest: m, Exec: executor},
		Workspaces: practical3d.GitWorkspaceManager{FixtureRepo: *f.fixture},
	}
}

// liveAccountRefs attests the accounts the injected credentials select: the
// Anthropic organization (one minimal request) and the ChatGPT account.
func liveAccountRefs(ctx context.Context, f realFlags, creds *practical3d.OperatorCredentials, helper, dir string) (map[string]string, error) {
	anthropic, err := practical3d.CaptureAccountRef(ctx, *f.realClaude, *f.upstream, helper, dir)
	if err != nil {
		return nil, err
	}
	openai, err := creds.AccountRef("openai")
	if err != nil {
		return nil, err
	}
	return map[string]string{"anthropic": anthropic, "openai": openai}, nil
}
