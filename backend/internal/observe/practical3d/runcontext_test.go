package practical3d

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
)

// checkRunContext must bind AO's own Project Memory record to the frozen
// treatment: OFF renders nothing; ASSISTED renders exactly the frozen pack,
// and only for the targeted role.
func TestCheckRunContextBindsFrozenPack(t *testing.T) {
	t.Parallel()
	present := true
	frozen := strings.Repeat("f", 64)
	m := Manifest{TreatmentMapping: []TreatmentCell{{TaskID: "A", Role: RoleWorker, CallClass: CallInitial,
		ASSISTED: TreatmentArm{AttachmentPresent: &present, AttachmentVersion: "ao-project-memory-pack:" + frozen}}}}
	for name, tc := range map[string]struct {
		arm      Arm
		ext      string
		packs    [][2]string // role, digest
		wantFail string
	}{
		"off clean":             {arm: ArmOff, ext: "off"},
		"off with pack":         {arm: ArmOff, ext: "off", packs: [][2]string{{"worker", frozen}}, wantFail: "OFF run"},
		"external context on":   {arm: ArmOff, ext: "on", wantFail: "externalContext"},
		"assisted frozen":       {arm: ArmAssisted, ext: "off", packs: [][2]string{{"worker", frozen}}},
		"assisted none":         {arm: ArmAssisted, ext: "off", wantFail: "no Project Memory"},
		"assisted other pack":   {arm: ArmAssisted, ext: "off", packs: [][2]string{{"worker", strings.Repeat("e", 64)}}, wantFail: "frozen pack"},
		"assisted reviewer too": {arm: ArmAssisted, ext: "off", packs: [][2]string{{"worker", frozen}, {"reviewer", frozen}}, wantFail: "only worker"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			db, err := sql.Open("sqlite", filepath.Join(dir, "ao.db"))
			if err != nil {
				t.Fatal(err)
			}
			for _, q := range []string{
				`CREATE TABLE workflow_runs (id TEXT, policy_snapshot TEXT)`,
				`CREATE TABLE project_memory_context_manifests (workflow_run_id TEXT, role TEXT, pack_digest TEXT)`,
				`INSERT INTO workflow_runs VALUES ('wf', '{"contextSources":{"externalContext":"` + tc.ext + `"}}')`,
			} {
				if _, err := db.Exec(q); err != nil {
					t.Fatal(err)
				}
			}
			for _, p := range tc.packs {
				if _, err := db.Exec(`INSERT INTO project_memory_context_manifests VALUES ('wf', ?, ?)`, p[0], p[1]); err != nil {
					t.Fatal(err)
				}
			}
			_ = db.Close()
			err = checkRunContext(context.Background(), dir, "wf", tc.arm, m, "A")
			switch {
			case tc.wantFail == "" && err != nil:
				t.Fatalf("unexpected failure: %v", err)
			case tc.wantFail != "" && (err == nil || !strings.Contains(err.Error(), tc.wantFail)):
				t.Fatalf("want failure %q, got %v", tc.wantFail, err)
			}
		})
	}
}
