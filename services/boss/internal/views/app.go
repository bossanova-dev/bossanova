// Package views implements the Bubbletea TUI for the boss CLI.
package views

import (
	"context"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/recurser/boss/internal/client"
	bosspty "github.com/recurser/boss/internal/pty"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// View identifies which screen is currently active.
type View int

const (
	ViewHome View = iota
	ViewNewSession
	ViewAttach
	ViewChatPicker
	ViewRepoAdd
	ViewRepoList
	ViewRepoSettings
	ViewTrash
	ViewSettings
	ViewAutopilot
)

// App is the root Bubbletea model that manages view routing and shared state.
type App struct {
	client          client.BossClient
	ctx             context.Context
	manager         *bosspty.Manager
	activeView      View
	home            HomeModel
	newSession      NewSessionModel
	chatPicker      ChatPickerModel
	repoAdd         RepoAddModel
	repoList        RepoListModel
	repoSettings    RepoSettingsModel
	trash           TrashModel
	settings        SettingsModel
	autopilot       AutopilotModel
	attach          AttachModel
	attachOrigin    View   // remembers how the user entered the attach view
	attachSessionID string // remembers which session to highlight on return
	width           int
	height          int
	quitting        bool

	// heartbeatStop signals the background heartbeat goroutine to exit.
	// The goroutine runs independently of the Bubbletea event loop so that
	// heartbeats continue even during tea.Exec (when the loop is suspended).
	heartbeatStop chan struct{}
}

// NewApp creates a new App wired to the daemon client.
// It starts a background heartbeat goroutine that keeps running even during
// tea.Exec (when the Bubbletea event loop is suspended).
func NewApp(c client.BossClient) App {
	ctx := context.Background()
	mgr := bosspty.NewManager()
	home := NewHomeModel(c, ctx, mgr)
	stop := make(chan struct{})
	a := App{
		client:        c,
		ctx:           ctx,
		manager:       mgr,
		activeView:    ViewHome,
		home:          home,
		heartbeatStop: stop,
	}
	go func() {
		ticker := time.NewTicker(heartbeatInterval)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				return
			case <-ticker.C:
				a.sendHeartbeat()
			}
		}
	}()
	return a
}

// SetInitialView overrides the default initial view before running the program.
func (a *App) SetInitialView(v View) {
	a.activeView = v
	switch v {
	case ViewNewSession:
		a.newSession = NewNewSessionModel(a.client, a.ctx)
	case ViewRepoAdd:
		a.repoAdd = NewRepoAddModel(a.client, a.ctx)
	case ViewRepoList:
		a.repoList = NewRepoListModel(a.client, a.ctx)
	default:
	}
}

// SetAttachSession sets the session ID to attach to. Must be called after SetInitialView(ViewAttach).
func (a *App) SetAttachSession(sessionID, resumeID string) {
	a.attach = NewAttachModel(a.client, a.ctx, a.manager, sessionID, resumeID)
}

func (a App) Init() tea.Cmd {
	var viewCmd tea.Cmd
	switch a.activeView {
	case ViewNewSession:
		viewCmd = a.newSession.Init()
	case ViewChatPicker:
		viewCmd = a.chatPicker.Init()
	case ViewRepoAdd:
		viewCmd = a.repoAdd.Init()
	case ViewRepoList:
		viewCmd = a.repoList.Init()
	case ViewAttach:
		viewCmd = a.attach.Init()
	default:
		viewCmd = a.home.Init()
	}
	return viewCmd
}

// switchViewMsg requests the app to switch to a different view.
type switchViewMsg struct {
	view      View
	sessionID string // used for ViewAttach and ViewChatPicker
	resumeID  string // Claude Code session UUID to resume (ViewAttach only)
	origin    View   // tracks where navigation came from (for back-routing)
}

const heartbeatInterval = 3 * time.Second

// sendHeartbeat pushes local PTY process statuses to the daemon.
func (a *App) sendHeartbeat() {
	if a.manager == nil {
		return
	}
	allStatuses := a.manager.AllStatuses()
	if len(allStatuses) == 0 {
		return
	}
	reports := make([]*pb.ChatStatusReport, 0, len(allStatuses))
	for claudeID, info := range allStatuses {
		report := &pb.ChatStatusReport{
			ClaudeId: claudeID,
			Status:   statusToProto(info.Status),
		}
		if !info.LastWrite.IsZero() {
			report.LastOutputAt = timestamppb.New(info.LastWrite)
		}
		reports = append(reports, report)
	}
	_ = a.client.ReportChatStatus(a.ctx, reports)
}

