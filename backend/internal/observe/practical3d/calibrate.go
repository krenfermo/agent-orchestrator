package practical3d

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// initialCalibrator is implemented by executors whose initial requests are
// built by real AO (not by the harness): the preflight proves the initial
// cells by letting AO assemble the real first prompt of each task and arm
// with a capturing, provider-free launch shim.
type initialCalibrator interface {
	CalibrateInitial(ctx context.Context, m Manifest, tasks []string, dir string) error
}

// CalibrationResult is the real initial prompt AO assembled for a task/arm
// and, for ASSISTED, the Project Memory attachment it contained.
type CalibrationResult struct {
	TaskID     string `json:"task_id"`
	Arm        Arm    `json:"arm"`
	Prompt     string `json:"prompt"`
	PackDigest string `json:"pack_digest,omitempty"`
	Attachment string `json:"attachment,omitempty"`
}

// FixtureConfig names the frozen fixture for calibration workspaces.
type FixtureConfig struct {
	Repo   string
	Commit string
}

// Calibrate runs one provider-free AO task run and returns the worker's
// initial prompt. The claude launch is captured by the shim, never executed.
func (e *AORealExecutor) Calibrate(ctx context.Context, fx FixtureConfig, spec TaskSpec, arm Arm, dir string) (CalibrationResult, error) {
	out := CalibrationResult{TaskID: spec.TaskID, Arm: arm}
	w := PositionWorkspace{Root: dir, AODataDir: filepath.Join(dir, "ao-data"), RuntimeHome: filepath.Join(dir, "runtime-home"), WorkingCopy: filepath.Join(dir, "work")}
	for _, d := range []string{w.AODataDir, w.RuntimeHome} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			return out, err
		}
	}
	if err := (GitWorkspaceManager{FixtureRepo: fx.Repo}).Prepare(ctx, Manifest{FixtureCommit: fx.Commit}, Position{}, w); err != nil {
		return out, err
	}
	r, err := e.newRig(w)
	if err != nil {
		return out, err
	}
	defer r.cleanup()
	r.proxyPort = 1 // capture mode never contacts a provider
	profile, err := WriteSandboxProfile(r.ctlDir)
	if err != nil {
		return out, err
	}
	cfg := r.shimConfig(profile, true)
	if err := r.writeShim(cfg); err != nil {
		return out, err
	}
	if err := r.startDaemon(ctx, arm); err != nil {
		return out, err
	}
	defer r.stopDaemon()
	if err := r.startTaskRun(ctx, spec); err != nil {
		return out, err
	}
	deadline := time.Now().Add(3 * time.Minute)
	var capture ShimCapture
	for time.Now().Before(deadline) && capture.Subject == "" {
		files, _ := filepath.Glob(filepath.Join(cfg.Capture, "*.json"))
		sort.Strings(files)
		for _, f := range files {
			raw, err := os.ReadFile(f)
			if err == nil && json.Unmarshal(raw, &capture) == nil && strings.HasPrefix(capture.Subject, "session:") {
				break
			}
			capture = ShimCapture{}
		}
		if capture.Subject == "" {
			time.Sleep(2 * time.Second)
		}
	}
	_, _ = r.api(ctx, http.MethodPost, "/workflows/"+r.runID+"/cancel", map[string]any{}, 30*time.Second)
	if capture.Subject == "" {
		return out, errors.New("AO never launched the worker during calibration")
	}
	if raw, err := json.Marshal(capture); err == nil {
		_ = os.WriteFile(filepath.Join(dir, "capture.json"), raw, 0o600)
	}
	out.Prompt = promptFromArgs(capture.Args)
	if out.Prompt == "" {
		return out, errors.New("captured worker launch carries no prompt")
	}
	if arm == ArmAssisted {
		digest, err := workerPackDigest(ctx, r.dataDir, r.runID)
		if err != nil {
			return out, err
		}
		out.PackDigest = digest
		att, err := ExtractAttachment(out.Prompt, digest)
		if err != nil {
			return out, err
		}
		out.Attachment = att
	}
	return out, nil
}

// promptFromArgs returns the positional prompt after `--`.
func promptFromArgs(args []string) string {
	for i, a := range args {
		if a == "--" && i+1 < len(args) {
			return strings.Join(args[i+1:], " ")
		}
	}
	return ""
}

