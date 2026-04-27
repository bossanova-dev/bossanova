package upstream

import (
	"context"
	"errors"
	"io"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossd/internal/tmux"
	"github.com/rs/zerolog"
)

// fakeTerminalStream is a hand-rolled bidi stream the tests drive. The
// reader pulls from `recv` (closed → EOF), the writer pushes to `sent`,
// and `recvErr` lets a test inject a non-EOF failure to drive the
// reconnect path.
type fakeTerminalStream struct {
	recv    chan *pb.TerminalClientMessage
	recvErr error // overrides EOF when recv closes if set
	sent    chan *pb.TerminalServerMessage

	// ctx is the streamCtx the opener handed to TerminalStream — when
	// it's cancelled, Receive returns an error, mirroring the real
	// connect.BidiStreamForClient semantics.
	ctx context.Context

	closeOnce sync.Once
	closed    chan struct{}
}

func newFakeTerminalStream() *fakeTerminalStream {
	return &fakeTerminalStream{
		recv:   make(chan *pb.TerminalClientMessage, 16),
		sent:   make(chan *pb.TerminalServerMessage, 16),
		ctx:    context.Background(), // overwritten in TerminalStream
		closed: make(chan struct{}),
	}
}

func (f *fakeTerminalStream) Send(m *pb.TerminalServerMessage) error {
	// nil = header-only flush (connect-go's "send headers without a body"
	// semantics). The production client uses this to dispatch the HTTP
	// request without a payload. Tests don't observe header flushes.
	if m == nil {
		select {
		case <-f.closed:
			return errors.New("stream closed")
		default:
			return nil
		}
	}
	select {
	case f.sent <- m:
		return nil
	case <-f.closed:
		return errors.New("stream closed")
	}
}

func (f *fakeTerminalStream) Receive() (*pb.TerminalClientMessage, error) {
	select {
	case msg, ok := <-f.recv:
		if !ok {
			if f.recvErr != nil {
				return nil, f.recvErr
			}
			return nil, io.EOF
		}
		return msg, nil
	case <-f.closed:
		return nil, io.EOF
	case <-f.ctx.Done():
		return nil, f.ctx.Err()
	}
}

func (f *fakeTerminalStream) CloseRequest() error {
	f.closeOnce.Do(func() { close(f.closed) })
	return nil
}

// fakeTerminalOpener returns a controllable stream for each TerminalStream
// call. Tracks open count so tests can assert lazy / reconnect behaviour.
type fakeTerminalOpener struct {
	mu        sync.Mutex
	openCalls int
	streams   []*fakeTerminalStream

	// nextStream, when non-nil, is returned on the next TerminalStream
	// call. Otherwise a fresh fakeTerminalStream is created.
	nextStream func() *fakeTerminalStream
}

func (o *fakeTerminalOpener) TerminalStream(ctx context.Context) terminalBidiStream {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.openCalls++
	var s *fakeTerminalStream
	if o.nextStream != nil {
		s = o.nextStream()
	} else {
		s = newFakeTerminalStream()
	}
	s.ctx = ctx
	o.streams = append(o.streams, s)
	return s
}

func (o *fakeTerminalOpener) calls() int {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.openCalls
}

func (o *fakeTerminalOpener) currentStream() *fakeTerminalStream {
	o.mu.Lock()
	defer o.mu.Unlock()
	if len(o.streams) == 0 {
		return nil
	}
	return o.streams[len(o.streams)-1]
}

// fakeChatLookup implements the chatLookup interface for tests.
type fakeChatLookup struct {
	rows map[string]chatRow
	err  error
}

func (f *fakeChatLookup) GetByClaudeID(_ context.Context, claudeID string) (chatRow, error) {
	if f.err != nil {
		return chatRow{}, f.err
	}
	row, ok := f.rows[claudeID]
	if !ok {
		return chatRow{}, errors.New("chat not found")
	}
	return row, nil
}

