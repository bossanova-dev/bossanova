package upstream

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossd/internal/chatupload"
	"github.com/recurser/bossd/internal/tmux"
	"github.com/rs/zerolog"
)

// recordingChatSender captures the prompts chatupload.Manager submits, so a
// test can assert exactly one message named the upload.
type recordingChatSender struct {
	mu       sync.Mutex
	messages []string
	// submits records the submit flag alongside each message, so a test can
	// pin the prefill decision at THIS level — the wire frame to the composer
	// — rather than only where chatupload calls the sender.
	submits []bool
	err     error
}

func (s *recordingChatSender) SendChatMessage(_ context.Context, _, message string, submit bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err != nil {
		return s.err
	}
	s.messages = append(s.messages, message)
	s.submits = append(s.submits, submit)
	return nil
}

func (s *recordingChatSender) submitted() []bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]bool(nil), s.submits...)
}

func (s *recordingChatSender) sent() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.messages...)
}

// stubUploadManager is a scriptable uploadManager. It exists for the cases
// the real manager cannot express on demand: an error class chosen by the
// test, a chunk write held open so the ack ordering can be observed, and a
// record of which cancel call the teardown path made.
type stubUploadManager struct {
	mu sync.Mutex

	startErr  error
	claimErr  error
	finishErr error

	// claimCalls counts ClaimFinish invocations. It is what proves the
	// finish gate runs synchronously on the reader goroutine: a repeated
	// upload_finish must increment this without ever reaching Finish.
	claimCalls  int
	finishCalls int
	claimed     map[string]bool

	// chunkGate, when non-nil, blocks Chunk until the test closes it.
	chunkGate chan struct{}

	// finishAwaitsAttachCancel makes Finish park until the stream teardown
	// has cancelled the attach — which openStream does immediately AFTER
	// cancelling the stream context — and then record the state of the
	// context this delivery was handed. finishCtxSeen/finishCtxErr are that
	// recording.
	finishAwaitsAttachCancel bool
	finishCtxSeen            bool
	finishCtxErr             error

	received        uint64
	cancelled       []string
	cancelledAttach []string
	cancelAllCalls  int
	startedChatIDs  []string
}

func (s *stubUploadManager) Start(_, _, chatID, _ string, _ uint64) error {
	s.mu.Lock()
	s.startedChatIDs = append(s.startedChatIDs, chatID)
	err := s.startErr
	s.mu.Unlock()
	return err
}

func (s *stubUploadManager) Chunk(_, _ string, _ uint64, data []byte) (uint64, error) {
	s.mu.Lock()
	gate := s.chunkGate
	s.mu.Unlock()
	if gate != nil {
		<-gate
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.received += uint64(len(data))
	return s.received, nil
}

// ClaimFinish mirrors the real manager's claim semantics: the first finish
// for an id is admitted, every later one is refused, so the caller may spawn
// at most one delivery goroutine per upload.
func (s *stubUploadManager) ClaimFinish(attachID, uploadID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.claimCalls++
	if s.claimErr != nil {
		return s.claimErr
	}
	if s.claimed == nil {
		s.claimed = make(map[string]bool)
	}
	k := attachID + "/" + uploadID
	if s.claimed[k] {
		return &chatupload.Error{Msg: "upload: duplicate finish"}
	}
	s.claimed[k] = true
	return nil
}

func (s *stubUploadManager) Finish(ctx context.Context, _, _ string) (string, error) {
	s.mu.Lock()
	s.finishCalls++
	await := s.finishAwaitsAttachCancel
	s.mu.Unlock()
	if await {
		start := time.Now()
		for !s.attachCancelSeen() && ctx.Err() == nil && time.Since(start) < 3*time.Second {
			time.Sleep(2 * time.Millisecond)
		}
		s.mu.Lock()
		s.finishCtxSeen, s.finishCtxErr = true, ctx.Err()
		s.mu.Unlock()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.finishErr != nil {
		return "", s.finishErr
	}
	return "/private/upload-path", nil
}

func (s *stubUploadManager) attachCancelSeen() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.cancelledAttach) > 0
}

func (s *stubUploadManager) finishCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.finishCalls
}

func (s *stubUploadManager) deliveryContext() (seen bool, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.finishCtxSeen, s.finishCtxErr
}

