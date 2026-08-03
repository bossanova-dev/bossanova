package upstream

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossd/internal/tmux"
	"github.com/rs/zerolog"
)

type emptySnapshotStores struct{}

func (emptySnapshotStores) SnapshotSessions(context.Context) ([]*pb.Session, error) {
	return nil, nil
}

func (emptySnapshotStores) SnapshotChats(context.Context) ([]*pb.ClaudeChatMetadata, error) {
	return nil, nil
}

func (emptySnapshotStores) SnapshotRepoIDs(context.Context) ([]string, error) {
	return nil, nil
}

func (emptySnapshotStores) SnapshotStatuses(context.Context) ([]*pb.ChatStatusEntry, error) {
	return nil, nil
}

type blockingDaemonOpener struct {
	stream *blockingDaemonStream
}

func (o *blockingDaemonOpener) DaemonStream(ctx context.Context) bidirectionalStream {
	o.stream.ctx = ctx
	return o.stream
}

type blockingDaemonStream struct {
	ctx     context.Context
	sent    chan *pb.DaemonEvent
	done    chan struct{}
	sendErr error
}

func newBlockingDaemonStream() *blockingDaemonStream {
	return &blockingDaemonStream{
		sent: make(chan *pb.DaemonEvent, 8),
		done: make(chan struct{}),
	}
}

func (s *blockingDaemonStream) Send(event *pb.DaemonEvent) error {
	if s.sendErr != nil {
		return s.sendErr
	}
	s.sent <- event
	return nil
}

func (s *blockingDaemonStream) Receive() (*pb.OrchestratorCommand, error) {
	close(s.done)
	<-s.ctx.Done()
	return nil, s.ctx.Err()
}

func (s *blockingDaemonStream) CloseRequest() error {
	return nil
}

func TestStreamClientOpenStreamStopsWhenAuthNeedsLogin(t *testing.T) {
	authState := NewAuthState()
	stream := newBlockingDaemonStream()
	opener := &blockingDaemonOpener{stream: stream}
	stores := emptySnapshotStores{}
	client := NewStreamClient(StreamClientConfig{
		Opener: opener,
		Stores: StreamStores{
			Sessions: stores,
			Chats:    stores,
			Repos:    stores,
			Statuses: stores,
		},
		Events:          NoopEventSource{},
		AuthState:       authState,
		RefreshInterval: time.Hour,
	})

	errCh := make(chan error, 1)
	go func() {
		errCh <- client.openStream(context.Background())
	}()

	select {
	case event := <-stream.sent:
		if event.GetSnapshot() == nil {
			t.Fatalf("first event = %T, want snapshot", event.GetEvent())
		}
	case <-time.After(100 * time.Millisecond):
		t.Fatal("timed out waiting for initial snapshot")
	}

	authState.MarkNeedsLogin()

	select {
	case <-stream.done:
	case <-time.After(100 * time.Millisecond):
		t.Fatal("timed out waiting for command reader to start")
	}

	select {
	case err := <-errCh:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("openStream error = %v, want context.Canceled", err)
		}
	case <-time.After(100 * time.Millisecond):
		t.Fatal("timed out waiting for openStream to return")
	}
}

// countingStreamMetrics records IncStreamError / IncReconnect calls so a
// test can assert that an intentional logout pause does NOT look like a
// stream failure.
type countingStreamMetrics struct {
	streamErrors atomic.Int32
	reconnects   atomic.Int32
}

func (m *countingStreamMetrics) IncReconnect()        { m.reconnects.Add(1) }
func (m *countingStreamMetrics) IncStreamError(error) { m.streamErrors.Add(1) }

