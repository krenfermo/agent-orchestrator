package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
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
