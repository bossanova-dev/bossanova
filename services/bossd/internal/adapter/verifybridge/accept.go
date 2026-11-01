package verifybridge

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/recurser/bossd/internal/domain"
)

// Accept records an operator's acceptance of a pinned review finding.
func (b *Bridge) Accept(ctx context.Context, t Target, pin domain.VerifyAccept) (domain.VerifyAcceptResult, error) {
	args, err := verbArgs("accept", t)
	if err != nil {
		return domain.VerifyAcceptResult{}, err
	}
	if !shaPattern.MatchString(pin.Head) || strings.TrimSpace(pin.Blocker) == "" {
		return domain.VerifyAcceptResult{}, fmt.Errorf("%w: accept needs a 40-hex head and a blocker id", ErrInvalidTarget)
	}
	var out []byte
	err = withPrivateFile("boss-accept-*.txt", pin.Reason, func(path string) error {
		args = append(args[:len(args)-1], "--head="+pin.Head, "--blocker="+pin.Blocker, "--reason-file="+path, "--json")
		var runErr error
		out, runErr = b.run(ctx, t.Dir, args)
		return runErr
	})
	if err != nil {
		return domain.VerifyAcceptResult{}, err
	}
	return decodeAccept(out, t.DryRun, pin)
}

func decodeAccept(out []byte, dryRun bool, pin domain.VerifyAccept) (domain.VerifyAcceptResult, error) {
	var wire struct {
		Accepted *bool  `json:"accepted"`
		Already  bool   `json:"already"`
		DryRun   bool   `json:"dryRun"`
		Reason   string `json:"reason"`
		Detail   string `json:"detail"`
		Head     string `json:"head"`
		Blocker  string `json:"blocker"`
		Ticket   struct {
			Identifier string `json:"identifier"`
			URL        string `json:"url"`
		} `json:"ticket"`
		CommentURL     string            `json:"commentUrl"`
		ExistingTicket bool              `json:"existingTicket"`
		Writes         []json.RawMessage `json:"writes"`
	}
	if err := decode(out, &wire); err != nil {
		if json.Valid(out) {
			return domain.VerifyAcceptResult{}, fmt.Errorf("%w: invalid accept fields: %v", ErrShape, err)
		}
		return domain.VerifyAcceptResult{}, err
	}
	if wire.Accepted == nil {
		return domain.VerifyAcceptResult{}, fmt.Errorf("%w: accept printed no accepted field", ErrShape)
	}
	switch {
	case *wire.Accepted && (!strings.EqualFold(wire.Head, pin.Head) || wire.Blocker != pin.Blocker || wire.Ticket.Identifier == "" || wire.CommentURL == "" || !wire.Already && wire.Ticket.URL == ""):
		return domain.VerifyAcceptResult{}, fmt.Errorf("%w: accept success missing pinned finding, ticket or acceptance record", ErrShape)
	case wire.DryRun && !dryRun:
		return domain.VerifyAcceptResult{}, fmt.Errorf("%w: accept returned an unrequested dry run", ErrShape)
	case wire.DryRun && wire.Writes == nil:
		return domain.VerifyAcceptResult{}, fmt.Errorf("%w: accept dry run missing writes array", ErrShape)
	case !*wire.Accepted && !wire.DryRun && wire.Reason == "":
		return domain.VerifyAcceptResult{}, fmt.Errorf("%w: accept refused without a reason", ErrShape)
	}
	r := domain.VerifyAcceptResult{
		Accepted: *wire.Accepted, Already: wire.Already, DryRun: wire.DryRun, Reason: wire.Reason,
		Detail: wire.Detail, Head: wire.Head, Blocker: wire.Blocker,
		TicketIdentifier: wire.Ticket.Identifier, TicketURL: wire.Ticket.URL,
		CommentURL: wire.CommentURL, ExistingTicket: wire.ExistingTicket,
	}
	for _, raw := range wire.Writes {
		var text string
		if err := json.Unmarshal(raw, &text); err == nil && len(raw) > 0 && raw[0] == '"' {
			r.Writes = append(r.Writes, text)
		} else {
			var compact bytes.Buffer
			if err := json.Compact(&compact, raw); err != nil {
				return domain.VerifyAcceptResult{}, fmt.Errorf("%w: accept write: %v", ErrShape, err)
			}
			r.Writes = append(r.Writes, compact.String())
		}
	}
	return r, nil
}