// syncBuffer is a mutex-guarded io.Writer so the Run-loop goroutine and the
// test goroutine can write/read the captured logs without racing.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// TestStreamClientRunPausesWithoutStreamErrorOnLogout drives the full Run
// loop (not just openStream) and proves that a logout pauses cleanly: it
// must NOT increment the stream-error metric or emit a "reconnecting" warn,
// and it must park at the re-login gate rather than returning.
func TestStreamClientRunPausesWithoutStreamErrorOnLogout(t *testing.T) {
	authState := NewAuthState()
	stream := newBlockingDaemonStream()
	opener := &blockingDaemonOpener{stream: stream}
	stores := emptySnapshotStores{}
	metrics := &countingStreamMetrics{}
	logs := &syncBuffer{}
	client := NewStreamClient(StreamClientConfig{
		Opener: opener,
		Stores: StreamStores{
			Sessions: stores,
			Chats:    stores,
			Repos:    stores,
			Statuses: stores,
		},
		Events:          NoopEventSource{},
		AuthState:       authState,
		Metrics:         metrics,
		RefreshInterval: time.Hour,
		Logger:          zerolog.New(logs),
	})

	runCtx, runCancel := context.WithCancel(context.Background())
	defer runCancel()
	done := make(chan struct{})
	go func() {
		client.Run(runCtx)
		close(done)
	}()

	// Snapshot sent + command reader started confirms the stream is fully open.
	select {
	case <-stream.sent:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for initial snapshot")
	}
	select {
	case <-stream.done:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for command reader to start")
	}

	authState.MarkNeedsLogin()

	// The Run loop should reach the NeedsLogin gate and block on Wait().
	deadline := time.After(2 * time.Second)
	for !strings.Contains(logs.String(), "stream paused: re-login required") {
		select {
		case <-done:
			t.Fatalf("Run returned instead of pausing on logout; logs=%s", logs.String())
		case <-deadline:
			t.Fatalf("Run did not pause on logout within 2s; logs=%s", logs.String())
		case <-time.After(5 * time.Millisecond):
		}
	}

	if got := metrics.streamErrors.Load(); got != 0 {
		t.Fatalf("IncStreamError called %d times on logout, want 0", got)
	}
	if got := metrics.reconnects.Load(); got != 0 {
		t.Fatalf("IncReconnect called %d times on logout, want 0", got)
	}
	if strings.Contains(logs.String(), "stream closed, reconnecting") {
		t.Fatalf("logout produced a misleading reconnect warning; logs=%s", logs.String())
	}

	runCancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after ctx cancel")
	}
}

// TestStreamClientRunPausesWithoutStreamErrorOnRefreshRelogin is the BOS-659
// sibling of the logout test above: the pause is triggered by the periodic
// refresher observing a terminal re-login error rather than by an explicit
// logout. Both terminal reasons must take the same intentional-pause path —
// no stream-error metric, no reconnect ramp, no "reconnecting" warning — and
// the daemon must not re-dial or re-attempt the uncertain refresh token.
func TestStreamClientRunPausesWithoutStreamErrorOnRefreshRelogin(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
	}{
		{name: "ambiguous outcome", err: ErrRefreshOutcomeUnknown},
		{name: "authoritative rejection", err: ErrRefreshTokenRejected},
	} {
		t.Run(tc.name, func(t *testing.T) {
			authState := NewAuthState()
			stream := newBlockingDaemonStream()
			stores := emptySnapshotStores{}
			metrics := &countingStreamMetrics{}
			logs := &syncBuffer{}
			refreshErr := tc.err
			tp := &fakeTokenProvider{
				token:     "old",
				expiresAt: time.Now().Add(time.Second),
				refreshFn: func(_ context.Context) (string, error) { return "", refreshErr },
			}
			client := NewStreamClient(StreamClientConfig{
				Opener: &blockingDaemonOpener{stream: stream},
				Stores: StreamStores{
					Sessions: stores,
					Chats:    stores,
					Repos:    stores,
					Statuses: stores,
				},
				Events:           NoopEventSource{},
				AuthState:        authState,
				Metrics:          metrics,
				TokenProvider:    tp,
				RefreshInterval:  5 * time.Millisecond,
				RefreshThreshold: time.Hour,
				Logger:           zerolog.New(logs),
			})

			runCtx, runCancel := context.WithCancel(context.Background())
			defer runCancel()
			done := make(chan struct{})
			go func() {
				client.Run(runCtx)
				close(done)
			}()

			deadline := time.After(5 * time.Second)
			for !strings.Contains(logs.String(), "stream paused: re-login required") {
				select {
				case <-done:
					t.Fatalf("Run returned instead of pausing; logs=%s", logs.String())
				case <-deadline:
					t.Fatalf("Run did not pause within 5s; logs=%s", logs.String())
				case <-time.After(5 * time.Millisecond):
				}
			}

			if got := metrics.streamErrors.Load(); got != 0 {
				t.Fatalf("IncStreamError called %d times, want 0", got)
			}
			if got := metrics.reconnects.Load(); got != 0 {
				t.Fatalf("IncReconnect called %d times, want 0", got)
			}
			if strings.Contains(logs.String(), "stream closed, reconnecting") {
				t.Fatalf("re-login pause produced a misleading reconnect warning; logs=%s", logs.String())
			}
			if strings.Contains(logs.String(), "invalid_grant") {
				t.Fatalf("re-login pause labelled the outcome invalid_grant; logs=%s", logs.String())
			}

			// Parked at the gate: the uncertain token is never re-exchanged.
			time.Sleep(50 * time.Millisecond)
			tp.mu.Lock()
			calls := tp.refreshCalls
			tp.mu.Unlock()
			if calls != 1 {
				t.Fatalf("Refresh calls = %d, want exactly 1 while paused", calls)
			}

			runCancel()
			select {
			case <-done:
			case <-time.After(2 * time.Second):
				t.Fatal("Run did not return after ctx cancel")
			}
		})
	}
}