// statusToProto converts a bosspty.Status* string to a protobuf ChatStatus.
func statusToProto(s string) pb.ChatStatus {
	switch s {
	case bosspty.StatusWorking:
		return pb.ChatStatus_CHAT_STATUS_WORKING
	case bosspty.StatusIdle:
		return pb.ChatStatus_CHAT_STATUS_IDLE
	case bosspty.StatusQuestion:
		return pb.ChatStatus_CHAT_STATUS_QUESTION
	default:
		return pb.ChatStatus_CHAT_STATUS_STOPPED
	}
}

func (a App) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		a.width = msg.Width
		a.height = msg.Height
		a.home.width = msg.Width
		a.home.height = msg.Height
		a.newSession.width = msg.Width
		a.repoAdd.width = msg.Width
		a.repoList.width = msg.Width
		a.repoList.height = msg.Height
		a.repoSettings.width = msg.Width
		a.trash.width = msg.Width
		a.trash.height = msg.Height
		a.chatPicker.width = msg.Width
		a.chatPicker.height = msg.Height
		a.settings.width = msg.Width
		a.autopilot.width = msg.Width
		a.autopilot.height = msg.Height

	case tea.KeyMsg:
		switch msg.String() {
		case "ctrl+c":
			a.quitting = true
			if a.heartbeatStop != nil {
				close(a.heartbeatStop)
				a.heartbeatStop = nil
			}
			return a, tea.Quit
		}

	case switchViewMsg:
		a.activeView = msg.view
		switch msg.view {
		case ViewNewSession:
			a.newSession = NewNewSessionModel(a.client, a.ctx)
			a.newSession.width = a.width
			return a, a.newSession.Init()
		case ViewChatPicker:
			a.chatPicker = NewChatPickerModel(a.client, a.ctx, a.manager, msg.sessionID, "")
			a.chatPicker.width = a.width
			a.chatPicker.height = a.height
			return a, a.chatPicker.Init()
		case ViewRepoAdd:
			a.repoAdd = NewRepoAddModel(a.client, a.ctx)
			a.repoAdd.width = a.width
			return a, a.repoAdd.Init()
		case ViewRepoList:
			a.repoList = NewRepoListModel(a.client, a.ctx)
			a.repoList.width = a.width
			a.repoList.height = a.height
			return a, a.repoList.Init()
		case ViewRepoSettings:
			a.repoSettings = NewRepoSettingsModel(a.client, a.ctx, msg.sessionID)
			a.repoSettings.width = a.width
			return a, a.repoSettings.Init()
		case ViewTrash:
			a.trash = NewTrashModel(a.client, a.ctx)
			a.trash.width = a.width
			a.trash.height = a.height
			return a, a.trash.Init()
		case ViewSettings:
			a.settings = NewSettingsModel()
			a.settings.width = a.width
			return a, a.settings.Init()
		case ViewAutopilot:
			a.autopilot = NewAutopilotModel(a.client, a.ctx)
			a.autopilot.width = a.width
			a.autopilot.height = a.height
			return a, a.autopilot.Init()
		case ViewAttach:
			a.attachOrigin = msg.origin
			a.attachSessionID = msg.sessionID
			a.attach = NewAttachModel(a.client, a.ctx, a.manager, msg.sessionID, msg.resumeID)
			return a, a.attach.Init()
		case ViewHome:
			a.home = NewHomeModel(a.client, a.ctx, a.manager)
			a.home.width = a.width
			a.home.height = a.height
			return a, a.home.Init()
		}
		return a, nil
	}

	switch a.activeView {
	case ViewHome:
		updated, cmd := a.home.Update(msg)
		a.home = updated.(HomeModel)
		return a, cmd
	case ViewNewSession:
		updated, cmd := a.newSession.Update(msg)
		a.newSession = updated.(NewSessionModel)
		if a.newSession.Cancelled() {
			return a, a.switchToHome()
		}
		if a.newSession.Done() {
			sess := a.newSession.CreatedSession()
			if sess != nil {
				a.attachOrigin = ViewHome
				a.attachSessionID = sess.Id
				a.attach = NewAttachModel(a.client, a.ctx, a.manager, sess.Id, "")
				a.activeView = ViewAttach
				return a, a.attach.Init()
			}
			return a, a.switchToHome()
		}
		return a, cmd
	case ViewChatPicker:
		updated, cmd := a.chatPicker.Update(msg)
		a.chatPicker = updated.(ChatPickerModel)
		if a.chatPicker.Cancelled() {
			return a, a.switchToHome()
		}
		return a, cmd
	case ViewRepoAdd:
		updated, cmd := a.repoAdd.Update(msg)
		a.repoAdd = updated.(RepoAddModel)
		if a.repoAdd.Cancelled() {
			a.repoList = NewRepoListModel(a.client, a.ctx)
			a.activeView = ViewRepoList
			return a, a.repoList.Init()
		}
		if a.repoAdd.Done() {
			a.repoList = NewRepoListModel(a.client, a.ctx)
			a.activeView = ViewRepoList
			return a, a.repoList.Init()
		}
		return a, cmd
	case ViewRepoList:
		updated, cmd := a.repoList.Update(msg)
		a.repoList = updated.(RepoListModel)
		if a.repoList.Cancelled() {
			return a, a.switchToHome()
		}
		return a, cmd
	case ViewRepoSettings:
		updated, cmd := a.repoSettings.Update(msg)
		a.repoSettings = updated.(RepoSettingsModel)
		if a.repoSettings.Cancelled() || a.repoSettings.Done() {
			// Return to repo list.
			a.repoList = NewRepoListModel(a.client, a.ctx)
			a.repoList.width = a.width
			a.repoList.height = a.height
			a.activeView = ViewRepoList
			return a, a.repoList.Init()
		}
		return a, cmd
	case ViewTrash:
		updated, cmd := a.trash.Update(msg)
		a.trash = updated.(TrashModel)
		if a.trash.Cancelled() {
			return a, a.switchToHome()
		}
		return a, cmd
	case ViewSettings:
		updated, cmd := a.settings.Update(msg)
		a.settings = updated.(SettingsModel)
		if a.settings.Cancelled() {
			return a, a.switchToHome()
		}
		return a, cmd
	case ViewAutopilot:
		updated, cmd := a.autopilot.Update(msg)
		a.autopilot = updated.(AutopilotModel)
		if a.autopilot.Cancelled() {
			return a, a.switchToHome()
		}
		return a, cmd
	case ViewAttach:
		updated, cmd := a.attach.Update(msg)
		a.attach = updated.(AttachModel)
		if a.attach.Detached() {
			if a.attachOrigin == ViewHome {
				a.activeView = ViewHome
				a.home = NewHomeModel(a.client, a.ctx, a.manager)
				a.home.highlightSessionID = a.attachSessionID
				a.home.width = a.width
				a.home.height = a.height
				return a, tea.Batch(cmd, a.home.Init())
			}
			sessionID := a.attach.SessionID()
			claudeID := a.attach.ClaudeID()
			a.chatPicker = NewChatPickerModel(a.client, a.ctx, a.manager, sessionID, claudeID)
			a.chatPicker.width = a.width
			a.chatPicker.height = a.height
			a.activeView = ViewChatPicker
			// Batch the attach cleanup cmd (e.g. orphan delete) with the chat picker init.
			return a, tea.Batch(cmd, a.chatPicker.Init())
		}
		return a, cmd
	}

	return a, nil
}

