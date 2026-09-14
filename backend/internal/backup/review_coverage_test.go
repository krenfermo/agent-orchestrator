package backup

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// Coverage the independent review asked for beyond the author's tests.

// §34 — secrets, prompts, env-like strings and a /Users path living IN the
// database never leak into anything backup/restore writes or prints: the
// manifest, the create/verify/list/restore/recover reports, the operations log.
func TestNoSecretReachesAnyReport(t *testing.T) {
	f := newFixture(t)
	secrets := []string{"Bearer sk-live-REVIEW-SECRET", "hunter2-review-password", "PROMPT-CONVERSATION-REVIEW",
		"AWS_SECRET_ACCESS_KEY=AKIAREVIEW", "/Users/victim/secret-project", testSecretKey}
	mutate(t, f.dataDir,
		`INSERT INTO projects (id, path, registered_at) VALUES ('Bearer sk-live-REVIEW-SECRET', '/Users/victim/secret-project', CURRENT_TIMESTAMP)`,
		`INSERT INTO projects (id, path, registered_at) VALUES ('hunter2-review-password PROMPT-CONVERSATION-REVIEW AWS_SECRET_ACCESS_KEY=AKIAREVIEW', '/tmp/x', CURRENT_TIMESTAMP)`)
	res := mustCreate(t, f.dataDir, f.root)
	vr, err := Verify(context.Background(), res.Path, VerifyOptions{})
	if err != nil || vr.Status != StatusValid {
		t.Fatalf("verify: %+v %v", vr, err)
	}
	lr, err := List(f.dataDir, f.root)
	if err != nil {
		t.Fatal(err)
	}
	rr, err := f.restore(t, func(o *RestoreOptions) { o.Source = res.Path })
	if err != nil {
		t.Fatal(err)
	}
	rec, err := Recover(context.Background(), RecoverOptions{DataDir: f.dataDir, CheckDaemon: noDaemon})
	if err != nil {
		t.Fatal(err)
	}
	pr, err := Prune(context.Background(), PruneOptions{Root: f.root})
	if err != nil {
		t.Fatal(err)
	}
	blobs := map[string]string{}
	for name, v := range map[string]any{"create": res, "verify": vr, "list": lr, "restore": rr, "recover": rec, "prune": pr} {
		b, _ := json.Marshal(v)
		blobs[name] = string(b)
	}
	for _, p := range []string{filepath.Join(res.Path, ManifestName), filepath.Join(f.root, opsLogName)} {
		b, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		blobs[filepath.Base(p)] = string(b)
	}
	for name, blob := range blobs {
		for _, s := range secrets {
			if strings.Contains(blob, s) {
				t.Errorf("%s contains %q", name, s)
			}
		}
	}
}

// §27 — creates running while prune --apply loops: no create fails, no prune
// errors, and everything left in the root is a finalized backup that verifies.
func TestCreateAndPruneConcurrently(t *testing.T) {
	parent := t.TempDir()
	dataDir := newInstallation(t, parent)
	root := filepath.Join(parent, "backups")
	var stop atomic.Bool
	var pruneErrs atomic.Int32
	var pwg sync.WaitGroup
	pwg.Add(1)
	go func() {
		defer pwg.Done()
		for !stop.Load() {
			if rep, err := Prune(context.Background(), PruneOptions{Root: root, KeepLast: 1, Apply: true}); err != nil || rep.Errors > 0 {
				pruneErrs.Add(1)
			}
		}
	}()
	var wg sync.WaitGroup
	var createErrs atomic.Int32
	for g := 0; g < 3; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 4; i++ {
				if _, err := Create(context.Background(), CreateOptions{DataDir: dataDir, Root: root, Tool: ToolInfo{Name: "ao-test"}}); err != nil {
					createErrs.Add(1)
				}
			}
		}()
	}
	wg.Wait()
	stop.Store(true)
	pwg.Wait()
	if createErrs.Load() != 0 || pruneErrs.Load() != 0 {
		t.Fatalf("create errors %d, prune errors %d", createErrs.Load(), pruneErrs.Load())
	}
	rep, err := List(dataDir, root)
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Entries) == 0 {
		t.Fatal("prune deleted every backup")
	}
	for _, e := range rep.Entries {
		vr, err := Verify(context.Background(), e.Path, VerifyOptions{Quick: true})
		if err != nil || e.State != StateOK || vr.Status != StatusValid {
			t.Errorf("%s: state %s verify %+v %v", e.Name, e.State, vr, err)
		}
	}
}

