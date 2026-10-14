package main

import (
	"context"
	"fmt"

	"github.com/recurser/boss/internal/client"
	"github.com/recurser/bossalib/trackerref"
	"github.com/spf13/cobra"
)

// sessionSetTrackerCmd builds `boss session set-tracker`. connectClient is the
// client factory (newClient in production), injected so tests can prove a bad
// id is rejected before the daemon is dialed.
func sessionSetTrackerCmd(connectClient func(*cobra.Command) (client.BossClient, error)) *cobra.Command {
	return &cobra.Command{
		Use:   "set-tracker <session-id> <issue-id>",
		Short: "Link a session to a tracker issue (sets or corrects the link)",
		Long: "Link a session to a tracker issue such as BOS-1539. The id must name an open issue " +
			"the repo's own tracker returns; the stored URL is the tracker's. An existing link is " +
			"overwritten. Local daemon only.",
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runSessionSetTracker(cmd, connectClient, args[0], args[1])
		},
	}
}

func runSessionSetTracker(cmd *cobra.Command, connectClient func(*cobra.Command) (client.BossClient, error), sessionID, issueID string) error {
	trackerID, ok := trackerref.ParseID(issueID)
	if !ok {
		return fmt.Errorf("%q is not a tracker issue id (e.g. ABC-123)", issueID)
	}
	c, err := connectClient(cmd)
	if err != nil {
		return err
	}
	ctx := context.Background()
	sessionID, err = resolveSessionID(c, ctx, sessionID)
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