func workerPackDigest(ctx context.Context, dataDir, runID string) (string, error) {
	db, err := sql.Open("sqlite", "file:"+url.PathEscape(filepath.Join(dataDir, "ao.db"))+"?mode=ro&_pragma=busy_timeout(5000)")
	if err != nil {
		return "", err
	}
	defer func() { _ = db.Close() }()
	var d string
	err = db.QueryRowContext(ctx, `SELECT pack_digest FROM project_memory_context_manifests WHERE workflow_run_id = ? AND role = 'worker' AND pack_digest <> '' ORDER BY created_at LIMIT 1`, runID).Scan(&d)
	if err != nil {
		return "", fmt.Errorf("worker Project Memory manifest: %w", err)
	}
	return d, nil
}

// ExtractAttachment recovers the exact attachment AO injected into a prompt:
// the freshness notice line followed by the rendered pack, identified by
// AO's own pack_digest (SHA-256 of the rendered pack).
func ExtractAttachment(prompt, packDigest string) (string, error) {
	start := strings.Index(prompt, "MEMORY FRESHNESS:")
	if start < 0 {
		return "", errors.New("prompt carries no Project Memory freshness notice")
	}
	nl := strings.IndexByte(prompt[start:], '\n')
	if nl < 0 {
		return "", errors.New("freshness notice is not terminated")
	}
	for _, packStart := range []int{start + nl + 1, start + nl + 2} {
		for end := len(prompt); end > packStart; end-- {
			body := prompt[packStart:end]
			for _, suffix := range []string{"", "\n", "\n\n"} {
				sum := sha256.Sum256([]byte(body + suffix))
				if hex.EncodeToString(sum[:]) == packDigest {
					return strings.TrimRight(prompt[start:end], "\n"), nil
				}
			}
		}
	}
	return "", errors.New("no prompt span matches AO's recorded pack digest")
}

// CalibrateInitial proves, before the first SAMPLE_START, that real AO
// assembles each executed task's initial worker prompt as frozen: OFF with
// no span of any frozen attachment; ASSISTED with the exact frozen
// attachment of the (task, worker, initial) cell.
func (e *AORealExecutor) CalibrateInitial(ctx context.Context, m Manifest, tasks []string, dir string) error {
	fx := FixtureConfig{Repo: e.Cfg.FixtureRepo, Commit: m.FixtureCommit}
	for _, task := range tasks {
		spec, err := e.spec(m, task)
		if err != nil {
			return err
		}
		cell, ok := treatmentCell(m, task, RoleWorker, CallInitial)
		if !ok {
			return fmt.Errorf("task %s has no worker initial cell", task)
		}
		for _, arm := range []Arm{ArmOff, ArmAssisted} {
			res, err := e.Calibrate(ctx, fx, spec, arm, filepath.Join(dir, task+"-"+string(arm)))
			if err != nil {
				return fmt.Errorf("calibrate %s/%s: %w", task, arm, err)
			}
			raw, _ := json.Marshal(res)
			_ = writeExclusive(filepath.Join(dir, task+"-"+string(arm)+".json"), raw)
			want := cell.OFF
			if arm == ArmAssisted {
				want = cell.ASSISTED
			}
			present := want.AttachmentPresent != nil && *want.AttachmentPresent
			if present {
				frozen, err := e.Artifacts.ReadArtifact(want.AttachmentArtifactRef)
				if err != nil {
					return err
				}
				if res.Attachment != string(frozen) || !strings.Contains(res.Prompt, string(frozen)) {
					return fmt.Errorf("calibration %s/%s: AO's attachment differs from the frozen one", task, arm)
				}
				continue
			}
			if res.PackDigest != "" || strings.Contains(res.Prompt, "MEMORY FRESHNESS:") {
				return fmt.Errorf("calibration %s/%s: prompt carries Project Memory", task, arm)
			}
			for _, c := range m.TreatmentMapping {
				a := c.ASSISTED
				if a.AttachmentPresent == nil || !*a.AttachmentPresent {
					continue
				}
				frozen, err := e.Artifacts.ReadArtifact(a.AttachmentArtifactRef)
				if err != nil {
					return err
				}
				if strings.Contains(res.Prompt, string(frozen)) {
					return fmt.Errorf("calibration %s/%s: OFF prompt contains the Project Memory attachment", task, arm)
				}
				for _, marker := range projectMemoryMarkers {
					if strings.Contains(res.Prompt, marker) {
						return fmt.Errorf("calibration %s/%s: OFF prompt carries a Project Memory marker", task, arm)
					}
				}
			}
		}
	}
	return nil
}
