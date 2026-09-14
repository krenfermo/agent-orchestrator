package backup

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// §36/§59 — measurements, opt-in. Neither test runs in a normal `go test`.
//
//	AO_P10_BENCH=1 [AO_P10_BENCH_MB=400]   synthetic database, full create/verify/restore
//	AO_P10_REAL_DB=/path/ao.db AO_P10_REAL_OUT=/scratch/dir
//	                                        read-only snapshot timing of a real database
//
// The real-database test opens the source with mode=ro&immutable=1 only: no
// lock, no -wal/-shm, no write of any kind; it proves that by comparing size,
// mtime and SHA-256 before and after and checking no sidecar appeared.

type diskSampler struct {
	dirs []string
	mu   sync.Mutex
	base int64
	peak int64
	done chan struct{}
	wg   sync.WaitGroup
}

func dirBytes(dirs ...string) int64 {
	var total int64
	for _, d := range dirs {
		_ = filepath.WalkDir(d, func(_ string, e fs.DirEntry, err error) error {
			if err == nil && !e.IsDir() {
				if info, ierr := e.Info(); ierr == nil {
					total += info.Size()
				}
			}
			return nil
		})
	}
	return total
}

func startDiskSampler(dirs ...string) *diskSampler {
	s := &diskSampler{dirs: dirs, done: make(chan struct{})}
	s.base = dirBytes(dirs...)
	s.peak = s.base
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		t := time.NewTicker(50 * time.Millisecond)
		defer t.Stop()
		for {
			select {
			case <-s.done:
				return
			case <-t.C:
				n := dirBytes(s.dirs...)
				s.mu.Lock()
				s.peak = max(s.peak, n)
				s.mu.Unlock()
			}
		}
	}()
	return s
}

// stop returns the peak extra bytes over the starting footprint.
func (s *diskSampler) stop() int64 {
	close(s.done)
	s.wg.Wait()
	s.peak = max(s.peak, dirBytes(s.dirs...))
	return s.peak - s.base
}

func maxRSS() int64 {
	var ru syscall.Rusage
	if err := syscall.Getrusage(syscall.RUSAGE_SELF, &ru); err != nil {
		return 0
	}
	return ru.Maxrss // bytes on darwin, kilobytes on linux
}

type measurement struct {
	Op            string `json:"op"`
	DurationMs    int64  `json:"durationMs"`
	HeapPeakBytes uint64 `json:"heapPeakAboveBaselineBytes"`
	DiskPeakBytes int64  `json:"diskPeakAboveStartBytes"`
	MaxRSS        int64  `json:"processMaxRSS"`
}

func measure(t *testing.T, op string, dirs []string, fn func() error) measurement {
	t.Helper()
	disk := startDiskSampler(dirs...)
	start := time.Now()
	var err error
	base, peak := peakHeapDuring(func() { err = fn() })
	m := measurement{Op: op, DurationMs: time.Since(start).Milliseconds(), HeapPeakBytes: peak - base, DiskPeakBytes: disk.stop(), MaxRSS: maxRSS()}
	if err != nil {
		t.Fatalf("%s: %v", op, err)
	}
	t.Logf("%-16s %8dms  heap+%6.1f MiB  disk+%7.1f MiB  maxrss %d", op, m.DurationMs,
		float64(m.HeapPeakBytes)/(1<<20), float64(m.DiskPeakBytes)/(1<<20), m.MaxRSS)
	return m
}

