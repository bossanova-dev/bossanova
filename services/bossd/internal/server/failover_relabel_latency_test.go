package server

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"github.com/recurser/bossd/internal/db"
	"github.com/recurser/bossd/internal/rotation"
	"github.com/recurser/bossd/internal/session"
	"github.com/recurser/bossd/internal/tmux"
)

// committingFailover is fakeFailover with the real commit, so the proxy runs
// Lifecycle.CommitFailover between the replay's headers and its body.
type committingFailover struct {
	*fakeFailover
	lc *session.Lifecycle
}

func (f *committingFailover) CommitFailover(ctx context.Context, sessionID string, r session.FailoverResult) error {
	return f.lc.CommitFailover(ctx, sessionID, r)
}

// slowPaneTmux answers show-environment from baked after wait returns, and
// every other command at once. wait models a slow or wedged tmux server.
func slowPaneTmux(baked map[string]string, wait func(context.Context)) *tmux.Client {
	return tmux.NewClient(tmux.WithCommandFactory(func(ctx context.Context, _ string, args ...string) *exec.Cmd {
		if len(args) == 4 && args[0] == "show-environment" {
			wait(ctx)
			if url, ok := baked[args[2]]; ok && ctx.Err() == nil {
				return exec.CommandContext(ctx, "printf", "%s\n", args[3]+"="+url)
			}
			return exec.CommandContext(ctx, "false")
		}
		return exec.CommandContext(ctx, "true")
	}))
}

// firstByteCeiling is far above the no-sibling time to first byte (about 2 ms)
// and below what twenty 10 ms tmux reads, or one wedged read, would add.
const firstByteCeiling = 250 * time.Millisecond

// A failover's replayed response reaches the agent without waiting on tmux. The
// session's account and the rotation audit are written before the response;
// the rows of the panes that follow the session are rewritten afterwards.
func TestFailover_ReplayDoesNotWaitOnTmuxToRelabelSiblingChats(t *testing.T) {
	const siblings = 20
	cases := []struct {
		name string
		// wait runs before each show-environment answer.
		wait func(release <-chan struct{}) func(context.Context)
		// rewritten reports whether the sibling rows reach acct-2 while tmux
		// still behaves this way.
		rewritten bool
	}{
		{"slow tmux", func(<-chan struct{}) func(context.Context) {
			return func(context.Context) { time.Sleep(10 * time.Millisecond) }
		}, true},
		{"wedged tmux", func(release <-chan struct{}) func(context.Context) {
			return func(ctx context.Context) {
				select {
				case <-release:
				case <-ctx.Done():
				}
			}
		}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			sqlDB := rebuildTestDB(t)
			sessions := db.NewSessionStore(sqlDB)
			chats := db.NewAgentChatStore(sqlDB)
			audits := db.NewRotationAuditStore(db.NewRotationEventStore(sqlDB))
			sessionID := rebuildTestSession(t, sqlDB)
			start := "acct-1"
			startPtr := &start
			sess, err := sessions.Update(ctx, sessionID, db.UpdateSessionParams{AccountID: &startPtr})
			if err != nil {
				t.Fatalf("bind session: %v", err)
			}

			f := &committingFailover{fakeFailover: &fakeFailover{result: session.FailoverResult{
				Rotate: true, Token: "second-token", FromAccountID: "acct-1", NextAccountID: "acct-2",
				Provider: "claude", Trigger: "ROTATION_TRIGGER_USAGE_LIMITED",
			}}}
			_, upstream := newFakeUpstream(t)
			ps, base := startProxy(t, f, upstream.URL, zerolog.Nop())
			sessionToken := ps.TokenForSession(sessionID)

			baked := map[string]string{}
			for i := range siblings {
				agentSessionID := fmt.Sprintf("agent-%02d", i)
				acct := "acct-1"
				if _, err := chats.Create(ctx, db.CreateAgentChatParams{
					SessionID: sessionID, AgentSessionID: agentSessionID, AgentName: "claude", AccountID: &acct,
				}); err != nil {
					t.Fatalf("create chat %s: %v", agentSessionID, err)
				}
				baked[tmux.ChatSessionName(sess.RepoID, agentSessionID)] = fmt.Sprintf("http://127.0.0.1:%d/s/%s", restartProxyPort, sessionToken)
			}

			release := make(chan struct{})
			t.Cleanup(func() { close(release) })
			lc := session.NewLifecycle(sessions, nil, chats, nil, nil, nil, slowPaneTmux(baked, tc.wait(release)), nil, zerolog.Nop())
			lc.SetProxyPort(restartProxyPort)
			lc.SetProxyRegistrar(ps)
			lc.SetChatRunningProbe(func(string) bool { return true })
			lc.SetRotationRecorder(rotation.NewRecorder(audits, zerolog.Nop()))
			f.lc = lc

			began := time.Now()
			resp := doMessages(t, base, sessionToken, "first-token", `{"prompt":"hi"}`)
			firstByte := time.Since(began)
			body, _ := io.ReadAll(resp.Body)
			_ = resp.Body.Close()

			if resp.StatusCode != http.StatusOK || string(body) != "pong" {
				t.Fatalf("replay = %d %q, want 200 pong", resp.StatusCode, body)
			}
			if firstByte > firstByteCeiling {
				t.Errorf("time to first byte = %v with %d sibling chats, want under %v", firstByte, siblings, firstByteCeiling)
			}
			if got := mustGetSession(ctx, t, sessions, sessionID).AccountID; got == nil || *got != "acct-2" {
				t.Fatalf("session account = %v when the response arrived, want acct-2", got)
			}
			events, err := audits.RecentBySession(ctx, sessionID, 10)
			if err != nil {
				t.Fatalf("read rotation history: %v", err)
			}
			if len(events) != 1 || events[0].Outcome != "ROTATION_OUTCOME_ROTATED" {
				t.Fatalf("rotation history = %+v when the response arrived, want one ROTATED row", events)
			}

			if !tc.rewritten {
				return
			}
			deadline := time.Now().Add(5 * time.Second)
			for i := range siblings {
				agentSessionID := fmt.Sprintf("agent-%02d", i)
				for chatAccountID(ctx, t, chats, agentSessionID) != "acct-2" {
					if time.Now().After(deadline) {
						t.Fatalf("sibling row %s = %q 5s after the replay, want acct-2", agentSessionID, chatAccountID(ctx, t, chats, agentSessionID))
					}
					time.Sleep(20 * time.Millisecond)
				}
			}
		})
	}
}

