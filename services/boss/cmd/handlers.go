package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"charm.land/bubbles/v2/table"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/spf13/cobra"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/recurser/boss/internal/client"
	"github.com/recurser/boss/internal/daemon"
	"github.com/recurser/boss/internal/views"
	"github.com/recurser/bossalib/buildinfo"
	"github.com/recurser/bossalib/config"
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

	// Skip auto-start when socket is explicitly provided (test mode).
	if os.Getenv("BOSS_SOCKET") == "" {
		if err := daemon.EnsureRunning(socketPath); err != nil {
			return nil, fmt.Errorf("daemon: %w\nRun 'boss daemon install' to set up automatic startup, or start bossd manually", err)
		}
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
	app := views.NewApp(c, newOptionalAuthManager())
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
		t = truncateString(t, 30)
		titles[i] = t
		stateStrs2[i] = views.StateLabel(sess.State)
		branchStrs[i] = sess.BranchName
		if sess.PrNumber != nil {
			prText := fmt.Sprintf("#%d", *sess.PrNumber)
			if sess.PrUrl != nil {
				prStrs[i] = lipgloss.NewStyle().Hyperlink(*sess.PrUrl).Render(prText)
			} else {
				prStrs[i] = prText
			}
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
	app := views.NewApp(c, newOptionalAuthManager())
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
	app := views.NewApp(c, newOptionalAuthManager())
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
	app := views.NewApp(c, newOptionalAuthManager())
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
	sessionID, err = resolveSessionID(c, ctx, sessionID)
	if err != nil {
		return err
	}
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
	// Resolve prefix among archived sessions only.
	sessionID, err = resolveArchivedSessionID(c, ctx, sessionID)
	if err != nil {
		return err
	}
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

	st, _ := daemon.GetStatus()
	fmt.Printf("Daemon installed and started.\n")
	fmt.Printf("  bossd:   %s\n", bossdPath)
	if st != nil && st.ServicePath != "" {
		fmt.Printf("  service: %s\n", st.ServicePath)
	}
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
		fmt.Println("  Run 'boss daemon install' to set up the daemon.")
		return nil
	}

	if st.Running {
		fmt.Println("Daemon is running.")
		if st.PID > 0 {
			fmt.Printf("  PID:     %d\n", st.PID)
		}
	} else {
		fmt.Println("Daemon is installed but not running.")
	}
	if st.ServicePath != "" {
		fmt.Printf("  service: %s\n", st.ServicePath)
	}
	return nil
}

// resolveSessionID resolves a (possibly prefix) session ID to a full session ID.
// If the prefix is at least 32 characters (full UUID length), it is used directly.
// Otherwise, it searches all sessions (including archived) for a unique prefix match.
func resolveSessionID(c client.BossClient, ctx context.Context, prefix string) (string, error) {
	if len(prefix) >= 32 {
		return prefix, nil
	}
	sessions, err := c.ListSessions(ctx, &pb.ListSessionsRequest{IncludeArchived: true})
	if err != nil {
		return "", fmt.Errorf("list sessions: %w", err)
	}
	var matches []string
	for _, s := range sessions {
		if strings.HasPrefix(s.Id, prefix) {
			matches = append(matches, s.Id)
		}
	}
	switch len(matches) {
	case 0:
		return "", fmt.Errorf("no session found matching prefix %q", prefix)
	case 1:
		return matches[0], nil
	default:
		return "", fmt.Errorf("ambiguous prefix %q matches %d sessions", prefix, len(matches))
	}
}

// resolveArchivedSessionID is like resolveSessionID but only matches archived sessions.
func resolveArchivedSessionID(c client.BossClient, ctx context.Context, prefix string) (string, error) {
	sessions, err := c.ListSessions(ctx, &pb.ListSessionsRequest{IncludeArchived: true})
	if err != nil {
		return "", fmt.Errorf("list sessions: %w", err)
	}
	var matches []string
	for _, s := range sessions {
		if s.ArchivedAt != nil && strings.HasPrefix(s.Id, prefix) {
			matches = append(matches, s.Id)
		}
	}
	switch len(matches) {
	case 0:
		return "", fmt.Errorf("no archived session found matching prefix %q", prefix)
	case 1:
		return matches[0], nil
	default:
		return "", fmt.Errorf("ambiguous prefix %q matches %d archived sessions", prefix, len(matches))
	}
}

