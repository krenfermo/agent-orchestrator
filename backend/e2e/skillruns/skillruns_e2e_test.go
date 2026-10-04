//go:build !windows

package skillruns

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	_ "modernc.org/sqlite" // read the scratch database directly, as evidence
)

// skillruns_e2e_test.go states the 2B contract as what an operator could see
// from outside AO: the HTTP API, the container runtime, the staging root and
// the database file on disk.

type runView struct {
	ID              string   `json:"id"`
	SkillID         string   `json:"skillId"`
	Version         string   `json:"version"`
	ModeID          string   `json:"modeId"`
	State           string   `json:"state"`
	Capabilities    []string `json:"capabilities"`
	RunnerID        string   `json:"runnerId"`
	RunnerControls  []string `json:"runnerControls"`
	PackageDigest   string   `json:"packageDigest"`
	ImageDigest     string   `json:"imageDigest"`
	ApprovalID      string   `json:"approvalId"`
	FindingCount    int      `json:"findingCount"`
	ReportSHA256    string   `json:"reportSha256"`
	ErrorCode       string   `json:"errorCode"`
	ErrorMessage    string   `json:"errorMessage"`
	CancelRequested bool     `json:"cancelRequested"`
}

type startView struct {
	Run     runView `json:"run"`
	Created bool    `json:"created"`
}

type detailView struct {
	Run      runView `json:"run"`
	Findings []struct {
		RuleID   string `json:"ruleId"`
		Severity string `json:"severity"`
		Path     string `json:"path"`
		Line     int    `json:"line"`
	} `json:"findings"`
	Report *struct {
		Evidence struct {
			EffectiveUID       int      `json:"EffectiveUID"`
			NetworkReachable   bool     `json:"NetworkReachable"`
			ReadOnlyRootFS     bool     `json:"ReadOnlyRootFS"`
			InheritedDaemonEnv int      `json:"InheritedDaemonEnv"`
			InputFilesVisible  int      `json:"InputFilesVisible"`
			Controls           []string `json:"Controls"`
		} `json:"evidence"`
		Coverage struct {
			FilesScanned int `json:"filesScanned"`
		} `json:"coverage"`
	} `json:"report"`
	Integrity string `json:"integrity"`
}

type apiError struct {
	Code string `json:"code"`
}

func terminal(state string) bool {
	switch state {
	case "succeeded", "failed", "refused", "cancelled":
		return true
	}
	return false
}

func (s *scratch) startRun(project, mode, key string) (startView, int, string) {
	s.t.Helper()
	body := map[string]any{"modeId": mode, "inputs": map[string]string{"mode": mode}}
	if key != "" {
		body["idempotencyKey"] = key
	}
	code, b, err := s.do(http.MethodPost, "/api/v1/projects/"+project+"/skills/security-audit/run", body)
	if err != nil {
		s.t.Fatalf("start run: %v", err)
	}
	var sv startView
	var ae apiError
	if code == http.StatusAccepted {
		if err := jsonDecode(b, &sv); err != nil {
			s.t.Fatalf("decode start: %v: %s", err, b)
		}
	} else {
		_ = jsonDecode(b, &ae)
	}
	return sv, code, ae.Code
}

func (s *scratch) waitRun(project, runID string) detailView {
	s.t.Helper()
	var d detailView
	waitFor(s.t, 3*time.Minute, "run "+runID+" to end", func() bool {
		code, b, err := s.do(http.MethodGet, "/api/v1/projects/"+project+"/skills/runs/"+runID, nil)
		if err != nil || code != http.StatusOK || jsonDecode(b, &d) != nil {
			return false
		}
		return terminal(d.Run.State)
	})
	return d
}

func (s *scratch) listRuns(project string) []runView {
	s.t.Helper()
	var out struct {
		Runs []runView `json:"runs"`
	}
	s.expect(http.MethodGet, "/api/v1/projects/"+project+"/skills/runs", nil, http.StatusOK, &out)
	return out.Runs
}

func (s *scratch) approveImage(project, version, digest string) string {
	s.t.Helper()
	var out struct {
		ID string `json:"id"`
	}
	s.expect(http.MethodPost, "/api/v1/skills/images", map[string]any{
		"tenantId": "tnt_default", "projectId": project, "skillId": "security-audit",
		"version": version, "modeId": "static-code", "tool": "ao.static-scan/v1",
		"reference": "alpine", "digest": digest,
		"note": "E2E: the local alpine:3.19 the runner's own live tests use", "confirm": true,
	}, http.StatusCreated, &out)
	if out.ID == "" {
		s.t.Fatal("image approval returned no id")
	}
	return out.ID
}

