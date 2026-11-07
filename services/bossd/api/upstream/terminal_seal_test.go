package upstream

import (
	"bytes"
	"context"
	"crypto/ecdh"
	"crypto/ed25519"
	"crypto/rand"
	"testing"

	"github.com/recurser/bossalib/kit/safego"
	"github.com/rs/zerolog"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossd/internal/domain"
	"github.com/recurser/bossd/internal/streamseal"
)

type terminalViewerApprovals struct {
	public  []byte
	revoked chan struct{}
}

func (v *terminalViewerApprovals) Get(context.Context, string) (*domain.ViewerDeviceApproval, error) {
	if v.public == nil {
		return nil, domain.ErrNotFound
	}
	return &domain.ViewerDeviceApproval{PublicKey: v.public}, nil
}
func (v *terminalViewerApprovals) Subscribe(string) (<-chan struct{}, func()) {
	return v.revoked, func() {}
}

func TestTerminalSealedRoundTrip_TerminalEncryption_AC219301_AC219302_AC219303(t *testing.T) {
	ctx := context.Background()
	h := newTestHarness(t)
	viewer, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	pub, identity, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	approvals := &terminalViewerApprovals{public: viewer.PublicKey().Bytes(), revoked: make(chan struct{})}
	h.client.sealing = TerminalSealingConfig{DaemonID: "daemon", IdentityKey: identity, Approvals: approvals}
	out := make(chan *pb.TerminalServerMessage, 16)
	h.client.handleAttach(ctx, &pb.TerminalAttachCommand{AttachId: "sealed", ChatId: "claude-1", ViewerDeviceId: "viewer"}, out)
	defer h.client.closeAllAttaches()
	first := <-out
	if first.GetSealedKey() == nil {
		t.Fatalf("expected sealed key first: %v", first)
	}
	key, err := streamseal.OpenAttachKey(viewer, pub, streamseal.AttachContext{DaemonID: "daemon", ViewerDeviceID: "viewer", AttachID: "sealed", ChatID: "claude-1"}, streamseal.SealedAttachKey{Box: first.GetSealedKey().Box, Signature: first.GetSealedKey().Signature})
	if err != nil {
		t.Fatal(err)
	}
	a := h.factory.get("sealed")
	a.output <- &pb.TerminalDataChunk{AttachId: "sealed", Data: []byte("secret PTY marker"), Seq: 9, Lost: true}
	msg := <-out
	chunk := msg.GetData()
	if chunk == nil || !chunk.IsSealed || bytes.Contains(chunk.Data, []byte("secret PTY marker")) {
		t.Fatalf("plaintext output: %v", msg)
	}
	opener, err := streamseal.NewFrameOpener(key, streamseal.DaemonToViewer, "sealed")
	if err != nil {
		t.Fatal(err)
	}
	plain, err := opener.Open(chunk.FrameCounter, chunk.Data)
	if err != nil || string(plain) != "secret PTY marker" || chunk.Seq != 9 || !chunk.Lost {
		t.Fatalf("open output: %q %v", plain, err)
	}
	sealer, err := streamseal.NewFrameSealer(key, streamseal.ViewerToDaemon, "sealed")
	if err != nil {
		t.Fatal(err)
	}
	counter, data, err := sealer.Seal([]byte("typed"))
	if err != nil {
		t.Fatal(err)
	}
	h.client.handleInput(&pb.TerminalInputCommand{AttachId: "sealed", Data: data, IsSealed: true, FrameCounter: counter})
	if string(a.lastInput()) != "typed" {
		t.Fatal("sealed input not delivered")
	}
	h.client.handleUploadStart(ctx, &pb.TerminalUploadStart{AttachId: "sealed", UploadId: "upload"}, out)
	if result := (<-out).GetUploadResult(); result == nil || result.IsOk || result.CanRetry || result.ErrorMessage != "uploads are unavailable on an encrypted terminal" {
		t.Fatalf("upload accepted: %v", result)
	}
	close(approvals.revoked)
	if exit := (<-out).GetExited(); exit == nil || exit.Reason != "viewer device is not approved on this daemon" {
		t.Fatalf("revocation: %v", exit)
	}
}

