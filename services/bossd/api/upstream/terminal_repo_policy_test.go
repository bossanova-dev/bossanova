package upstream

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"testing/synctest"
	"time"

	"github.com/rs/zerolog"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/kit/config"
)

func terminalRepoTargets() *targetFixture {
	return &targetFixture{objects: map[targetKey]CommandTargetObject{
		{"chat", "claude-1"}: {RepoID: "a", SessionID: "sa"},
		{"chat", "chat-b"}:   {RepoID: "b", SessionID: "sb"},
		{"session", "sa"}:    {RepoID: "a"}, {"session", "sb"}: {RepoID: "b"},
		{"repo", "a"}: {RepoID: "a"}, {"repo", "b"}: {RepoID: "b"},
	}}
}

func TestTerminalRepoInputAndDowngrade(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newTestHarness(t)
		h.client.targetObjects = terminalRepoTargets()
		policy := targetPolicy{machine: config.CloudControlReadOnly, repos: map[string]config.CloudControlMode{"a": config.CloudControlFull}}
		h.client.cloudControl = policy
		h.chats.rows["chat-b"] = h.chats.rows["claude-1"]
		out := make(chan *pb.TerminalServerMessage, 16)
		for _, target := range []struct{ id, chat string }{{"a", "claude-1"}, {"b", "chat-b"}} {
			h.client.handleAttach(t.Context(), &pb.TerminalAttachCommand{AttachId: target.id, ChatId: target.chat}, out)
			h.client.handleClientMessage(t.Context(), &pb.TerminalClientMessage{Msg: &pb.TerminalClientMessage_Input{Input: &pb.TerminalInputCommand{AttachId: target.id, Data: []byte("hello")}}}, out, nil)
		}
		defer h.client.closeAllAttaches()
		if h.factory.get("a").inputCount() != 1 || h.factory.get("b").inputCount() != 0 {
			t.Fatal("input did not respect repo grants")
		}
		policy.repos["a"] = config.CloudControlReadOnly
		h.client.handleClientMessage(t.Context(), &pb.TerminalClientMessage{Msg: &pb.TerminalClientMessage_Resize{Resize: &pb.TerminalResizeCommand{AttachId: "a", Cols: 100, Rows: 50}}}, out, nil)
		synctest.Wait()
		if !h.factory.get("a").closed.Load() {
			t.Fatal("downgrade left writable tmux client alive")
		}
		if msg := <-out; msg.GetExited().GetReason() != "cloud control changed for this repo" {
			t.Fatalf("downgrade exit = %v", msg)
		}
		if h.factory.get("b").closed.Load() {
			t.Fatal("downgrade killed another repo")
		}
	})
}

func TestTerminalWriterRejectsRecordedDowngrade(t *testing.T) {
	h := newTestHarness(t)
	h.client.cloudControl = fixedCloudControl(config.CloudControlFull)
	state := &activeAttach{id: "viewer", writable: true}
	h.client.attaches[state.id] = state
	state.policyRevoked.Store(true)

	refused := make(map[string]string)
	data := &pb.TerminalDataChunk{AttachId: state.id, Data: []byte("queued")}
	if !h.client.refuseTerminalData(t.Context(), data, refused) {
		t.Fatal("writer forwarded output after the writable downgrade was recorded")
	}
	if refused[state.id] != "cloud control changed for this repo" {
		t.Fatalf("writer refusal = %q", refused[state.id])
	}
}

func TestTerminalRepoReadOnlyDoesNotWake(t *testing.T) {
	h := newTestHarness(t)
	h.client.targetObjects = terminalRepoTargets()
	h.client.cloudControl = targetPolicy{machine: config.CloudControlFull, repos: map[string]config.CloudControlMode{"a": config.CloudControlReadOnly}}
	out := make(chan *pb.TerminalServerMessage, 8)
	h.client.handleAttach(t.Context(), &pb.TerminalAttachCommand{AttachId: "view", ChatId: "claude-1", Cols: 80, Rows: 24}, out)
	cfg := h.factory.lastConfig()
	if !cfg.ReadOnly || cfg.Cols != 0 || cfg.Rows != 0 {
		t.Fatalf("attach config = %+v", cfg)
	}
	h.client.handleResize(&pb.TerminalResizeCommand{AttachId: "view", Cols: 100, Rows: 50})
	if len(h.factory.get("view").resizes) != 0 {
		t.Fatal("read-only resize reached tmux")
	}
	h.client.closeAllAttaches()
	h.chats.rows["claude-1"] = chatRow{}
	h.client.handleAttach(t.Context(), &pb.TerminalAttachCommand{AttachId: "sleep", ChatId: "claude-1"}, out)
	if msg := <-out; msg.GetExited().GetReason() != "cloud control is read-only; cannot wake chat" {
		t.Fatalf("wake refusal = %v", msg)
	}
}