// fakeAttachImpl is the test-injected terminalAttach. Output() / Exited()
// are driven by the test; Input/Resize/Close record their invocations.
type fakeAttachImpl struct {
	id     string
	output chan *pb.TerminalDataChunk
	exited chan *pb.TerminalAttachExited

	inputs  [][]byte
	resizes [][2]uint32
	closed  atomic.Bool
	mu      sync.Mutex
}

func newFakeAttach(id string) *fakeAttachImpl {
	return &fakeAttachImpl{
		id:     id,
		output: make(chan *pb.TerminalDataChunk, 16),
		exited: make(chan *pb.TerminalAttachExited, 1),
	}
}

func (f *fakeAttachImpl) Output() <-chan *pb.TerminalDataChunk    { return f.output }
func (f *fakeAttachImpl) Exited() <-chan *pb.TerminalAttachExited { return f.exited }
func (f *fakeAttachImpl) Input(data []byte) error {
	if f.closed.Load() {
		return tmux.ErrAttachClosed
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	cp := make([]byte, len(data))
	copy(cp, data)
	f.inputs = append(f.inputs, cp)
	return nil
}

func (f *fakeAttachImpl) Resize(cols, rows uint32) error {
	if f.closed.Load() {
		return tmux.ErrAttachClosed
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.resizes = append(f.resizes, [2]uint32{cols, rows})
	return nil
}

func (f *fakeAttachImpl) Close() error {
	if f.closed.CompareAndSwap(false, true) {
		// Close output and emit a synthetic exit so the pump unwinds
		// cleanly even in tests that don't drive Exited explicitly.
		close(f.output)
		select {
		case f.exited <- &pb.TerminalAttachExited{
			AttachId: f.id,
			ExitCode: 0,
			Reason:   "closed",
		}:
		default:
		}
	}
	return nil
}

func (f *fakeAttachImpl) inputCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.inputs)
}

func (f *fakeAttachImpl) lastInput() []byte {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.inputs) == 0 {
		return nil
	}
	return f.inputs[len(f.inputs)-1]
}

// fakeAttachFactory builds fakeAttachImpls and records the SessionName +
// AttachID it was asked for. Lets tests assert that the persisted
// tmux_session_name was used (not recomputed).
type fakeAttachFactory struct {
	mu       sync.Mutex
	configs  []tmux.AttachConfig
	attaches map[string]*fakeAttachImpl
	err      error
}

func newFakeAttachFactory() *fakeAttachFactory {
	return &fakeAttachFactory{attaches: make(map[string]*fakeAttachImpl)}
}

func (f *fakeAttachFactory) build(_ context.Context, cfg tmux.AttachConfig) (terminalAttach, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return nil, f.err
	}
	f.configs = append(f.configs, cfg)
	a := newFakeAttach(cfg.AttachID)
	f.attaches[cfg.AttachID] = a
	return a, nil
}

func (f *fakeAttachFactory) get(attachID string) *fakeAttachImpl {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.attaches[attachID]
}

func (f *fakeAttachFactory) lastConfig() tmux.AttachConfig {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.configs) == 0 {
		return tmux.AttachConfig{}
	}
	return f.configs[len(f.configs)-1]
}

func (f *fakeAttachFactory) configCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.configs)
}

// makeTmuxClient builds a tmux.Client that records calls but does nothing.
// Lets tests verify RefreshClient is fired without spawning a real tmux.
type recordingCmdFactory struct {
	mu    sync.Mutex
	calls [][]string
}

func (r *recordingCmdFactory) factory(ctx context.Context, name string, args ...string) *exec.Cmd {
	r.mu.Lock()
	r.calls = append(r.calls, append([]string{name}, args...))
	r.mu.Unlock()
	return exec.CommandContext(ctx, "true")
}

func (r *recordingCmdFactory) refreshCalls(sessionName string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for _, call := range r.calls {
		if len(call) >= 4 && call[0] == "tmux" && call[1] == "refresh-client" && call[3] == sessionName {
			n++
		}
	}
	return n
}

