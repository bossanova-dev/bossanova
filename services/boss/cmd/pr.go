package main

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/google/uuid"
	"github.com/spf13/cobra"

	"github.com/recurser/boss/internal/client"
	"github.com/recurser/boss/internal/verifybridge"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/vcs"
)

// sendBackChatTitle names the chat `boss pr send-back` starts in the PR's
// session to carry the operator's note into a repair run.
const sendBackChatTitle = "send back"

func prCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "pr",
		Short: "Act on a pull request by number: approve a verify park or send it back",
	}

	approve := &cobra.Command{
		Use:   "approve <pr-number>",
		Short: "Approve the PR's needs-human verify park so verify re-checks and merges it",
		Long: "Approve hands a parked PR back to verify: when a `needs human:` park with known codes\n" +
			"stands on the PR's current head, it posts a head-bound approval record on the PR, and\n" +
			"the next verify run waives exactly those codes on exactly that head; then it removes the\n" +
			"tracker's needs-human label. A head pushed meanwhile is not approved. On a head verify\n" +
			"passed outright, it lifts an earlier send-back of that head. It refuses otherwise.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runPrVerb(cmd, args[0], func(ctx context.Context, c client.BossClient, opts prOptions) error {
				return prApprove(ctx, cmd, c, &verifybridge.Bridge{}, opts)
			})
		},
	}

	sendBack := &cobra.Command{
		Use:   "send-back <pr-number> <note>",
		Short: "Reject the PR: post the note on it and start a repair chat in its session",
		Long: "Send-back posts the note as a PR comment headed \"Sent back by the operator\", then\n" +
			"starts a `send back` chat in the PR's session that runs /boss-repair with the note as\n" +
			"the feedback to address. It revokes any approval of the head it rejects, so verify\n" +
			"neither waives nor merges that head; the next head is judged afresh.",
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runPrVerb(cmd, args[0], func(ctx context.Context, c client.BossClient, opts prOptions) error {
				return prSendBack(ctx, cmd, c, &verifybridge.Bridge{}, opts, args[1])
			})
		},
	}

	for _, c := range []*cobra.Command{approve, sendBack} {
		c.Flags().String("repo", "", "Repository as owner/repo, when sessions in two repos share the PR number")
		c.Flags().Bool("dry-run", false, "Show what would be written without writing anything")
		c.Flags().Bool(jsonFlagName, false, "Emit a stable JSON envelope instead of human-readable text")
	}
	cmd.AddCommand(approve, sendBack)
	return cmd
}

type prOptions struct {
	pr     int
	repo   string
	dryRun bool
	asJSON bool
}

// prSessionLister is the read `boss pr` resolves its session through.
type prSessionLister interface {
	ListSessions(context.Context, *pb.ListSessionsRequest, client.SessionReadOptions) ([]*pb.Session, error)
}

// prChatClient is what send-back needs beyond the session read: start a chat
// and deliver into it.
type prChatClient interface {
	prSessionLister
	RecordChat(ctx context.Context, sessionID, agentSessionID, title, agentName string, resume bool) (*pb.ClaudeChat, error)
	SendChatMessage(context.Context, *pb.SendChatMessageRequest) (*pb.SendChatMessageResponse, error)
}

// verifyVerbs is the verify-gate operator surface; *verifybridge.Bridge is the
// real one.
type verifyVerbs interface {
	Approve(context.Context, verifybridge.Target) (verifybridge.ApproveResult, error)
	SendBack(context.Context, verifybridge.Target, string) (verifybridge.SendBackResult, error)
}

func runPrVerb(cmd *cobra.Command, prArg string, run func(context.Context, client.BossClient, prOptions) error) error {
	asJSON, _ := cmd.Flags().GetBool(jsonFlagName)
	dryRun, _ := cmd.Flags().GetBool("dry-run")
	repo, _ := cmd.Flags().GetString("repo")
	pr, err := parsePRNumber(prArg)
	if err != nil {
		return emitJSONFailure(cmd, asJSON, err)
	}
	c, err := newClient(cmd)
	if err != nil {
		return emitJSONFailure(cmd, asJSON, err)
	}
	return run(cmd.Context(), c, prOptions{pr: pr, repo: strings.TrimSpace(repo), dryRun: dryRun, asJSON: asJSON})
}

func parsePRNumber(arg string) (int, error) {
	n, err := strconv.Atoi(strings.TrimPrefix(strings.TrimSpace(arg), "#"))
	if err != nil || n < 1 {
		return 0, codedError(codeInvalidArgument, fmt.Errorf("PR number must be a positive integer, got %q", arg))
	}
	return n, nil
}

