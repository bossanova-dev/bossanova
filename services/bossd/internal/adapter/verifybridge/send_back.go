package verifybridge

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/recurser/bossd/internal/domain"
)

// SendBack posts an operator note through verify's ordinary blocked verdict.
// The note never enters argv: a private temporary file carries its bytes.
func (b *Bridge) SendBack(ctx context.Context, t Target, pin domain.VerifySendBack) (domain.VerifySendBackResult, error) {
	args, err := verbArgs("send-back", t)
	if err != nil {
		return domain.VerifySendBackResult{}, err
	}
	if !shaPattern.MatchString(pin.Head) {
		return domain.VerifySendBackResult{}, fmt.Errorf("%w: send-back head must be a 40-hex SHA", ErrInvalidTarget)
	}
	note, err := os.CreateTemp("", "boss-send-back-*.txt")
	if err != nil {
		return domain.VerifySendBackResult{}, fmt.Errorf("create operator note: %w", err)
	}
	defer func() { _ = os.Remove(note.Name()) }()
	if _, err := note.WriteString(pin.Note); err != nil {
		_ = note.Close()
		return domain.VerifySendBackResult{}, fmt.Errorf("write operator note: %w", err)
	}
	if err := note.Close(); err != nil {
		return domain.VerifySendBackResult{}, fmt.Errorf("close operator note: %w", err)
	}
	args = append(args[:len(args)-1], "--head="+pin.Head, "--note-file="+note.Name(), "--json")
	out, err := b.run(ctx, t.Dir, args)
	if err != nil {
		return domain.VerifySendBackResult{}, err
	}
	var wire struct {
		domain.VerifySendBackResult
		SentBack *bool `json:"sentBack"`
	}
	if err := decode(out, &wire); err != nil {
		return domain.VerifySendBackResult{}, err
	}
	if wire.SentBack == nil {
		return domain.VerifySendBackResult{}, fmt.Errorf("%w: send-back printed no sentBack", ErrShape)
	}
	r := wire.VerifySendBackResult
	r.SentBack = *wire.SentBack
	switch {
	case r.SentBack && (r.Head == "" || r.CommentURL == "" || !strings.EqualFold(r.Head, pin.Head)):
		return domain.VerifySendBackResult{}, fmt.Errorf("%w: send-back success missing pinned head or comment URL", ErrShape)
	case !r.SentBack && r.Reason == "":
		return domain.VerifySendBackResult{}, fmt.Errorf("%w: send-back refused without a reason", ErrShape)
	}
	return r, nil
}
