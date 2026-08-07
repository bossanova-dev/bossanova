package tuitest_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/recurser/boss/internal/client"
	"github.com/recurser/boss/internal/tuitest"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

// stallWindow is long enough that a call that did NOT wait for it is
// unmistakable, and short enough to keep the suite fast. It sits well under the
// client's 30s unary bound, which is deliberately NOT shrunk here: the point is
// that the stall itself delays the answer, not that the client gives up.
const stallWindow = 400 * time.Millisecond

// newStallDaemon starts a mock daemon on a socket in its OWN short /tmp
// directory, rather than using NewMockDaemon's shared /tmp.
//
// socketauth.TokenPath derives the token from the socket's DIRECTORY, so every
// daemon NewMockDaemon starts shares /tmp/bossd.token — and stopping any one of
// them deletes it. That is tolerable within a single serialized package, but
// `go test ./...` runs package binaries concurrently, and these tests hold a
// daemon open for the length of a stall window, widening the interval in which
// a sibling binary's client can read a token that has just been removed. A
// private directory keeps the token private too. /tmp rather than t.TempDir()
// because macOS caps a Unix socket path at 104 bytes.
func newStallDaemon(t *testing.T) *tuitest.MockDaemon {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "bos723-")
	if err != nil {
		t.Fatalf("mkdtemp: %v", err)
	}
	d, stop, err := tuitest.StartMockDaemon(filepath.Join(dir, "d.sock"))
	if err != nil {
		_ = os.RemoveAll(dir)
		t.Fatalf("start mock daemon: %v", err)
	}
	t.Cleanup(func() {
		// Clear any stall first so a parked handler is released rather than
		// holding the server's Close.
		d.SetRPCStall(0)
		_ = stop()
		_ = os.RemoveAll(dir)
	})
	return d
}

// TestSetRPCStallDelaysEveryUnaryRPC proves the stall is real (a unary call
// answers only after the window closes) and that the window expiring restores
// immediate answers. Two different procedures are exercised so the interceptor
// is shown to be blanket rather than wired to one handler.
//
// Every assertion measures from the instant the window was ARMED, not from the
// call: the window ends at a fixed wall-clock time, so anything done between
// arming and calling comes out of it.
func TestSetRPCStallDelaysEveryUnaryRPC(t *testing.T) {
	d := newStallDaemon(t)
	c := client.NewLocal(d.SocketPath())
	ctx := context.Background()

	// Baseline: no stall, so the call is effectively instant.
	start := time.Now()
	listSessions(t, ctx, c)
	if elapsed := time.Since(start); elapsed >= stallWindow {
		t.Fatalf("unstalled ListSessions took %v; the mock is slow independent of the stall", elapsed)
	}

	armed := time.Now()
	d.SetRPCStall(stallWindow)
	listSessions(t, ctx, c)
	if waited := time.Since(armed); waited < stallWindow {
		t.Fatalf("stalled ListSessions answered %v after the wedge, want at least %v", waited, stallWindow)
	}

	// A second procedure, so this is not a one-handler edit.
	armed = time.Now()
	d.SetRPCStall(stallWindow)
	if _, err := c.ListRepos(ctx); err != nil {
		t.Fatalf("ListRepos: %v", err)
	}
	if waited := time.Since(armed); waited < stallWindow {
		t.Fatalf("stalled ListRepos answered %v after the wedge, want at least %v", waited, stallWindow)
	}

	// The window has closed on its own by now, so the next call is immediate
	// with no explicit clear.
	start = time.Now()
	listSessions(t, ctx, c)
	if elapsed := time.Since(start); elapsed >= stallWindow {
		t.Fatalf("ListSessions after the window closed took %v; the stall did not expire", elapsed)
	}
}

// TestSetRPCStallClearReleasesWaiters proves d <= 0 clears the stall
// IMMEDIATELY, including for a call already parked inside the window. Without
// that, a scenario could only ever wait a wedge out, never end one — which is
// exactly the self-recovery a proof scene has to show.
func TestSetRPCStallClearReleasesWaiters(t *testing.T) {
	d := newStallDaemon(t)
	c := client.NewLocal(d.SocketPath())
	ctx := context.Background()

	const longWindow = 30 * time.Second
	armed := time.Now()
	d.SetRPCStall(longWindow)

	done := make(chan error, 1)
	go func() {
		_, err := c.ListSessions(ctx, &pb.ListSessionsRequest{}, client.SessionReadOptions{})
		done <- err
	}()

	// Give the request time to reach the interceptor and park. If it answered
	// anyway the stall never took hold, and the release this test claims to
	// prove would be unobservable.
	time.Sleep(50 * time.Millisecond)
	select {
	case <-done:
		t.Fatal("ListSessions answered while the stall window was open; nothing was parked to release")
	default:
	}
	d.SetRPCStall(0)

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("ListSessions failed after the stall was cleared: %v", err)
		}
		if waited := time.Since(armed); waited >= longWindow {
			t.Fatalf("parked call waited %v; clearing the stall did not release it", waited)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("clearing the stall did not release the parked call within 5s")
	}
}