// relabelFixture is one session on acct-1 with one running claude chat whose
// pane holds the session token, on a real ProxyServer and real SQLite stores.
// Its tmux answers each show-environment after wait returns.
type relabelFixture struct {
	lc        *session.Lifecycle
	sessions  db.SessionStore
	chats     db.AgentChatStore
	sessionID string
	// reads counts show-environment calls.
	reads atomic.Int32
}

// newRelabelFixture builds the fixture. extraChats adds more running chats on
// the session whose panes hold the session token too.
func newRelabelFixture(t *testing.T, logger zerolog.Logger, wait func(context.Context), extraChats ...string) *relabelFixture {
	t.Helper()
	ctx := context.Background()
	sqlDB := rebuildTestDB(t)
	f := &relabelFixture{sessions: db.NewSessionStore(sqlDB), chats: db.NewAgentChatStore(sqlDB), sessionID: rebuildTestSession(t, sqlDB)}
	start := "acct-1"
	startPtr := &start
	sess, err := f.sessions.Update(ctx, f.sessionID, db.UpdateSessionParams{AccountID: &startPtr})
	if err != nil {
		t.Fatalf("bind session: %v", err)
	}
	ps := newRebuildProxy(t, db.NewProxyTokenStore(sqlDB), &bytes.Buffer{})
	sessionURL := fmt.Sprintf("http://127.0.0.1:%d/s/%s", restartProxyPort, ps.TokenForSession(f.sessionID))
	baked := map[string]string{}
	for _, agentSessionID := range append([]string{"agent-build"}, extraChats...) {
		if _, err := f.chats.Create(ctx, db.CreateAgentChatParams{
			SessionID: f.sessionID, AgentSessionID: agentSessionID, AgentName: "claude", AccountID: &start,
		}); err != nil {
			t.Fatalf("create chat %s: %v", agentSessionID, err)
		}
		baked[tmux.ChatSessionName(sess.RepoID, agentSessionID)] = sessionURL
	}
	counted := func(ctx context.Context) {
		f.reads.Add(1)
		wait(ctx)
	}
	f.lc = session.NewLifecycle(f.sessions, nil, f.chats, nil, nil, nil, slowPaneTmux(baked, counted), nil, logger)
	f.lc.SetProxyPort(restartProxyPort)
	f.lc.SetProxyRegistrar(ps)
	f.lc.SetChatRunningProbe(func(string) bool { return true })
	return f
}

