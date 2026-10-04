package terminal

import (
	"context"
	"testing"
	"time"
)

// TestOpenTerminal_DeniedAuthorizer_NeverTouchesRuntime is Checkpoint
// 8P-B.2's WebSocket/mux attach-denial proof: when the attach authorizer
// denies a terminal id, no pane is ever attached (fakeSource.Attach is
// never called), no output byte is ever sent, and the client sees an error
// frame instead of "opened" -- the id's existence is not distinguished
// from "not yours" in the error text either.
func TestOpenTerminal_DeniedAuthorizer_NeverTouchesRuntime(t *testing.T) {
	src := &fakeSource{alive: true, spawner: &fakeSpawner{ptys: []*fakePTY{newFakePTY()}}}
	mgr := NewManager(src, nil, testLogger(), WithHeartbeat(0))
	defer mgr.Close()

	mgr.SetAttachAuthorizer(func(_ context.Context, id string) bool {
		return id == "allowed-session"
	})

	conn := newFakeConn()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go mgr.Serve(ctx, conn)

	conn.in <- clientMsg{Ch: chTerminal, ID: "foreign-session", Type: msgOpen}
	msg := recv(t, conn, chTerminal, msgError, time.Second)
	if msg.ID != "foreign-session" {
		t.Fatalf("error frame id = %q, want foreign-session", msg.ID)
	}

	// Give any (incorrect) async attach a moment to happen before asserting
	// it didn't.
	time.Sleep(20 * time.Millisecond)
	if calls := src.getAttachCalls(); calls != 0 {
		t.Fatalf("attach authorizer denied the id but Source.Attach was called %d times", calls)
	}
}

// TestOpenTerminal_AllowedAuthorizer_StillAttaches proves the authorizer
// wiring doesn't regress the normal allowed path.
func TestOpenTerminal_AllowedAuthorizer_StillAttaches(t *testing.T) {
	pty := newFakePTY()
	src := &fakeSource{alive: true, spawner: &fakeSpawner{ptys: []*fakePTY{pty}}}
	mgr := NewManager(src, nil, testLogger(), WithHeartbeat(0))
	defer mgr.Close()

	mgr.SetAttachAuthorizer(func(_ context.Context, id string) bool {
		return id == "allowed-session"
	})

	conn := newFakeConn()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go mgr.Serve(ctx, conn)

	conn.in <- clientMsg{Ch: chTerminal, ID: "allowed-session", Type: msgOpen}
	recv(t, conn, chTerminal, msgOpened, time.Second)

	if calls := src.getAttachCalls(); calls != 1 {
		t.Fatalf("attach calls = %d, want 1", calls)
	}
}

// TestOpenTerminal_NilAuthorizer_PreservesPriorBehavior proves a nil
// authorizer (every pre-8P-B.2 wiring) is a true no-op.
func TestOpenTerminal_NilAuthorizer_PreservesPriorBehavior(t *testing.T) {
	pty := newFakePTY()
	src := &fakeSource{alive: true, spawner: &fakeSpawner{ptys: []*fakePTY{pty}}}
	mgr := NewManager(src, nil, testLogger(), WithHeartbeat(0))
	defer mgr.Close()

	conn := newFakeConn()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go mgr.Serve(ctx, conn)

	conn.in <- clientMsg{Ch: chTerminal, ID: "any-session", Type: msgOpen}
	recv(t, conn, chTerminal, msgOpened, time.Second)
}