func TestP10BenchmarkSyntheticDatabase(t *testing.T) {
	if os.Getenv("AO_P10_BENCH") != "1" {
		t.Skip("set AO_P10_BENCH=1 to measure backup/verify/restore on a synthetic database")
	}
	mb := 400
	if v, err := strconv.Atoi(os.Getenv("AO_P10_BENCH_MB")); err == nil && v > 0 {
		mb = v
	}
	parent := t.TempDir()
	dataDir := newInstallation(t, parent)
	root := filepath.Join(parent, "backups")

	db := openRW(t, dataDir)
	if _, err := db.Exec(addProject("bench")); err != nil {
		t.Fatal(err)
	}
	blob := strings.Repeat("o", 4000)
	rows := mb * 1024 * 1024 / 4200
	for done := 0; done < rows; {
		tx, err := db.Begin()
		if err != nil {
			t.Fatal(err)
		}
		stmt, err := tx.Prepare(`INSERT INTO workflow_runs (id, project_id, objective, state, policy_version, policy_snapshot, created_at, updated_at)
			VALUES (?, 'bench', ?, 'running', 'v1', '{}', CURRENT_TIMESTAMP, CURRENT_TIMESTAMP)`)
		if err != nil {
			t.Fatal(err)
		}
		for i := 0; i < 2000 && done < rows; i++ {
			if _, err := stmt.Exec(fmt.Sprintf("r%09d", done), blob); err != nil {
				t.Fatal(err)
			}
			done++
		}
		_ = stmt.Close()
		if err := tx.Commit(); err != nil {
			t.Fatal(err)
		}
	}
	_ = db.Close()
	fi, _ := os.Stat(filepath.Join(dataDir, DatabaseAsset))
	t.Logf("synthetic database: %d rows, %.1f MiB", rows, float64(fi.Size())/(1<<20))

	ctx := context.Background()
	var res *CreateResult
	results := []measurement{measure(t, "create", []string{root}, func() (err error) {
		res, err = Create(ctx, CreateOptions{DataDir: dataDir, Root: root, Tool: ToolInfo{Name: "ao-bench"}})
		return err
	})}
	results = append(results, measure(t, "verify (full)", nil, func() error {
		rep, err := Verify(ctx, res.Path, VerifyOptions{})
		if err == nil && rep.Status != StatusValid {
			err = fmt.Errorf("status %s", rep.Status)
		}
		return err
	}))
	results = append(results, measure(t, "verify (quick)", nil, func() error {
		_, err := Verify(ctx, res.Path, VerifyOptions{Quick: true})
		return err
	}))
	mutate(t, dataDir, addProject("after"))
	results = append(results, measure(t, "restore", []string{dataDir, root}, func() error {
		_, err := Restore(ctx, RestoreOptions{DataDir: dataDir, Source: res.Path, Root: root, CheckDaemon: noDaemon, Tool: ToolInfo{Name: "ao-bench"}})
		return err
	}))
	if out := os.Getenv("AO_P10_BENCH_OUT"); out != "" {
		b, _ := json.MarshalIndent(map[string]any{"databaseBytes": fi.Size(), "backupBytes": res.SizeBytes, "results": results}, "", "  ")
		_ = os.WriteFile(out, b, 0o600)
	}
}

type fileFacts struct {
	size  int64
	mtime time.Time
	sum   string
}

func factsOf(t *testing.T, p string) fileFacts {
	t.Helper()
	fi, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	_, sum, err := hashFile(context.Background(), p)
	if err != nil {
		t.Fatal(err)
	}
	return fileFacts{size: fi.Size(), mtime: fi.ModTime(), sum: sum}
}

func TestP10RealDatabaseReadOnlySnapshot(t *testing.T) {
	src, outDir := os.Getenv("AO_P10_REAL_DB"), os.Getenv("AO_P10_REAL_OUT")
	if src == "" || outDir == "" {
		t.Skip("set AO_P10_REAL_DB and AO_P10_REAL_OUT to measure a read-only snapshot of a real database")
	}
	sidecarsBefore := map[string]bool{}
	for _, s := range sqliteSidecars {
		_, err := os.Lstat(src + strings.TrimPrefix(s, DatabaseAsset))
		sidecarsBefore[s] = err == nil
	}
	hashStart := time.Now()
	before := factsOf(t, src)
	hashMs := time.Since(hashStart).Milliseconds()

	if err := os.MkdirAll(outDir, 0o700); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(outDir, "ao-real-snapshot.db")
	if _, err := os.Lstat(dst); err == nil {
		t.Fatalf("%s already exists", dst)
	}
	t.Cleanup(func() { _ = os.Remove(dst) })

	db, err := sql.Open("sqlite", "file:"+src+"?mode=ro&immutable=1")
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	var snapMs int64
	base, peak := peakHeapDuring(func() {
		start := time.Now()
		_, err = db.Exec(`VACUUM INTO ?`, dst)
		snapMs = time.Since(start).Milliseconds()
	})
	_ = db.Close()
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	out, _ := os.Stat(dst)

	start := time.Now()
	facts, err := inspectDatabase(context.Background(), dst, false)
	checkMs := time.Since(start).Milliseconds()
	if err != nil {
		t.Fatal(err)
	}
	start = time.Now()
	_, _, _ = hashFile(context.Background(), dst)
	outHashMs := time.Since(start).Milliseconds()

	after := factsOf(t, src)
	if after != before {
		t.Fatalf("THE SOURCE CHANGED: before %+v after %+v", before, after)
	}
	for _, s := range sqliteSidecars {
		_, err := os.Lstat(src + strings.TrimPrefix(s, DatabaseAsset))
		if (err == nil) != sidecarsBefore[s] {
			t.Fatalf("sidecar %s appeared or vanished next to the source", s)
		}
	}
	t.Logf("source %.1f MiB sha256 %s (hash %dms), unchanged after", float64(before.size)/(1<<20), before.sum, hashMs)
	t.Logf("VACUUM INTO %dms -> %.1f MiB, heap+%.1f MiB, maxrss %d", snapMs, float64(out.Size())/(1<<20), float64(peak-base)/(1<<20), maxRSS())
	t.Logf("integrity_check+fk+goose on the copy %dms: integrity=%q fk=%d goose=%d; copy hash %dms",
		checkMs, facts.Integrity, facts.ForeignKeyViolations, facts.GooseVersion, outHashMs)
	if facts.Integrity != "ok" {
		t.Errorf("the snapshot of the real database failed integrity_check: %s", facts.Integrity)
	}
	if errors.Is(err, os.ErrNotExist) {
		t.Fatal("unreachable")
	}
}