// resolvePRSession finds the one live session whose PR is pr, narrowed to repo
// (owner/repo, case-insensitive) when given. It returns the session and its
// repo slug, which the verify-gate verbs use as --repo.
func resolvePRSession(ctx context.Context, c prSessionLister, pr int, repo string) (*pb.Session, string, error) {
	sessions, err := c.ListSessions(ctx, &pb.ListSessionsRequest{}, client.SessionReadOptions{})
	if err != nil {
		return nil, "", fmt.Errorf("list sessions: %w", err)
	}
	var matches []*pb.Session
	for _, s := range sessions {
		if s.GetPrNumber() != int32(pr) {
			continue
		}
		if repo != "" && !strings.EqualFold(vcs.RepoSlug(s.GetRepoOriginUrl()), repo) {
			continue
		}
		matches = append(matches, s)
	}
	where := ""
	if repo != "" {
		where = " in " + repo
	}
	switch len(matches) {
	case 0:
		return nil, "", codedError(codeNotFound, fmt.Errorf("no session found for PR #%d%s", pr, where))
	case 1:
		s := matches[0]
		return s, strings.ToLower(vcs.RepoSlug(s.GetRepoOriginUrl())), nil
	}
	described := make([]string, 0, len(matches))
	for _, s := range matches {
		slug := vcs.RepoSlug(s.GetRepoOriginUrl())
		if slug == "" {
			slug = s.GetRepoDisplayName()
		}
		described = append(described, fmt.Sprintf("%s (%s)", s.GetId(), slug))
	}
	hint := "; pass --repo owner/repo"
	if repo != "" {
		hint = "; name the session directly with `boss show`"
	}
	return nil, "", codedError(codeAmbiguousPR, fmt.Errorf("PR #%d%s matches %d sessions: %s%s", pr, where, len(matches), strings.Join(described, ", "), hint))
}

func prTarget(s *pb.Session, slug string, opts prOptions) verifybridge.Target {
	return verifybridge.Target{PR: opts.pr, Repo: slug, Dir: s.GetWorktreePath(), DryRun: opts.dryRun}
}

// prApproveJSON is the `boss pr approve --json` envelope.
type prApproveJSON struct {
	PR        int                        `json:"pr"`
	Repo      string                     `json:"repo"`
	SessionID string                     `json:"session_id"`
	Result    verifybridge.ApproveResult `json:"result"`
}

func prApprove(ctx context.Context, cmd *cobra.Command, c prSessionLister, verbs verifyVerbs, opts prOptions) error {
	s, slug, err := resolvePRSession(ctx, c, opts.pr, opts.repo)
	if err != nil {
		return emitJSONFailure(cmd, opts.asJSON, err)
	}
	r, err := verbs.Approve(ctx, prTarget(s, slug, opts))
	if errors.Is(err, verifybridge.ErrNodeTimeout) && !opts.dryRun {
		// The record may already be on the PR, so its effect is unknown, not refused.
		err = fmt.Errorf("%w; an approval record may already be posted, so re-run approve to confirm", err)
	}
	if err != nil {
		return emitJSONFailure(cmd, opts.asJSON, fmt.Errorf("approve PR #%d: %w", opts.pr, err))
	}
	env := prApproveJSON{PR: opts.pr, Repo: slug, SessionID: s.GetId(), Result: r}
	if !r.Approved && !r.Planned {
		refusal := fmt.Errorf("PR #%d not approved: %s%s", opts.pr, refusalDetail(r.Reason, r.Why, r.Park), postedRecordDetail(r))
		if opts.asJSON {
			return emitJSONReportedFailure(cmd, env, refusal)
		}
		return refusal
	}
	if opts.asJSON {
		return emitJSON(cmd, env)
	}
	w := cmd.OutOrStdout()
	if r.Planned {
		if r.Lifted == "sent-back" {
			_, err = fmt.Fprintf(w, "Would approve #%d: record an approval lifting the send-back of head %s (dry run; nothing written).\n", opts.pr, shortSHA(r.HeadSHA))
			return err
		}
		_, err = fmt.Fprintf(w, "Would approve #%d: record an approval waiving %s on head %s, then remove the needs-human label (dry run; nothing written).\n", opts.pr, r.Waives, shortSHA(r.HeadSHA))
		return err
	}
	if r.Lifted == "sent-back" {
		_, err = fmt.Fprintf(w, "Approved #%d: lifted the send-back of head %s (record: %s). Run /boss-verify %d to merge it.\n", opts.pr, shortSHA(r.HeadSHA), r.ApprovalURL, opts.pr)
		return err
	}
	if _, err = fmt.Fprintf(w, "Approved #%d: waived %s on head %s (record: %s). Verify will re-check and merge.\n", opts.pr, r.Waives, shortSHA(r.HeadSHA), r.ApprovalURL); err != nil {
		return err
	}
	switch r.TrackerWrites {
	case "ok", "":
	case "skipped":
		// verify-gate left the label on deliberately: what was approved no longer stands.
		switch r.Why {
		case "pr-not-open":
			_, err = fmt.Fprintf(w, "The PR is no longer open, so the needs-human label was left on; remove it on the ticket by hand if the work is done.\n")
		case "head-moved":
			_, err = fmt.Fprintf(w, "The PR's head or its park changed after the approval, so the needs-human label was left on.\n")
		default:
			// An older toolbox reported a failed re-read as skipped too.
			_, err = fmt.Fprintf(w, "The needs-human label was left on (%s); remove it on the ticket by hand if this head still stands.\n", r.Why)
		}
	default:
		why := r.TrackerWrites
		if r.Why != "" {
			why += ": " + r.Why
		}
		// The head may have moved and parked meanwhile, so the removal is conditional.
		_, err = fmt.Fprintf(w, "The needs-human label could not be removed (%s); remove it on the ticket by hand if this head still stands.\n", why)
	}
	return err
}