func (s *stubUploadManager) Cancel(attachID, uploadID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cancelled = append(s.cancelled, attachID+"/"+uploadID)
}

func (s *stubUploadManager) CancelAttach(attachID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cancelledAttach = append(s.cancelledAttach, attachID)
}

func (s *stubUploadManager) CancelAll() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cancelAllCalls++
}

func (s *stubUploadManager) snapshot() (cancelled, cancelledAttach []string, cancelAll int, chats []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.cancelled...),
		append([]string(nil), s.cancelledAttach...),
		s.cancelAllCalls,
		append([]string(nil), s.startedChatIDs...)
}

// newUploadHarness mirrors newTestHarness but wires an upload manager.
func newUploadHarness(t *testing.T, uploads UploadManager) *testHarness {
	t.Helper()
	opener := &fakeTerminalOpener{}
	tmuxName := "boss-rep-chat-1"
	chats := &fakeChatLookup{
		rows: map[string]chatRow{"claude-1": {TmuxSessionName: &tmuxName}},
	}
	factory := newFakeAttachFactory()
	cmdRec := &recordingCmdFactory{}
	client := NewTerminalStreamClient(TerminalStreamClientConfig{
		Opener:        opener,
		Chats:         chats,
		TmuxClient:    tmux.NewClient(tmux.WithCommandFactory(cmdRec.factory)),
		AttachFactory: factory.build,
		Uploads:       uploads,
		Logger:        zerolog.Nop(),
	})
	return &testHarness{opener: opener, chats: chats, factory: factory, tmuxCmd: cmdRec, client: client}
}

// uploadAttachID is the single attach every upload test drives. Fixed
// rather than a parameter: the dispatch is keyed by attach_id and the
// multi-attach routing is already covered by the terminal-stream tests.
const uploadAttachID = "att-1"

// attachOne drives one TerminalAttachCommand and waits for the attach.
func attachOne(t *testing.T, h *testHarness, stream *fakeTerminalStream) {
	t.Helper()
	stream.recv <- &pb.TerminalClientMessage{
		Msg: &pb.TerminalClientMessage_Attach{Attach: &pb.TerminalAttachCommand{
			AttachId: uploadAttachID, ChatId: "claude-1", Cols: 80, Rows: 24,
		}},
	}
	waitFor(t, "attach created", func() bool { return h.factory.get(uploadAttachID) != nil })
}

// waitForUploadFrame drains `sent` until an upload ack or result arrives,
// skipping unrelated terminal frames (data / exited / pong).
func waitForUploadFrame(t *testing.T, s *fakeTerminalStream) *pb.TerminalServerMessage {
	t.Helper()
	deadline := time.After(3 * time.Second)
	for {
		select {
		case m := <-s.sent:
			switch m.GetMsg().(type) {
			case *pb.TerminalServerMessage_UploadAck, *pb.TerminalServerMessage_UploadResult:
				return m
			}
		case <-deadline:
			t.Fatal("no upload frame within 3s")
		}
	}
}

// dirBytes totals the bytes currently stored in dir.
func dirBytes(t *testing.T, dir string) int64 {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read upload dir: %v", err)
	}
	var total int64
	for _, e := range entries {
		info, err := e.Info()
		if err != nil {
			continue
		}
		total += info.Size()
	}
	return total
}

func newRealManager(t *testing.T, sender chatupload.ChatMessageSender) (*chatupload.Manager, string) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "uploads")
	mgr, err := chatupload.NewManager(dir, sender)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	return mgr, mgr.Dir()
}