type NoopEventSource struct{}

func (NoopEventSource) Subscribe(ctx context.Context) <-chan StreamEvent {
	ch := make(chan StreamEvent)
	go func() {
		defer close(ch)
		<-ctx.Done()
	}()
	return ch
}

type blockingTerminalOpener struct {
	stream *blockingTerminalStream
}

func (o *blockingTerminalOpener) TerminalStream(ctx context.Context) terminalBidiStream {
	o.stream.ctx = ctx
	return o.stream
}

type blockingTerminalStream struct {
	ctx     context.Context
	sent    chan *pb.TerminalServerMessage
	done    chan struct{}
	sendErr error
}

func newBlockingTerminalStream() *blockingTerminalStream {
	return &blockingTerminalStream{
		sent: make(chan *pb.TerminalServerMessage, 8),
		done: make(chan struct{}),
	}
}

func (s *blockingTerminalStream) Send(message *pb.TerminalServerMessage) error {
	if s.sendErr != nil {
		return s.sendErr
	}
	s.sent <- message
	return nil
}

func (s *blockingTerminalStream) Receive() (*pb.TerminalClientMessage, error) {
	close(s.done)
	<-s.ctx.Done()
	return nil, s.ctx.Err()
}

func (s *blockingTerminalStream) CloseRequest() error {
	return nil
}

func TestTerminalStreamClientOpenStreamStopsWhenAuthNeedsLogin(t *testing.T) {
	authState := NewAuthState()
	stream := newBlockingTerminalStream()
	client := NewTerminalStreamClient(TerminalStreamClientConfig{
		Opener:     &blockingTerminalOpener{stream: stream},
		AuthState:  authState,
		TmuxClient: &tmux.Client{},
	})

	errCh := make(chan error, 1)
	go func() {
		errCh <- client.openStream(context.Background())
	}()

	select {
	case msg := <-stream.sent:
		if msg != nil {
			t.Fatalf("initial terminal send = %#v, want nil header flush", msg)
		}
	case <-time.After(100 * time.Millisecond):
		t.Fatal("terminal stream did not flush headers")
	}

	authState.MarkNeedsLogin()

	select {
	case <-stream.done:
	case <-time.After(100 * time.Millisecond):
		t.Fatal("terminal stream Receive did not unblock after logout")
	}

	select {
	case err := <-errCh:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("openStream error = %v, want context.Canceled", err)
		}
	case <-time.After(100 * time.Millisecond):
		t.Fatal("terminal openStream did not return after logout")
	}
}

