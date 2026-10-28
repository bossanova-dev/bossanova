package session

import (
	"context"
	"fmt"
	"strings"

	"github.com/recurser/boss/api/cli/cliutil"

	"github.com/recurser/bossalib/contract/daemon/trackerref"
	"github.com/spf13/cobra"
)

// sessionSetTrackerCmd builds `boss session set-tracker`. connectClient is the
// client factory (newClient in production), injected so tests can prove a bad
// id is rejected before the daemon is dialed.
func (g *Group) sessionSetTrackerCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "set-tracker <session-id> <issue-id>",
		Short: "Link a session to a tracker issue (sets or corrects the link)",
		Long: "Link a session to a tracker issue such as BOS-1539. The id must name an open issue " +
			"the repo's own tracker returns; the stored URL is the tracker's. An existing link is " +
			"overwritten. GitHub-tracker repos accept GH-N, #N, N or the issue URL; " +
			"quote #N in the shell. For example: boss session set-tracker abc123 GH-12. Local daemon only.",
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			return g.runSessionSetTracker(cmd, args[0], args[1])
		},
	}
}

func (g *Group) runSessionSetTracker(cmd *cobra.Command, sessionID, issueID string) error {
	trackerID := strings.TrimSpace(issueID)
	canonicalID, linearOK := trackerref.ParseID(trackerID)
	_, githubOK := trackerref.ParseGitHubIssue(trackerID)
	if !linearOK && !githubOK {
		return fmt.Errorf("%q is not a tracker issue id (e.g. ABC-123, GH-12 or #12)", issueID)
	}
	if linearOK {
		trackerID = canonicalID
	}
	c, err := g.newClient(cmd)
	if err != nil {
		return err
	}
	ctx := context.Background()
	sessionID, err = cliutil.ResolveSessionID(c, ctx, sessionID)
	if err != nil {
		return err
	}
	sess, err := c.SetSessionTracker(ctx, sessionID, trackerID)
	if err != nil {
		return fmt.Errorf("set tracker: %w", err)
	}
	_, err = fmt.Fprintf(cmd.OutOrStdout(), "Session %s linked to %s (%s).\n", sess.GetId(), sess.GetTrackerId(), sess.GetTrackerUrl())
	return err
}
