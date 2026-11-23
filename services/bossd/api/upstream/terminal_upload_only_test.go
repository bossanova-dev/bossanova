package upstream

import (
	"os"
	"testing"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/kit/config"
)

// attachUploadOnly opens a chat reply box's upload channel on uploadAttachID.
func attachUploadOnly(stream *fakeTerminalStream) {
	stream.recv <- &pb.TerminalClientMessage{
		Msg: &pb.TerminalClientMessage_Attach{Attach: &pb.TerminalAttachCommand{
			AttachId: uploadAttachID, ChatId: "claude-1", IsUploadOnly: true, ActingUserId: "user-alice",
		}},
	}
}

// uploadWhole sends one file in a single chunk and finishes it.
func uploadWhole(t *testing.T, stream *fakeTerminalStream, uploadID string, payload []byte) *pb.TerminalUploadResult {
	t.Helper()
	stream.recv <- &pb.TerminalClientMessage{Msg: &pb.TerminalClientMessage_UploadStart{UploadStart: &pb.TerminalUploadStart{
		AttachId: uploadAttachID, UploadId: uploadID, Filename: "shot.png", SizeBytes: uint64(len(payload)),
	}}}
	stream.recv <- &pb.TerminalClientMessage{Msg: &pb.TerminalClientMessage_UploadChunk{UploadChunk: &pb.TerminalUploadChunk{
		AttachId: uploadAttachID, UploadId: uploadID, Seq: 0, Data: payload,
	}}}
	if ack := waitForUploadFrame(t, stream).GetUploadAck(); ack == nil {
		t.Fatal("expected an ack before finish")
	}
	stream.recv <- &pb.TerminalClientMessage{Msg: &pb.TerminalClientMessage_UploadFinish{UploadFinish: &pb.TerminalUploadFinish{
		AttachId: uploadAttachID, UploadId: uploadID,
	}}}
	result := waitForUploadFrame(t, stream).GetUploadResult()
	if result == nil {
		t.Fatal("expected an upload result")
	}
	return result
}

// waitForExit drains sent until the attach's exit frame arrives; the test's
// own deadline bounds the wait.
func waitForExit(t *testing.T, s *fakeTerminalStream) *pb.TerminalAttachExited {
	t.Helper()
	for {
		select {
		case m := <-s.sent:
			if exit := m.GetExited(); exit != nil {
				return exit
			}
		case <-t.Context().Done():
			t.Fatal("no exit frame before the test ended")
		}
	}
}

func TestUploadOnlyAttach_HoldsTheFileForASendAndSpawnsNoTerminal(t *testing.T) {
	t.Parallel()
	sender := &recordingChatSender{}
	mgr, dir := newRealManager(t, sender)
	h := newUploadHarness(t, mgr)
	stop := runClient(t, h.client)
	defer stop()
	stream := waitForStream(t, h.opener)

	attachUploadOnly(stream)
	result := uploadWhole(t, stream, "up-1", []byte("png bytes"))

	if !result.GetIsOk() {
		t.Fatalf("upload failed: %q", result.GetErrorMessage())
	}
	if h.factory.get(uploadAttachID) != nil {
		t.Fatal("an upload-only attach spawned a terminal attach")
	}
	if got := sender.sent(); len(got) != 0 {
		t.Fatalf("a deferred upload reached the composer: %q", got)
	}
	paths, err := mgr.Attachments("claude-1", "user-alice", []string{"up-1"})
	if err != nil {
		t.Fatalf("the finished upload does not resolve for its chat: %v", err)
	}
	if body, err := os.ReadFile(paths[0]); err != nil || string(body) != "png bytes" {
		t.Fatalf("held file = %q, %v (dir %s)", body, err, dir)
	}

	stream.recv <- &pb.TerminalClientMessage{Msg: &pb.TerminalClientMessage_Close{Close: &pb.TerminalCloseCommand{AttachId: uploadAttachID}}}
	waitForExit(t, stream)
	if _, err := mgr.Attachments("claude-1", "user-alice", []string{"up-1"}); err != nil {
		t.Fatalf("closing the upload channel dropped a held file: %v", err)
	}
}

func TestUploadOnlyAttach_RefusedWithoutFullCloudControl(t *testing.T) {
	t.Parallel()
	for mode, reason := range map[config.CloudControlMode]string{
		config.CloudControlReadOnly: uploadOnlyReadOnlyReason,
		config.CloudControlPrivate:  terminalPrivateReason,
	} {
		mgr, _ := newRealManager(t, &recordingChatSender{})
		h := newUploadHarness(t, mgr)
		h.client.cloudControl = fixedCloudControl(mode)
		stop := runClient(t, h.client)
		stream := waitForStream(t, h.opener)

		attachUploadOnly(stream)
		if exit := waitForExit(t, stream); exit.GetReason() != reason {
			t.Fatalf("%s: exit reason = %q, want %q", mode, exit.GetReason(), reason)
		}
		stop()
	}
}

func TestUploadOnlyAttach_RefusedWithoutAnActingUser(t *testing.T) {
	t.Parallel()
	mgr, _ := newRealManager(t, &recordingChatSender{})
	h := newUploadHarness(t, mgr)
	stop := runClient(t, h.client)
	defer stop()
	stream := waitForStream(t, h.opener)

	stream.recv <- &pb.TerminalClientMessage{
		Msg: &pb.TerminalClientMessage_Attach{Attach: &pb.TerminalAttachCommand{
			AttachId: uploadAttachID, ChatId: "claude-1", IsUploadOnly: true,
		}},
	}
	if exit := waitForExit(t, stream); exit.GetReason() != "upload attach: no acting user" {
		t.Fatalf("exit reason = %q, want the missing acting user", exit.GetReason())
	}
}