func (f *relabelFixture) failover(t *testing.T, from, next string) {
	t.Helper()
	if err := f.lc.CommitFailover(context.Background(), f.sessionID, session.FailoverResult{
		Rotate: true, FromAccountID: from, NextAccountID: next, Provider: "claude", Trigger: "ROTATION_TRIGGER_USAGE_LIMITED",
	}); err != nil {
		t.Fatalf("CommitFailover to %s: %v", next, err)
	}
}

// move is the manual move: it rebinds the session and rewrites the rows inline.
func (f *relabelFixture) move(t *testing.T, next string) {
	t.Helper()
	ctx := context.Background()
	if err := f.lc.MoveSessionAccount(ctx, mustGetSession(ctx, t, f.sessions, f.sessionID), next); err != nil {
		t.Fatalf("MoveSessionAccount to %s: %v", next, err)
	}
}

// heldFirstRead blocks the first show-environment until release closes, and
// answers later ones at once. With honourCtx it also gives up when the read's
// context ends; without it the read is wedged past any deadline.
func heldFirstRead(honourCtx bool) (wait func(context.Context), entered <-chan struct{}, release chan struct{}) {
	enteredCh, releaseCh := make(chan struct{}), make(chan struct{})
	var once sync.Once
	return func(ctx context.Context) {
		first := false
		once.Do(func() { first = true })
		if !first {
			return
		}
		close(enteredCh)
		if !honourCtx {
			<-releaseCh
			return
		}
		select {
		case <-releaseCh:
		case <-ctx.Done():
		}
	}, enteredCh, releaseCh
}

// Two failovers land back to back while the first rewrite is stuck on tmux.
// The row still ends on the later account.
func TestFailover_BackToBackRelabelsEndOnTheLaterAccount(t *testing.T) {
	ctx := context.Background()
	wait, entered, release := heldFirstRead(true)
	f := newRelabelFixture(t, zerolog.Nop(), wait)

	f.failover(t, "acct-1", "acct-2")
	<-entered
	f.failover(t, "acct-2", "acct-3")
	time.Sleep(300 * time.Millisecond)
	close(release)

	if got := awaitChatAccountID(ctx, t, f.chats, "agent-build", "acct-3"); got != "acct-3" {
		t.Fatalf("build pane row = %q after two failovers, want acct-3", got)
	}
	time.Sleep(300 * time.Millisecond)
	if got := chatAccountID(ctx, t, f.chats, "agent-build"); got != "acct-3" {
		t.Fatalf("build pane row = %q once both rewrites finished, want acct-3", got)
	}
}

// The first rewrite's tmux read is wedged past its whole 5s budget when a
// second failover lands. The second move still wins, and the first pass logs
// the rows its budget did not reach.
func TestFailover_WedgedRelabelStillEndsOnTheLaterAccount(t *testing.T) {
	if testing.Short() {
		t.Skip("waits out the 5s relabel budget; run make test-bossd for coverage")
	}
	ctx := context.Background()
	logs := &lockedBuffer{}
	wait, entered, release := heldFirstRead(false)
	f := newRelabelFixture(t, zerolog.New(logs), wait)

	f.failover(t, "acct-1", "acct-2")
	<-entered
	f.failover(t, "acct-2", "acct-3")
	time.Sleep(5*time.Second + 500*time.Millisecond)
	close(release)

	if got := awaitChatAccountID(ctx, t, f.chats, "agent-build", "acct-3"); got != "acct-3" {
		t.Fatalf("build pane row = %q after a wedged rewrite and a later failover, want acct-3", got)
	}
	if !strings.Contains(logs.String(), "ran out of time moving chat rows to the session's new account; 1 of 1 running chats may still show the previous account (1 cut off mid-read, 0 never reached)") {
		t.Fatalf("logs = %q, want the rows the expired budget did not reach", logs.String())
	}
}