func runShow(cmd *cobra.Command, sessionID string) error {
	c, err := newClient(cmd)
	if err != nil {
		return err
	}

	ctx := context.Background()
	sessionID, err = resolveSessionID(c, ctx, sessionID)
	if err != nil {
		return err
	}

	sess, err := c.GetSession(ctx, sessionID)
	if err != nil {
		return fmt.Errorf("get session: %w", err)
	}

	// Print key-value header.
	id := sess.Id
	if len(id) > 8 {
		id = id[:8]
	}
	fmt.Printf("  ID:       %s\n", id)
	fmt.Printf("  Title:    %s\n", sess.Title)
	fmt.Printf("  Repo:     %s\n", sess.RepoDisplayName)
	fmt.Printf("  Branch:   %s\n", sess.BranchName)
	fmt.Printf("  State:    %s\n", views.StateLabel(sess.State))
	if sess.PrNumber != nil {
		fmt.Printf("  PR:       #%d\n", *sess.PrNumber)
	}
	if sess.GetWorktreePath() != "" {
		fmt.Printf("  Worktree: %s\n", sess.GetWorktreePath())
	}
	if sess.CreatedAt != nil {
		fmt.Printf("  Created:  %s\n", views.RelativeTime(sess.CreatedAt.AsTime()))
	}
	if sess.ArchivedAt != nil {
		fmt.Printf("  Archived: %s\n", views.RelativeTime(sess.ArchivedAt.AsTime()))
	}

	// List chats as a table.
	chats, err := c.ListChats(ctx, sessionID)
	if err != nil {
		return fmt.Errorf("list chats: %w", err)
	}
	if len(chats) == 0 {
		fmt.Println("\n  No chats.")
		return nil
	}

	fmt.Println()
	printChatsTable(cmd, chats)
	return nil
}

func runChats(cmd *cobra.Command, sessionID string) error {
	c, err := newClient(cmd)
	if err != nil {
		return err
	}

	ctx := context.Background()
	sessionID, err = resolveSessionID(c, ctx, sessionID)
	if err != nil {
		return err
	}

	chats, err := c.ListChats(ctx, sessionID)
	if err != nil {
		return fmt.Errorf("list chats: %w", err)
	}
	if len(chats) == 0 {
		fmt.Println("No chats found.")
		return nil
	}

	printChatsTable(cmd, chats)
	return nil
}