// prSendBackJSON is the `boss pr send-back --json` envelope. Chat and Delivery
// are absent on a dry run.
type prSendBackJSON struct {
	PR        int                         `json:"pr"`
	Repo      string                      `json:"repo"`
	SessionID string                      `json:"session_id"`
	Result    verifybridge.SendBackResult `json:"result"`
	Message   string                      `json:"message"`
	Chat      *chatNewChatJSON            `json:"chat,omitempty"`
	Delivery  *prDeliveryJSON             `json:"delivery,omitempty"`
}

type prDeliveryJSON struct {
	Delivered     bool   `json:"delivered"`
	DeliveryState string `json:"delivery_state"`
	Notice        string `json:"notice,omitempty"`
}

// sendBackMessage is what the send-back chat receives: the repair skill, then
// the operator's note as the feedback it must address.
func sendBackMessage(pr int, note string) string {
	return fmt.Sprintf("/boss-repair %d\n\nThe operator sent PR #%d back. Address this feedback before anything else:\n\n%s", pr, pr, strings.TrimSpace(note))
}

func prSendBack(ctx context.Context, cmd *cobra.Command, c prChatClient, verbs verifyVerbs, opts prOptions, note string) error {
	if strings.TrimSpace(note) == "" {
		return emitJSONFailure(cmd, opts.asJSON, codedError(codeInvalidArgument, fmt.Errorf("send-back needs a non-empty note")))
	}
	s, slug, err := resolvePRSession(ctx, c, opts.pr, opts.repo)
	if err != nil {
		return emitJSONFailure(cmd, opts.asJSON, err)
	}
	r, err := verbs.SendBack(ctx, prTarget(s, slug, opts), note)
	if errors.Is(err, verifybridge.ErrNodeTimeout) && !opts.dryRun {
		// The note is posted last, so it may already be on the PR: check before re-running.
		err = fmt.Errorf("%w; the note may already be posted, so check the PR before sending it back again", err)
	}
	if err != nil {
		return emitJSONFailure(cmd, opts.asJSON, fmt.Errorf("send back PR #%d: %w", opts.pr, err))
	}
	message := sendBackMessage(opts.pr, note)
	env := prSendBackJSON{PR: opts.pr, Repo: slug, SessionID: s.GetId(), Result: r, Message: message}
	if !r.Posted && !r.Planned {
		refusal := fmt.Errorf("PR #%d not sent back: %s", opts.pr, refusalDetail(r.Reason, "", ""))
		if opts.asJSON {
			return emitJSONReportedFailure(cmd, env, refusal)
		}
		return refusal
	}
	if r.Planned {
		if opts.asJSON {
			return emitJSON(cmd, env)
		}
		_, err = fmt.Fprintf(cmd.OutOrStdout(), "Would post the note on #%d, then start a %q chat in session %s delivering:\n\n%s\n\n(dry run; nothing written)\n", opts.pr, sendBackChatTitle, s.GetId(), message)
		return err
	}

	// The comment is on the PR from here on, so every later failure names it:
	// the operator must not re-run and post the note twice.
	posted := fmt.Sprintf("the note is posted at %s", r.CommentURL)
	chat, err := c.RecordChat(ctx, s.GetId(), uuid.NewString(), sendBackChatTitle, "", false)
	if err != nil {
		return emitJSONFailure(cmd, opts.asJSON, fmt.Errorf("start the %q chat for PR #%d (%s): %w", sendBackChatTitle, opts.pr, posted, err))
	}
	if chat.GetTmuxSessionName() == "" {
		return emitJSONFailure(cmd, opts.asJSON, codedError(codeAgentNotSpawned,
			fmt.Errorf("could not spawn a live agent for the %q chat %s in session %s (%s); start a repair with `boss chat new %s`", sendBackChatTitle, chat.GetAgentSessionId(), s.GetId(), posted, s.GetId())))
	}
	chatJSON := newChatJSON(chat).Chat
	env.Chat = &chatJSON
	resp, err := c.SendChatMessage(ctx, &pb.SendChatMessageRequest{
		AgentSessionId: chat.GetAgentSessionId(),
		Message:        message,
		WakeIfAsleep:   true,
		Submit:         true,
	})
	if err != nil {
		return emitJSONFailure(cmd, opts.asJSON, fmt.Errorf("deliver the repair to chat %s (%s): %w; retry with `boss chat send %s \"/boss-repair %d\"`", chat.GetAgentSessionId(), posted, err, chat.GetAgentSessionId(), opts.pr))
	}
	env.Delivery = &prDeliveryJSON{
		Delivered:     resp.GetDelivered(),
		DeliveryState: resp.GetDeliveryState().String(),
		Notice:        resp.GetNoticeText(),
	}
	if opts.asJSON {
		return emitJSON(cmd, env)
	}
	w := cmd.OutOrStdout()
	_, _ = fmt.Fprintf(w, "Sent #%d back: %s\n", opts.pr, r.CommentURL)
	_, _ = fmt.Fprintf(w, "Repair chat: %s (session %s)\n", chat.GetAgentSessionId(), s.GetId())
	reportChatSendOutcome(w, resp)
	return nil
}