func TestTerminalStreamClientRunPausesWithoutReconnectWarningOnLogout(t *testing.T) {
	authState := NewAuthState()
	stream := newBlockingTerminalStream()
	logs := &syncBuffer{}
	client := NewTerminalStreamClient(TerminalStreamClientConfig{
		Opener:     &blockingTerminalOpener{stream: stream},
		AuthState:  authState,
		TmuxClient: &tmux.Client{},
		Logger:     zerolog.New(logs),
	})

	runCtx, runCancel := context.WithCancel(context.Background())
	defer runCancel()
	done := make(chan error, 1)
	go func() {
		done <- client.Run(runCtx)
	}()

	select {
	case msg := <-stream.sent:
		if msg != nil {
			t.Fatalf("initial terminal send = %#v, want nil header flush", msg)
		}
	case <-time.After(time.Second):
		t.Fatal("terminal stream did not flush headers")
	}
	select {
	case <-stream.done:
	case <-time.After(time.Second):
		t.Fatal("terminal stream Receive did not start")
	}

	authState.MarkNeedsLogin()

	deadline := time.After(2 * time.Second)
	for !strings.Contains(logs.String(), "terminal stream paused: re-login required") {
		select {
		case err := <-done:
			t.Fatalf("Run returned instead of pausing on logout: %v; logs=%s", err, logs.String())
		case <-deadline:
			t.Fatalf("Run did not pause on logout within 2s; logs=%s", logs.String())
		case <-time.After(5 * time.Millisecond):
		}
	}
	if strings.Contains(logs.String(), "terminal stream closed, reconnecting") {
		t.Fatalf("logout produced a misleading reconnect warning; logs=%s", logs.String())
	}

	runCancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run returned error after ctx cancel: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after ctx cancel")
	}
}