func printChatsTable(cmd *cobra.Command, chats []*pb.ClaudeChat) {
	ids := make([]string, len(chats))
	titles := make([]string, len(chats))
	createds := make([]string, len(chats))
	for i, chat := range chats {
		id := chat.ClaudeId
		if len(id) > 8 {
			id = id[:8]
		}
		ids[i] = id
		t := chat.Title
		if t == "" {
			t = "New chat"
		}
		t = truncateString(t, 50)
		titles[i] = t
		if chat.CreatedAt != nil {
			createds[i] = views.RelativeTime(chat.CreatedAt.AsTime())
		} else {
			createds[i] = "-"
		}
	}

	cols := []table.Column{
		{Title: "ID", Width: views.MaxColWidth("ID", ids, 8)},
		{Title: "TITLE", Width: views.MaxColWidth("TITLE", titles, 50)},
		{Title: "CREATED", Width: views.MaxColWidth("CREATED", createds, 12)},
	}

	rows := make([]table.Row, len(chats))
	for i := range chats {
		rows[i] = table.Row{ids[i], titles[i], createds[i]}
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
}

// truncateString truncates a string to maxRunes runes, appending "..." if truncated.
func truncateString(s string, maxRunes int) string {
	runes := []rune(s)
	if len(runes) <= maxRunes {
		return s
	}
	return string(runes[:maxRunes-3]) + "..."
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

// --- Trash ---

func runTrashLS(cmd *cobra.Command) error {
	c, err := newClient(cmd)
	if err != nil {
		return err
	}

	ctx := context.Background()
	sessions, err := c.ListSessions(ctx, &pb.ListSessionsRequest{IncludeArchived: true})
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
		id := sess.Id
		if len(id) > 8 {
			id = id[:8]
		}
		ids[i] = id
		t := sess.Title
		t = truncateString(t, 30)
		titles[i] = t
		repos[i] = sess.RepoDisplayName
		if sess.PrNumber != nil {
			prStrs[i] = fmt.Sprintf("#%d", *sess.PrNumber)
		} else {
			prStrs[i] = "-"
		}
		archiveds[i] = views.RelativeTime(sess.ArchivedAt.AsTime())
	}

	cols := []table.Column{
		{Title: "ID", Width: views.MaxColWidth("ID", ids, 8)},
		{Title: "TITLE", Width: views.MaxColWidth("TITLE", titles, 30)},
		{Title: "REPO", Width: views.MaxColWidth("REPO", repos, 20)},
		{Title: "PR", Width: views.MaxColWidth("PR", prStrs, 8)},
		{Title: "ARCHIVED", Width: views.MaxColWidth("ARCHIVED", archiveds, 12)},
	}

	rows := make([]table.Row, len(archived))
	for i := range archived {
		rows[i] = table.Row{ids[i], titles[i], repos[i], prStrs[i], archiveds[i]}
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

func runTrashDelete(cmd *cobra.Command, sessionID string) error {
	c, err := newClient(cmd)
	if err != nil {
		return err
	}

	ctx := context.Background()
	sessionID, err = resolveArchivedSessionID(c, ctx, sessionID)
	if err != nil {
		return err
	}

	yes, _ := cmd.Flags().GetBool("yes")
	if !yes {
		id := sessionID
		if len(id) > 8 {
			id = id[:8]
		}
		fmt.Printf("Permanently delete session %s? [y/N] ", id)
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

// --- Repo Update ---

func runRepoUpdate(cmd *cobra.Command, repoID string) error {
	c, err := newClient(cmd)
	if err != nil {
		return err
	}

	req := &pb.UpdateRepoRequest{Id: repoID}
	anyChanged := false

	if cmd.Flags().Changed("name") {
		v, _ := cmd.Flags().GetString("name")
		req.DisplayName = &v
		anyChanged = true
	}
	if cmd.Flags().Changed("setup-script") {
		v, _ := cmd.Flags().GetString("setup-script")
		req.SetupScript = &v
		anyChanged = true
	}
	if cmd.Flags().Changed("merge-strategy") {
		v, _ := cmd.Flags().GetString("merge-strategy")
		switch v {
		case "merge", "rebase", "squash":
			req.MergeStrategy = &v
		default:
			return fmt.Errorf("invalid merge strategy %q (use merge, rebase, or squash)", v)
		}
		anyChanged = true
	}

	// Boolean flag pairs.
	boolPairs := []struct {
		enable, disable string
		setter          func(v bool)
	}{
		{"auto-merge", "no-auto-merge", func(v bool) { req.CanAutoMerge = &v }},
		{"auto-merge-dependabot", "no-auto-merge-dependabot", func(v bool) { req.CanAutoMergeDependabot = &v }},
		{"auto-address-reviews", "no-auto-address-reviews", func(v bool) { req.CanAutoAddressReviews = &v }},
		{"auto-resolve-conflicts", "no-auto-resolve-conflicts", func(v bool) { req.CanAutoResolveConflicts = &v }},
	}
	for _, bp := range boolPairs {
		enableChanged := cmd.Flags().Changed(bp.enable)
		disableChanged := cmd.Flags().Changed(bp.disable)
		if enableChanged && disableChanged {
			return fmt.Errorf("cannot use both --%s and --%s", bp.enable, bp.disable)
		}
		if enableChanged {
			bp.setter(true)
			anyChanged = true
		}
		if disableChanged {
			bp.setter(false)
			anyChanged = true
		}
	}

	if !anyChanged {
		return fmt.Errorf("no flags provided — use --name, --setup-script, --merge-strategy, or boolean flags")
	}

	ctx := context.Background()
	repo, err := c.UpdateRepo(ctx, req)
	if err != nil {
		return fmt.Errorf("update repo: %w", err)
	}

	fmt.Printf("Repository updated.\n")
	fmt.Printf("  ID:       %s\n", repo.Id)
	fmt.Printf("  Name:     %s\n", repo.DisplayName)
	fmt.Printf("  Strategy: %s\n", repo.MergeStrategy)
	if repo.SetupScript != nil {
		fmt.Printf("  Setup:    %s\n", *repo.SetupScript)
	}
	fmt.Printf("  Auto-merge:            %v\n", repo.CanAutoMerge)
	fmt.Printf("  Auto-merge Dependabot: %v\n", repo.CanAutoMergeDependabot)
	fmt.Printf("  Auto-address reviews:  %v\n", repo.CanAutoAddressReviews)
	fmt.Printf("  Auto-resolve conflicts: %v\n", repo.CanAutoResolveConflicts)
	return nil
}

// --- Settings ---

func runSettings(cmd *cobra.Command) error {
	s, err := config.Load()
	if err != nil {
		return fmt.Errorf("load settings: %w", err)
	}

	// If no flags provided, display current settings.
	anyChanged := cmd.Flags().Changed("skip-permissions") ||
		cmd.Flags().Changed("no-skip-permissions") ||
		cmd.Flags().Changed("worktree-dir") ||
		cmd.Flags().Changed("poll-interval")

	if !anyChanged {
		fmt.Printf("  Skip permissions: %v\n", s.DangerouslySkipPermissions)
		fmt.Printf("  Worktree dir:     %s\n", s.WorktreeBaseDir)
		interval := "30 (default)"
		if s.PollIntervalSeconds > 0 {
			interval = strconv.Itoa(s.PollIntervalSeconds)
		}
		fmt.Printf("  Poll interval:    %s seconds\n", interval)
		return nil
	}

	// Apply changes.
	if cmd.Flags().Changed("skip-permissions") && cmd.Flags().Changed("no-skip-permissions") {
		return fmt.Errorf("cannot use both --skip-permissions and --no-skip-permissions")
	}
	if cmd.Flags().Changed("skip-permissions") {
		s.DangerouslySkipPermissions = true
	}
	if cmd.Flags().Changed("no-skip-permissions") {
		s.DangerouslySkipPermissions = false
	}
	if cmd.Flags().Changed("worktree-dir") {
		v, _ := cmd.Flags().GetString("worktree-dir")
		if v == "" {
			return fmt.Errorf("worktree-dir cannot be empty")
		}
		s.WorktreeBaseDir = v
	}
	if cmd.Flags().Changed("poll-interval") {
		v, _ := cmd.Flags().GetInt("poll-interval")
		if v < 0 {
			return fmt.Errorf("poll-interval must be non-negative")
		}
		s.PollIntervalSeconds = v
	}

	if err := config.Save(s); err != nil {
		return fmt.Errorf("save settings: %w", err)
	}

	fmt.Println("Settings updated.")
	return nil
}

// --- Config Init ---

func runConfigInit(cmd *cobra.Command) error {
	pluginDir, _ := cmd.Flags().GetString("plugin-dir")

	var foundPlugins map[string]string // name -> path

	if pluginDir != "" {
		// Explicit --plugin-dir provided: validate and scan it.
		info, err := os.Stat(pluginDir)
		if err != nil {
			if os.IsNotExist(err) {
				return fmt.Errorf("plugin directory not found: %s", pluginDir)
			}
			return fmt.Errorf("cannot access plugin directory: %w", err)
		}
		if !info.IsDir() {
			return fmt.Errorf("plugin-dir must be a directory: %s", pluginDir)
		}

		absPluginDir, err := filepath.Abs(pluginDir)
		if err != nil {
			return fmt.Errorf("resolve plugin directory: %w", err)
		}

		entries, err := os.ReadDir(absPluginDir)
		if err != nil {
			return fmt.Errorf("read plugin directory: %w", err)
		}

		foundPlugins = make(map[string]string)
		for _, e := range entries {
			if e.IsDir() || !strings.HasPrefix(e.Name(), "bossd-plugin-") {
				continue
			}
			foundPlugins[e.Name()] = filepath.Join(absPluginDir, e.Name())
		}
	} else {
		// No --plugin-dir: try auto-discovery relative to binary.
		discovered := config.DiscoverPlugins()
		if len(discovered) > 0 {
			foundPlugins = make(map[string]string)
			for _, p := range discovered {
				foundPlugins["bossd-plugin-"+p.Name] = p.Path
			}
		}
	}

	if len(foundPlugins) == 0 {
		if pluginDir != "" {
			fmt.Fprintf(os.Stderr, "Warning: no plugin binaries found in %s\n", pluginDir)
		} else {
			fmt.Fprintf(os.Stderr, "Warning: no plugin binaries found (use --plugin-dir to specify location)\n")
		}
		return nil
	}

	// Load existing settings
	s, err := config.Load()
	if err != nil {
		return fmt.Errorf("load settings: %w", err)
	}

	// Create or update plugin entries
	pluginMap := make(map[string]int)
	for i := range s.Plugins {
		pluginMap[s.Plugins[i].Name] = i
	}

	for name, path := range foundPlugins {
		// Extract plugin name from binary name (bossd-plugin-foo -> foo)
		pluginName := strings.TrimPrefix(name, "bossd-plugin-")

		if idx, ok := pluginMap[pluginName]; ok {
			// Update existing entry path and version, but preserve Enabled state
			// so we don't re-enable plugins the user explicitly disabled.
			s.Plugins[idx].Path = path
			s.Plugins[idx].Version = buildinfo.Version
		} else {
			// Add new entry (default to enabled for newly discovered plugins)
			newPlugin := config.PluginConfig{
				Name:    pluginName,
				Path:    path,
				Enabled: true,
				Version: buildinfo.Version,
			}
			s.Plugins = append(s.Plugins, newPlugin)
			pluginMap[pluginName] = len(s.Plugins) - 1
		}
	}

	// Save settings
	if err := config.Save(s); err != nil {
		return fmt.Errorf("save settings: %w", err)
	}

	settingsPath, _ := config.Path()
	fmt.Printf("Configured %d plugins in %s\n", len(foundPlugins), settingsPath)
	return nil
}