func TestSkillRunIsDurableThroughARealDaemonAndDocker(t *testing.T) {
	s := newScratch(t)
	medusa := filepath.Join(s.root, "projects", "medusa")
	other := filepath.Join(s.root, "projects", "other")
	s.initRepo(medusa)
	s.initRepo(other)
	containersBefore := skillRunContainers(t)

	s.startDaemon()
	log := s.daemonLogText()
	for _, want := range []string{`msg="skills: builtin package" skill=security-audit`, `available=true`} {
		if !strings.Contains(log, want) {
			t.Fatalf("daemon boot log is missing %q:\n%s", want, log)
		}
	}
	s.expect(http.MethodPost, "/api/v1/projects", map[string]any{"path": medusa, "projectId": "medusa", "name": "medusa"}, http.StatusCreated, nil)
	s.expect(http.MethodPost, "/api/v1/projects", map[string]any{"path": other, "projectId": "other", "name": "other"}, http.StatusCreated, nil)

	// ---- builtin: available, never enabled ----
	var catalog struct {
		Skills []struct {
			ID      string `json:"id"`
			Version string `json:"version"`
		} `json:"skills"`
	}
	s.expect(http.MethodGet, "/api/v1/skills", nil, http.StatusOK, &catalog)
	version := ""
	for _, sk := range catalog.Skills {
		if sk.ID == "security-audit" {
			version = sk.Version
		}
	}
	if version == "" {
		t.Fatalf("security-audit is not available after boot: %+v", catalog.Skills)
	}
	var project struct {
		Activations []any `json:"activations"`
	}
	s.expect(http.MethodGet, "/api/v1/projects/medusa/skills", nil, http.StatusOK, &project)
	if len(project.Activations) != 0 {
		t.Fatalf("a builtin was activated on a project automatically: %+v", project.Activations)
	}

	// ---- negative: not enabled -> refused before acceptance, no run ----
	if _, code, _ := s.startRun("medusa", "static-code", ""); code < 400 || code >= 500 {
		t.Fatalf("a run of a skill this project never enabled answered %d", code)
	}
	// ---- negative: capability not granted -> 403, no run ----
	s.expect(http.MethodPut, "/api/v1/projects/medusa/skills/security-audit",
		map[string]any{"version": version, "capabilities": []string{"repo.read"}}, http.StatusOK, nil)
	if _, code, ecode := s.startRun("medusa", "static-code", ""); code != http.StatusForbidden || ecode != "SKILL_RUN_REFUSED" {
		t.Fatalf("a run without report.write granted answered %d %s", code, ecode)
	}
	if n := len(s.listRuns("medusa")); n != 0 {
		t.Fatalf("refusals before acceptance created %d run(s)", n)
	}
	s.expect(http.MethodPut, "/api/v1/projects/medusa/skills/security-audit",
		map[string]any{"version": version, "capabilities": []string{"repo.read", "report.write"}}, http.StatusOK, nil)

	// ---- negative: mode authorized but not executable -> 409, no run ----
	// (secret-scan became executable in 2D; api-infra-review is still not.)
	if _, code, ecode := s.startRun("medusa", "api-infra-review", ""); code != http.StatusConflict || ecode != "SKILL_MODE_NOT_EXECUTABLE" {
		t.Fatalf("a non-executable mode answered %d %s", code, ecode)
	}

	// ---- negative: no approved image -> accepted, ends REFUSED ----
	noImage, code, _ := s.startRun("medusa", "static-code", "")
	if code != http.StatusAccepted {
		t.Fatalf("start without approval: %d", code)
	}
	if d := s.waitRun("medusa", noImage.Run.ID); d.Run.State != "refused" || d.Run.ErrorCode != "SKILL_IMAGE_NOT_APPROVED" {
		t.Fatalf("run without an approved image ended %s %s", d.Run.State, d.Run.ErrorCode)
	}

	digest := imageDigest(t)
	approval := s.approveImage("medusa", version, digest)

	// ---- the real run, requested twice concurrently with one idempotency key ----
	var (
		wg     sync.WaitGroup
		starts [2]startView
		codes  [2]int
	)
	for i := range starts {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			starts[i], codes[i], _ = s.startRun("medusa", "static-code", "e2e-click-1")
		}(i)
	}
	wg.Wait()
	if codes[0] != http.StatusAccepted || codes[1] != http.StatusAccepted || starts[0].Run.ID != starts[1].Run.ID {
		t.Fatalf("two requests with one idempotency key: %v %s %s", codes, starts[0].Run.ID, starts[1].Run.ID)
	}
	if starts[0].Created == starts[1].Created {
		t.Fatalf("exactly one of the two requests must have created the run: %v %v", starts[0].Created, starts[1].Created)
	}
	runID := starts[0].Run.ID
	d := s.waitRun("medusa", runID)
	if d.Run.State != "succeeded" || d.Integrity != "verified" || d.Report == nil {
		t.Fatalf("real run ended %s (%s: %s) integrity=%s", d.Run.State, d.Run.ErrorCode, d.Run.ErrorMessage, d.Integrity)
	}
	rules := map[string]bool{}
	for _, f := range d.Findings {
		rules[f.RuleID] = true
		if f.Path != "src/app.go" {
			t.Fatalf("finding outside the planted file: %+v", f)
		}
	}
	for _, want := range []string{"AOSS-004", "AOSS-005", "AOSS-006"} {
		if !rules[want] {
			t.Fatalf("the scan did not report %s; findings %+v", want, d.Findings)
		}
	}
	if d.Run.FindingCount != len(d.Findings) || d.Run.ApprovalID != approval || d.Run.ImageDigest == "" ||
		d.Run.Version != version || d.Run.PackageDigest == "" || d.Run.RunnerID == "" {
		t.Fatalf("run evidence incomplete: %+v", d.Run)
	}
	// ---- isolation preserved: the boundary the run demonstrated ----
	ev := d.Report.Evidence
	if ev.NetworkReachable || !ev.ReadOnlyRootFS || ev.EffectiveUID != 65534 || ev.InheritedDaemonEnv != 0 || ev.InputFilesVisible == 0 {
		t.Fatalf("the run did not demonstrate the boundary: %+v", ev)
	}

	// ---- double click without a key reaches the in-flight run ----
	var dbl [2]startView
	for i := range dbl {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			dbl[i], _, _ = s.startRun("medusa", "static-code", "")
		}(i)
	}
	wg.Wait()
	if dbl[0].Run.ID != dbl[1].Run.ID || dbl[0].Run.ID == runID {
		t.Fatalf("double click produced %s and %s (first run %s)", dbl[0].Run.ID, dbl[1].Run.ID, runID)
	}
	if d2 := s.waitRun("medusa", dbl[0].Run.ID); d2.Run.State != "succeeded" {
		t.Fatalf("second run ended %s %s", d2.Run.State, d2.Run.ErrorCode)
	}

	// ---- another project cannot read or cancel it ----
	for _, req := range [][2]string{
		{http.MethodGet, "/api/v1/projects/other/skills/runs/" + runID},
		{http.MethodPost, "/api/v1/projects/other/skills/runs/" + runID + "/cancel"},
	} {
		if code, _, _ := s.do(req[0], req[1], nil); code != http.StatusNotFound {
			t.Fatalf("%s %s from another project answered %d", req[0], req[1], code)
		}
	}

	// ---- negative: revoked approval -> accepted, ends REFUSED ----
	s.expect(http.MethodDelete, "/api/v1/skills/images/"+approval, nil, http.StatusOK, nil)
	revoked, _, _ := s.startRun("medusa", "static-code", "")
	if d := s.waitRun("medusa", revoked.Run.ID); d.Run.State != "refused" {
		t.Fatalf("a run after revocation ended %s %s", d.Run.State, d.Run.ErrorCode)
	}

	// ---- restart: everything is still there, and the stored bytes verify ----
	before := s.listRuns("medusa")
	s.stopDaemon(syscall.SIGTERM)
	checkDatabase(t, s.dataDir, runID)
	s.startDaemon()
	after := s.listRuns("medusa")
	if len(after) != len(before) {
		t.Fatalf("history after restart has %d runs, before %d", len(after), len(before))
	}
	d3 := s.waitRun("medusa", runID)
	if d3.Run.State != "succeeded" || d3.Integrity != "verified" || d3.Run.ReportSHA256 != d.Run.ReportSHA256 || len(d3.Findings) != len(d.Findings) {
		t.Fatalf("the run changed across a restart: %s %s %s", d3.Run.State, d3.Integrity, d3.Run.ReportSHA256)
	}

	// ---- crash mid-run: the next daemon ends it, never leaves it running ----
	s.approveImage("medusa", version, digest)
	crash, code, _ := s.startRun("medusa", "static-code", "")
	if code != http.StatusAccepted {
		t.Fatalf("start before crash: %d", code)
	}
	s.stopDaemon(syscall.SIGKILL)
	s.startDaemon()
	dc := s.waitRun("medusa", crash.Run.ID)
	switch {
	case dc.Run.State == "failed" && dc.Run.ErrorCode == "SKILL_RUN_INTERRUPTED":
		t.Logf("crash landed mid-run: %s ended failed SKILL_RUN_INTERRUPTED (%s)", crash.Run.ID, dc.Run.ErrorMessage)
	case dc.Run.State == "succeeded" && dc.Integrity == "verified":
		t.Logf("the run finished before the SIGKILL landed; recorded succeeded and verified")
	default:
		t.Fatalf("after a crash the run is %s %s integrity=%s", dc.Run.State, dc.Run.ErrorCode, dc.Integrity)
	}
	out, _ := exec.Command("docker", "ps", "-aq", "--filter", "label=ao.skillrun.id="+crash.Run.ID).Output()
	if strings.TrimSpace(string(out)) != "" {
		t.Fatalf("the crashed run's container survived: %s", out)
	}
	if _, err := os.Stat(filepath.Join(s.shared, ".ao-skill-staging", crash.Run.ID)); !os.IsNotExist(err) {
		t.Fatalf("the crashed run's staged copy of the source survived (stat err %v)", err)
	}

	// ---- no leaks ----
	s.stopDaemon(syscall.SIGTERM)
	if now := skillRunContainers(t); len(now) != len(containersBefore) {
		t.Fatalf("skill-run containers before %v, after %v", containersBefore, now)
	}
	entries, _ := os.ReadDir(filepath.Join(s.shared, ".ao-skill-staging"))
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "skr-") || strings.HasPrefix(e.Name(), "run-") {
			t.Fatalf("a staged copy of the source survived: %s", e.Name())
		}
	}
}