// postedRecordDetail adds what a refused approve's record still does: a posted
// record is inert for a moved head or a superseding operator decision; for an
// unconfirmed approval it may stand (or may not have been posted), so the
// operator re-runs approve to confirm.
func postedRecordDetail(r verifybridge.ApproveResult) string {
	switch {
	case r.Reason == "approval-unconfirmed" && r.ApprovalURL != "":
		return fmt.Sprintf("; the record at %s may still stand for head %s, so re-run approve to confirm", r.ApprovalURL, shortSHA(r.HeadSHA))
	case r.Reason == "approval-unconfirmed":
		return "; re-run approve to confirm"
	case r.ApprovalURL != "":
		return fmt.Sprintf("; the record at %s names head %s only and waives nothing that stands now", r.ApprovalURL, shortSHA(r.HeadSHA))
	}
	return ""
}

// refusalDetail renders a verify-gate refusal reason for a person.
func refusalDetail(reason, why, park string) string {
	var detail string
	switch reason {
	case "no-standing-park":
		detail = "no `needs human:` park stands on the PR's current head"
	case "head-moved":
		detail = "the PR's head or its park changed while approving, so what stands now is not approved; re-check and approve again"
	case "approval-record-failed":
		detail = "the approval record could not be posted on the PR, so nothing is approved and the needs-human label was left alone"
	case "approval-superseded":
		detail = "a later operator decision on the head (a send-back or another approval) landed while approving and replaces this one"
	case "approval-unconfirmed":
		detail = "whether the approval record was posted and took effect could not be read back"
	case "approver-not-writer":
		detail = "the account boss runs gh as cannot write to the repository, so its record on the PR would not count"
	case "approver-unreadable":
		detail = "the repository permission of the account boss runs gh as could not be read"
	case "sent-back-approved-head":
		detail = "the head verify passed under an approval was sent back; run /boss-verify on the PR to re-raise its park, then approve that"
	case "comments-unreadable":
		detail = "the PR's comments could not be read, so an earlier send-back cannot be ruled out"
	case "park-codes-unknown":
		detail = "the park names a code this toolbox does not know"
		if park != "" {
			detail += " (" + park + ")"
		}
	case "pr-not-open":
		detail = "the PR is not open"
	case "pr-unreadable":
		detail = "the PR could not be read"
	case "status-unreadable":
		detail = "the PR's verify status could not be read"
	case "head-unreadable":
		detail = "the PR's head commit could not be read"
	case "pr-comment-failed":
		detail = "the PR comment could not be posted"
	default:
		detail = "refused"
	}
	detail += " [" + reason + "]"
	if why != "" {
		detail += ": " + why
	}
	return detail
}
