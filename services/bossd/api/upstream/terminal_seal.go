package upstream

import (
	"context"
	"crypto/ed25519"
	"errors"
	"sync"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossd/internal/domain"
	"github.com/recurser/bossd/internal/streamseal"
)

const (
	viewerNotApproved       = "viewer device is not approved on this daemon"
	terminalSealUnavailable = "terminal encryption is unavailable on this daemon"
	terminalFrameInvalid    = "terminal encryption frame is invalid"
)

// ViewerApprovalReader supplies locally pinned keys and one-shot revocation signals.
type ViewerApprovalReader interface {
	Get(context.Context, string) (*domain.ViewerDeviceApproval, error)
	Subscribe(string) (<-chan struct{}, func())
}

// TerminalSealingConfig enables opt-in sealing independently of the cloud relay.
type TerminalSealingConfig struct {
	DaemonID    string
	IdentityKey ed25519.PrivateKey
	Approvals   ViewerApprovalReader
}

// IsAvailable reports whether local identity and approval dependencies can seal attaches.
func (s TerminalSealingConfig) IsAvailable() bool {
	return s.DaemonID != "" && len(s.IdentityKey) == ed25519.PrivateKeySize && s.Approvals != nil
}

type terminalSeal struct {
	mu          sync.Mutex
	key         []byte
	sealer      *streamseal.FrameSealer
	opener      *streamseal.FrameOpener
	revoked     <-chan struct{}
	unsubscribe func()
	failed      chan struct{}
	reason      string
}

func (c *TerminalStreamClient) prepareSeal(ctx context.Context, cmd *pb.TerminalAttachCommand) (*terminalSeal, *pb.TerminalSealedKey, string) {
	if cmd.GetViewerDeviceId() == "" {
		return nil, nil, ""
	}
	if !c.sealing.IsAvailable() {
		return nil, nil, terminalSealUnavailable
	}
	revoked, unsubscribe := c.sealing.Approvals.Subscribe(cmd.ViewerDeviceId)
	approval, err := c.sealing.Approvals.Get(ctx, cmd.ViewerDeviceId)
	if err != nil || approval == nil {
		unsubscribe()
		return nil, nil, viewerNotApproved
	}
	key, err := streamseal.NewAttachKey()
	if err != nil {
		clear(key)
		unsubscribe()
		return nil, nil, terminalSealUnavailable
	}
	s := &terminalSeal{key: key, revoked: revoked, unsubscribe: unsubscribe, failed: make(chan struct{})}
	ac := streamseal.AttachContext{DaemonID: c.sealing.DaemonID, ViewerDeviceID: cmd.ViewerDeviceId, AttachID: cmd.AttachId, ChatID: cmd.ChatId}
	box, err := streamseal.SealAttachKey(approval.PublicKey, key, ac, c.sealing.IdentityKey)
	if err != nil {
		s.destroy()
		return nil, nil, terminalSealUnavailable
	}
	s.sealer, err = streamseal.NewFrameSealer(key, streamseal.DaemonToViewer, cmd.AttachId)
	if err == nil {
		s.opener, err = streamseal.NewFrameOpener(key, streamseal.ViewerToDaemon, cmd.AttachId)
	}
	if err != nil {
		s.destroy()
		return nil, nil, terminalSealUnavailable
	}
	select {
	case <-revoked:
		s.destroy()
		return nil, nil, viewerNotApproved
	default:
	}
	return s, &pb.TerminalSealedKey{AttachId: cmd.AttachId, Box: box.Box, Signature: box.Signature}, ""
}

func (s *terminalSeal) destroy() {
	s.mu.Lock()
	defer s.mu.Unlock()
	clear(s.key)
	s.key = nil
	s.sealer = nil
	s.opener = nil
	if s.unsubscribe != nil {
		s.unsubscribe()
		s.unsubscribe = nil
	}
}

func (s *terminalSeal) fail(reason string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.reason == "" {
		s.reason = reason
		close(s.failed)
	}
}

func (s *terminalSeal) seal(chunk *pb.TerminalDataChunk) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	select {
	case <-s.revoked:
		return errors.New(viewerNotApproved)
	default:
	}
	if s.reason != "" || s.sealer == nil {
		return errors.New(terminalFrameInvalid)
	}
	counter, data, err := s.sealer.Seal(chunk.Data)
	if err != nil {
		return err
	}
	chunk.Data = data
	chunk.IsSealed = true
	chunk.FrameCounter = counter
	return nil
}

func (s *terminalSeal) input(cmd *pb.TerminalInputCommand) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	select {
	case <-s.revoked:
		return nil, errors.New(viewerNotApproved)
	default:
	}
	if !cmd.IsSealed || s.reason != "" || s.opener == nil {
		return nil, errors.New(terminalFrameInvalid)
	}
	return s.opener.Open(cmd.FrameCounter, cmd.Data)
}

func (c *TerminalStreamClient) refuseSealedUpload(ctx context.Context, outbound chan<- *pb.TerminalServerMessage, attachID, uploadID string) bool {
	state := c.lookupAttach(attachID)
	if state == nil || state.seal == nil {
		return false
	}
	c.failUpload(ctx, outbound, attachID, uploadID, "uploads are unavailable on an encrypted terminal", false)
	return true
}

func (s *terminalSeal) failureReason() string {
	select {
	case <-s.revoked:
		return viewerNotApproved
	default:
		return terminalFrameInvalid
	}
}

func (s *terminalSeal) stopReason() string {
	select {
	case <-s.revoked:
		return viewerNotApproved
	default:
	}
	select {
	case <-s.failed:
		return terminalFrameInvalid
	default:
		return ""
	}
}

// publishSealedKey gates the pump so even an immediately chatty PTY cannot
// precede its key. Revocation interrupts a congested outbound queue.
func (c *TerminalStreamClient) publishSealedKey(ctx context.Context, state *activeAttach, outbound chan<- *pb.TerminalServerMessage, key *pb.TerminalSealedKey) bool {
	if key == nil {
		return true
	}
	select {
	case outbound <- &pb.TerminalServerMessage{Msg: &pb.TerminalServerMessage_SealedKey{SealedKey: key}}:
		return true
	case <-ctx.Done():
	case <-state.seal.revoked:
	}
	_ = state.attach.Close()
	state.cancel()
	return false
}
