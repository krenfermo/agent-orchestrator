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
	e := &practical3d.AORealExecutor{Cfg: practical3d.AORealConfig{AOBinary: *aoBinary, RealClaude: *realClaude, ShimExecutable: self, AOSrc: src, ToolsRO: filepath.Dir(*aoBinary), WebRoot: filepath.Join(filepath.Dir(*aoBinary), "webroot"),
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
	tools := fs.String("tools", "", "dir with ao and ao3dpractical built from a clean clone, webroot/ and task-specs/{A,B,C,D}.json")
	fixture := fs.String("fixture-repo", "", "fixture repository")
	commit := fs.String("fixture-commit", "", "fixture commit")
	oracleDir := fs.String("oracle-dir", "", "dir with oracle.sh and hidden/")
	realClaude := fs.String("real-claude", "", "real Claude Code executable")
	primary := fs.String("primary-model", "", "frozen primary model")
	helper := fs.String("helper-model", "", "frozen helper (small fast) model")
	upstream := fs.String("upstream", "https://api.anthropic.com", "provider origin")
	if err := fs.Parse(args); err != nil {
		return err
	}
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
	aoBin := filepath.Join(*tools, "ao")
	self, err := os.Executable()
	if err != nil {
		return err
	}
	src, _ := filepath.Abs("../..")
	modelEnv := map[string]string{"ANTHROPIC_MODEL": *primary, "ANTHROPIC_DEFAULT_OPUS_MODEL": *primary, "ANTHROPIC_DEFAULT_SONNET_MODEL": *primary, "ANTHROPIC_DEFAULT_HAIKU_MODEL": *helper, "ANTHROPIC_SMALL_FAST_MODEL": *helper}
	cfg := practical3d.AORealConfig{AOBinary: aoBin, RealClaude: *realClaude, ShimExecutable: self, AOSrc: src, ToolsRO: *tools, Upstream: *upstream, FixtureRepo: *fixture, WebRoot: filepath.Join(*tools, "webroot"),
		ModelEnv: modelEnv, DaemonTimeout: 120 * time.Second, SettleTimeout: 8 * time.Minute, Log: out}
	envTemplate := practical3d.EnvironmentInputs{
		RuntimeVersions:                     []practical3d.VersionInput{{Component: "go"}},
		ProviderClientCLIVersions:           []practical3d.VersionInput{{Component: "claude"}},
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
	_, _ = fmt.Fprintln(out, "[mini-real] capturing the account reference (one minimal request)")
	account, err := practical3d.CaptureAccountRef(ctx, *realClaude, *upstream, *helper, base)
	if err != nil {
		return err
	}
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
	verify := []string{"/bin/sh", "-c", "go build ./... && go test -count=1 ./..."}
	claudeVersion := ""
	for _, v := range env.ProviderClientCLIVersions {
		claudeVersion = strings.Fields(v.Version)[0]
	}
	m, blobs, err := practical3d.BuildRealMiniManifest(practical3d.RealMiniInputs{AOCommit: env.AOCommit, FixtureCommit: *commit, AccountRefSHA256: account, ClaudeVersion: claudeVersion,
		PrimaryModel: *primary, HelperModel: *helper, Env: env, TaskSpecs: specs, Attachment: []byte(cal.Attachment), AttachmentRef: "attachment-A.bin",
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
	executor.Artifacts = resolver
	label := "TECHNICAL-MINI-E2E-REAL (not official evidence)"
	_, _ = fmt.Fprintln(out, "[mini-real] running the first A/OFF + A/ASSISTED pair for real")
	res, err := practical3d.Run(ctx, m, practical3d.RunnerOptions{
		Root: filepath.Join(base, "run"), Metadata: practical3d.EnvelopeMetadata{HumanLabel: &label},
		Environment: observer, Artifacts: resolver, Transport: noTransport{}, Executor: executor,
		Oracle:     practical3d.RealOracle{Script: filepath.Join(*oracleDir, "oracle.sh"), HiddenDir: filepath.Join(*oracleDir, "hidden"), Verify: verify, Manifest: m, Exec: executor},
		Workspaces: practical3d.GitWorkspaceManager{FixtureRepo: *fixture},
		MiniE2E:    true, MiniPositions: 2,
	})
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
