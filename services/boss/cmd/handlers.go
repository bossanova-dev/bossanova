package main

import (
	"context"
	"fmt"
	"strings"
	"text/tabwriter"

	tea "charm.land/bubbletea/v2"
	"github.com/spf13/cobra"

	"github.com/recurser/boss/internal/client"
	"github.com/recurser/boss/internal/views"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

// newClient creates a daemon client using the default socket path.
func newClient() (*client.Client, error) {
	socketPath, err := client.DefaultSocketPath()
	if err != nil {
		return nil, fmt.Errorf("socket path: %w", err)
	}
	return client.New(socketPath), nil
}

func runTUI(_ *cobra.Command) error {
	c, err := newClient()
	if err != nil {
		return err
	}
	app := views.NewApp(c)
	p := tea.NewProgram(app)
	_, err = p.Run()
	return err
}

func runLS(cmd *cobra.Command) error {
	c, err := newClient()
	if err != nil {
		return err
	}

	repoID, _ := cmd.Flags().GetString("repo")
	archived, _ := cmd.Flags().GetBool("archived")
	stateStrs, _ := cmd.Flags().GetStringSlice("state")

	// Parse state filters.
	var states []pb.SessionState
	for _, s := range stateStrs {
		key := "SESSION_STATE_" + strings.ToUpper(s)
		if val, ok := pb.SessionState_value[key]; ok {
			states = append(states, pb.SessionState(val))
		} else {
			return fmt.Errorf("unknown state: %s", s)
		}
	}

	req := &pb.ListSessionsRequest{
		IncludeArchived: archived,
		States:          states,
	}
	if repoID != "" {
		req.RepoId = &repoID
	}

	ctx := context.Background()
	sessions, err := c.ListSessions(ctx, req)
	if err != nil {
		return fmt.Errorf("list sessions: %w", err)
	}

	if len(sessions) == 0 {
		fmt.Println("No sessions found.")
		return nil
	}

	w := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 2, 2, ' ', 0)
	fmt.Fprintln(w, "ID\tTITLE\tSTATE\tBRANCH\tPR\tCI")
	for _, sess := range sessions {
		id := sess.Id
		if len(id) > 8 {
			id = id[:8]
		}
		title := sess.Title
		if len(title) > 30 {
			title = title[:27] + "..."
		}
		state := views.StateLabel(sess.State)
		branch := sess.BranchName
		pr := "-"
		if sess.PrNumber != nil {
			pr = fmt.Sprintf("#%d", *sess.PrNumber)
		}
		ci := views.ChecksLabel(sess.LastCheckState)
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\n", id, title, state, branch, pr, ci)
	}
	return w.Flush()
}

func runNew(_ *cobra.Command) error {
	fmt.Println("boss new: create session (not yet implemented)")
	return nil
}

func runAttach(_ *cobra.Command, _ string) error {
	fmt.Println("boss attach: attach to session (not yet implemented)")
	return nil
}

func runRepoAdd(_ *cobra.Command) error {
	fmt.Println("boss repo add: add repository (not yet implemented)")
	return nil
}

func runRepoLS(_ *cobra.Command) error {
	fmt.Println("boss repo ls: list repositories (not yet implemented)")
	return nil
}

func runRepoRemove(_ *cobra.Command, _ string) error {
	fmt.Println("boss repo remove: remove repository (not yet implemented)")
	return nil
}

func runArchive(_ *cobra.Command, _ string) error {
	fmt.Println("boss archive: archive session (not yet implemented)")
	return nil
}

func runResurrect(_ *cobra.Command, _ string) error {
	fmt.Println("boss resurrect: resurrect session (not yet implemented)")
	return nil
}

func runTrashEmpty(_ *cobra.Command) error {
	fmt.Println("boss trash empty: empty trash (not yet implemented)")
	return nil
}