// Blockers reads verify's standing blockers file. Its special exit codes are
// outcomes only for this verb; every mutating verb continues to fail closed.
func (b *Bridge) Blockers(ctx context.Context, t Target, head string) (domain.VerifyBlockers, error) {
	t.DryRun = false
	args, err := verbArgs("blockers", t)
	if err != nil {
		return domain.VerifyBlockers{}, err
	}
	if head != "" && !shaPattern.MatchString(head) {
		return domain.VerifyBlockers{}, fmt.Errorf("%w: blockers head must be a 40-hex SHA", ErrInvalidTarget)
	}
	dir, err := os.MkdirTemp("", "boss-blockers-*")
	if err != nil {
		return domain.VerifyBlockers{}, fmt.Errorf("create blockers directory: %w", err)
	}
	defer func() { _ = os.RemoveAll(dir) }()
	path := filepath.Join(dir, "blockers.json")
	args = args[:len(args)-1]
	if head != "" {
		args = append(args, "--head="+head)
	}
	args = append(args, "--out="+path)
	out, code, err := b.runAllowing(ctx, t.Dir, args, 1, 3)
	if err != nil {
		return domain.VerifyBlockers{}, err
	}
	var result struct {
		Outcome string `json:"outcome"`
	}
	if err := decode(out, &result); err != nil {
		if code != 0 {
			return domain.VerifyBlockers{}, fmt.Errorf("%w: blockers exited %d without an outcome: %v", ErrNodeExit, code, err)
		}
		return domain.VerifyBlockers{}, fmt.Errorf("%w: invalid blockers outcome: %v", ErrShape, err)
	}
	switch {
	case code == 3 && result.Outcome == "absent", code == 1 && result.Outcome == "unreadable":
		return domain.VerifyBlockers{Outcome: result.Outcome}, nil
	case code == 0 && result.Outcome == "written":
		return readBlockers(path, t.PR, head)
	default:
		return domain.VerifyBlockers{}, fmt.Errorf("%w: blockers outcome %q with exit %d", ErrShape, result.Outcome, code)
	}
}

var reviewIDPattern = regexp.MustCompile(`^review:[a-f0-9]{12}$`)

func readBlockers(path string, pr int, head string) (domain.VerifyBlockers, error) {
	out, err := os.ReadFile(path)
	if err != nil {
		return domain.VerifyBlockers{}, fmt.Errorf("%w: read blockers file: %v", ErrShape, err)
	}
	var file struct {
		Version  int                    `json:"version"`
		PR       int                    `json:"pr"`
		Head     string                 `json:"head"`
		Blockers []domain.VerifyBlocker `json:"blockers"`
	}
	if err := decode(bytes.TrimSpace(out), &file); err != nil {
		return domain.VerifyBlockers{}, fmt.Errorf("%w: invalid blockers file: %v", ErrShape, err)
	}
	if file.Version != 1 || file.PR != pr || !shaPattern.MatchString(file.Head) || head != "" && !strings.EqualFold(file.Head, head) || len(file.Blockers) == 0 {
		return domain.VerifyBlockers{}, fmt.Errorf("%w: blockers file has wrong version, PR, head or blockers", ErrShape)
	}
	ids := make(map[string]bool, len(file.Blockers))
	for _, blocker := range file.Blockers {
		if strings.TrimSpace(blocker.ID) == "" || ids[blocker.ID] {
			return domain.VerifyBlockers{}, fmt.Errorf("%w: blockers file has empty or duplicate id", ErrShape)
		}
		ids[blocker.ID] = true
		switch blocker.Kind {
		case "review-finding":
			if !reviewIDPattern.MatchString(blocker.ID) {
				return domain.VerifyBlockers{}, fmt.Errorf("%w: invalid review id", ErrShape)
			}
		case "operator-note", "check", "conflict", "no-review":
		default:
			return domain.VerifyBlockers{}, fmt.Errorf("%w: unknown blocker kind %q", ErrShape, blocker.Kind)
		}
	}
	return domain.VerifyBlockers{Outcome: "written", Head: file.Head, Blockers: file.Blockers}, nil
}
