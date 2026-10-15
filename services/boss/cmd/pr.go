package main

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"connectrpc.com/connect"
	"github.com/google/uuid"
	"github.com/spf13/cobra"

	"github.com/recurser/boss/internal/client"
	"github.com/recurser/boss/internal/prverdict"
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
			"passed outright, it lifts an earlier send-back of that head. It refuses otherwise.\n\n" +
			"The daemon runs it on its own host, in the PR session's worktree, with the repo's stored\n" +
			"tracker credentials, so it works from any machine that reaches the daemon. It refuses to\n" +
			"run inside an agent chat: approving a park is the operator's call.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runPrVerb(cmd, args[0], func(ctx context.Context, c client.BossClient, opts prOptions) error {
				return prApprove(ctx, cmd, c, opts)
			})
		},
	}

	sendBack := &cobra.Command{
		Use:   "send-back <pr-number> <note>",
		Short: "Reject the PR: post the note on it and start a repair chat in its session",
		Long: "Send-back posts the note as a PR comment headed \"Sent back by the operator\", then\n" +
			"starts a `send back` chat in the PR's session that runs /boss-repair with the note as\n" +
			"the feedback to address. It revokes any approval of the head it rejects, so verify\n" +
			"neither waives nor merges that head; the next head is judged afresh. The daemon posts\n" +
			"the note from its own host; like approve, it refuses to run inside an agent chat.",
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runPrVerb(cmd, args[0], func(ctx context.Context, c client.BossClient, opts prOptions) error {
				return prSendBack(ctx, cmd, c, opts, args[1])
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

// prApproveClient is what approve needs: the session read and the daemon's
// approve verb.
type prApproveClient interface {
	prSessionLister
	ApproveSessionPR(ctx context.Context, id string, dryRun bool) (*pb.VerifyApproveResult, error)
}

// prChatClient is what send-back needs: the session read, the daemon's
// send-back verb, and starting a chat and delivering into it.
type prChatClient interface {
	prSessionLister
	SendBackSessionPR(ctx context.Context, id, note string, dryRun bool) (*pb.SendBackSessionPRResponse, error)
	RecordChat(ctx context.Context, sessionID, agentSessionID, title, agentName string, role pb.ChatRole, resume bool) (*pb.ClaudeChat, error)
	SendChatMessage(context.Context, *pb.SendChatMessageRequest) (*pb.SendChatMessageResponse, error)
}

func runPrVerb(cmd *cobra.Command, prArg string, run func(context.Context, client.BossClient, prOptions) error) error {
	asJSON, _ := cmd.Flags().GetBool(jsonFlagName)
	dryRun, _ := cmd.Flags().GetBool("dry-run")
	repo, _ := cmd.Flags().GetString("repo")
	if chat := strings.TrimSpace(osGetenv("BOSS_AGENT_SESSION_ID")); chat != "" {
		// An agent must not approve or send back the park it may itself be under.
		return emitJSONFailure(cmd, asJSON, connect.NewError(connect.CodePermissionDenied,
			fmt.Errorf("`boss pr %s` refuses to run inside agent chat %s: approving or sending back a verify park is the operator's call; run it from your own terminal or press [m] on the session in the TUI", cmd.Name(), chat)))
	}
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
// repo slug, which the `--json` envelope reports.
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

// prApproveResultJSON is the approve outcome in the `--json` envelope; its tags
// are verify-gate's own field names.
type prApproveResultJSON struct {
	Approved      bool   `json:"approved"`
	Planned       bool   `json:"planned,omitempty"`
	DryRun        bool   `json:"dryRun,omitempty"`
	Reason        string `json:"reason,omitempty"`
	Waives        string `json:"waives,omitempty"`
	Lifted        string `json:"lifted,omitempty"`
	HeadSHA       string `json:"headSha,omitempty"`
	TrackerWrites string `json:"trackerWrites,omitempty"`
	ApprovalURL   string `json:"approvalUrl,omitempty"`
	Why           string `json:"why,omitempty"`
	Park          string `json:"park,omitempty"`
	State         string `json:"state,omitempty"`
}

func approveResultJSON(r *pb.VerifyApproveResult) prApproveResultJSON {
	return prApproveResultJSON{
		Approved:      r.GetIsApproved(),
		Planned:       r.GetIsPlanned(),
		DryRun:        r.GetIsDryRun(),
		Reason:        r.GetReason(),
		Waives:        r.GetWaives(),
		Lifted:        r.GetLifted(),
		HeadSHA:       r.GetHeadSha(),
		TrackerWrites: r.GetTrackerWrites(),
		ApprovalURL:   r.GetApprovalUrl(),
		Why:           r.GetWhy(),
		Park:          r.GetPark(),
		State:         r.GetState(),
	}
}

// prApproveJSON is the `boss pr approve --json` envelope.
type prApproveJSON struct {
	PR        int                 `json:"pr"`
	Repo      string              `json:"repo"`
	SessionID string              `json:"session_id"`
	Result    prApproveResultJSON `json:"result"`
}

// verbTimedOut reports whether the daemon's verify verb ran out of time, in
// which case its write may already be on the PR.
func verbTimedOut(err error, opts prOptions) bool {
	return !opts.dryRun && connect.CodeOf(err) == connect.CodeDeadlineExceeded
}

func prApprove(ctx context.Context, cmd *cobra.Command, c prApproveClient, opts prOptions) error {
	s, slug, err := resolvePRSession(ctx, c, opts.pr, opts.repo)
	if err != nil {
		return emitJSONFailure(cmd, opts.asJSON, err)
	}
	r, err := c.ApproveSessionPR(ctx, s.GetId(), opts.dryRun)
	if verbTimedOut(err, opts) {
		// The record may already be on the PR, so its effect is unknown, not refused.
		err = fmt.Errorf("%w; an approval record may already be posted, so re-run approve to confirm", err)
	}
	if err != nil {
		return emitJSONFailure(cmd, opts.asJSON, fmt.Errorf("approve PR #%d: %w", opts.pr, err))
	}
	env := prApproveJSON{PR: opts.pr, Repo: slug, SessionID: s.GetId(), Result: approveResultJSON(r)}
	if !r.GetIsApproved() && !r.GetIsPlanned() {
		refusal := errors.New(prverdict.ApproveRefusal(opts.pr, r))
		if opts.asJSON {
			return emitJSONReportedFailure(cmd, env, refusal)
		}
		return refusal
	}
	if opts.asJSON {
		return emitJSON(cmd, env)
	}
	for _, line := range prverdict.ApproveLines(opts.pr, r) {
		if _, err := fmt.Fprintln(cmd.OutOrStdout(), line); err != nil {
			return err
		}
	}
	return nil
}

// prSendBackResultJSON is the send-back outcome in the `--json` envelope; its
// tags are verify-gate's own field names.
type prSendBackResultJSON struct {
	Posted     bool   `json:"posted"`
	Planned    bool   `json:"planned,omitempty"`
	DryRun     bool   `json:"dryRun,omitempty"`
	Reason     string `json:"reason,omitempty"`
	CommentURL string `json:"commentUrl,omitempty"`
	HeadSHA    string `json:"headSha,omitempty"`
	State      string `json:"state,omitempty"`
}

func sendBackResultJSON(r *pb.SendBackSessionPRResponse) prSendBackResultJSON {
	return prSendBackResultJSON{
		Posted:     r.GetIsPosted(),
		Planned:    r.GetIsPlanned(),
		DryRun:     r.GetIsDryRun(),
		Reason:     r.GetReason(),
		CommentURL: r.GetCommentUrl(),
		HeadSHA:    r.GetHeadSha(),
		State:      r.GetState(),
	}
}

// prSendBackJSON is the `boss pr send-back --json` envelope. Chat and Delivery
// are absent on a dry run.
type prSendBackJSON struct {
	PR        int                  `json:"pr"`
	Repo      string               `json:"repo"`
	SessionID string               `json:"session_id"`
	Result    prSendBackResultJSON `json:"result"`
	Message   string               `json:"message"`
	Chat      *chatNewChatJSON     `json:"chat,omitempty"`
	Delivery  *chatDeliveryJSON    `json:"delivery,omitempty"`
}

// sendBackMessage is what the send-back chat receives: the repair skill, then
// the operator's note as the feedback it must address.
func sendBackMessage(pr int, note string) string {
	return fmt.Sprintf("/boss-repair %d\n\nThe operator sent PR #%d back. Address this feedback before anything else:\n\n%s", pr, pr, strings.TrimSpace(note))
}

func prSendBack(ctx context.Context, cmd *cobra.Command, c prChatClient, opts prOptions, note string) error {
	if strings.TrimSpace(note) == "" {
		return emitJSONFailure(cmd, opts.asJSON, codedError(codeInvalidArgument, fmt.Errorf("send-back needs a non-empty note")))
	}
	s, slug, err := resolvePRSession(ctx, c, opts.pr, opts.repo)
	if err != nil {
		return emitJSONFailure(cmd, opts.asJSON, err)
	}
	r, err := c.SendBackSessionPR(ctx, s.GetId(), note, opts.dryRun)
	if verbTimedOut(err, opts) {
		// The note is posted last, so it may already be on the PR: check before re-running.
		err = fmt.Errorf("%w; the note may already be posted, so check the PR before sending it back again", err)
	}
	if err != nil {
		return emitJSONFailure(cmd, opts.asJSON, fmt.Errorf("send back PR #%d: %w", opts.pr, err))
	}
	message := sendBackMessage(opts.pr, note)
	env := prSendBackJSON{PR: opts.pr, Repo: slug, SessionID: s.GetId(), Result: sendBackResultJSON(r), Message: message}
	if !r.GetIsPosted() && !r.GetIsPlanned() {
		refusal := errors.New(prverdict.SendBackRefusal(opts.pr, r))
		if opts.asJSON {
			return emitJSONReportedFailure(cmd, env, refusal)
		}
		return refusal
	}
	if r.GetIsPlanned() {
		if opts.asJSON {
			return emitJSON(cmd, env)
		}
		_, err = fmt.Fprintf(cmd.OutOrStdout(), "Would post the note on #%d, then start a %q chat in session %s delivering:\n\n%s\n\n(dry run; nothing written)\n", opts.pr, sendBackChatTitle, s.GetId(), message)
		return err
	}

	// The comment is on the PR from here on, so every later failure names it:
	// the operator must not re-run and post the note twice.
	posted := fmt.Sprintf("the note is posted at %s", r.GetCommentUrl())
	chat, err := c.RecordChat(ctx, s.GetId(), uuid.NewString(), sendBackChatTitle, "", pb.ChatRole_CHAT_ROLE_OPERATOR, false)
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
	delivery := newChatDeliveryJSON(resp)
	env.Delivery = &delivery
	if opts.asJSON {
		return emitJSON(cmd, env)
	}
	w := cmd.OutOrStdout()
	_, _ = fmt.Fprintf(w, "Sent #%d back: %s\n", opts.pr, r.GetCommentUrl())
	_, _ = fmt.Fprintf(w, "Repair chat: %s (session %s)\n", chat.GetAgentSessionId(), s.GetId())
	reportChatSendOutcome(w, resp)
	return nil
}