// §11 — several online backups while a real writer keeps committing: each is a
// self-consistent snapshot (integrity, FK, a dense prefix of the writes).
func TestOnlineBackupsUnderWritesAreAlwaysSnapshots(t *testing.T) {
	dataDir := newInstallation(t, t.TempDir())
	root := filepath.Join(t.TempDir(), "backups")
	mutate(t, dataDir, addProject("p"))
	writer := openRW(t, dataDir)
	defer func() { _ = writer.Close() }()
	var written atomic.Int64
	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			if _, err := writer.Exec(
				`INSERT INTO workflow_runs (id, project_id, objective, state, policy_version, policy_snapshot, created_at, updated_at)
				 VALUES (?, 'p', 'o', 'running', 'v1', '{}', CURRENT_TIMESTAMP, CURRENT_TIMESTAMP)`, fmt.Sprintf("wf-%07d", i)); err != nil {
				t.Errorf("writer: %v", err)
				return
			}
			written.Store(int64(i + 1))
		}
	}()
	prev := 0
	for iter := 0; iter < 5; iter++ {
		time.Sleep(20 * time.Millisecond)
		res, err := Create(context.Background(), CreateOptions{DataDir: dataDir, Root: root, Tool: ToolInfo{Name: "ao-test"}})
		if err != nil {
			t.Fatalf("online create %d: %v", iter, err)
		}
		vr, err := Verify(context.Background(), res.Path, VerifyOptions{})
		if err != nil || vr.Status != StatusValid {
			t.Fatalf("verify %d: %+v %v", iter, vr, err)
		}
		db, err := openImmutable(filepath.Join(res.Path, DatabaseAsset))
		if err != nil {
			t.Fatal(err)
		}
		var runs int
		var maxID string
		err = db.QueryRow(`SELECT COUNT(*), COALESCE(MAX(id), '') FROM workflow_runs`).Scan(&runs, &maxID)
		_ = db.Close()
		if err != nil {
			t.Fatal(err)
		}
		if runs < prev || int64(runs) > written.Load() || (runs > 0 && maxID != fmt.Sprintf("wf-%07d", runs-1)) {
			t.Fatalf("snapshot %d is not a prefix: %d runs (previous %d, written %d), max %s", iter, runs, prev, written.Load(), maxID)
		}
		prev = runs
	}
	close(stop)
	wg.Wait()
	if prev == 0 {
		t.Fatal("no snapshot saw any write; the test proved nothing")
	}
}

// §29 — a create that fails while copying an asset (an unreadable skill file
// stands in for a failing disk) keeps nothing and never touches the source.
func TestCreateFailingMidCopyKeepsNothing(t *testing.T) {
	parent := t.TempDir()
	dataDir := newInstallation(t, parent)
	root := filepath.Join(parent, "backups")
	before := stripSidecars(managedDigest(t, dataDir))
	p := filepath.Join(dataDir, "skills", "catalog", "pkg-a", "SKILL.md")
	if err := os.Chmod(p, 0); err != nil {
		t.Fatal(err)
	}
	_, err := Create(context.Background(), CreateOptions{DataDir: dataDir, Root: root, Tool: ToolInfo{Name: "ao-test"}})
	if chmodErr := os.Chmod(p, 0o644); chmodErr != nil {
		t.Fatal(chmodErr)
	}
	if err == nil {
		t.Fatal("create succeeded without being able to read a skill file")
	}
	entries, _ := os.ReadDir(root)
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "aob-") || strings.HasPrefix(e.Name(), stagingPrefix) {
			t.Fatalf("a failed create left %s", e.Name())
		}
	}
	if stripSidecars(managedDigest(t, dataDir)) != before {
		t.Fatal("a failed create changed the source")
	}
}