// newTestClient bundles the common setup the tests use: an opener, a chat
// lookup with one canned chat row, a fake attach factory, and a
// recording-only tmux client.
type testHarness struct {
	opener  *fakeTerminalOpener
	chats   *fakeChatLookup
	factory *fakeAttachFactory
	tmuxCmd *recordingCmdFactory
	client  *TerminalStreamClient
}

func newTestHarness(t *testing.T) *testHarness {
	t.Helper()
	opener := &fakeTerminalOpener{}
	tmuxName := "boss-rep-chat-1"
	chats := &fakeChatLookup{
		rows: map[string]chatRow{
			"claude-1": {TmuxSessionName: &tmuxName},
		},
	}
	factory := newFakeAttachFactory()
	cmdRec := &recordingCmdFactory{}
	tmuxClient := tmux.NewClient(tmux.WithCommandFactory(cmdRec.factory))
	client := NewTerminalStreamClient(TerminalStreamClientConfig{
		Opener:        opener,
		Chats:         chats,
		TmuxClient:    tmuxClient,
		AttachFactory: factory.build,
		Logger:        zerolog.Nop(),
	})
	return &testHarness{
		opener:  opener,
		chats:   chats,
		factory: factory,
		tmuxCmd: cmdRec,
		client:  client,
	}
}

// runClient launches client.Run on its own goroutine and returns a stop
// func that cancels and waits.
func runClient(t *testing.T, c *TerminalStreamClient) func() {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- c.Run(ctx)
	}()
	return func() {
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatal("Run did not return within 5s of cancel")
		}
	}
}

// TestTerminalStreamClient_LazyOpen verifies that constructing a client
// does not dial; Run is what triggers the first TerminalStream open.
//
// (Stream "lazy open" in the architecture spec means the daemon doesn't
// connect on import — the connect/reconnect loop only runs while Run is
// active. There's no out-of-band signal that a TerminalAttach is about
// to arrive, so once Run is in flight the stream is connected; that's
// the practical upper bound on "lazy".)
func TestTerminalStreamClient_LazyOpen(t *testing.T) {
	t.Parallel()
	h := newTestHarness(t)
	if got := h.opener.calls(); got != 0 {
		t.Fatalf("expected 0 stream opens after construction, got %d", got)
	}

	stop := runClient(t, h.client)
	defer stop()

	// Wait briefly for Run to actually open a stream.
	deadline := time.After(2 * time.Second)
	for h.opener.calls() == 0 {
		select {
		case <-deadline:
			t.Fatalf("expected stream open within 2s of Run, got %d", h.opener.calls())
		case <-time.After(10 * time.Millisecond):
		}
	}
}

