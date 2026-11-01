package session

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/recurser/boss/api/cli/cliutil"
	"github.com/recurser/boss/internal/domain/prverdict"
	"github.com/recurser/boss/internal/domain/rpcerr"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/spf13/cobra"
)

func (g *Group) prAcceptCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "accept <pr-number> <blocker-id>", Short: "Accept a review finding and record a follow-up ticket for verify", Args: cobra.ExactArgs(2), RunE: func(cmd *cobra.Command, args []string) error {
		asJSON, _ := cmd.Flags().GetBool(cliutil.JSONFlagName)
		if g.Getenv("BOSS_AGENT_SESSION_ID") != "" {
			return cliutil.EmitJSONFailure(cmd, asJSON, rpcerr.New(rpcerr.PermissionDenied, errors.New("boss pr accept is the operator's call; run it from your own terminal to act on a verify verdict")))
		}
		reason, _ := cmd.Flags().GetString("reason")
		if strings.TrimSpace(reason) == "" {
			return cliutil.EmitJSONFailure(cmd, asJSON, cliutil.CodedError(cliutil.CodeInvalidArgument, errors.New("--reason must contain text")))
		}
		return g.runPrVerb(cmd, args[0], func(ctx context.Context, c Client, opts prOptions) error {
			return prAccept(ctx, cmd, c, opts, args[1], reason)
		})
	}}
	cmd.Flags().String("reason", "", "Why this review finding is accepted for a follow-up")
	cmd.Flags().String("repo", "", "Repository as owner/repo, when sessions in two repos share the PR number")
	cmd.Flags().Bool("dry-run", false, "Show planned writes without writing anything")
	cmd.Flags().Bool(cliutil.JSONFlagName, false, "Emit a stable JSON envelope instead of human-readable text")
	return cmd
}

type prAcceptClient interface {
	prSessionLister
	AcceptSessionPRBlocker(context.Context, *pb.AcceptSessionPRBlockerRequest) (*pb.AcceptSessionPRBlockerResponse, error)
}

type acceptTicketJSON struct {
	Identifier string `json:"identifier"`
	URL        string `json:"url"`
}
type prAcceptResultJSON struct {
	Accepted       bool              `json:"accepted"`
	Already        bool              `json:"already,omitempty"`
	DryRun         bool              `json:"dryRun,omitempty"`
	Reason         string            `json:"reason,omitempty"`
	Detail         string            `json:"detail,omitempty"`
	Head           string            `json:"head,omitempty"`
	Blocker        string            `json:"blocker,omitempty"`
	Ticket         *acceptTicketJSON `json:"ticket,omitempty"`
	CommentURL     string            `json:"commentUrl,omitempty"`
	ExistingTicket bool              `json:"existingTicket,omitempty"`
	Writes         []string          `json:"writes,omitempty"`
}

func acceptResultJSON(r *pb.AcceptSessionPRBlockerResponse) prAcceptResultJSON {
	result := prAcceptResultJSON{Accepted: r.GetIsAccepted(), Already: r.GetIsAlreadyAccepted(), DryRun: r.GetIsDryRun(), Reason: r.GetReason(), Detail: r.GetDetail(), Head: r.GetHeadSha(), Blocker: r.GetBlockerId(), CommentURL: r.GetCommentUrl(), ExistingTicket: r.GetIsExistingTicket(), Writes: r.GetPlannedWrites()}
	if r.GetTicketIdentifier() != "" || r.GetTicketUrl() != "" {
		result.Ticket = &acceptTicketJSON{r.GetTicketIdentifier(), r.GetTicketUrl()}
	}
	return result
}

func prAccept(ctx context.Context, cmd *cobra.Command, c prAcceptClient, opts prOptions, blocker, reason string) error {
	s, slug, err := resolvePRSession(ctx, c, opts.pr, opts.repo)
	if err != nil {
		return cliutil.EmitJSONFailure(cmd, opts.asJSON, err)
	}
	if s.GetPrDisplayHeadSha() == "" {
		return cliutil.EmitJSONFailure(cmd, opts.asJSON, rpcerr.New(rpcerr.FailedPrecondition, errors.New("the PR's head is not known yet; try boss session refresh-pr or wait for the next poll")))
	}
	r, err := c.AcceptSessionPRBlocker(ctx, &pb.AcceptSessionPRBlockerRequest{Id: s.GetId(), BlockerId: blocker, Reason: reason, ExpectedHeadSha: s.GetPrDisplayHeadSha(), IsDryRun: opts.dryRun})
	if verbTimedOut(err, opts) {
		err = fmt.Errorf("%w; the follow-up ticket or acceptance may already be posted, and rerunning is safe", err)
	}
	if rpcerr.KindOf(err) == rpcerr.Unimplemented && !strings.Contains(err.Error(), "only available on a local daemon") {
		err = fmt.Errorf("%w; the daemon is older than this CLI; upgrade the daemon", err)
	}
	if err != nil {
		return cliutil.EmitJSONFailure(cmd, opts.asJSON, fmt.Errorf("accept PR #%d blocker %s: %w", opts.pr, blocker, err))
	}
	env := struct {
		PR        int                `json:"pr"`
		Repo      string             `json:"repo"`
		SessionID string             `json:"session_id"`
		Result    prAcceptResultJSON `json:"result"`
	}{opts.pr, slug, s.GetId(), acceptResultJSON(r)}
	if !r.GetIsAccepted() && !r.GetIsDryRun() {
		refusal := errors.New(prverdict.AcceptRefusal(opts.pr, blocker, r))
		if opts.asJSON {
			return cliutil.EmitJSONReportedFailure(cmd, env, refusal)
		}
		return refusal
	}
	if opts.asJSON {
		return cliutil.EmitJSON(cmd, env)
	}
	for _, line := range prverdict.AcceptLines(opts.pr, r) {
		if _, err := fmt.Fprintln(cmd.OutOrStdout(), line); err != nil {
			return err
		}
	}
	return nil
}
