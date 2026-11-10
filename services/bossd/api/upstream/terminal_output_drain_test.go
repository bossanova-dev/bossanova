package upstream

import (
	"crypto/ecdh"
	"crypto/ed25519"
	"crypto/rand"
	"testing"
	"testing/synctest"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/kit/config"
)

func TestTerminalOutputDrainsAfterPumpExit(t *testing.T) {
	for _, mode := range []config.CloudControlMode{config.CloudControlFull, config.CloudControlReadOnly, config.CloudControlPrivate} {
		t.Run(string(mode), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				h := newTestHarness(t)
				policy := targetPolicy{machine: config.CloudControlReadOnly, repos: map[string]config.CloudControlMode{"a": config.CloudControlFull}}
				h.client.targetObjects = terminalRepoTargets()
				h.client.cloudControl = policy
				out := make(chan *pb.TerminalServerMessage, 8)
				h.client.handleAttach(t.Context(), &pb.TerminalAttachCommand{AttachId: "tail", ChatId: "claude-1"}, out)
				defer h.client.closeAllAttaches()
				attach := h.factory.get("tail")
				state := h.client.lookupAttach("tail")
				for _, text := range []string{"last line\n", "prompt"} {
					attach.output <- &pb.TerminalDataChunk{AttachId: "tail", Data: []byte(text)}
				}
				if err := attach.Close(); err != nil {
					t.Fatal(err)
				}
				synctest.Wait()
				select {
				case <-state.done:
				default:
					t.Fatal("output pump did not exit before the writer started")
				}
				if h.client.lookupAttach("tail") != nil {
					t.Fatal("finished attach still accepts control or uploads")
				}
				policy.repos["a"] = mode
				close(out)
				stream := newFakeTerminalStream()
				h.client.runWriter(t.Context(), stream, out)
				var output string
				var exit *pb.TerminalAttachExited
				for len(stream.sent) > 0 {
					msg := <-stream.sent
					if data := msg.GetData(); data != nil {
						output += string(data.GetData())
					}
					if event := msg.GetExited(); event != nil {
						exit = event
					}
				}
				if mode == config.CloudControlFull && output != "last line\nprompt" {
					t.Fatalf("final output = %q", output)
				}
				if mode != config.CloudControlFull && output != "" {
					t.Fatalf("downgrade leaked queued output: %q", output)
				}
				if exit == nil || exit.GetAttachId() != "tail" {
					t.Fatalf("terminal exit = %v", exit)
				}
				h.client.mu.Lock()
				remaining := len(h.client.attaches)
				h.client.mu.Unlock()
				if remaining != 0 {
					t.Fatal("writer retained finished attach state")
				}
			})
		})
	}
}

func TestTerminalSealedOutputDrainsWithLiveApproval(t *testing.T) {
	for _, revoke := range []bool{false, true} {
		t.Run(map[bool]string{false: "approved", true: "revoked"}[revoke], func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				h := newTestHarness(t)
				viewer, err := ecdh.X25519().GenerateKey(rand.Reader)
				if err != nil {
					t.Fatal(err)
				}
				_, identity, err := ed25519.GenerateKey(rand.Reader)
				if err != nil {
					t.Fatal(err)
				}
				approvals := &terminalViewerApprovals{public: viewer.PublicKey().Bytes(), revoked: make(chan struct{})}
				h.client.sealing = TerminalSealingConfig{DaemonID: "daemon", IdentityKey: identity, Approvals: approvals}
				out := make(chan *pb.TerminalServerMessage, 8)
				h.client.handleAttach(t.Context(), &pb.TerminalAttachCommand{AttachId: "sealed-tail", ChatId: "claude-1", ViewerDeviceId: "viewer"}, out)
				defer h.client.closeAllAttaches()
				<-out // The sealed key precedes output.
				state := h.client.lookupAttach("sealed-tail")
				attach := h.factory.get("sealed-tail")
				attach.output <- &pb.TerminalDataChunk{AttachId: "sealed-tail", Data: []byte("tail")}
				if err := attach.Close(); err != nil {
					t.Fatal(err)
				}
				synctest.Wait()
				if revoke {
					close(approvals.revoked)
				}
				close(out)
				stream := newFakeTerminalStream()
				h.client.runWriter(t.Context(), stream, out)
				first := <-stream.sent
				if revoke {
					if first.GetExited().GetReason() != viewerNotApproved || len(stream.sent) != 0 {
						t.Fatalf("revoked viewer received queued output: %v", first)
					}
				} else if !first.GetData().GetIsSealed() || (<-stream.sent).GetExited() == nil {
					t.Fatal("approved viewer lost sealed tail or exit")
				}
				if state.seal.key != nil || state.seal.unsubscribe != nil {
					t.Fatal("writer retained encryption state after exit")
				}
			})
		})
	}
}