// TestTerminalStreamClient_Multiplex drives two TerminalAttachCommands
// with different attach_ids and verifies that two distinct attaches are
// created and that input is routed to the correct one based on attach_id.
func TestTerminalStreamClient_Multiplex(t *testing.T) {
	t.Parallel()
	h := newTestHarness(t)
	tmuxName2 := "boss-rep-chat-2"
	h.chats.rows["claude-2"] = chatRow{TmuxSessionName: &tmuxName2}

	stop := runClient(t, h.client)
	defer stop()

	stream := waitForStream(t, h.opener)

	// Send two attach commands.
	stream.recv <- &pb.TerminalClientMessage{
		Msg: &pb.TerminalClientMessage_Attach{Attach: &pb.TerminalAttachCommand{
			AttachId: "att-1", ChatId: "claude-1", Cols: 80, Rows: 24,
		}},
	}
	stream.recv <- &pb.TerminalClientMessage{
		Msg: &pb.TerminalClientMessage_Attach{Attach: &pb.TerminalAttachCommand{
			AttachId: "att-2", ChatId: "claude-2", Cols: 100, Rows: 30,
		}},
	}

	// Wait for both attaches to be built.
	waitFor(t, "two attaches created", func() bool {
		return h.factory.configCount() == 2
	})

	// Both attach configs must use the persisted name verbatim — not a
	// recomputed ChatSessionName.
	got1 := h.factory.configs[0]
	got2 := h.factory.configs[1]
	if got1.SessionName != "boss-rep-chat-1" || got2.SessionName != "boss-rep-chat-2" {
		t.Errorf("session names mismatch: got1=%q got2=%q", got1.SessionName, got2.SessionName)
	}

	// Send input to att-1; verify only att-1's fake receives it.
	stream.recv <- &pb.TerminalClientMessage{
		Msg: &pb.TerminalClientMessage_Input{Input: &pb.TerminalInputCommand{
			AttachId: "att-1", Data: []byte("hello"),
		}},
	}
	waitFor(t, "att-1 input arrived", func() bool {
		return h.factory.get("att-1").inputCount() == 1
	})
	if h.factory.get("att-2").inputCount() != 0 {
		t.Errorf("att-2 received input that was meant for att-1")
	}
	if got := string(h.factory.get("att-1").lastInput()); got != "hello" {
		t.Errorf("att-1 input = %q, want %q", got, "hello")
	}

	// And the inverse for att-2.
	stream.recv <- &pb.TerminalClientMessage{
		Msg: &pb.TerminalClientMessage_Input{Input: &pb.TerminalInputCommand{
			AttachId: "att-2", Data: []byte("world"),
		}},
	}
	waitFor(t, "att-2 input arrived", func() bool {
		return h.factory.get("att-2").inputCount() == 1
	})
	if got := string(h.factory.get("att-2").lastInput()); got != "world" {
		t.Errorf("att-2 input = %q, want %q", got, "world")
	}
}

// TestTerminalStreamClient_PersistedSessionName verifies the lookup uses
// the persisted tmux_session_name field rather than recomputing one. A
// test-supplied session name with characters that ChatSessionName
// (boss-{first8}-{first8}) wouldn't produce proves the persisted field
// is the one we ended up with.
func TestTerminalStreamClient_PersistedSessionName(t *testing.T) {
	t.Parallel()
	h := newTestHarness(t)
	// Override the canned row with a session name that ChatSessionName
	// would never produce: it's not derived from any 8-char prefix.
	custom := "boss-CUSTOM-SESSION-NAME"
	h.chats.rows["claude-1"] = chatRow{TmuxSessionName: &custom}

	stop := runClient(t, h.client)
	defer stop()

	stream := waitForStream(t, h.opener)
	stream.recv <- &pb.TerminalClientMessage{
		Msg: &pb.TerminalClientMessage_Attach{Attach: &pb.TerminalAttachCommand{
			AttachId: "att-1", ChatId: "claude-1",
		}},
	}
	waitFor(t, "attach created", func() bool {
		return h.factory.configCount() == 1
	})

	if got := h.factory.lastConfig().SessionName; got != custom {
		t.Errorf("attach session name = %q, want persisted %q", got, custom)
	}
}

// TestTerminalStreamClient_MissingTmuxSessionName verifies that an attach
// for a chat without a persisted tmux_session_name is rejected with a
// clean TerminalAttachExited (rather than silently falling back to
// ChatSessionName). Plan Codex catch #3.
func TestTerminalStreamClient_MissingTmuxSessionName(t *testing.T) {
	t.Parallel()
	h := newTestHarness(t)
	// Empty / nil: chat exists but no tmux session yet.
	h.chats.rows["claude-pending"] = chatRow{TmuxSessionName: nil}

	stop := runClient(t, h.client)
	defer stop()

	stream := waitForStream(t, h.opener)
	stream.recv <- &pb.TerminalClientMessage{
		Msg: &pb.TerminalClientMessage_Attach{Attach: &pb.TerminalAttachCommand{
			AttachId: "att-pending", ChatId: "claude-pending",
		}},
	}

	msg := waitForServerMessage(t, stream)
	exited := msg.GetExited()
	if exited == nil {
		t.Fatalf("expected TerminalAttachExited, got %+v", msg)
	}
	if exited.GetAttachId() != "att-pending" {
		t.Errorf("exited.attach_id = %q, want att-pending", exited.GetAttachId())
	}
	if exited.GetReason() == "" {
		t.Errorf("expected non-empty reason on exited frame")
	}
	if h.factory.configCount() != 0 {
		t.Errorf("expected no attach factory invocations, got %d", h.factory.configCount())
	}
}

