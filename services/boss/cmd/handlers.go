package main

import (
	"context"
	"fmt"
	"strings"
	"text/tabwriter"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/spf13/cobra"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/recurser/boss/internal/client"
	"github.com/recurser/boss/internal/views"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

// newClient creates either a local or remote client depending on the --remote flag.
func newClient(cmd *cobra.Command) (client.BossClient, error) {
	remote, _ := cmd.Root().Flags().GetString("remote")
	if remote != "" {
		return newRemoteClient(remote)
	}
	socketPath, err := client.DefaultSocketPath()
	if err != nil {
		return nil, fmt.Errorf("socket path: %w", err)
	}
	return client.NewLocal(socketPath), nil
}

// newRemoteClient creates a RemoteClient with a JWT from the keychain.
func newRemoteClient(baseURL string) (client.BossClient, error) {
	mgr, err := newAuthManager()
	if err != nil {
		return nil, fmt.Errorf("auth: %w (run 'boss login' first)", err)
	}
	token, err := mgr.AccessToken(context.Background())
	if err != nil {
		return nil, fmt.Errorf("access token: %w (run 'boss login' first)", err)
	}
	return client.NewRemote(baseURL, token), nil
}

func runTUI(cmd *cobra.Command) error {
	c, err := newClient(cmd)
	if err != nil {
		return err
	}
	app := views.NewApp(c)
	p := tea.NewProgram(app)
	_, err = p.Run()
	return err
}

func runLS(cmd *cobra.Command) error {
	c, err := newClient(cmd)
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
	if _, err := fmt.Fprintln(w, "ID\tTITLE\tSTATE\tBRANCH\tPR\tCI"); err != nil {
		return err
	}
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
		if _, err := fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\n", id, title, state, branch, pr, ci); err != nil {
			return err
		}
	}
	return w.Flush()
}

func runNew(cmd *cobra.Command) error {
	c, err := newClient(cmd)
	if err != nil {
		return err
	}
	app := views.NewApp(c)
	app.SetInitialView(views.ViewNewSession)
	p := tea.NewProgram(app)
	_, err = p.Run()
	return err
}

func runAttach(cmd *cobra.Command, sessionID string) error {
	c, err := newClient(cmd)
	if err != nil {
		return err
	}
	app := views.NewApp(c)
	app.SetInitialView(views.ViewAttach)
	app.SetAttachSession(sessionID)
	p := tea.NewProgram(app)
	_, err = p.Run()
	return err
}

func runRepoAdd(cmd *cobra.Command) error {
	c, err := newClient(cmd)
	if err != nil {
		return err
	}
	app := views.NewApp(c)
	app.SetInitialView(views.ViewRepoAdd)
	p := tea.NewProgram(app)
	_, err = p.Run()
	return err
}

func runRepoLS(cmd *cobra.Command) error {
	c, err := newClient(cmd)
	if err != nil {
		return err
	}

	ctx := context.Background()
	repos, err := c.ListRepos(ctx)
	if err != nil {
		return fmt.Errorf("list repos: %w", err)
	}

	if len(repos) == 0 {
		fmt.Println("No repositories registered.")
		return nil
	}

	w := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 2, 2, ' ', 0)
	if _, err := fmt.Fprintln(w, "ID\tNAME\tPATH\tBRANCH\tSETUP"); err != nil {
		return err
	}
	for _, repo := range repos {
		id := repo.Id
		if len(id) > 8 {
			id = id[:8]
		}
		setup := "-"
		if repo.SetupScript != nil {
			setup = *repo.SetupScript
		}
		if _, err := fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n", id, repo.DisplayName, repo.LocalPath, repo.DefaultBaseBranch, setup); err != nil {
			return err
		}
	}
	return w.Flush()
}

func runRepoRemove(cmd *cobra.Command, id string) error {
	c, err := newClient(cmd)
	if err != nil {
		return err
	}
	ctx := context.Background()
	if err := c.RemoveRepo(ctx, id); err != nil {
		return fmt.Errorf("remove repo: %w", err)
	}
	fmt.Printf("Repository %s removed.\n", id)
	return nil
}

func runArchive(cmd *cobra.Command, sessionID string) error {
	c, err := newClient(cmd)
	if err != nil {
		return err
	}
	ctx := context.Background()
	sess, err := c.ArchiveSession(ctx, sessionID)
	if err != nil {
		return fmt.Errorf("archive session: %w", err)
	}
	fmt.Printf("Session %s archived (%s).\n", sess.Id, sess.Title)
	return nil
}

func runResurrect(cmd *cobra.Command, sessionID string) error {
	c, err := newClient(cmd)
	if err != nil {
		return err
	}
	ctx := context.Background()
	sess, err := c.ResurrectSession(ctx, sessionID)
	if err != nil {
		return fmt.Errorf("resurrect session: %w", err)
	}
	fmt.Printf("Session %s resurrected (%s).\n", sess.Id, sess.Title)
	return nil
}

func runTrashEmpty(cmd *cobra.Command) error {
	c, err := newClient(cmd)
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

// parseDuration parses a human-friendly duration like "30d", "2w", "1h".
func parseDuration(s string) (time.Duration, error) {
	if len(s) < 2 {
		return 0, fmt.Errorf("invalid duration: %s", s)
	}

	unit := s[len(s)-1]
	numStr := s[:len(s)-1]
	var n int
	if _, err := fmt.Sscanf(numStr, "%d", &n); err != nil {
		return 0, fmt.Errorf("invalid duration number: %s", numStr)
	}

	switch unit {
	case 'h':
		return time.Duration(n) * time.Hour, nil
	case 'd':
		return time.Duration(n) * 24 * time.Hour, nil
	case 'w':
		return time.Duration(n) * 7 * 24 * time.Hour, nil
	default:
		return 0, fmt.Errorf("unknown duration unit: %c (use h, d, or w)", unit)
	}
}