// TestTerminalUpload_AckOnlyAfterDurableWrite is the backpressure contract:
// the ack releases a slot in the browser's four-chunk window, so it must
// not be emitted until the bytes have reached the daemon's file handle.
// The stub holds Chunk open; no ack may appear while it is blocked.
func TestTerminalUpload_AckOnlyAfterDurableWrite(t *testing.T) {
	t.Parallel()
	gate := make(chan struct{})
	stub := &stubUploadManager{chunkGate: gate}
	h := newUploadHarness(t, stub)
	stop := runClient(t, h.client)
	defer stop()

	stream := waitForStream(t, h.opener)
	attachOne(t, h, stream)

	stream.recv <- &pb.TerminalClientMessage{
		Msg: &pb.TerminalClientMessage_UploadStart{UploadStart: &pb.TerminalUploadStart{
			AttachId: "att-1", UploadId: "up-1", Filename: "notes.txt", SizeBytes: 4,
		}},
	}
	stream.recv <- &pb.TerminalClientMessage{
		Msg: &pb.TerminalClientMessage_UploadChunk{UploadChunk: &pb.TerminalUploadChunk{
			AttachId: "att-1", UploadId: "up-1", Seq: 0, Data: []byte("abcd"),
		}},
	}

	select {
	case m := <-stream.sent:
		t.Fatalf("frame emitted while the write was still in flight: %T", m.GetMsg())
	case <-time.After(150 * time.Millisecond):
	}

	close(gate)
	frame := waitForUploadFrame(t, stream)
	ack := frame.GetUploadAck()
	if ack == nil {
		t.Fatalf("expected an upload ack, got %T", frame.GetMsg())
	}
	if ack.GetSeq() != 0 || ack.GetBytesReceived() != 4 {
		t.Fatalf("ack = seq %d / %d bytes, want seq 0 / 4 bytes", ack.GetSeq(), ack.GetBytesReceived())
	}
}

// TestTerminalUpload_AckImpliesBytesOnDisk is the same contract measured
// against the real manager: when the ack lands, the bytes are already in
// the upload directory.
func TestTerminalUpload_AckImpliesBytesOnDisk(t *testing.T) {
	t.Parallel()
	sender := &recordingChatSender{}
	mgr, dir := newRealManager(t, sender)
	h := newUploadHarness(t, mgr)
	stop := runClient(t, h.client)
	defer stop()

	stream := waitForStream(t, h.opener)
	attachOne(t, h, stream)

	payload := []byte("hello upload")
	stream.recv <- &pb.TerminalClientMessage{
		Msg: &pb.TerminalClientMessage_UploadStart{UploadStart: &pb.TerminalUploadStart{
			AttachId: "att-1", UploadId: "up-1", Filename: "hello.txt", SizeBytes: uint64(len(payload)),
		}},
	}
	stream.recv <- &pb.TerminalClientMessage{
		Msg: &pb.TerminalClientMessage_UploadChunk{UploadChunk: &pb.TerminalUploadChunk{
			AttachId: "att-1", UploadId: "up-1", Seq: 0, Data: payload,
		}},
	}

	ack := waitForUploadFrame(t, stream).GetUploadAck()
	if ack == nil {
		t.Fatal("expected an upload ack")
	}
	if got := dirBytes(t, dir); got < int64(len(payload)) {
		t.Fatalf("upload dir held %d bytes at ack time, want >= %d", got, len(payload))
	}
}

// TestTerminalUpload_FinishDeliversPromptAndResult covers the success
// path: one chat message naming the file, one ok result, and the bytes
// still on disk for the agent to read.
func TestTerminalUpload_FinishDeliversPromptAndResult(t *testing.T) {
	t.Parallel()
	sender := &recordingChatSender{}
	mgr, dir := newRealManager(t, sender)
	h := newUploadHarness(t, mgr)
	stop := runClient(t, h.client)
	defer stop()

	stream := waitForStream(t, h.opener)
	attachOne(t, h, stream)

	payload := []byte("payload bytes")
	stream.recv <- &pb.TerminalClientMessage{
		Msg: &pb.TerminalClientMessage_UploadStart{UploadStart: &pb.TerminalUploadStart{
			AttachId: "att-1", UploadId: "up-1", Filename: "report.pdf", SizeBytes: uint64(len(payload)),
		}},
	}
	stream.recv <- &pb.TerminalClientMessage{
		Msg: &pb.TerminalClientMessage_UploadChunk{UploadChunk: &pb.TerminalUploadChunk{
			AttachId: "att-1", UploadId: "up-1", Seq: 0, Data: payload,
		}},
	}
	if ack := waitForUploadFrame(t, stream).GetUploadAck(); ack == nil {
		t.Fatal("expected an upload ack before finish")
	}
	stream.recv <- &pb.TerminalClientMessage{
		Msg: &pb.TerminalClientMessage_UploadFinish{UploadFinish: &pb.TerminalUploadFinish{
			AttachId: "att-1", UploadId: "up-1",
		}},
	}

	result := waitForUploadFrame(t, stream).GetUploadResult()
	if result == nil {
		t.Fatal("expected a terminal upload result")
	}
	if !result.GetIsOk() {
		t.Fatalf("result not ok: %q", result.GetErrorMessage())
	}
	if result.GetErrorMessage() != "" {
		t.Fatalf("ok result carried an error message: %q", result.GetErrorMessage())
	}

	msgs := sender.sent()
	if len(msgs) != 1 {
		t.Fatalf("expected exactly 1 chat message, got %d", len(msgs))
	}
	if !strings.HasPrefix(msgs[0], dir) {
		t.Fatalf("delivered text %q is not the uploaded path under %q", msgs[0], dir)
	}
	// The upload prefills its path; it never sends. A submit here would spend
	// the user's turn on a half-written thought and put the path wherever the
	// composer happened to be, which is the whole reason this is not a submit.
	if submits := sender.submitted(); len(submits) != 1 || submits[0] {
		t.Fatalf("upload delivery submitted = %v, want exactly one prefill", submits)
	}
	if got := dirBytes(t, dir); got != int64(len(payload)) {
		t.Fatalf("upload dir holds %d bytes after finish, want %d", got, len(payload))
	}
}