func TestTerminalRepoUploadDowngrade(t *testing.T) {
	manager, dir := newRealManager(t, &recordingChatSender{})
	defer manager.CancelAll()
	h := newUploadHarness(t, manager)
	policy := targetPolicy{machine: config.CloudControlReadOnly, repos: map[string]config.CloudControlMode{"a": config.CloudControlFull}}
	h.client.cloudControl = policy
	h.client.attaches["a"] = &activeAttach{id: "a", repoID: "a", chatID: "claude-1", attach: newFakeAttach("a")}
	out := make(chan *pb.TerminalServerMessage, 8)
	h.client.handleUploadStart(t.Context(), &pb.TerminalUploadStart{AttachId: "a", UploadId: "u", Filename: "file.txt", SizeBytes: 1}, out)
	h.client.handleUploadChunk(t.Context(), &pb.TerminalUploadChunk{AttachId: "a", UploadId: "u", Data: []byte("x")}, out)
	<-out
	policy.repos["a"] = config.CloudControlReadOnly
	sess := newTerminalStreamSession()
	h.client.handleUploadFinish(t.Context(), &pb.TerminalUploadFinish{AttachId: "a", UploadId: "u"}, out, sess)
	sess.uploadWG.Wait()
	if msg := <-out; msg.GetUploadResult().GetIsOk() || msg.GetUploadResult().GetErrorMessage() != "upload: cloud control is read-only on this machine" {
		t.Fatalf("upload refusal = %v", msg)
	}
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 0 {
		t.Fatalf("staged bytes retained: %v %v", entries, err)
	}
}

func TestSessionRepoRevocationIsIsolated(t *testing.T) {
	policy := targetPolicy{machine: config.CloudControlFull, repos: map[string]config.CloudControlMode{"a": config.CloudControlPrivate}}
	client := NewStreamClient(StreamClientConfig{CloudControl: policy, TargetObjects: terminalRepoTargets()})
	ctxA, cancelA := context.WithCancel(t.Context())
	defer cancelA()
	ctxB, cancelB := context.WithCancel(t.Context())
	defer cancelB()
	client.sessionAttaches = map[string]*sessionAttachState{"a": {cancel: cancelA, sessionID: "sa"}, "b": {cancel: cancelB, sessionID: "sb"}}
	if client.allowSessionAttach(t.Context(), "a", "sa") || !client.allowSessionAttach(t.Context(), "b", "sb") {
		t.Fatal("session attach policy crossed repo boundaries")
	}
	if ctxA.Err() == nil || ctxB.Err() != nil {
		t.Fatal("session cancellation crossed repo boundaries")
	}
}

func TestTerminalRepoPrivateOutputIsIsolated(t *testing.T) {
	h := newTestHarness(t)
	h.client.cloudControl = targetPolicy{machine: config.CloudControlFull, repos: map[string]config.CloudControlMode{"a": config.CloudControlPrivate}}
	h.client.attaches["a"] = &activeAttach{id: "a", repoID: "a", attach: newFakeAttach("a")}
	h.client.attaches["b"] = &activeAttach{id: "b", repoID: "b", attach: newFakeAttach("b")}
	out := make(chan *pb.TerminalServerMessage, 4)
	for _, id := range []string{"a", "missing", "b"} {
		out <- &pb.TerminalServerMessage{Msg: &pb.TerminalServerMessage_Data{Data: &pb.TerminalDataChunk{AttachId: id, Data: []byte("output")}}}
	}
	close(out)
	stream := newFakeTerminalStream()
	h.client.runWriter(t.Context(), stream, out)
	if len(stream.sent) != 1 {
		t.Fatalf("writer sent %d chunks, want one", len(stream.sent))
	}
	if msg := <-stream.sent; msg.GetData().GetAttachId() != "b" {
		t.Fatalf("wrong repo output: %v", msg)
	}
}

func TestTerminalRepoStrictGrantWithMachineReadOnly(t *testing.T) {
	h := newTestHarness(t)
	h.client.targetObjects = terminalRepoTargets()
	checkout := t.TempDir()
	path := filepath.Join(t.TempDir(), "settings.json")
	t.Setenv("BOSS_SETTINGS_PATH", path)
	if err := config.SaveTo(path, config.Settings{CloudControl: "read-only", CloudControlRepos: []config.CloudControlRepoGrant{{Host: "github.com", RepoID: 42, Checkout: checkout, Mode: "full"}}}); err != nil {
		t.Fatal(err)
	}
	policy := NewCloudControlPolicy(zerolog.Nop())
	policy.RepoIdentities = targetIdentityFixture{repoID: "a", identity: config.RepoIdentity{Host: "github.com", ProviderRepoID: 42, IsIDVerified: true, LocalPath: checkout}}
	h.client.cloudControl = policy
	h.chats.rows["chat-b"] = h.chats.rows["claude-1"]
	out := make(chan *pb.TerminalServerMessage, 8)
	for _, target := range []struct{ id, chat string }{{"a", "claude-1"}, {"b", "chat-b"}} {
		h.client.handleAttach(t.Context(), &pb.TerminalAttachCommand{AttachId: target.id, ChatId: target.chat}, out)
		h.client.handleInput(&pb.TerminalInputCommand{AttachId: target.id, Data: []byte("hello")})
	}
	defer h.client.closeAllAttaches()
	if h.factory.get("a").inputCount() != 1 || h.factory.get("b").inputCount() != 0 {
		t.Fatal("strict repo grant did not isolate input")
	}
}