// TestTerminalStreamClient_RefreshClientOnLost verifies that when an
// attach emits a TerminalDataChunk with lost=true, the client invokes
// tmux refresh-client for that attach's session. Catches the resync
// repaint mechanism end-to-end.
func TestTerminalStreamClient_RefreshClientOnLost(t *testing.T) {
	t.Parallel()
	h := newTestHarness(t)

	stop := runClient(t, h.client)
	defer stop()

	stream := waitForStream(t, h.opener)
	stream.recv <- &pb.TerminalClientMessage{
		Msg: &pb.TerminalClientMessage_Attach{Attach: &pb.TerminalAttachCommand{
			AttachId: "att-1", ChatId: "claude-1",
		}},
	}
	waitFor(t, "attach built", func() bool {
		return h.factory.get("att-1") != nil
	})
	att := h.factory.get("att-1")

	// Push a chunk with lost=true.
	att.output <- &pb.TerminalDataChunk{
		AttachId: "att-1", Seq: 1, Data: []byte("xxx"), Lost: true,
	}

	// Drain the data frame on the wire.
	_ = waitForServerMessage(t, stream)

	// Wait for refresh-client to fire on the recording cmd factory.
	waitFor(t, "refresh-client fired", func() bool {
		return h.tmuxCmd.refreshCalls("boss-rep-chat-1") >= 1
	})
}

// TestTerminalStreamClient_StreamDropClosesAttaches verifies that when
// the gRPC stream errors out, all active attaches are Closed and the
// client attempts to reconnect.
func TestTerminalStreamClient_StreamDropClosesAttaches(t *testing.T) {
	t.Parallel()
	h := newTestHarness(t)

	// Use a tight clock so reconnect happens quickly.
	h.client.clock = realClock{}

	stop := runClient(t, h.client)
	defer stop()

	stream := waitForStream(t, h.opener)
	stream.recv <- &pb.TerminalClientMessage{
		Msg: &pb.TerminalClientMessage_Attach{Attach: &pb.TerminalAttachCommand{
			AttachId: "att-1", ChatId: "claude-1",
		}},
	}
	waitFor(t, "attach built", func() bool {
		return h.factory.get("att-1") != nil
	})
	att := h.factory.get("att-1")

	// Drop the stream by closing the recv channel with a non-EOF error so
	// the outer loop treats it as a reconnect-worthy failure.
	stream.recvErr = errors.New("connection reset")
	close(stream.recv)

	// The attach should be Closed.
	waitFor(t, "attach closed after stream drop", func() bool {
		return att.closed.Load()
	})

	// The reconnect loop should re-open. Backoff is 1s on the first
	// failure, so allow up to 3s.
	waitForN(t, "reconnect opened", 3*time.Second, func() bool {
		return h.opener.calls() >= 2
	})
}