// TestTerminalUpload_FinishFailureIsReported covers the finish half of the
// mapping. It also pins the unconfirmed-delivery decision: chatupload
// reports that case as PERMANENT because a retry would submit a second
// prompt into a chat that may already hold the first.
func TestTerminalUpload_FinishFailureIsReported(t *testing.T) {
	t.Parallel()
	stub := &stubUploadManager{
		finishErr: &chatupload.Error{Msg: "upload: chat delivery unconfirmed; file retained"},
	}
	h := newUploadHarness(t, stub)
	stop := runClient(t, h.client)
	defer stop()

	stream := waitForStream(t, h.opener)
	attachOne(t, h, stream)
	stream.recv <- &pb.TerminalClientMessage{
		Msg: &pb.TerminalClientMessage_UploadFinish{UploadFinish: &pb.TerminalUploadFinish{
			AttachId: uploadAttachID, UploadId: "up-1",
		}},
	}

	result := waitForUploadFrame(t, stream).GetUploadResult()
	if result == nil || result.GetIsOk() {
		t.Fatalf("expected a failure result, got %+v", result)
	}
	if result.GetErrorMessage() != "upload: chat delivery unconfirmed; file retained" {
		t.Fatalf("error_message = %q", result.GetErrorMessage())
	}
	if result.GetCanRetry() {
		t.Fatal("an unconfirmed delivery must not invite a retry")
	}
}

// TestTerminalUpload_ErrorMapping checks the retryable classification and
// the refusal to echo an unrecognised error's text back to the browser.
func TestTerminalUpload_ErrorMapping(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name        string
		err         error
		wantMessage string
		wantRetry   bool
	}{
		{
			name:        "permanent rejection",
			err:         &chatupload.Error{Msg: "upload: file exceeds limit"},
			wantMessage: "upload: file exceeds limit",
		},
		{
			name:        "retryable transport failure",
			err:         &chatupload.Error{Msg: "upload: write failed", Retryable: true},
			wantMessage: "upload: write failed",
			wantRetry:   true,
		},
		{
			name: "wrapped upload error keeps its classification",
			err: func() error {
				return errors.Join(errors.New("context"), &chatupload.Error{Msg: "upload: sync failed", Retryable: true})
			}(),
			wantMessage: "upload: sync failed",
			wantRetry:   true,
		},
		{
			// An error the manager never produces must not reach the wire
			// verbatim — it could carry a path or a driver string.
			name:        "unknown error is generic and permanent",
			err:         errors.New("open /var/lib/bossd/chat-uploads/upload-123.bin: boom"),
			wantMessage: "upload failed",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			stub := &stubUploadManager{startErr: tc.err}
			h := newUploadHarness(t, stub)
			stop := runClient(t, h.client)
			defer stop()

			stream := waitForStream(t, h.opener)
			attachOne(t, h, stream)
			stream.recv <- &pb.TerminalClientMessage{
				Msg: &pb.TerminalClientMessage_UploadStart{UploadStart: &pb.TerminalUploadStart{
					AttachId: "att-1", UploadId: "up-1", Filename: "f.txt", SizeBytes: 1,
				}},
			}

			result := waitForUploadFrame(t, stream).GetUploadResult()
			if result == nil {
				t.Fatal("expected a terminal upload result")
			}
			if result.GetIsOk() {
				t.Fatal("expected a failure result")
			}
			if result.GetErrorMessage() != tc.wantMessage {
				t.Fatalf("error_message = %q, want %q", result.GetErrorMessage(), tc.wantMessage)
			}
			if result.GetCanRetry() != tc.wantRetry {
				t.Fatalf("can_retry = %v, want %v", result.GetCanRetry(), tc.wantRetry)
			}
		})
	}
}

