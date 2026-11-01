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
	var out []byte
	err = withPrivateFile("boss-send-back-*.txt", pin.Note, func(path string) error {
		args = append(args[:len(args)-1], "--head="+pin.Head, "--note-file="+path, "--json")
		var runErr error
		out, runErr = b.run(ctx, t.Dir, args)
		return runErr
	})
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

func withPrivateFile(prefix, text string, fn func(path string) error) error {
	file, err := os.CreateTemp("", prefix)
	if err != nil {
		return fmt.Errorf("create operator text file: %w", err)
	}
	defer func() { _ = os.Remove(file.Name()) }()
	if _, err := file.WriteString(text); err != nil {
		_ = file.Close()
		return fmt.Errorf("write operator text file: %w", err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("close operator text file: %w", err)
	}
	return fn(file.Name())
}