func TestTerminalStreamClientOpenStreamAllowsNilAuthState(t *testing.T) {
	stream := newBlockingTerminalStream()
	client := NewTerminalStreamClient(TerminalStreamClientConfig{
		Opener:     &blockingTerminalOpener{stream: stream},
		TmuxClient: &tmux.Client{},
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	errCh := make(chan error, 1)
	panicCh := make(chan any, 1)
	go func() {
		defer func() {
			if r := recover(); r != nil {
				panicCh <- r
			}
		}()
		errCh <- client.openStream(ctx)
	}()

	select {
	case panicValue := <-panicCh:
		t.Fatalf("openStream panicked with nil AuthState: %v", panicValue)
	case msg := <-stream.sent:
		if msg != nil {
			t.Fatalf("initial terminal send = %#v, want nil header flush", msg)
		}
	case <-time.After(100 * time.Millisecond):
		t.Fatal("terminal stream did not flush headers")
	}

	cancel()

	select {
	case err := <-errCh:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("openStream error = %v, want context.Canceled", err)
		}
	case panicValue := <-panicCh:
		t.Fatalf("openStream panicked with nil AuthState: %v", panicValue)
	case <-time.After(100 * time.Millisecond):
		t.Fatal("terminal openStream did not return after context cancel")
	}
}

type delayedCancelTerminalOpener struct {
	stream *delayedCancelTerminalStream
}

func (o *delayedCancelTerminalOpener) TerminalStream(ctx context.Context) terminalBidiStream {
	o.stream.ctx = ctx
	return o.stream
}

type delayedCancelTerminalStream struct {
	ctx  context.Context
	recv chan *pb.TerminalClientMessage
	sent chan *pb.TerminalServerMessage
	done chan struct{}
}

func newDelayedCancelTerminalStream() *delayedCancelTerminalStream {
	return &delayedCancelTerminalStream{
		recv: make(chan *pb.TerminalClientMessage, 8),
		sent: make(chan *pb.TerminalServerMessage, 8),
		done: make(chan struct{}),
	}
}

func (s *delayedCancelTerminalStream) Send(message *pb.TerminalServerMessage) error {
	s.sent <- message
	return nil
}

func (s *delayedCancelTerminalStream) Receive() (*pb.TerminalClientMessage, error) {
	select {
	case msg := <-s.recv:
		return msg, nil
	case <-s.ctx.Done():
		close(s.done)
		time.Sleep(50 * time.Millisecond)
		return nil, s.ctx.Err()
	}
}

func (s *delayedCancelTerminalStream) CloseRequest() error {
	return nil
}

type closeOrderAttach struct {
	*fakeAttachImpl
	ctx              context.Context
	closeAfterCancel chan bool
}

func (a *closeOrderAttach) Close() error {
	canceled := false
	select {
	case <-a.ctx.Done():
		canceled = true
	default:
	}
	a.closeAfterCancel <- canceled
	return a.fakeAttachImpl.Close()
}

type closeOrderAttachFactory struct {
	created chan *closeOrderAttach
}

func newCloseOrderAttachFactory() *closeOrderAttachFactory {
	return &closeOrderAttachFactory{created: make(chan *closeOrderAttach, 1)}
}

func (f *closeOrderAttachFactory) build(ctx context.Context, cfg tmux.AttachConfig) (terminalAttach, error) {
	attach := &closeOrderAttach{
		fakeAttachImpl:   newFakeAttach(cfg.AttachID),
		ctx:              ctx,
		closeAfterCancel: make(chan bool, 1),
	}
	f.created <- attach
	return attach, nil
}

type logoutDuringAttachFactory struct {
	authState *AuthState
	created   chan *closeOrderAttach
}

func newLogoutDuringAttachFactory(authState *AuthState) *logoutDuringAttachFactory {
	return &logoutDuringAttachFactory{
		authState: authState,
		created:   make(chan *closeOrderAttach, 1),
	}
}

func (f *logoutDuringAttachFactory) build(ctx context.Context, cfg tmux.AttachConfig) (terminalAttach, error) {
	attach := &closeOrderAttach{
		fakeAttachImpl:   newFakeAttach(cfg.AttachID),
		ctx:              ctx,
		closeAfterCancel: make(chan bool, 1),
	}
	f.created <- attach
	f.authState.MarkNeedsLogin()
	<-ctx.Done()
	return attach, nil
}

func waitForTerminalAttachPublished(t *testing.T, client *TerminalStreamClient, attachID string, errCh <-chan error) {
	t.Helper()
	deadline := time.After(time.Second)
	for {
		if client.lookupAttach(attachID) != nil {
			return
		}
		select {
		case err := <-errCh:
			t.Fatalf("terminal openStream returned before attach was published: %v", err)
		case <-deadline:
			t.Fatal("terminal attach was not published")
		case <-time.After(5 * time.Millisecond):
		}
	}
}

func TestTerminalStreamClientOpenStreamClosesAttachSpawnedDuringLogout(t *testing.T) {
	authState := NewAuthState()
	stream := newDelayedCancelTerminalStream()
	factory := newLogoutDuringAttachFactory(authState)
	tmuxName := "boss-rep-chat-spawn-logout"
	client := NewTerminalStreamClient(TerminalStreamClientConfig{
		Opener:        &delayedCancelTerminalOpener{stream: stream},
		AuthState:     authState,
		TmuxClient:    &tmux.Client{},
		Chats:         &fakeChatLookup{rows: map[string]chatRow{"claude-spawn-logout": {TmuxSessionName: &tmuxName}}},
		AttachFactory: factory.build,
	})

	errCh := make(chan error, 1)
	go func() {
		errCh <- client.openStream(context.Background())
	}()

	select {
	case msg := <-stream.sent:
		if msg != nil {
			t.Fatalf("initial terminal send = %#v, want nil header flush", msg)
		}
	case <-time.After(100 * time.Millisecond):
		t.Fatal("terminal stream did not flush headers")
	}

	stream.recv <- &pb.TerminalClientMessage{
		Msg: &pb.TerminalClientMessage_Attach{Attach: &pb.TerminalAttachCommand{
			AttachId: "att-spawn-logout",
			ChatId:   "claude-spawn-logout",
			Cols:     80,
			Rows:     24,
		}},
	}

	var attach *closeOrderAttach
	select {
	case attach = <-factory.created:
	case <-time.After(100 * time.Millisecond):
		t.Fatal("terminal attach was not created")
	}

	select {
	case <-attach.closeAfterCancel:
	case <-time.After(200 * time.Millisecond):
		t.Fatal("terminal attach created during logout was not closed")
	}
	if client.lookupAttach("att-spawn-logout") != nil {
		t.Fatal("terminal attach created during logout was published after stream close began")
	}

	select {
	case err := <-errCh:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("openStream error = %v, want context.Canceled", err)
		}
	case <-time.After(200 * time.Millisecond):
		t.Fatal("terminal openStream did not return after spawn-time logout")
	}
	if !attach.closed.Load() {
		t.Fatal("terminal attach Close was not called on spawn-time logout")
	}
}