// TestTerminalUpload_OutOfOrderChunkIsTerminalAndUnacked proves a rejected
// chunk produces a permanent result and no ack, so the client cannot read
// a gap as accepted.
func TestTerminalUpload_OutOfOrderChunkIsTerminalAndUnacked(t *testing.T) {
	t.Parallel()
	sender := &recordingChatSender{}
	mgr, dir := newRealManager(t, sender)
	h := newUploadHarness(t, mgr)
	stop := runClient(t, h.client)
	defer stop()

	stream := waitForStream(t, h.opener)
	attachOne(t, h, stream)

	stream.recv <- &pb.TerminalClientMessage{
		Msg: &pb.TerminalClientMessage_UploadStart{UploadStart: &pb.TerminalUploadStart{
			AttachId: "att-1", UploadId: "up-1", Filename: "gap.bin", SizeBytes: 8,
		}},
	}
	// seq 1 without seq 0.
	stream.recv <- &pb.TerminalClientMessage{
		Msg: &pb.TerminalClientMessage_UploadChunk{UploadChunk: &pb.TerminalUploadChunk{
			AttachId: "att-1", UploadId: "up-1", Seq: 1, Data: []byte("abcd"),
		}},
	}

	frame := waitForUploadFrame(t, stream)
	if frame.GetUploadAck() != nil {
		t.Fatal("an out-of-order chunk was acknowledged")
	}
	result := frame.GetUploadResult()
	if result == nil || result.GetIsOk() {
		t.Fatalf("expected a failure result, got %+v", frame.GetMsg())
	}
	if result.GetCanRetry() {
		t.Fatal("an out-of-order chunk is a permanent protocol violation, not retryable")
	}
	waitFor(t, "partial file removed", func() bool { return dirBytes(t, dir) == 0 })
	if mgr.InFlight() != 0 {
		t.Fatalf("upload still in flight after a terminal failure: %d", mgr.InFlight())
	}
}

// TestTerminalUpload_UnknownAttachIsRetryable covers an upload for an
// attach this daemon does not have: the file was fine, the attach was not.
func TestTerminalUpload_UnknownAttachIsRetryable(t *testing.T) {
	t.Parallel()
	stub := &stubUploadManager{}
	h := newUploadHarness(t, stub)
	stop := runClient(t, h.client)
	defer stop()

	stream := waitForStream(t, h.opener)
	stream.recv <- &pb.TerminalClientMessage{
		Msg: &pb.TerminalClientMessage_UploadStart{UploadStart: &pb.TerminalUploadStart{
			AttachId: "ghost", UploadId: "up-1", Filename: "f.txt", SizeBytes: 1,
		}},
	}

	result := waitForUploadFrame(t, stream).GetUploadResult()
	if result == nil || result.GetIsOk() {
		t.Fatal("expected a failure result for an unknown attach")
	}
	if !result.GetCanRetry() {
		t.Fatal("an unknown attach is transport loss and must be retryable")
	}
	if _, _, _, chats := stub.snapshot(); len(chats) != 0 {
		t.Fatalf("manager was called for an unknown attach: %v", chats)
	}
}