func (a *App) switchToHome() tea.Cmd {
	a.activeView = ViewHome
	a.home = NewHomeModel(a.client, a.ctx, a.manager)
	a.home.width = a.width
	a.home.height = a.height
	return a.home.Init()
}

func (a App) View() tea.View {
	if a.quitting {
		return tea.NewView("")
	}

	var v tea.View
	switch a.activeView {
	case ViewHome:
		v = a.home.View()
	case ViewNewSession:
		v = a.newSession.View()
	case ViewChatPicker:
		v = a.chatPicker.View()
	case ViewRepoAdd:
		v = a.repoAdd.View()
	case ViewRepoList:
		v = a.repoList.View()
	case ViewRepoSettings:
		v = a.repoSettings.View()
	case ViewTrash:
		v = a.trash.View()
	case ViewSettings:
		v = a.settings.View()
	case ViewAutopilot:
		v = a.autopilot.View()
	case ViewAttach:
		v = a.attach.View()
	default:
		v = tea.NewView("Unknown view")
	}

	// Prepend the banner to every screen except during tea.Exec (AttachModel
	// returns empty content while Claude Code owns the terminal).
	if v.Content != "" {
		var opts bannerOpts
		switch a.activeView { //nolint:exhaustive // only override for specific views
		case ViewChatPicker:
			opts.session = a.chatPicker.session
			opts.spinner = a.chatPicker.spinner
		case ViewRepoSettings:
			opts.repo = a.repoSettings.repo
		}
		v.Content = renderBanner(a.activeView, opts) + "\n" + v.Content
	}

	v.AltScreen = true
	return v
}