func TestTerminalSealedRefusals_TerminalEncryption_AC219302(t *testing.T) {
	for _, tc := range []struct {
		name               string
		approved, identity bool
		reason             string
	}{
		{"unapproved", false, true, viewerNotApproved},
		{"identity missing", true, false, terminalSealUnavailable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newTestHarness(t)
			viewer, err := ecdh.X25519().GenerateKey(rand.Reader)
			if err != nil {
				t.Fatal(err)
			}
			approvals := &terminalViewerApprovals{revoked: make(chan struct{})}
			if tc.approved {
				approvals.public = viewer.PublicKey().Bytes()
			}
			var identity ed25519.PrivateKey
			if tc.identity {
				_, identity, err = ed25519.GenerateKey(rand.Reader)
				if err != nil {
					t.Fatal(err)
				}
			}
			h.client.sealing = TerminalSealingConfig{DaemonID: "daemon", IdentityKey: identity, Approvals: approvals}
			out := make(chan *pb.TerminalServerMessage, 2)
			h.client.handleAttach(context.Background(), &pb.TerminalAttachCommand{AttachId: "sealed", ChatId: "claude-1", ViewerDeviceId: "viewer"}, out)
			exit := (<-out).GetExited()
			if exit == nil || exit.Reason != tc.reason || h.factory.configCount() != 0 {
				t.Fatalf("refusal: %v; spawned %d", exit, h.factory.configCount())
			}
		})
	}
}

func TestTerminalSealedInvalidInput_TerminalEncryption_AC219301(t *testing.T) {
	for _, kind := range []string{"plaintext", "replay", "tamper", "gap"} {
		t.Run(kind, func(t *testing.T) {
			h := newTestHarness(t)
			viewer, err := ecdh.X25519().GenerateKey(rand.Reader)
			if err != nil {
				t.Fatal(err)
			}
			pub, identity, err := ed25519.GenerateKey(rand.Reader)
			if err != nil {
				t.Fatal(err)
			}
			h.client.sealing = TerminalSealingConfig{DaemonID: "daemon", IdentityKey: identity, Approvals: &terminalViewerApprovals{public: viewer.PublicKey().Bytes(), revoked: make(chan struct{})}}
			out := make(chan *pb.TerminalServerMessage, 8)
			h.client.handleAttach(context.Background(), &pb.TerminalAttachCommand{AttachId: "sealed", ChatId: "claude-1", ViewerDeviceId: "viewer"}, out)
			defer h.client.closeAllAttaches()
			box := (<-out).GetSealedKey()
			key, err := streamseal.OpenAttachKey(viewer, pub, streamseal.AttachContext{DaemonID: "daemon", ViewerDeviceID: "viewer", AttachID: "sealed", ChatID: "claude-1"}, streamseal.SealedAttachKey{Box: box.Box, Signature: box.Signature})
			if err != nil {
				t.Fatal(err)
			}
			sealer, err := streamseal.NewFrameSealer(key, streamseal.ViewerToDaemon, "sealed")
			if err != nil {
				t.Fatal(err)
			}
			counter, data, err := sealer.Seal([]byte("input"))
			if err != nil {
				t.Fatal(err)
			}
			cmd := &pb.TerminalInputCommand{AttachId: "sealed", IsSealed: true, FrameCounter: counter, Data: data}
			wantInputs := 0
			switch kind {
			case "plaintext":
				cmd.IsSealed = false
				cmd.Data = []byte("plaintext")
			case "replay":
				h.client.handleInput(cmd)
				wantInputs = 1
			case "tamper":
				cmd.Data[0] ^= 1
			case "gap":
				cmd.FrameCounter++
			}
			state := h.client.lookupAttach("sealed")
			h.client.handleInput(cmd)
			if exit := (<-out).GetExited(); exit == nil || exit.Reason != terminalFrameInvalid {
				t.Fatalf("invalid frame accepted: %v", exit)
			}
			<-state.done
			if a := h.factory.get("sealed"); !a.closed.Load() || a.inputCount() != wantInputs {
				t.Fatalf("input delivered: %d", a.inputCount())
			}
			if state.seal.key != nil || state.seal.opener != nil || state.seal.sealer != nil {
				t.Fatal("key retained after teardown")
			}
		})
	}
}

