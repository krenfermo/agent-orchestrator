package daemonlock

import (
	"errors"
	"path/filepath"
	"sync"
	"testing"
)

// Two holders can never coexist, a release frees the lock, and N concurrent
// acquirers produce exactly one winner.
func TestAcquireIsExclusiveAndReleasable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "data", "daemon.lock")
	first, err := Acquire(path)
	if err != nil {
		t.Fatalf("first acquire: %v", err)
	}
	if _, err := Acquire(path); !errors.Is(err, ErrHeld) {
		t.Fatalf("second acquire = %v, want ErrHeld", err)
	}
	if err := first.Release(); err != nil {
		t.Fatal(err)
	}
	again, err := Acquire(path)
	if err != nil {
		t.Fatalf("acquire after release: %v", err)
	}
	_ = again.Release()
}

func TestConcurrentAcquireHasExactlyOneWinner(t *testing.T) {
	path := filepath.Join(t.TempDir(), "daemon.lock")
	const n = 16
	var wg sync.WaitGroup
	var mu sync.Mutex
	winners, held := 0, 0
	locks := make([]*Lock, 0, n)
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			l, err := Acquire(path)
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				winners++
				locks = append(locks, l)
			case errors.Is(err, ErrHeld):
				held++
			default:
				t.Errorf("acquire: %v", err)
			}
		}()
	}
	close(start)
	wg.Wait()
	for _, l := range locks {
		_ = l.Release()
	}
	if winners != 1 || held != n-1 {
		t.Fatalf("winners=%d held=%d, want exactly one winner", winners, held)
	}
}
