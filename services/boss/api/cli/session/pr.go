package session

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/recurser/boss/internal/domain/rpc"

	"github.com/recurser/boss/api/cli/cliutil"

	"github.com/recurser/boss/internal/domain/rpcerr"
	"github.com/spf13/cobra"

	"github.com/recurser/boss/internal/domain/prverdict"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/vcs"
)

func (g *Group) PRCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "pr",
		Short: "Act on a pull request by number: approve a verify park",
	}

	approve := &cobra.Command{
		Use:   "approve <pr-number>",
		Short: "Approve the PR's needs-human verify park so verify re-checks and merges it",
		Long: "Approve hands a parked PR back to verify: when a `needs human:` park with known codes\n" +
			"stands on the PR's current head, it posts a head-bound approval record on the PR, and\n" +
			"the next verify run waives exactly those codes on exactly that head; then it removes the\n" +
			"tracker's needs-human label. A head pushed meanwhile is not approved. On a head verify\n" +
			"passed outright, it lifts an older send-back of that head. It refuses otherwise.\n\n" +
			"In the TUI, [m]erge on a review row records this same approval and merges at once; this\n" +
			"command is the escape hatch for a park the TUI offers no merge for.\n\n" +
			"The daemon runs it on its own host, in the PR session's worktree, with the repo's stored\n" +
			"tracker credentials, so it works from any machine that reaches the daemon. It refuses to\n" +
			"run inside an agent chat: approving a park is the operator's call.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return g.runPrVerb(cmd, args[0], func(ctx context.Context, c Client, opts prOptions) error {
				return prApprove(ctx, cmd, c, opts)
			})
		},
	}

	approve.Flags().String("repo", "", "Repository as owner/repo, when sessions in two repos share the PR number")
	approve.Flags().Bool("dry-run", false, "Show what would be written without writing anything")
	approve.Flags().Bool(cliutil.JSONFlagName, false, "Emit a stable JSON envelope instead of human-readable text")
	cmd.AddCommand(approve)
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
	ListSessions(context.Context, *pb.ListSessionsRequest, rpc.SessionReadOptions) ([]*pb.Session, error)
}

// prApproveClient is what approve needs: the session read and the daemon's
// approve verb.
type prApproveClient interface {
	prSessionLister
	ApproveSessionPR(ctx context.Context, id string, dryRun bool) (*pb.VerifyApproveResult, error)
}

func (g *Group) runPrVerb(cmd *cobra.Command, prArg string, run func(context.Context, Client, prOptions) error) error {
	asJSON, _ := cmd.Flags().GetBool(cliutil.JSONFlagName)
	dryRun, _ := cmd.Flags().GetBool("dry-run")
	repo, _ := cmd.Flags().GetString("repo")
	if chat := strings.TrimSpace(g.Getenv("BOSS_AGENT_SESSION_ID")); chat != "" {
		// An agent must not approve the park it may itself be under.
		return cliutil.EmitJSONFailure(cmd, asJSON, rpcerr.New(rpcerr.PermissionDenied,
			fmt.Errorf("`boss pr %s` refuses to run inside agent chat %s: approving a verify park is the operator's call; run it from your own terminal or press [m] on the session in the TUI", cmd.Name(), chat)))
	}
	pr, err := parsePRNumber(prArg)
	if err != nil {
		return cliutil.EmitJSONFailure(cmd, asJSON, err)
	}
	c, err := g.newClient(cmd)
	if err != nil {
		return cliutil.EmitJSONFailure(cmd, asJSON, err)
	}
	return run(cmd.Context(), c, prOptions{pr: pr, repo: strings.TrimSpace(repo), dryRun: dryRun, asJSON: asJSON})
}

func parsePRNumber(arg string) (int, error) {
	n, err := strconv.Atoi(strings.TrimPrefix(strings.TrimSpace(arg), "#"))
	if err != nil || n < 1 {
		return 0, cliutil.CodedError(cliutil.CodeInvalidArgument, fmt.Errorf("PR number must be a positive integer, got %q", arg))
	}
	return n, nil
}

// resolvePRSession finds the one live session whose PR is pr, narrowed to repo
// (owner/repo, case-insensitive) when given. It returns the session and its
// repo slug, which the `--json` envelope reports.
func resolvePRSession(ctx context.Context, c prSessionLister, pr int, repo string) (*pb.Session, string, error) {
	sessions, err := c.ListSessions(ctx, &pb.ListSessionsRequest{}, rpc.SessionReadOptions{})
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
		return nil, "", cliutil.CodedError(cliutil.CodeNotFound, fmt.Errorf("no session found for PR #%d%s", pr, where))
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
	return nil, "", cliutil.CodedError(cliutil.CodeAmbiguousPR, fmt.Errorf("PR #%d%s matches %d sessions: %s%s", pr, where, len(matches), strings.Join(described, ", "), hint))
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
	return !opts.dryRun && rpcerr.KindOf(err) == rpcerr.DeadlineExceeded
}

func prApprove(ctx context.Context, cmd *cobra.Command, c prApproveClient, opts prOptions) error {
	s, slug, err := resolvePRSession(ctx, c, opts.pr, opts.repo)
	if err != nil {
		return cliutil.EmitJSONFailure(cmd, opts.asJSON, err)
	}
	r, err := c.ApproveSessionPR(ctx, s.GetId(), opts.dryRun)
	if verbTimedOut(err, opts) {
		// The record may already be on the PR, so its effect is unknown, not refused.
		err = fmt.Errorf("%w; an approval may already be posted", err)
	}
	if err != nil {
		return cliutil.EmitJSONFailure(cmd, opts.asJSON, fmt.Errorf("approve PR #%d: %w", opts.pr, err))
	}
	env := prApproveJSON{PR: opts.pr, Repo: slug, SessionID: s.GetId(), Result: approveResultJSON(r)}
	if !r.GetIsApproved() && !r.GetIsPlanned() {
		refusal := errors.New(prverdict.ApproveRefusal(opts.pr, r))
		if opts.asJSON {
			return cliutil.EmitJSONReportedFailure(cmd, env, refusal)
		}
		return refusal
	}
	if opts.asJSON {
		return cliutil.EmitJSON(cmd, env)
	}
	for _, line := range prverdict.ApproveLines(opts.pr, r) {
		if _, err := fmt.Fprintln(cmd.OutOrStdout(), line); err != nil {
			return err
		}
	}
	return nil
}