func TestTerminalStreamClientOpenStreamClosesActiveAttachBeforeCancelWhenAuthNeedsLogin(t *testing.T) {
	authState := NewAuthState()
	stream := newDelayedCancelTerminalStream()
	factory := newCloseOrderAttachFactory()
	tmuxName := "boss-rep-chat-logout"
	client := NewTerminalStreamClient(TerminalStreamClientConfig{
		Opener:        &delayedCancelTerminalOpener{stream: stream},
		AuthState:     authState,
		TmuxClient:    &tmux.Client{},
		Chats:         &fakeChatLookup{rows: map[string]chatRow{"claude-logout": {TmuxSessionName: &tmuxName}}},
		AttachFactory: factory.build,
	})

	errCh := make(chan error, 1)
	go func() {
		errCh <- client.openStream(context.Background())
	}()

	select {
	case msg := <-stream.sent:
		if msg != nil {
			t.Fatalf("initial terminal send = %#v, want nil header flush", msg)
		}
	case <-time.After(100 * time.Millisecond):
		t.Fatal("terminal stream did not flush headers")
	}

	stream.recv <- &pb.TerminalClientMessage{
		Msg: &pb.TerminalClientMessage_Attach{Attach: &pb.TerminalAttachCommand{
			AttachId: "att-logout",
			ChatId:   "claude-logout",
			Cols:     80,
			Rows:     24,
		}},
	}

	var attach *closeOrderAttach
	select {
	case attach = <-factory.created:
	case <-time.After(100 * time.Millisecond):
		t.Fatal("terminal attach was not created")
	}
	waitForTerminalAttachPublished(t, client, "att-logout", errCh)

	authState.MarkNeedsLogin()

	select {
	case canceledAtClose := <-attach.closeAfterCancel:
		if canceledAtClose {
			t.Fatal("terminal attach Close was called after attach context cancellation")
		}
	case <-time.After(100 * time.Millisecond):
		t.Fatal("terminal attach Close was not called after logout")
	}

	select {
	case <-stream.done:
	case <-time.After(100 * time.Millisecond):
		t.Fatal("terminal stream Receive did not unblock after logout")
	}

	select {
	case err := <-errCh:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("openStream error = %v, want context.Canceled", err)
		}
	case <-time.After(200 * time.Millisecond):
		t.Fatal("terminal openStream did not return after logout")
	}

	if !attach.closed.Load() {
		t.Fatal("terminal attach Close was not called on logout")
	}
}

type stalledWriterTerminalOpener struct {
	stream *stalledWriterTerminalStream
}

func (o *stalledWriterTerminalOpener) TerminalStream(ctx context.Context) terminalBidiStream {
	o.stream.ctx = ctx
	return o.stream
}

type stalledWriterTerminalStream struct {
	ctx         context.Context
	recv        chan *pb.TerminalClientMessage
	headerSent  chan struct{}
	sendStarted chan struct{}
	done        chan struct{}
}

func newStalledWriterTerminalStream() *stalledWriterTerminalStream {
	return &stalledWriterTerminalStream{
		recv:        make(chan *pb.TerminalClientMessage, 8),
		headerSent:  make(chan struct{}, 1),
		sendStarted: make(chan struct{}, 1),
		done:        make(chan struct{}),
	}
}

func (s *stalledWriterTerminalStream) Send(message *pb.TerminalServerMessage) error {
	if message == nil {
		s.headerSent <- struct{}{}
		return nil
	}
	select {
	case s.sendStarted <- struct{}{}:
	default:
	}
	<-s.ctx.Done()
	return s.ctx.Err()
}

func (s *stalledWriterTerminalStream) Receive() (*pb.TerminalClientMessage, error) {
	select {
	case msg := <-s.recv:
		return msg, nil
	case <-s.ctx.Done():
		close(s.done)
		return nil, s.ctx.Err()
	}
}

