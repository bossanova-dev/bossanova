package main

import (
	"context"
	"fmt"
	"strings"
	"time"

	"charm.land/bubbles/v2/table"
	tea "charm.land/bubbletea/v2"
	"github.com/spf13/cobra"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/recurser/boss/internal/client"
	"github.com/recurser/boss/internal/daemon"
	"github.com/recurser/boss/internal/views"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

// newClient creates either a local or remote client depending on the --remote flag.
// For local connections, it ensures the daemon is running first.
func newClient(cmd *cobra.Command) (client.BossClient, error) {
	remote, _ := cmd.Root().Flags().GetString("remote")
	if remote != "" {
		return newRemoteClient(remote)
	}
	socketPath, err := client.DefaultSocketPath()
	if err != nil {
		return nil, fmt.Errorf("socket path: %w", err)
	}

	// Auto-start daemon if not running.
	if err := daemon.EnsureRunning(socketPath); err != nil {
		return nil, fmt.Errorf("daemon: %w\nRun 'boss daemon install' to set up automatic startup, or start bossd manually", err)
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

	ids := make([]string, len(sessions))
	titles := make([]string, len(sessions))
	stateStrs2 := make([]string, len(sessions))
	branchStrs := make([]string, len(sessions))
	prStrs := make([]string, len(sessions))
	for i, sess := range sessions {
		id := sess.Id
		if len(id) > 8 {
			id = id[:8]
		}
		ids[i] = id
		t := sess.Title
		if len(t) > 30 {
			t = t[:27] + "..."
		}
		titles[i] = t
		stateStrs2[i] = views.StateLabel(sess.State)
		branchStrs[i] = sess.BranchName
		if sess.PrNumber != nil {
			prStrs[i] = fmt.Sprintf("#%d", *sess.PrNumber)
		} else {
			prStrs[i] = "-"
		}
	}

	cols := []table.Column{
		{Title: "ID", Width: views.MaxColWidth("ID", ids, 8)},
		{Title: "TITLE", Width: views.MaxColWidth("TITLE", titles, 30)},
		{Title: "STATE", Width: views.MaxColWidth("STATE", stateStrs2, 14)},
		{Title: "BRANCH", Width: views.MaxColWidth("BRANCH", branchStrs, 40)},
		{Title: "PR", Width: views.MaxColWidth("PR", prStrs, 8)},
	}

	rows := make([]table.Row, len(sessions))
	for i := range sessions {
		rows[i] = table.Row{ids[i], titles[i], stateStrs2[i], branchStrs[i], prStrs[i]}
	}

	t := table.New(
		table.WithColumns(cols),
		table.WithRows(rows),
		table.WithHeight(len(rows)+1),
		table.WithWidth(views.CLIColumnsWidth(cols)),
		table.WithStyles(views.CLITableStyles()),
		table.WithFocused(false),
	)
	_, _ = fmt.Fprintln(cmd.OutOrStdout(), t.View())
	return nil
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
	app.SetAttachSession(sessionID, "")
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

	ids := make([]string, len(repos))
	names := make([]string, len(repos))
	paths := make([]string, len(repos))
	branches := make([]string, len(repos))
	setups := make([]string, len(repos))
	for i, repo := range repos {
		id := repo.Id
		if len(id) > 8 {
			id = id[:8]
		}
		ids[i] = id
		names[i] = repo.DisplayName
		paths[i] = repo.LocalPath
		branches[i] = repo.DefaultBaseBranch
		if repo.SetupScript != nil {
			setups[i] = *repo.SetupScript
		} else {
			setups[i] = "-"
		}
	}

	cols := []table.Column{
		{Title: "ID", Width: views.MaxColWidth("ID", ids, 8)},
		{Title: "NAME", Width: views.MaxColWidth("NAME", names, 30)},
		{Title: "PATH", Width: views.MaxColWidth("PATH", paths, 60)},
		{Title: "BRANCH", Width: views.MaxColWidth("BRANCH", branches, 30)},
		{Title: "SETUP", Width: views.MaxColWidth("SETUP", setups, 40)},
	}

	rows := make([]table.Row, len(repos))
	for i := range repos {
		rows[i] = table.Row{ids[i], names[i], paths[i], branches[i], setups[i]}
	}

	t := table.New(
		table.WithColumns(cols),
		table.WithRows(rows),
		table.WithHeight(len(rows)+1),
		table.WithWidth(views.CLIColumnsWidth(cols)),
		table.WithStyles(views.CLITableStyles()),
		table.WithFocused(false),
	)
	_, _ = fmt.Fprintln(cmd.OutOrStdout(), t.View())
	return nil
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

// --- Daemon Management ---

func runDaemonInstall(_ *cobra.Command) error {
	bossdPath, err := daemon.ResolveBossdPath()
	if err != nil {
		return err
	}

	if err := daemon.Install(bossdPath); err != nil {
		return fmt.Errorf("install daemon: %w", err)
	}

	fmt.Printf("Daemon installed and started.\n")
	fmt.Printf("  bossd: %s\n", bossdPath)
	plistPath, _ := daemon.PlistPath()
	fmt.Printf("  plist: %s\n", plistPath)
	return nil
}

func runDaemonUninstall(_ *cobra.Command) error {
	if err := daemon.Uninstall(); err != nil {
		return fmt.Errorf("uninstall daemon: %w", err)
	}
	fmt.Println("Daemon uninstalled.")
	return nil
}

func runDaemonStatus(_ *cobra.Command) error {
	st, err := daemon.GetStatus()
	if err != nil {
		return fmt.Errorf("daemon status: %w", err)
	}

	if !st.Installed {
		fmt.Println("Daemon is not installed.")
		fmt.Println("  Run 'boss daemon install' to set up the LaunchAgent.")
		return nil
	}

	if st.Running {
		fmt.Println("Daemon is running.")
		if st.PID > 0 {
			fmt.Printf("  PID:   %d\n", st.PID)
		}
	} else {
		fmt.Println("Daemon is installed but not running.")
	}
	fmt.Printf("  Plist: %s\n", st.PlistPath)
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