// TestTerminalStreamClient_HealthyDropResetsBackoff verifies that a stream
// which lived past terminalStreamHealthyDuration and then drops causes the
// next reconnect to use the initial backoff, not the accumulated value
// from prior fast-failures. Without this reset, a stream that ran for
// hours would inherit whatever backoff prior failures had ramped to and
// stall the user-visible reconnect for up to terminalStreamMaxBackoff.
func TestTerminalStreamClient_HealthyDropResetsBackoff(t *testing.T) {
	t.Parallel()
	h := newTestHarness(t)
	clock := newFakeClock()
	h.client.clock = clock

	streams := make(chan *fakeTerminalStream, 8)
	h.opener.nextStream = func() *fakeTerminalStream {
		s := newFakeTerminalStream()
		s.recvErr = errors.New("transient drop")
		streams <- s
		return s
	}

	dropFast := func(t *testing.T) {
		t.Helper()
		select {
		case s := <-streams:
			close(s.recv)
		case <-time.After(2 * time.Second):
			t.Fatal("opener did not produce a stream within 2s")
		}
	}

	// nextAfterDuration peeks at the duration of the soonest pending
	// timer on the fake clock. Used to assert the Run loop's backoff
	// progression without needing to fire the timer first.
	nextAfterDuration := func(t *testing.T) time.Duration {
		t.Helper()
		clock.mu.Lock()
		defer clock.mu.Unlock()
		var d time.Duration
		for _, tm := range clock.timers {
			if tm.fired || tm.stopped {
				continue
			}
			gap := tm.deadline.Sub(clock.now)
			if d == 0 || gap < d {
				d = gap
			}
		}
		return d
	}

	advancePastNext := func(t *testing.T) {
		t.Helper()
		waitForTimer(clock, 2*time.Second)
		clock.Advance(nextAfterDuration(t) + time.Millisecond)
	}

	stop := runClient(t, h.client)
	defer stop()

	// Three fast failures ramp the backoff: 1s → 2s → 4s.
	dropFast(t)
	waitForTimer(clock, 2*time.Second)
	if got, want := nextAfterDuration(t), 1*time.Second; got != want {
		t.Fatalf("after fail #1: backoff = %v, want %v", got, want)
	}
	advancePastNext(t)

	dropFast(t)
	waitForTimer(clock, 2*time.Second)
	if got, want := nextAfterDuration(t), 2*time.Second; got != want {
		t.Fatalf("after fail #2: backoff = %v, want %v", got, want)
	}
	advancePastNext(t)

	dropFast(t)
	waitForTimer(clock, 2*time.Second)
	if got, want := nextAfterDuration(t), 4*time.Second; got != want {
		t.Fatalf("after fail #3: backoff = %v, want %v", got, want)
	}
	advancePastNext(t)

	// Fourth attempt: let the stream "live" past the healthy threshold,
	// then drop. Because the lifetime crosses the threshold, the loop
	// should treat this as a healthy connection and reset backoff.
	var s *fakeTerminalStream
	select {
	case s = <-streams:
	case <-time.After(2 * time.Second):
		t.Fatal("opener did not produce a stream within 2s for healthy attempt")
	}
	clock.Advance(terminalStreamHealthyDuration + time.Second)
	close(s.recv)

	waitForTimer(clock, 2*time.Second)
	if got, want := nextAfterDuration(t), terminalStreamInitialBackoff; got != want {
		t.Fatalf("after healthy drop: backoff = %v, want %v (reset)", got, want)
	}
}

// TestTerminalStreamClient_TerminalCloseCommand verifies that a
// TerminalCloseCommand routes to the right attach and triggers Close.
// The TerminalAttach's own Exited frame is what surfaces back to bosso —
// the client does not synthesise an exited frame in response to Close.
func TestTerminalStreamClient_TerminalCloseCommand(t *testing.T) {
	t.Parallel()
	h := newTestHarness(t)

	stop := runClient(t, h.client)
	defer stop()

	stream := waitForStream(t, h.opener)
	stream.recv <- &pb.TerminalClientMessage{
		Msg: &pb.TerminalClientMessage_Attach{Attach: &pb.TerminalAttachCommand{
			AttachId: "att-1", ChatId: "claude-1",
		}},
	}
	waitFor(t, "attach built", func() bool {
		return h.factory.get("att-1") != nil
	})
	att := h.factory.get("att-1")

	stream.recv <- &pb.TerminalClientMessage{
		Msg: &pb.TerminalClientMessage_Close{Close: &pb.TerminalCloseCommand{
			AttachId: "att-1",
		}},
	}

	waitFor(t, "attach closed", func() bool {
		return att.closed.Load()
	})

	// The fake attach's Close emits a synthetic Exited frame; the pump
	// should forward it to the wire so bosso sees the close completion.
	msg := waitForServerMessage(t, stream)
	exited := msg.GetExited()
	if exited == nil {
		t.Fatalf("expected exited frame on close, got %+v", msg)
	}
	if exited.GetAttachId() != "att-1" {
		t.Errorf("exited.attach_id = %q, want att-1", exited.GetAttachId())
	}
}

