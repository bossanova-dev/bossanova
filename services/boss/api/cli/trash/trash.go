package trash

import (
	"context"
	"fmt"
	"time"

	"github.com/recurser/boss/internal/domain/rpc"

	"github.com/recurser/boss/api/cli/cliutil"

	"charm.land/bubbles/v2/table"
	"github.com/recurser/boss/internal/domain/display"

	"github.com/spf13/cobra"
	"google.golang.org/protobuf/types/known/timestamppb"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

func (g *group) runResurrect(cmd *cobra.Command, sessionID string) error {
	c, err := g.newClient(cmd)
	if err != nil {
		return err
	}
	ctx := context.Background()
	// Resolve prefix among archived sessions only.
	sessionID, err = cliutil.ResolveArchivedSessionID(c, ctx, sessionID)
	if err != nil {
		return err
	}
	stream, err := c.ResurrectSession(ctx, sessionID)
	if err != nil {
		return fmt.Errorf("resurrect session: %w", err)
	}
	defer func() { _ = stream.Close() }()

	// Setup output goes to stderr, matching `boss new` — the resurrected line on
	// stdout is the scripting surface.
	var sess *pb.Session
	setupError := ""
	for stream.Receive() {
		msg := stream.Msg()
		if so := msg.GetSetupOutput(); so != nil {
			cliutil.PrintSetupOutput(cmd.ErrOrStderr(), so.GetText())
		}
		if r := msg.GetSessionResurrected(); r != nil {
			sess = r.GetSession()
			setupError = r.GetSetupError()
		}
	}
	if err := stream.Err(); err != nil {
		return fmt.Errorf("resurrect session: %w", err)
	}
	if sess == nil {
		return fmt.Errorf("resurrect session: daemon did not return a session")
	}
	fmt.Printf("Session %s resurrected (%s).\n", sess.Id, sess.Title)
	if setupError != "" {
		// Non-fatal: the session IS back. Say so on stderr rather than failing,
		// so the user knows dependencies may be missing (BOS-984).
		_, _ = fmt.Fprintf(cmd.ErrOrStderr(),
			"warning: the repo setup script failed, so this worktree's dependencies may be missing: %s\n",
			setupError)
	}
	return nil
}

func (g *group) runTrashEmpty(cmd *cobra.Command) error {
	c, err := g.newClient(cmd)
	if err != nil {
		return err
	}

	req := &pb.EmptyTrashRequest{}

	olderThan, _ := cmd.Flags().GetString("older-than")
	if olderThan != "" {
		d, err := parseDuration(olderThan)
		if err != nil {
			return fmt.Errorf("invalid --older-than: %w", err)
		}
		cutoff := time.Now().Add(-d)
		ts := timestamppb.New(cutoff)
		req.OlderThan = ts
	}

	ctx := context.Background()
	count, err := c.EmptyTrash(ctx, req)
	if err != nil {
		return fmt.Errorf("empty trash: %w", err)
	}
	if count == 0 {
		fmt.Println("No archived sessions to delete.")
	} else {
		fmt.Printf("Deleted %d archived session(s).\n", count)
	}
	return nil
}

func (g *group) runTrashLS(cmd *cobra.Command) error {
	c, err := g.newClient(cmd)
	if err != nil {
		return err
	}

	ctx := context.Background()
	sessions, err := c.ListSessions(ctx, &pb.ListSessionsRequest{IncludeArchived: true}, rpc.SessionReadOptions{})
	if err != nil {
		return fmt.Errorf("list sessions: %w", err)
	}

	// Filter to archived only.
	var archived []*pb.Session
	for _, s := range sessions {
		if s.ArchivedAt != nil {
			archived = append(archived, s)
		}
	}

	if len(archived) == 0 {
		fmt.Println("Trash is empty.")
		return nil
	}

	ids := make([]string, len(archived))
	titles := make([]string, len(archived))
	repos := make([]string, len(archived))
	prStrs := make([]string, len(archived))
	archiveds := make([]string, len(archived))
	for i, sess := range archived {
		ids[i] = sess.Id
		t := sess.Title
		t = cliutil.TruncateString(t, 30)
		titles[i] = t
		repos[i] = sess.RepoDisplayName
		if sess.PrNumber != nil {
			prStrs[i] = fmt.Sprintf("#%d", *sess.PrNumber)
		} else {
			prStrs[i] = "-"
		}
		archiveds[i] = display.RelativeTime(sess.ArchivedAt.AsTime())
	}

	cols := []table.Column{
		{Title: "ID", Width: display.MaxColWidth("ID", ids, 0)},
		{Title: "TITLE", Width: display.MaxColWidth("TITLE", titles, 30)},
		{Title: "REPO", Width: display.MaxColWidth("REPO", repos, 20)},
		{Title: "PR", Width: display.MaxColWidth("PR", prStrs, 8)},
		{Title: "ARCHIVED", Width: display.MaxColWidth("ARCHIVED", archiveds, 12)},
	}

	rows := make([]table.Row, len(archived))
	for i := range archived {
		rows[i] = table.Row{ids[i], titles[i], repos[i], prStrs[i], archiveds[i]}
	}

	t := table.New(
		table.WithColumns(cols),
		table.WithRows(rows),
		table.WithHeight(len(rows)+1),
		table.WithWidth(display.CLIColumnsWidth(cols)),
		table.WithStyles(display.CLITableStyles()),
		table.WithFocused(false),
	)
	_, _ = fmt.Fprintln(cmd.OutOrStdout(), t.View())
	return nil
}

func (g *group) runTrashDelete(cmd *cobra.Command, sessionID string) error {
	c, err := g.newClient(cmd)
	if err != nil {
		return err
	}

	ctx := context.Background()
	sessionID, err = cliutil.ResolveArchivedSessionID(c, ctx, sessionID)
	if err != nil {
		return err
	}

	yes, _ := cmd.Flags().GetBool("yes")
	if !yes {
		fmt.Printf("Permanently delete session %s? [y/N] ", sessionID)
		var answer string
		if _, err := fmt.Scanln(&answer); err != nil || (answer != "y" && answer != "Y") {
			fmt.Println("Cancelled.")
			return nil
		}
	}

	if err := c.RemoveSession(ctx, sessionID); err != nil {
		return fmt.Errorf("delete session: %w", err)
	}
	fmt.Printf("Session %s permanently deleted.\n", sessionID)
	return nil
}