// TestTerminalUpload_ChatIDComesFromTheAttachRegistry proves the daemon
// resolves the target chat from the live attach rather than from anything
// the client can name on the wire.
func TestTerminalUpload_ChatIDComesFromTheAttachRegistry(t *testing.T) {
	t.Parallel()
	stub := &stubUploadManager{}
	h := newUploadHarness(t, stub)
	stop := runClient(t, h.client)
	defer stop()

	stream := waitForStream(t, h.opener)
	attachOne(t, h, stream)
	stream.recv <- &pb.TerminalClientMessage{
		Msg: &pb.TerminalClientMessage_UploadStart{UploadStart: &pb.TerminalUploadStart{
			AttachId: "att-1", UploadId: "up-1", Filename: "f.txt", SizeBytes: 1,
		}},
	}

	waitFor(t, "start reached the manager", func() bool {
		_, _, _, chats := stub.snapshot()
		return len(chats) == 1
	})
	_, _, _, chats := stub.snapshot()
	if chats[0] != "claude-1" {
		t.Fatalf("upload started against chat %q, want the attach's chat claude-1", chats[0])
	}
}

// TestTerminalUpload_DisabledManagerRejectsPermanently covers a daemon
// whose upload directory could not be created: the frames must be answered,
// not dropped, or the browser waits forever.
func TestTerminalUpload_DisabledManagerRejectsPermanently(t *testing.T) {
	t.Parallel()
	h := newUploadHarness(t, nil)
	stop := runClient(t, h.client)
	defer stop()

	stream := waitForStream(t, h.opener)
	attachOne(t, h, stream)
	stream.recv <- &pb.TerminalClientMessage{
		Msg: &pb.TerminalClientMessage_UploadStart{UploadStart: &pb.TerminalUploadStart{
			AttachId: "att-1", UploadId: "up-1", Filename: "f.txt", SizeBytes: 1,
		}},
	}

	result := waitForUploadFrame(t, stream).GetUploadResult()
	if result == nil || result.GetIsOk() || result.GetCanRetry() {
		t.Fatalf("expected a permanent failure result, got %+v", result)
	}
}

// TestTerminalUpload_AttachCloseCancelsInFlightUpload proves a detach
// removes the partial file rather than leaving it for the janitor.
func TestTerminalUpload_AttachCloseCancelsInFlightUpload(t *testing.T) {
	t.Parallel()
	sender := &recordingChatSender{}
	mgr, dir := newRealManager(t, sender)
	h := newUploadHarness(t, mgr)
	stop := runClient(t, h.client)
	defer stop()

	stream := waitForStream(t, h.opener)
	attachOne(t, h, stream)

	stream.recv <- &pb.TerminalClientMessage{
		Msg: &pb.TerminalClientMessage_UploadStart{UploadStart: &pb.TerminalUploadStart{
			AttachId: "att-1", UploadId: "up-1", Filename: "big.bin", SizeBytes: 64,
		}},
	}
	stream.recv <- &pb.TerminalClientMessage{
		Msg: &pb.TerminalClientMessage_UploadChunk{UploadChunk: &pb.TerminalUploadChunk{
			AttachId: "att-1", UploadId: "up-1", Seq: 0, Data: []byte("partial"),
		}},
	}
	if ack := waitForUploadFrame(t, stream).GetUploadAck(); ack == nil {
		t.Fatal("expected an ack for the first chunk")
	}
	if dirBytes(t, dir) == 0 {
		t.Fatal("expected a partial file on disk before the close")
	}

	stream.recv <- &pb.TerminalClientMessage{
		Msg: &pb.TerminalClientMessage_Close{Close: &pb.TerminalCloseCommand{AttachId: "att-1"}},
	}

	waitFor(t, "in-flight upload cancelled", func() bool { return mgr.InFlight() == 0 })
	waitFor(t, "partial file removed", func() bool { return dirBytes(t, dir) == 0 })
	if len(sender.sent()) != 0 {
		t.Fatal("a cancelled upload must not submit a chat message")
	}
}

