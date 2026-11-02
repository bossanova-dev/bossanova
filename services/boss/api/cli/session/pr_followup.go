package session

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/recurser/boss/api/cli/cliutil"
	"github.com/recurser/boss/internal/domain/rpcerr"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/spf13/cobra"
)

type prFollowUpClient interface {
	prSessionLister
	FollowUpSessionPRNotes(context.Context, string, []string, bool, bool) (*pb.VerifyFollowUpResult, error)
}

func (g *Group) prFollowUpCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "follow-up <pr-number>",
		Short: "File selected verify notes as follow-up tickets on the daemon host",
		Long: "File selected notes from verify's folded notes comment as follow-up tickets, then link\n" +
			"them in that comment. Notes never block verify. The daemon runs this on its own host\n" +
			"with the repository's tracker credentials. Run it from your own terminal; agent chats\n" +
			"cannot file follow-ups. Re-running finds tickets by marker instead of filing twice.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ids, _ := cmd.Flags().GetStringSlice("note")
			all, _ := cmd.Flags().GetBool("all")
			return g.runPrVerb(cmd, args[0], func(ctx context.Context, c Client, opts prOptions) error {
				followUp, ok := c.(prFollowUpClient)
				if !ok {
					return cliutil.EmitJSONFailure(cmd, opts.asJSON, rpcerr.New(rpcerr.Unimplemented, errors.New("boss pr follow-up is unavailable on this client")))
				}
				return prFollowUp(ctx, cmd, followUp, opts, ids, all)
			})
		},
	}
	cmd.Flags().StringSlice("note", nil, "Verify note id to follow up (repeatable)")
	cmd.Flags().Bool("all", false, "Follow up every note in the verify notes comment")
	cmd.Flags().String("repo", "", "Repository as owner/repo, when sessions in two repos share the PR number")
	cmd.Flags().Bool("dry-run", false, "Show what would be written without writing anything")
	cmd.Flags().Bool(cliutil.JSONFlagName, false, "Emit a stable JSON envelope instead of human-readable text")
	cmd.MarkFlagsMutuallyExclusive("note", "all")
	cmd.MarkFlagsOneRequired("note", "all")
	return cmd
}

type prFollowUpTicketJSON struct {
	Identifier string `json:"identifier"`
	URL        string `json:"url"`
}
type prNoteFollowUpJSON struct {
	Note    string                `json:"note"`
	Outcome string                `json:"outcome"`
	Ticket  *prFollowUpTicketJSON `json:"ticket,omitempty"`
	Linked  bool                  `json:"linked"`
	Reason  string                `json:"reason,omitempty"`
}
type prFollowUpResultJSON struct {
	OK      bool                 `json:"ok"`
	DryRun  bool                 `json:"dryRun,omitempty"`
	Reason  string               `json:"reason,omitempty"`
	IDs     []string             `json:"ids,omitempty"`
	Head    string               `json:"head,omitempty"`
	Comment string               `json:"comment,omitempty"`
	Results []prNoteFollowUpJSON `json:"results,omitempty"`
}
type prFollowUpJSON struct {
	PR        int                  `json:"pr"`
	Repo      string               `json:"repo"`
	SessionID string               `json:"session_id"`
	Result    prFollowUpResultJSON `json:"result"`
}

func followUpResultJSON(r *pb.VerifyFollowUpResult) prFollowUpResultJSON {
	out := prFollowUpResultJSON{OK: r.GetIsOk(), DryRun: r.GetIsDryRun(), Reason: r.GetReason(), IDs: r.GetUnknownVerifyNoteIds(), Head: r.GetHeadSha(), Comment: r.GetComment()}
	for _, n := range r.GetNotes() {
		note := prNoteFollowUpJSON{Note: n.GetVerifyNoteId(), Outcome: n.GetOutcome(), Linked: n.GetIsLinked(), Reason: n.GetReason()}
		if n.GetTicketIdentifier() != "" || n.GetTicketUrl() != "" {
			note.Ticket = &prFollowUpTicketJSON{Identifier: n.GetTicketIdentifier(), URL: n.GetTicketUrl()}
		}
		out.Results = append(out.Results, note)
	}
	return out
}
func prFollowUp(ctx context.Context, cmd *cobra.Command, c prFollowUpClient, opts prOptions, ids []string, all bool) error {
	s, slug, err := resolvePRSessionWithArchived(ctx, c, opts.pr, opts.repo, true)
	if err != nil {
		return cliutil.EmitJSONFailure(cmd, opts.asJSON, err)
	}
	r, err := c.FollowUpSessionPRNotes(ctx, s.GetId(), ids, all, opts.dryRun)
	if verbTimedOut(err, opts) {
		err = fmt.Errorf("%w; some tickets may already be filed — re-run the same command, which finds them by marker instead of filing twice", err)
	}
	if err != nil {
		return cliutil.EmitJSONFailure(cmd, opts.asJSON, fmt.Errorf("follow up PR #%d: %w", opts.pr, err))
	}
	env := prFollowUpJSON{PR: opts.pr, Repo: slug, SessionID: s.GetId(), Result: followUpResultJSON(r)}
	failure := fmt.Errorf("PR #%d follow-up did not complete", opts.pr)
	if r.GetReason() != "" {
		status := "did not complete"
		if len(r.GetNotes()) == 0 {
			status = "refused"
		}
		failure = fmt.Errorf("PR #%d follow-up %s: %s%s", opts.pr, status, r.GetReason(), followUpUnknownIDs(r))
	}
	if opts.asJSON {
		if !r.GetIsOk() {
			return cliutil.EmitJSONReportedFailure(cmd, env, failure)
		}
		return cliutil.EmitJSON(cmd, env)
	}
	for _, line := range followUpLines(opts.pr, r) {
		if _, err := fmt.Fprintln(cmd.OutOrStdout(), line); err != nil {
			return err
		}
	}
	if !r.GetIsOk() {
		return failure
	}
	return nil
}
func followUpUnknownIDs(r *pb.VerifyFollowUpResult) string {
	if len(r.GetUnknownVerifyNoteIds()) == 0 {
		return ""
	}
	return ": " + strings.Join(r.GetUnknownVerifyNoteIds(), ", ")
}
func followUpLines(pr int, r *pb.VerifyFollowUpResult) []string {
	if len(r.GetNotes()) == 0 && r.GetReason() != "" {
		return []string{fmt.Sprintf("PR #%d: follow-up refused (%s)%s", pr, r.GetReason(), followUpUnknownIDs(r))}
	}
	lines := make([]string, 0, len(r.GetNotes())+1)
	for _, n := range r.GetNotes() {
		var detail string
		switch n.GetOutcome() {
		case "created", "already-filed":
			detail = strings.ReplaceAll(n.GetOutcome(), "-", " ") + " " + n.GetTicketIdentifier() + " " + n.GetTicketUrl()
			if !n.GetIsLinked() {
				detail += " (not linked: " + n.GetReason() + ")"
			}
		case "planned":
			detail = "would file"
		case "indeterminate":
			detail = "indeterminate: the tracker may have filed it; re-run to settle"
		default:
			detail = "failed: " + n.GetReason()
		}
		lines = append(lines, n.GetVerifyNoteId()+"  "+detail)
	}
	summary := map[string]string{"updated": "updated", "current": "already current", "planned": "would be updated", "failed": "could not be updated"}[r.GetComment()]
	return append(lines, "notes comment "+summary)
}