func TestTerminalSealedPrivate_TerminalEncryption_AC219303(t *testing.T) {
	h := newTestHarness(t)
	h.client.cloudControl = fixedCloudControl("private")
	viewer, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	_, identity, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	h.client.sealing = TerminalSealingConfig{DaemonID: "daemon", IdentityKey: identity, Approvals: &terminalViewerApprovals{public: viewer.PublicKey().Bytes(), revoked: make(chan struct{})}}
	out := make(chan *pb.TerminalServerMessage, 8)
	ctx := context.Background()
	h.client.handleClientMessage(ctx, &pb.TerminalClientMessage{Msg: &pb.TerminalClientMessage_Attach{Attach: &pb.TerminalAttachCommand{AttachId: "sealed", ChatId: "claude-1", ViewerDeviceId: "viewer"}}}, out, &terminalStreamSession{})
	defer h.client.closeAllAttaches()
	if key := (<-out).GetSealedKey(); key == nil {
		t.Fatal("private refused sealed attach")
	}
	if !h.factory.lastConfig().ReadOnly {
		t.Fatal("encryption granted write access")
	}
	a := h.factory.get("sealed")
	a.output <- &pb.TerminalDataChunk{AttachId: "sealed", Data: []byte("private output")}
	chunk := <-out
	if !chunk.GetData().GetIsSealed() {
		t.Fatal("private output not encrypted")
	}
	stream := newFakeTerminalStream()
	writerCtx, cancel := context.WithCancel(ctx)
	done := safego.Go(zerolog.Nop(), func() { h.client.runWriter(writerCtx, stream, out) })
	out <- chunk
	if received := <-stream.sent; !received.GetData().GetIsSealed() {
		t.Fatal("writer refused sealed output")
	}
	cancel()
	<-done
	h.client.handleClientMessage(ctx, &pb.TerminalClientMessage{Msg: &pb.TerminalClientMessage_Attach{Attach: &pb.TerminalAttachCommand{AttachId: "plain", ChatId: "claude-1"}}}, out, &terminalStreamSession{})
	if exit := (<-out).GetExited(); exit == nil || exit.Reason != "cloud control is private on this machine" {
		t.Fatalf("private allowed plaintext: %v", exit)
	}
	h.client.handleClientMessage(ctx, &pb.TerminalClientMessage{Msg: &pb.TerminalClientMessage_UploadChunk{UploadChunk: &pb.TerminalUploadChunk{AttachId: "sealed", UploadId: "upload", Data: []byte("secret")}}}, out, &terminalStreamSession{})
	if result := (<-out).GetUploadResult(); result == nil || result.ErrorMessage != "uploads are unavailable on an encrypted terminal" || result.IsOk || result.CanRetry {
		t.Fatalf("private upload: %v", result)
	}
}

func TestTerminalSealingSnapshot(t *testing.T) {
	for _, supported := range []bool{false, true} {
		client := NewStreamClient(StreamClientConfig{TerminalSealingSupported: supported, Logger: zerolog.Nop()})
		snapshot, err := client.buildSnapshot(context.Background())
		if err != nil || snapshot.IsTerminalSealingSupported != supported {
			t.Fatalf("capability hint: %v %v", snapshot, err)
		}
	}
}

func TestTerminalSealedReadOnlyInput_TerminalEncryption_AC219303(t *testing.T) {
	for _, mode := range []fixedCloudControl{"private", "read-only"} {
		t.Run(string(mode), func(t *testing.T) {
			h := newTestHarness(t)
			h.client.cloudControl = mode
			viewer, err := ecdh.X25519().GenerateKey(rand.Reader)
			if err != nil {
				t.Fatal(err)
			}
			_, identity, err := ed25519.GenerateKey(rand.Reader)
			if err != nil {
				t.Fatal(err)
			}
			h.client.sealing = TerminalSealingConfig{DaemonID: "daemon", IdentityKey: identity, Approvals: &terminalViewerApprovals{public: viewer.PublicKey().Bytes(), revoked: make(chan struct{})}}
			out := make(chan *pb.TerminalServerMessage, 8)
			ctx := context.Background()
			h.client.handleAttach(ctx, &pb.TerminalAttachCommand{AttachId: "sealed", ChatId: "claude-1", ViewerDeviceId: "viewer"}, out)
			defer h.client.closeAllAttaches()
			<-out
			state := h.client.lookupAttach("sealed")
			sealer, err := streamseal.NewFrameSealer(state.seal.key, streamseal.ViewerToDaemon, "sealed")
			if err != nil {
				t.Fatal(err)
			}
			counter, data, err := sealer.Seal([]byte("write forbidden"))
			if err != nil {
				t.Fatal(err)
			}
			msg := &pb.TerminalClientMessage{Msg: &pb.TerminalClientMessage_Input{Input: &pb.TerminalInputCommand{AttachId: "sealed", Data: data, IsSealed: true, FrameCounter: counter}}}
			h.client.handleClientMessage(ctx, msg, out, &terminalStreamSession{})
			if h.factory.get("sealed").inputCount() != 0 || !h.factory.lastConfig().ReadOnly {
				t.Fatal("encryption granted control")
			}
			// A replay must still close a read-only attach: input was authenticated before being dropped.
			h.client.handleClientMessage(ctx, msg, out, &terminalStreamSession{})
			if exit := (<-out).GetExited(); exit == nil || exit.Reason != terminalFrameInvalid {
				t.Fatalf("replay ignored before authentication: %v", exit)
			}
		})
	}
}