// TestTerminalUpload_StreamTeardownCancelsAll proves the stream teardown
// path drops uploads whose attach never produced a pump exit.
func TestTerminalUpload_StreamTeardownCancelsAll(t *testing.T) {
	t.Parallel()
	stub := &stubUploadManager{}
	h := newUploadHarness(t, stub)
	stop := runClient(t, h.client)

	stream := waitForStream(t, h.opener)
	attachOne(t, h, stream)
	stream.recv <- &pb.TerminalClientMessage{
		Msg: &pb.TerminalClientMessage_UploadStart{UploadStart: &pb.TerminalUploadStart{
			AttachId: "att-1", UploadId: "up-1", Filename: "f.txt", SizeBytes: 4,
		}},
	}
	waitFor(t, "start reached the manager", func() bool {
		_, _, _, chats := stub.snapshot()
		return len(chats) == 1
	})

	stop()

	_, cancelledAttach, cancelAll, _ := stub.snapshot()
	if cancelAll == 0 {
		t.Fatal("stream teardown did not cancel all in-flight uploads")
	}
	found := false
	for _, id := range cancelledAttach {
		if id == "att-1" {
			found = true
		}
	}
	if !found {
		t.Fatalf("attach teardown did not cancel its uploads: %v", cancelledAttach)
	}
}

// TestTerminalUpload_CancelIsSilentAndForwarded proves an explicit cancel
// reaches the manager and emits no second terminal result.
func TestTerminalUpload_CancelIsSilentAndForwarded(t *testing.T) {
	t.Parallel()
	stub := &stubUploadManager{}
	h := newUploadHarness(t, stub)
	stop := runClient(t, h.client)
	defer stop()

	stream := waitForStream(t, h.opener)
	attachOne(t, h, stream)
	stream.recv <- &pb.TerminalClientMessage{
		Msg: &pb.TerminalClientMessage_UploadCancel{UploadCancel: &pb.TerminalUploadCancel{
			AttachId: "att-1", UploadId: "up-1", Reason: "user cancelled",
		}},
	}

	waitFor(t, "cancel reached the manager", func() bool {
		cancelled, _, _, _ := stub.snapshot()
		return len(cancelled) == 1 && cancelled[0] == "att-1/up-1"
	})
	select {
	case m := <-stream.sent:
		t.Fatalf("a cancel emitted a frame: %T", m.GetMsg())
	case <-time.After(100 * time.Millisecond):
	}
}

// TestTerminalUpload_UnknownMessageIsSkippedNotFatal covers forward
// compatibility: a message this daemon does not understand must be dropped
// while the stream keeps serving terminal traffic.
func TestTerminalUpload_UnknownMessageIsSkippedNotFatal(t *testing.T) {
	t.Parallel()
	stub := &stubUploadManager{}
	h := newUploadHarness(t, stub)
	stop := runClient(t, h.client)
	defer stop()

	stream := waitForStream(t, h.opener)
	attachOne(t, h, stream)

	// An empty oneof stands in for a message kind from a newer bosso.
	stream.recv <- &pb.TerminalClientMessage{}

	stream.recv <- &pb.TerminalClientMessage{
		Msg: &pb.TerminalClientMessage_Input{Input: &pb.TerminalInputCommand{
			AttachId: "att-1", Data: []byte("ls\n"),
		}},
	}
	waitFor(t, "input still routed after an unknown message", func() bool {
		return h.factory.get("att-1").inputCount() == 1
	})
	if got := h.opener.calls(); got != 1 {
		t.Fatalf("stream reconnected after an unknown message: %d opens", got)
	}
}