// TestTerminalStreamClient_UnknownAttachIdDropped verifies that input
// commands for unknown attach_ids are silently dropped (forward-compat
// per the task spec).
func TestTerminalStreamClient_UnknownAttachIdDropped(t *testing.T) {
	t.Parallel()
	h := newTestHarness(t)

	stop := runClient(t, h.client)
	defer stop()

	stream := waitForStream(t, h.opener)

	// Input for an unknown attach — must not crash, must not produce an
	// outgoing message.
	stream.recv <- &pb.TerminalClientMessage{
		Msg: &pb.TerminalClientMessage_Input{Input: &pb.TerminalInputCommand{
			AttachId: "ghost", Data: []byte("nope"),
		}},
	}

	// Brief wait — there should be NO server message.
	select {
	case msg := <-stream.sent:
		t.Fatalf("expected no outbound message for unknown attach, got %+v", msg)
	case <-time.After(100 * time.Millisecond):
	}
}

// waitForStream blocks until the opener has opened at least one stream
// and returns the most recent one.
func waitForStream(t *testing.T, o *fakeTerminalOpener) *fakeTerminalStream {
	t.Helper()
	deadline := time.After(2 * time.Second)
	for {
		if s := o.currentStream(); s != nil {
			return s
		}
		select {
		case <-deadline:
			t.Fatalf("no stream opened within 2s")
		case <-time.After(5 * time.Millisecond):
		}
	}
}

// waitForServerMessage drains one message from the stream's `sent` channel.
func waitForServerMessage(t *testing.T, s *fakeTerminalStream) *pb.TerminalServerMessage {
	t.Helper()
	select {
	case m := <-s.sent:
		return m
	case <-time.After(2 * time.Second):
		t.Fatal("no server message within 2s")
	}
	return nil
}

// waitFor polls a predicate until it returns true or 2s elapse.
func waitFor(t *testing.T, label string, fn func() bool) {
	t.Helper()
	waitForN(t, label, 2*time.Second, fn)
}

// waitForN polls a predicate with a configurable timeout.
func waitForN(t *testing.T, label string, timeout time.Duration, fn func() bool) {
	t.Helper()
	deadline := time.After(timeout)
	for {
		if fn() {
			return
		}
		select {
		case <-deadline:
			t.Fatalf("timeout waiting for %s", label)
		case <-time.After(5 * time.Millisecond):
		}
	}
}

// TestTerminalStreamClient_NilTmuxClientPanics verifies that constructing
// a TerminalStreamClient without a TmuxClient panics fast at construction
// time. Without this, fireRefreshClient's nil-guard would silently no-op
// every overflow RESYNC repaint with no log line — a miswiring that's
// invisible until users complain about stale tmux output.
func TestTerminalStreamClient_NilTmuxClientPanics(t *testing.T) {
	t.Parallel()
	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("expected NewTerminalStreamClient to panic when TmuxClient is nil")
		}
	}()
	_ = NewTerminalStreamClient(TerminalStreamClientConfig{
		Opener: &fakeTerminalOpener{},
		Chats:  &fakeChatLookup{},
		Logger: zerolog.Nop(),
		// TmuxClient deliberately omitted.
	})
}