func (s *stalledWriterTerminalStream) CloseRequest() error {
	return nil
}

type blockedPumpAttach struct {
	*fakeAttachImpl
	ctx              context.Context
	closeAfterCancel chan bool
}

func newBlockedPumpAttach(id string, ctx context.Context) *blockedPumpAttach {
	attach := &blockedPumpAttach{
		fakeAttachImpl:   newFakeAttach(id),
		ctx:              ctx,
		closeAfterCancel: make(chan bool, 1),
	}
	attach.output = make(chan *pb.TerminalDataChunk, 128)
	for i := 0; i < 128; i++ {
		attach.output <- &pb.TerminalDataChunk{AttachId: id, Data: []byte{byte(i)}}
	}
	return attach
}

func (a *blockedPumpAttach) Close() error {
	canceled := false
	select {
	case <-a.ctx.Done():
		canceled = true
	default:
	}
	a.closeAfterCancel <- canceled
	return a.fakeAttachImpl.Close()
}

type blockedPumpAttachFactory struct {
	created chan *blockedPumpAttach
}

func newBlockedPumpAttachFactory() *blockedPumpAttachFactory {
	return &blockedPumpAttachFactory{created: make(chan *blockedPumpAttach, 1)}
}

func (f *blockedPumpAttachFactory) build(ctx context.Context, cfg tmux.AttachConfig) (terminalAttach, error) {
	attach := newBlockedPumpAttach(cfg.AttachID, ctx)
	f.created <- attach
	return attach, nil
}

func TestTerminalStreamClientOpenStreamLogoutCancelsBeforeWaitingForBlockedAttachPump(t *testing.T) {
	authState := NewAuthState()
	stream := newStalledWriterTerminalStream()
	factory := newBlockedPumpAttachFactory()
	tmuxName := "boss-rep-chat-stalled"
	client := NewTerminalStreamClient(TerminalStreamClientConfig{
		Opener:        &stalledWriterTerminalOpener{stream: stream},
		AuthState:     authState,
		TmuxClient:    &tmux.Client{},
		Chats:         &fakeChatLookup{rows: map[string]chatRow{"claude-stalled": {TmuxSessionName: &tmuxName}}},
		AttachFactory: factory.build,
	})

	errCh := make(chan error, 1)
	go func() {
		errCh <- client.openStream(context.Background())
	}()

	select {
	case <-stream.headerSent:
	case <-time.After(100 * time.Millisecond):
		t.Fatal("terminal stream did not flush headers")
	}

	stream.recv <- &pb.TerminalClientMessage{
		Msg: &pb.TerminalClientMessage_Attach{Attach: &pb.TerminalAttachCommand{
			AttachId: "att-stalled",
			ChatId:   "claude-stalled",
			Cols:     80,
			Rows:     24,
		}},
	}

	var attach *blockedPumpAttach
	attachDeadline := time.After(time.Second)
	select {
	case attach = <-factory.created:
	case err := <-errCh:
		t.Fatalf("terminal openStream returned before attach was created: %v", err)
	case <-attachDeadline:
		t.Fatal("terminal attach was not created")
	}
	waitForTerminalAttachPublished(t, client, "att-stalled", errCh)

	select {
	case <-stream.sendStarted:
	case <-time.After(100 * time.Millisecond):
		t.Fatal("terminal stream writer did not stall in Send")
	}
	time.Sleep(20 * time.Millisecond)

	authState.MarkNeedsLogin()

	select {
	case canceledAtClose := <-attach.closeAfterCancel:
		if canceledAtClose {
			t.Fatal("terminal attach Close was called after attach context cancellation")
		}
	case <-time.After(100 * time.Millisecond):
		t.Fatal("terminal attach Close was not called after logout")
	}

	select {
	case <-stream.done:
	case <-time.After(100 * time.Millisecond):
		t.Fatal("terminal stream was not canceled while attach pump was blocked")
	}

	select {
	case err := <-errCh:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("openStream error = %v, want context.Canceled", err)
		}
	case <-time.After(200 * time.Millisecond):
		t.Fatal("terminal openStream did not return after stalled logout")
	}
}