// One rewrite pass gets 5s, and when it stops early it logs how many rows it
// did not reach.
func TestRelabel_PassIsBoundedAndCountsTheRowsItDidNotReach(t *testing.T) {
	logs := &lockedBuffer{}
	deadlines := make(chan time.Duration, 1)
	before := time.Now()
	wait := func(ctx context.Context) {
		if d, ok := ctx.Deadline(); ok {
			select {
			case deadlines <- d.Sub(before):
			default:
			}
		}
		<-ctx.Done()
	}
	f := newRelabelFixture(t, zerolog.New(logs), wait)

	f.failover(t, "acct-1", "acct-2")
	var budget time.Duration
	select {
	case budget = <-deadlines:
	case <-time.After(2 * time.Second):
		t.Fatal("the rewrite never read tmux with a deadline")
	}
	if budget < 5*time.Second || budget > 6*time.Second {
		t.Fatalf("rewrite deadline is %v after the failover, want 5s", budget)
	}
	if !f.lc.StopBackgroundRelabels(withTimeout(t, 2*time.Second)) {
		t.Fatal("StopBackgroundRelabels gave up on a rewrite that honours cancellation")
	}
	if !strings.Contains(logs.String(), "1 of 1 running chats may still show the previous account") {
		t.Fatalf("logs = %q, want the count of rows the stopped pass did not reach", logs.String())
	}
}

// A pass that shutdown stops says so instead of claiming it ran out of time,
// and its count separates the row whose read was cut off from the rows it never
// reached.
func TestRelabel_ShutdownStopIsNotReportedAsATimeout(t *testing.T) {
	logs := &lockedBuffer{}
	var reads atomic.Int32
	cutOff := make(chan struct{})
	wait := func(ctx context.Context) {
		if reads.Add(1) == 1 {
			return
		}
		close(cutOff)
		<-ctx.Done()
	}
	f := newRelabelFixture(t, zerolog.New(logs), wait, "agent-review", "agent-docs")

	f.failover(t, "acct-1", "acct-2")
	select {
	case <-cutOff:
	case <-time.After(2 * time.Second):
		t.Fatal("the rewrite never reached its second row")
	}
	if !f.lc.StopBackgroundRelabels(withTimeout(t, 2*time.Second)) {
		t.Fatal("StopBackgroundRelabels gave up on a rewrite that honours cancellation")
	}
	got := logs.String()
	if strings.Contains(got, "ran out of time") {
		t.Fatalf("logs = %q, want a shutdown stop not reported as a timeout", got)
	}
	if !strings.Contains(got, "was stopped by daemon shutdown while moving chat rows to the session's new account; 2 of 3 running chats may still show the previous account (1 cut off mid-read, 1 never reached)") {
		t.Fatalf("logs = %q, want the shutdown stop with one row cut off and one never reached", got)
	}
}

// A panic inside a rewrite pass releases the session's claim, so a later move
// still rewrites the rows. Left set, the claim turns every later move into a
// mark that nothing runs until the daemon restarts.
func TestRelabel_PanicInAPassLeavesLaterMovesWorking(t *testing.T) {
	ctx := context.Background()
	panicOnce := func(f *relabelFixture) {
		var armed atomic.Bool
		armed.Store(true)
		f.lc.SetChatRunningProbe(func(string) bool {
			if armed.CompareAndSwap(true, false) {
				panic("chat status probe exploded")
			}
			return true
		})
	}

	t.Run("background pass", func(t *testing.T) {
		logs := &lockedBuffer{}
		f := newRelabelFixture(t, zerolog.New(logs), func(context.Context) {})
		panicOnce(f)
		f.failover(t, "acct-1", "acct-2")
		deadline := time.Now().Add(5 * time.Second)
		for !strings.Contains(logs.String(), "recovered from panic in goroutine") {
			if time.Now().After(deadline) {
				t.Fatalf("logs = %q, want the pass's panic recovered", logs.String())
			}
			time.Sleep(10 * time.Millisecond)
		}

		f.move(t, "acct-3")
		if got := chatAccountID(ctx, t, f.chats, "agent-build"); got != "acct-3" {
			t.Fatalf("build pane row = %q after a move that followed a panicked pass, want acct-3", got)
		}
	})

	t.Run("inline pass", func(t *testing.T) {
		f := newRelabelFixture(t, zerolog.Nop(), func(context.Context) {})
		panicOnce(f)
		func() {
			defer func() {
				if recover() == nil {
					t.Fatal("the inline pass did not panic")
				}
			}()
			f.move(t, "acct-2")
		}()

		f.failover(t, "acct-2", "acct-3")
		if got := awaitChatAccountID(ctx, t, f.chats, "agent-build", "acct-3"); got != "acct-3" {
			t.Fatalf("build pane row = %q after a failover that followed a panicked pass, want acct-3", got)
		}
	})
}

