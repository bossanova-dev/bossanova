package verifybridge

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/recurser/bossd/internal/domain"
)

// FollowUpScriptRelPath names the follow-up verb in an installed skills tree.
var FollowUpScriptRelPath = filepath.Join("boss-verify", "toolbox", "verify", "verify-followups.mjs")

// FollowUp files selected verify notes on the daemon host. A verb refusal or
// partial failure is a result; execution and malformed output remain errors.
func (b *Bridge) FollowUp(ctx context.Context, t Target, selection domain.VerifyFollowUp) (domain.VerifyFollowUpResult, error) {
	selection, err := selection.Normalize()
	if err != nil {
		return domain.VerifyFollowUpResult{}, err
	}
	args, err := verbArgs("follow-up", t)
	if err != nil {
		return domain.VerifyFollowUpResult{}, err
	}
	args = args[:len(args)-1]
	if selection.All {
		args = append(args, "--all")
	} else {
		for _, id := range selection.VerifyNoteIDs {
			args = append(args, "--note="+id)
		}
	}
	args = append(args, "--json")
	script := b.FollowUpScript
	if script == "" {
		home, _ := os.UserHomeDir()
		script, err = locate(os.Getenv, home, FollowUpScriptRelPath)
		if err != nil {
			return domain.VerifyFollowUpResult{}, err
		}
	}
	out, code, err := b.execute(ctx, t.Dir, script, "verify-followups follow-up", args, 1)
	if err != nil {
		return domain.VerifyFollowUpResult{}, err
	}
	return decodeFollowUp(out, code, t.DryRun)
}

type followUpWire struct {
	OK      *bool              `json:"ok"`
	DryRun  bool               `json:"dryRun"`
	Reason  string             `json:"reason"`
	IDs     []string           `json:"ids"`
	Head    string             `json:"head"`
	Comment string             `json:"comment"`
	Results []followUpNoteWire `json:"results"`
}

type followUpNoteWire struct {
	domain.VerifyNoteFollowUp
	Linked *bool `json:"linked"`
}

func decodeFollowUp(out []byte, code int, dryRun bool) (domain.VerifyFollowUpResult, error) {
	var wire followUpWire
	if err := decode(out, &wire); err != nil {
		return domain.VerifyFollowUpResult{}, err
	}
	bad := func(reason string) (domain.VerifyFollowUpResult, error) {
		return domain.VerifyFollowUpResult{}, fmt.Errorf("%w: follow-up %s: %s", ErrShape, reason, truncate(out))
	}
	if wire.OK == nil {
		return bad("printed no ok field")
	}
	if *wire.OK != (code == 0) {
		return bad("exit status disagrees with ok")
	}
	if wire.DryRun != dryRun && *wire.OK {
		return bad("dry-run disagrees with request")
	}
	switch wire.Comment {
	case "updated", "current", "planned", "failed":
	case "":
		if *wire.OK || wire.Results != nil {
			return bad("printed no comment outcome")
		}
	default:
		return bad("printed an unknown comment outcome")
	}
	r := domain.VerifyFollowUpResult{OK: *wire.OK, DryRun: wire.DryRun, Reason: wire.Reason, UnknownVerifyNoteIDs: wire.IDs, HeadSHA: wire.Head, Comment: wire.Comment}
	hasFailure := wire.Reason != "" || wire.Comment == "failed"
	if r.OK && (wire.Results == nil || !followUpCommentOK(wire.Comment, dryRun)) {
		return bad("success missing results or a successful comment outcome")
	}
	for _, item := range wire.Results {
		n, failed, why := item.result(dryRun, wire.Comment != "failed")
		if why != "" {
			return bad(why)
		}
		if failed && r.OK {
			return bad("success contains a failed result")
		}
		if wire.Comment == "failed" && n.Ticket != nil && !n.IsLinked && n.Reason == "" {
			n.Reason = wire.Reason
			if n.Reason == "" {
				n.Reason = "comment-update-failed"
			}
		}
		hasFailure = hasFailure || failed
		r.Notes = append(r.Notes, n)
	}
	if !r.OK && !hasFailure {
		return bad("refused without a reason or failed outcome")
	}
	return r, nil
}

func followUpCommentOK(comment string, dryRun bool) bool {
	if dryRun {
		return comment == "planned"
	}
	return comment == "updated" || comment == "current"
}

func (item followUpNoteWire) result(dryRun, commentLinked bool) (domain.VerifyNoteFollowUp, bool, string) {
	n := item.VerifyNoteFollowUp
	if _, err := (domain.VerifyFollowUp{VerifyNoteIDs: []string{n.VerifyNoteID}}).Normalize(); err != nil {
		return n, false, "result has an invalid verify note id"
	}
	switch n.Outcome {
	case "created", "already-filed":
		if n.Ticket == nil || n.Ticket.Identifier == "" || n.Ticket.URL == "" {
			return n, false, "filed result missing ticket identifier or URL"
		}
		n.IsLinked = commentLinked
		if item.Linked != nil {
			n.IsLinked = *item.Linked
		}
	case "planned":
		if !dryRun {
			return n, false, "planned result outside dry-run"
		}
	case "indeterminate", "failed":
		return n, true, ""
	default:
		return n, false, "printed an unknown note outcome"
	}
	return n, false, ""
}