type idleTerminalIdentities struct {
	targetIdentityFixture
	liveReads int
}

func (f *idleTerminalIdentities) Identity(ctx context.Context, id string) (config.RepoIdentity, error) {
	f.liveReads++
	return f.targetIdentityFixture.Identity(ctx, id)
}

func (f *idleTerminalIdentities) LastVerifiedIdentity(ctx context.Context, id string) (config.RepoIdentity, error) {
	return f.targetIdentityFixture.Identity(ctx, id)
}

func TestTerminalIdlePolicyUsesCachedIdentity(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")
	t.Setenv("BOSS_SETTINGS_PATH", path)
	checkout := t.TempDir()
	settings := config.Settings{
		CloudControl: "private",
		CloudControlRepos: []config.CloudControlRepoGrant{
			{Host: "github.com", RepoID: 42, Checkout: checkout, Mode: "read-only"},
		},
	}
	if err := config.SaveTo(path, settings); err != nil {
		t.Fatal(err)
	}
	identities := &idleTerminalIdentities{targetIdentityFixture: targetIdentityFixture{
		repoID:   "a",
		identity: config.RepoIdentity{Host: "github.com", ProviderRepoID: 42, IsIDVerified: true, LocalPath: checkout},
	}}
	policy := NewCloudControlPolicy(zerolog.Nop())
	policy.RepoIdentities = identities
	h := newTestHarness(t)
	h.client.cloudControl = policy
	state := &activeAttach{repoID: "a"}
	if h.client.recheckIdleAttach(t.Context(), state) {
		t.Fatal("idle recheck refused a verified read-only grant")
	}

	settings.CloudControlRepos[0].Mode = "private"
	if err := config.SaveTo(path, settings); err != nil {
		t.Fatal(err)
	}
	if !h.client.recheckIdleAttach(t.Context(), state) {
		t.Fatal("idle recheck missed the local policy change")
	}
	settings.CloudControl = "full"
	settings.CloudControlRepos[0].Checkout = t.TempDir()
	if err := config.SaveTo(path, settings); err != nil {
		t.Fatal(err)
	}
	if !h.client.recheckIdleAttach(t.Context(), &activeAttach{repoID: "a"}) {
		t.Fatal("idle recheck missed an ID-only restrictive grant")
	}
	if identities.liveReads != 0 {
		t.Fatalf("idle rechecks made %d live identity reads", identities.liveReads)
	}
}

func TestTerminalUpload_TeardownWaitsOnlyForItsOwnAttemptsDeliveries(t *testing.T) {
	t.Parallel()
	wedged := newWedgedTerminalStream()
	stream := &connClosableWedgedStream{wedgedTerminalStream: wedged, closeConn: func() error { return nil }}
	client, clock, health, _ := boundedTeardownClient(t, &fixedTerminalOpener{stream: stream})
	gate := make(chan struct{})
	stub := &stubUploadManager{finishGate: gate}
	client.uploads = stub
	tmuxName := "boss-upload-test"
	client.chats = &fakeChatLookup{rows: map[string]chatRow{"chat": {TmuxSessionName: &tmuxName}}}
	wedged.recv <- &pb.TerminalClientMessage{Msg: &pb.TerminalClientMessage_Attach{Attach: &pb.TerminalAttachCommand{AttachId: "att-1", ChatId: "chat"}}}
	// Release delivery before cleanup joins the second attempt on a red run.
	defer close(gate)
	wedged.recv <- &pb.TerminalClientMessage{Msg: &pb.TerminalClientMessage_UploadFinish{UploadFinish: &pb.TerminalUploadFinish{AttachId: "att-1", UploadId: "up-1"}}}
	first := startWedgedOpenStream(t, client, clock, health, wedged)
	waitForN(t, "first delivery parked", 2*time.Second, func() bool { return stub.finishCount() == 1 })
	abandonTerminalAttempt(t, clock, first)
	_, second, cancel := startNextTerminalAttempt(t, client)
	cancel()
	if err := receiveOpenStreamErr(t, second, "while first delivery is still parked"); !errors.Is(err, context.Canceled) {
		t.Fatalf("second attempt: %v", err)
	}
}