// A failover that lands while a manual move's inline pass is reading tmux is
// rerun once the inline pass ends, so the row ends on the failover's account
// rather than the manual move's.
func TestRelabel_FailoverDuringAManualMoveIsRerun(t *testing.T) {
	ctx := context.Background()
	wait, entered, release := heldFirstRead(true)
	f := newRelabelFixture(t, zerolog.Nop(), wait)
	sess := mustGetSession(ctx, t, f.sessions, f.sessionID)

	moved := make(chan error, 1)
	go func() { moved <- f.lc.MoveSessionAccount(ctx, sess, "acct-2") }()
	<-entered
	f.failover(t, "acct-2", "acct-3")
	close(release)
	if err := <-moved; err != nil {
		t.Fatalf("MoveSessionAccount: %v", err)
	}

	if got := awaitChatAccountID(ctx, t, f.chats, "agent-build", "acct-3"); got != "acct-3" {
		t.Fatalf("build pane row = %q after a failover during the manual move, want acct-3", got)
	}
}

// Rewrites are serialised per session, not across sessions: one session's
// rewrite stuck on tmux does not hold up another session's move.
func TestRelabel_ClaimIsPerSession(t *testing.T) {
	ctx := context.Background()
	sqlDB := rebuildTestDB(t)
	sessions := db.NewSessionStore(sqlDB)
	chats := db.NewAgentChatStore(sqlDB)
	ps := newRebuildProxy(t, db.NewProxyTokenStore(sqlDB), &bytes.Buffer{})
	stuckID := rebuildTestSession(t, sqlDB)
	stuck := mustGetSession(ctx, t, sessions, stuckID)
	other, err := sessions.Create(ctx, db.CreateSessionParams{
		RepoID: stuck.RepoID, Title: "second", WorktreePath: "/tmp/wt/second",
		BranchName: "feat/second", BaseBranch: "main", AgentName: "claude",
	})
	if err != nil {
		t.Fatalf("create second session: %v", err)
	}

	start := "acct-1"
	startPtr := &start
	baked := map[string]string{}
	for sessionID, agentSessionID := range map[string]string{stuckID: "agent-stuck", other.ID: "agent-other"} {
		if _, err := sessions.Update(ctx, sessionID, db.UpdateSessionParams{AccountID: &startPtr}); err != nil {
			t.Fatalf("bind session %s: %v", sessionID, err)
		}
		if _, err := chats.Create(ctx, db.CreateAgentChatParams{
			SessionID: sessionID, AgentSessionID: agentSessionID, AgentName: "claude", AccountID: &start,
		}); err != nil {
			t.Fatalf("create chat %s: %v", agentSessionID, err)
		}
		baked[tmux.ChatSessionName(stuck.RepoID, agentSessionID)] = fmt.Sprintf("http://127.0.0.1:%d/s/%s", restartProxyPort, ps.TokenForSession(sessionID))
	}

	stuckPane := tmux.ChatSessionName(stuck.RepoID, "agent-stuck")
	entered, release := make(chan struct{}), make(chan struct{})
	var once, released sync.Once
	unstick := func() { released.Do(func() { close(release) }) }
	client := tmux.NewClient(tmux.WithCommandFactory(func(ctx context.Context, _ string, args ...string) *exec.Cmd {
		if len(args) != 4 || args[0] != "show-environment" {
			return exec.CommandContext(ctx, "true")
		}
		if args[2] == stuckPane {
			once.Do(func() {
				close(entered)
				<-release
			})
		}
		return exec.CommandContext(ctx, "printf", "%s\n", args[3]+"="+baked[args[2]])
	}))
	lc := session.NewLifecycle(sessions, nil, chats, nil, nil, nil, client, nil, zerolog.Nop())
	lc.SetProxyPort(restartProxyPort)
	lc.SetProxyRegistrar(ps)
	lc.SetChatRunningProbe(func(string) bool { return true })
	t.Cleanup(func() {
		unstick()
		stopCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		lc.StopBackgroundRelabels(stopCtx)
	})

	if err := lc.CommitFailover(ctx, stuckID, session.FailoverResult{
		Rotate: true, FromAccountID: "acct-1", NextAccountID: "acct-2", Provider: "claude", Trigger: "ROTATION_TRIGGER_USAGE_LIMITED",
	}); err != nil {
		t.Fatalf("CommitFailover: %v", err)
	}
	<-entered
	if err := lc.MoveSessionAccount(ctx, mustGetSession(ctx, t, sessions, other.ID), "acct-2"); err != nil {
		t.Fatalf("MoveSessionAccount: %v", err)
	}
	if got := chatAccountID(ctx, t, chats, "agent-other"); got != "acct-2" {
		t.Fatalf("other session's row = %q while the first session's rewrite is stuck, want acct-2", got)
	}
	unstick()
	if got := awaitChatAccountID(ctx, t, chats, "agent-stuck", "acct-2"); got != "acct-2" {
		t.Fatalf("stuck session's row = %q once its read was released, want acct-2", got)
	}
}