// TestSetRPCStallForMatchesTheMethodSegmentExactly proves the scope is an exact
// method match, not a substring one: "Chat" names no procedure, so it must wedge
// nothing — least of all RecordChat, ListChats, AddChat and DeleteChat together,
// which a strings.Contains match over the full Connect path would do.
func TestSetRPCStallForMatchesTheMethodSegmentExactly(t *testing.T) {
	d := newStallDaemon(t)
	c := client.NewLocal(d.SocketPath())
	ctx := context.Background()

	armed := time.Now()
	d.SetRPCStallFor("Chat", stallWindow)

	if _, err := c.RecordChat(ctx, "sess-1", "agent-1", "New chat", "", false); err != nil {
		t.Fatalf("RecordChat: %v", err)
	}
	if waited := time.Since(armed); waited >= stallWindow {
		t.Fatalf("RecordChat waited %v under a stall scoped to %q; the scope over-matched", waited, "Chat")
	}
	if _, err := c.ListChats(ctx, "sess-1"); err != nil {
		t.Fatalf("ListChats: %v", err)
	}
	if waited := time.Since(armed); waited >= stallWindow {
		t.Fatalf("ListChats waited %v under a stall scoped to %q; the scope over-matched", waited, "Chat")
	}

	// The same window, named exactly, DOES wedge — so the case above is a
	// scoping result and not a stall that silently failed to arm.
	armed = time.Now()
	d.SetRPCStallFor("RecordChat", stallWindow)
	if _, err := c.RecordChat(ctx, "sess-1", "agent-1", "New chat", "", false); err != nil {
		t.Fatalf("RecordChat: %v", err)
	}
	if waited := time.Since(armed); waited < stallWindow {
		t.Fatalf("exactly-named RecordChat answered %v after the wedge, want at least %v", waited, stallWindow)
	}
}

// TestSetRPCStallForScopesToOneProcedure proves the procedure-scoped stall
// blocks only the named procedure and leaves every other unary RPC untouched.
// The proof scenario needs exactly this: GetSession/ListChats must succeed so
// the attach view reaches RecordChat, and only RecordChat may be wedged.
func TestSetRPCStallForScopesToOneProcedure(t *testing.T) {
	d := newStallDaemon(t)
	c := client.NewLocal(d.SocketPath())
	ctx := context.Background()

	armed := time.Now()
	d.SetRPCStallFor("RecordChat", stallWindow)

	// The unnamed procedure is not stalled.
	start := time.Now()
	listSessions(t, ctx, c)
	if elapsed := time.Since(start); elapsed >= stallWindow {
		t.Fatalf("ListSessions took %v under a RecordChat-scoped stall; the scope leaked", elapsed)
	}

	if _, err := c.RecordChat(ctx, "sess-1", "agent-1", "New chat", "", false); err != nil {
		t.Fatalf("RecordChat: %v", err)
	}
	if waited := time.Since(armed); waited < stallWindow {
		t.Fatalf("scoped RecordChat answered %v after the wedge, want at least %v", waited, stallWindow)
	}
}

// TestSetRPCStallLeavesStreamingAlone proves the interceptor's streaming
// wrappers are pass-throughs: the mock's AttachSession stream must stay
// openable while every unary RPC is wedged, because a scenario wedges the daemon
// while the TUI is attached.
func TestSetRPCStallLeavesStreamingAlone(t *testing.T) {
	d := newStallDaemon(t)
	c := client.NewLocal(d.SocketPath())

	d.SetRPCStall(30 * time.Second)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	const id = "sess-stall-stream"
	opened := make(chan struct{})
	go func() {
		defer close(opened)
		stream, err := c.AttachSession(ctx, id)
		if err != nil {
			return
		}
		defer func() { _ = stream.Close() }()
		<-ctx.Done()
	}()

	deadline := time.Now().Add(3 * time.Second)
	for !d.SessionAttached(id) {
		if time.Now().After(deadline) {
			t.Fatal("AttachSession never reached the handler while unary RPCs were stalled")
		}
		time.Sleep(10 * time.Millisecond)
	}

	// The wedge must genuinely have been in force while the stream flowed —
	// otherwise "streaming is exempt" would be proved by a daemon that was
	// never stalled at all. A unary call under the same window must not answer.
	unary := make(chan struct{})
	go func() {
		defer close(unary)
		_, _ = c.ListSessions(ctx, &pb.ListSessionsRequest{}, client.SessionReadOptions{})
	}()
	select {
	case <-unary:
		t.Fatal("a unary RPC answered while the stall window was open; the daemon was not wedged")
	case <-time.After(250 * time.Millisecond):
	}

	cancel()
	<-opened
	<-unary
}

func listSessions(t *testing.T, ctx context.Context, c *client.LocalClient) {
	t.Helper()
	if _, err := c.ListSessions(ctx, &pb.ListSessionsRequest{}, client.SessionReadOptions{}); err != nil {
		t.Fatalf("ListSessions: %v", err)
	}
}