// checkDatabase reads the scratch database file directly, with the daemon
// stopped: the stored report bytes hash to the stored digest, and the planted
// secret VALUE appears nowhere in what AO wrote.
func checkDatabase(t *testing.T, dataDir, runID string) {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+filepath.Join(dataDir, "ao.db")+"?mode=ro&immutable=1")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var report, sum string
	if err := db.QueryRow(`SELECT report_json, report_sha256 FROM skill_runs WHERE id = ?`, runID).Scan(&report, &sum); err != nil {
		t.Fatalf("read the stored run: %v", err)
	}
	got := sha256.Sum256([]byte(report))
	if hex.EncodeToString(got[:]) != sum {
		t.Fatalf("stored report bytes do not hash to the stored digest")
	}
	var executed, succeeded int
	_ = db.QueryRow(`SELECT count(*) FROM skill_audit WHERE action = 'run_executed'`).Scan(&executed)
	_ = db.QueryRow(`SELECT count(*) FROM skill_runs WHERE state = 'succeeded'`).Scan(&succeeded)
	if executed != succeeded {
		t.Fatalf("%d run_executed audit rows for %d succeeded runs", executed, succeeded)
	}
	for _, name := range []string{"ao.db", "ao.db-wal"} {
		b, err := os.ReadFile(filepath.Join(dataDir, name))
		if err != nil {
			continue
		}
		if strings.Contains(string(b), secretSentinel) {
			t.Fatalf("the planted secret value was persisted in %s", name)
		}
	}
}