// TestTerminalUpload_RepeatedFinishSpawnsOneDelivery is the spawn bound.
//
// handleUploadFinish delivers on its own goroutine because the chat submit
// drives tmux and polls the pane. Without a SYNCHRONOUS claim first, the
// spawn rate is the client's frame rate: a repeated upload_finish would
// start one goroutine per ~30-byte frame, each able to park on a full
// outbound queue, and nothing would bound the pile. The claim must run on
// the reader goroutine and admit exactly one finish per upload id.
func TestTerminalUpload_RepeatedFinishSpawnsOneDelivery(t *testing.T) {
	t.Parallel()
	stub := &stubUploadManager{}
	h := newUploadHarness(t, stub)
	stop := runClient(t, h.client)
	defer stop()

	stream := waitForStream(t, h.opener)
	attachOne(t, h, stream)

	stream.recv <- &pb.TerminalClientMessage{
		Msg: &pb.TerminalClientMessage_UploadStart{UploadStart: &pb.TerminalUploadStart{
			AttachId: "att-1", UploadId: "up-1", Filename: "notes.txt", SizeBytes: 4,
		}},
	}
	stream.recv <- &pb.TerminalClientMessage{
		Msg: &pb.TerminalClientMessage_UploadChunk{UploadChunk: &pb.TerminalUploadChunk{
			AttachId: "att-1", UploadId: "up-1", Seq: 0, Data: []byte("abcd"),
		}},
	}
	if frame := waitForUploadFrame(t, stream); frame.GetUploadAck() == nil {
		t.Fatalf("expected an ack, got %T", frame.GetMsg())
	}

	const repeats = 6
	for range repeats {
		stream.recv <- &pb.TerminalClientMessage{
			Msg: &pb.TerminalClientMessage_UploadFinish{UploadFinish: &pb.TerminalUploadFinish{
				AttachId: "att-1", UploadId: "up-1",
			}},
		}
	}

	// Exactly one success and repeats-1 permanent rejections arrive. The
	// assertion is deliberately ORDER-INDEPENDENT: the success comes from
	// the spawned delivery goroutine while the rejections come from the
	// reader, and nothing synchronises the two — the reader can plough
	// through every repeat before the delivery goroutine is even scheduled.
	// Ordering here would be a flake, not a contract.
	var successes, rejections int
	for range repeats {
		res := waitForUploadFrame(t, stream).GetUploadResult()
		if res == nil {
			t.Fatal("expected an upload result frame")
		}
		if res.GetIsOk() {
			successes++
			continue
		}
		rejections++
		if res.GetCanRetry() {
			t.Fatalf("a duplicate finish must not invite a retry: %+v", res)
		}
	}
	if successes != 1 || rejections != repeats-1 {
		t.Fatalf("results = %d success / %d rejections, want 1 / %d", successes, rejections, repeats-1)
	}

	stub.mu.Lock()
	claims, finishes := stub.claimCalls, stub.finishCalls
	stub.mu.Unlock()
	if claims != repeats {
		t.Fatalf("ClaimFinish calls = %d, want %d — the gate must see every frame", claims, repeats)
	}
	// The whole point: N frames, ONE delivery goroutine.
	if finishes != 1 {
		t.Fatalf("Finish calls = %d, want exactly 1 — %d finish frames spawned %d deliveries", finishes, repeats, finishes)
	}
}

// The chat submission must NOT run on the stream context.
//
// openStream's teardown cancels the stream context BEFORE it joins the
// delivery goroutine (cancel() → cancelAndWaitAttaches → <-readerDone →
// waitUploads), so every ordinary stream cycle — heartbeat miss, CycleStream,
// re-auth, reconnect — used to poison an in-flight finish. chatupload.Finish
// treats a cancelled context as UNCONFIRMED, which is reported PERMANENT
// (can_retry=0) and retains the file for the full 24h TTL: a routine
// reconnect became an orphan file plus a failure the user cannot retry.
// Since teardown waits for the goroutine anyway, cancelling it first bought
// nothing.
//
// The stub parks inside Finish until the teardown has cancelled the attach —
// which happens immediately after the stream context is cancelled — and then
// records the context it was handed. That context must still be live.
func TestTerminalUpload_DeliveryContextSurvivesStreamTeardown(t *testing.T) {
	t.Parallel()
	stub := &stubUploadManager{finishAwaitsAttachCancel: true}
	h := newUploadHarness(t, stub)
	stop := runClient(t, h.client)

	stream := waitForStream(t, h.opener)
	attachOne(t, h, stream)
	stream.recv <- &pb.TerminalClientMessage{
		Msg: &pb.TerminalClientMessage_UploadFinish{UploadFinish: &pb.TerminalUploadFinish{
			AttachId: uploadAttachID, UploadId: "up-1",
		}},
	}
	waitFor(t, "finish reached the manager", func() bool { return stub.finishCount() > 0 })

	// Tear the stream down while the delivery is in flight.
	stop()

	seen, ctxErr := stub.deliveryContext()
	if !seen {
		t.Fatal("the delivery never observed its context")
	}
	if ctxErr != nil {
		t.Fatalf("the chat submission ran on a context the stream teardown had already cancelled (%v); "+
			"chatupload reports that as a permanent unconfirmed delivery and retains the file for 24h", ctxErr)
	}
}