// Shutdown waits for a rewrite stuck on tmux, cancels one that honours
// cancellation, and starts no rewrite afterwards.
func TestStopBackgroundRelabels_JoinsInFlightRewritesAndStartsNoneAfter(t *testing.T) {
	ctx := context.Background()

	t.Run("waits for a rewrite stuck on tmux", func(t *testing.T) {
		wait, entered, release := heldFirstRead(false)
		f := newRelabelFixture(t, zerolog.Nop(), wait)
		f.failover(t, "acct-1", "acct-2")
		<-entered
		stopped := make(chan bool, 1)
		go func() { stopped <- f.lc.StopBackgroundRelabels(withTimeout(t, 5*time.Second)) }()
		select {
		case <-stopped:
			t.Fatal("StopBackgroundRelabels returned while a rewrite was still reading tmux")
		case <-time.After(200 * time.Millisecond):
		}
		close(release)
		if ok := <-stopped; !ok {
			t.Fatal("StopBackgroundRelabels reported the released rewrite as still running")
		}
	})

	t.Run("cancels a rewrite that honours cancellation", func(t *testing.T) {
		wait, entered, _ := heldFirstRead(true)
		f := newRelabelFixture(t, zerolog.Nop(), wait)
		f.failover(t, "acct-1", "acct-2")
		<-entered
		// The rewrite's own budget is 5s, so only cancellation ends it within 2s.
		if !f.lc.StopBackgroundRelabels(withTimeout(t, 2*time.Second)) {
			t.Fatal("StopBackgroundRelabels gave up instead of cancelling the rewrite")
		}
	})

	t.Run("starts no rewrite after it returns", func(t *testing.T) {
		logs := &lockedBuffer{}
		f := newRelabelFixture(t, zerolog.New(logs), func(context.Context) {})
		if !f.lc.StopBackgroundRelabels(withTimeout(t, time.Second)) {
			t.Fatal("StopBackgroundRelabels with nothing running reported a straggler")
		}
		f.failover(t, "acct-1", "acct-2")
		time.Sleep(300 * time.Millisecond)
		if n := f.reads.Load(); n != 0 {
			t.Fatalf("%d tmux reads after shutdown, want none", n)
		}
		if strings.Contains(logs.String(), "follow") {
			t.Fatalf("logs = %q, want no rewrite attempted after shutdown", logs.String())
		}
		if got := chatAccountID(ctx, t, f.chats, "agent-build"); got != "acct-1" {
			t.Fatalf("build pane row = %q after shutdown, want acct-1 left alone", got)
		}
	})
}

func withTimeout(t *testing.T, d time.Duration) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), d)
	t.Cleanup(cancel)
	return ctx
}

// lockedBuffer is a bytes.Buffer safe for a logger writing from a background
// goroutine while the test reads it.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}