// TestTerminalStreamClient_ChatNotFoundReason verifies that when chat
// lookup fails with a not-found-style error, the wire `Reason` is the
// safe fixed string ("chat not found") rather than the raw DB error
// (which can include "sql: no rows in result set" or driver text). The
// full error stays in the daemon log; only the sanitised reason crosses
// the wire to the browser.
func TestTerminalStreamClient_ChatNotFoundReason(t *testing.T) {
	t.Parallel()
	h := newTestHarness(t)
	// The real chat store wraps with this prefix; emulate it here.
	h.chats.err = errors.New("get claude_chat by claude_id: claude_chat not found for claude_id \"missing\"")

	stop := runClient(t, h.client)
	defer stop()

	stream := waitForStream(t, h.opener)
	stream.recv <- &pb.TerminalClientMessage{
		Msg: &pb.TerminalClientMessage_Attach{Attach: &pb.TerminalAttachCommand{
			AttachId: "att-missing", ChatId: "missing",
		}},
	}

	msg := waitForServerMessage(t, stream)
	exited := msg.GetExited()
	if exited == nil {
		t.Fatalf("expected TerminalAttachExited, got %+v", msg)
	}
	if got, want := exited.GetReason(), "chat not found"; got != want {
		t.Errorf("exited.reason = %q, want %q (must be the fixed safe string, not the leaky DB error)", got, want)
	}
}

// TestTerminalStreamClient_GenericChatLookupErrorReason verifies that an
// arbitrary (non-not-found) DB error is reported as the generic
// "chat lookup failed" string, again without leaking the raw error text.
func TestTerminalStreamClient_GenericChatLookupErrorReason(t *testing.T) {
	t.Parallel()
	h := newTestHarness(t)
	h.chats.err = errors.New("driver: connection reset by peer")

	stop := runClient(t, h.client)
	defer stop()

	stream := waitForStream(t, h.opener)
	stream.recv <- &pb.TerminalClientMessage{
		Msg: &pb.TerminalClientMessage_Attach{Attach: &pb.TerminalAttachCommand{
			AttachId: "att-flaky", ChatId: "claude-1",
		}},
	}

	msg := waitForServerMessage(t, stream)
	exited := msg.GetExited()
	if exited == nil {
		t.Fatalf("expected TerminalAttachExited, got %+v", msg)
	}
	if got, want := exited.GetReason(), "chat lookup failed"; got != want {
		t.Errorf("exited.reason = %q, want %q", got, want)
	}
	// The driver text MUST NOT leak onto the wire.
	if exited.GetReason() == "driver: connection reset by peer" || strings.Contains(exited.GetReason(), "connection reset") {
		t.Errorf("exited.reason leaks driver text: %q", exited.GetReason())
	}
}

// TestSessionTokenHolder_RotationFansOutToBothOpeners verifies the core
// invariant that issue #2 is about: when StreamClient.tryReRegister
// rotates the daemon session_token, both the DaemonStream opener AND the
// TerminalStream opener pick up the new value on their next dial. A
// shared *SessionTokenHolder is what makes that work — without one, a
// rotation would silently break TerminalStream auth until daemon restart.
func TestSessionTokenHolder_RotationFansOutToBothOpeners(t *testing.T) {
	t.Parallel()

	holder := NewSessionTokenHolder("initial")

	// Build both real openers against the same holder. We only need the
	// header-stamping path; no real RPCs are dialled.
	dOpener := &connectOpener{
		sessionToken: holder,
	}
	tOpener := &terminalConnectOpener{
		sessionToken: holder,
	}

	if got := holder.Get(); got != "initial" {
		t.Fatalf("holder.Get() = %q, want initial", got)
	}

	// Simulate the re-register path: StreamClient.tryReRegister calls
	// connectOpener.SetSessionToken on stream-side rotation.
	dOpener.SetSessionToken("rotated")

	if got := holder.Get(); got != "rotated" {
		t.Errorf("after rotation, holder.Get() = %q, want rotated", got)
	}
	// And critically — the TerminalStream opener observes the same value.
	if got := tOpener.sessionToken.Get(); got != "rotated" {
		t.Errorf("after rotation, terminal opener token = %q, want rotated (rotation must fan out to both openers)", got)
	}
}