func TestSkillRunWithoutARunnerIsRefusedWithAReason(t *testing.T) {
	s := newScratch(t)
	medusa := filepath.Join(s.root, "projects", "medusa")
	s.initRepo(medusa)
	s.startDaemon("DOCKER_HOST=unix:///nonexistent/ao-e2e-docker.sock")
	log := s.daemonLogText()
	if !strings.Contains(log, "available=false") || !strings.Contains(log, "reason=") {
		t.Fatalf("the probe log does not say why the runner is unavailable:\n%s", log)
	}
	s.expect(http.MethodPost, "/api/v1/projects", map[string]any{"path": medusa, "projectId": "medusa", "name": "medusa"}, http.StatusCreated, nil)
	var catalog struct {
		Skills []struct {
			ID      string `json:"id"`
			Version string `json:"version"`
		} `json:"skills"`
	}
	s.expect(http.MethodGet, "/api/v1/skills", nil, http.StatusOK, &catalog)
	s.expect(http.MethodPut, "/api/v1/projects/medusa/skills/security-audit",
		map[string]any{"version": catalog.Skills[0].Version, "capabilities": []string{"repo.read", "report.write"}}, http.StatusOK, nil)
	// With no runtime the runner attests no control, so authorization refuses
	// the mode before anything is accepted -- and names what is missing.
	code, body, err := s.do(http.MethodPost, "/api/v1/projects/medusa/skills/security-audit/run",
		map[string]any{"modeId": "static-code", "inputs": map[string]string{"mode": "static-code"}})
	if err != nil {
		t.Fatal(err)
	}
	if code != http.StatusForbidden || !strings.Contains(string(body), "SKILL_RUN_REFUSED") ||
		!strings.Contains(string(body), "filesystem_isolation") {
		t.Fatalf("a run with no runtime answered %d %s", code, body)
	}
	if n := len(s.listRuns("medusa")); n != 0 {
		t.Fatalf("a refused run was recorded: %d", n)
	}
}

func imageDigest(t *testing.T) string {
	t.Helper()
	out, err := exec.Command("docker", "image", "inspect", "alpine:3.19", "--format", "{{.Id}}").Output()
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(out))
}
