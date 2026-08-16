package views

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync/atomic"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/google/uuid"
	"github.com/recurser/boss/internal/agent"
	"github.com/recurser/boss/internal/client"
	bosspty "github.com/recurser/boss/internal/pty"
	"github.com/recurser/boss/internal/remotehost"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/telemetry"
	"github.com/recurser/bossalib/termnorm"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// minLaunchingDisplay is the floor on how long the "Launching... Press
// Ctrl+X to detach" message stays on screen before we hand off to tmux.
// Tuned so users actually see the detach shortcut even on a warm daemon.
const minLaunchingDisplay = 1500 * time.Millisecond

// errChatSessionGone is shown when a resume-attach targets a chat whose
// tmux session no longer exists (e.g. the session was interrupted during
// setup, or torn down by another bossd). Surfacing it through m.err routes
// the user to the dismissable "[esc] back" view instead of handing the
// terminal to a `tmux attach` that would print a raw "can't find session"
// error and trap them. See BOS-108.
var errChatSessionGone = errors.New(
	"this chat's agent session is no longer available (it may have been interrupted during setup) — press esc to go back and start a new chat")

// startExecMsg fires once the minLaunchingDisplay budget has elapsed.
// It triggers the deferred tea.Exec that attaches to tmux.
type startExecMsg struct{}

// attachTmuxEnv returns the environment for the `tmux attach` child with a
// resolvable TERM (falling back to xterm-256color when the user's TERM has no
// terminfo entry) plus the effective TERM string for diagnostics.
//
// The terminfo entry is probed on the machine the tmux client actually runs on:
// this host for an ordinary attach, the --host destination otherwise. That
// distinction is the whole point — under --host the client is on the remote
// machine, so a TERM that resolves here and is missing there starts a tmux that
// dies with "missing or unsuitable terminal". An empty destination is the local
// case, which keeps every non---host attach on exactly the path it used before.
//
// effectiveTERM is read back from the normalized env itself (rather than
// re-deriving it from os.Getenv) so the two never disagree, even when base
// isn't the current process environment (e.g. in tests).
func attachTmuxEnv(base []string, destination string) (env []string, effectiveTERM string) {
	env = termnorm.NormalizeWith(base, hostTerminfoProber(destination))
	for _, e := range env {
		if strings.HasPrefix(e, "TERM=") {
			return env, strings.TrimPrefix(e, "TERM=")
		}
	}
	return env, ""
}

// hostTerminfoProber returns the probe for the machine whose terminfo actually
// decides whether tmux can start: the local host for an ordinary attach, the
// --host machine otherwise. An empty destination is the local case, which keeps
// every non---host attach on exactly the path it used before.
//
// There is deliberately no "the host already looked unreachable, skip it" arm.
// One was tried and removed: the liveness probe that would supply that verdict
// runs a bare `ssh -o BatchMode=yes` while this probe rides the tunnel's
// authenticated master, so the two disagree exactly for the password /
// uncached-passphrase / touch-key users Tunnel.ControlPath exists to serve —
// and gating the skip on there being no master instead left a branch that is
// unreachable wherever multiplexing works, i.e. everywhere but Windows. It
// bought a bool through this selector to save one bounded, cached, fail-open
// probe. remotehost.TerminfoPresent already bounds, caches and fails open.
func hostTerminfoProber(destination string) termnorm.Prober {
	if destination == "" {
		return termnorm.Resolvable
	}
	return newRemoteTerminfoProber(destination)
}

// newRemoteTerminfoProber is indirected through a package var for the same
// reason newPasteUploader (attachpaste.go) is: it is the only way a test can
// prove the LOCAL path never reaches the ssh machinery at all. Everything a
// remote prober goes on to do is an ssh call a local test never makes, so
// "nothing crashed" would otherwise be indistinguishable from "one was built
// and simply never used" — and the second of those is the bug.
var newRemoteTerminfoProber = remoteTerminfoProber

// remoteTerminfoPresent is the probe call itself behind a second package var, so
// the Options this file BUILDS are observable. newRemoteTerminfoProber above is
// the wrong seam for that: every test stubs it, which means the body below —
// the only place ControlPath is wired from the tunnel into remotehost — would
// otherwise never execute under test at all, and dropping that field would leave
// the suite green while every probe stopped riding the master.
var remoteTerminfoPresent = remotehost.TerminfoPresent

// remoteTerminfoProber probes destination over ssh, riding the tunnel's
// multiplexing socket when there is one.
//
// The control path is read late — at probe time, not at construction — exactly
// as remotePasteUploader.options() does it (attachpaste.go), so a prober can
// never hold a path from a tunnel that has since been replaced.
//
// No timeout, cache, or error handling is layered on here: remotehost.
// TerminfoPresent bounds its own probe, caches per (destination, term) for the
// process, and fails open on every mode where it could not answer.
func remoteTerminfoProber(destination string) termnorm.Prober {
	return func(term string) bool {
		return remoteTerminfoPresent(context.Background(), remotehost.Options{
			Destination: destination,
			ControlPath: remoteHostControlPath(),
		}, term)
	}
}

// pendingExec carries the prepared exec across the launching delay.
// Populated in the chatRecordedMsg handler, consumed in startExecMsg.
type pendingExec struct {
	ptycmd   *bosspty.PTYCommand
	tmuxName string
	// cmd is the very process ptycmd wraps — not a copy of the recipe it was
	// built from. bosspty.PTYCommand keeps its *exec.Cmd unexported, so without
	// this handle the two lines that actually wire buildAttachCommand into the
	// process (its argv and its working directory) are unassertable, and
	// reverting them to the old local-only body would leave every test in the
	// package green. Holding the builder's OUTPUT here instead would not help:
	// it would agree with itself while the exec quietly disagreed with both.
	cmd         *exec.Cmd
	prefillPlan string // empty unless first attach with a plan to paste
}

// agentFinishedMsg signals that the interactive Claude process has exited or
// the user has detached from it.
type agentFinishedMsg struct {
	err      error
	detached bool
}

// chatTitleUpdatedMsg signals a best-effort title update completed (ignored).
type chatTitleUpdatedMsg struct{}

// launchDescribedMsg carries the daemon's description of a chat's launch
// command, fetched after the agent pane dies so the error screen can show a
// reproduction command. info is nil when the lookup failed (best-effort).
type launchDescribedMsg struct {
	info *pb.DescribeChatLaunchResponse
}

// attachReadyMsg carries the session plus its chat list, fetched via RPC
// before launching Claude. The chat list is needed so first-attach prefill
// can be gated on len(chats) == 0 (matching the original semantics from the
// daemon-side prefill that was removed in PR #179).
type attachReadyMsg struct {
	session *pb.Session
	chats   []*pb.ClaudeChat
}

// chatRecordedMsg carries the result of the RecordChat RPC, which runs inside
// a tea.Cmd rather than inside Update. Calling it on the update loop froze the
// whole TUI — no keys, no repaint — for as long as the daemon took to answer,
// so a wedged daemon made esc do nothing and left Ctrl+C as the only way out.
// session, prefillPlan and resume ride along because the model value in Update
// is a copy: the closure cannot hand them over by mutating it.
// launchID identifies the launch attempt this result belongs to, so a result
// that outlived its own attempt is dropped instead of steering a later one:
// esc → chat picker → open a chat replaces a.attach wholesale
// (app_routing.go, app_delegate.go) while the abandoned RecordChat is still in
// flight, and the late message would otherwise drive the NEW model.
//
// It is a nonce rather than the chat id on purpose. Resuming the chat the user
// just backed out of gives both attempts the same agentSessionID, and that
// collision is NOT benign: the handler below sets m.pendingExec
// unconditionally, so a duplicate arriving after startExecMsg already consumed
// the stash re-primes it — re-running probeTmuxSession (which can overwrite an
// agent-exited diagnostic screen with errChatSessionGone, since View() checks
// m.err before m.returned) and scheduling a second exec that startExecMsg's
// pendingExec == nil guard can no longer absorb. A duplicate landing the other
// way round, before the model's own result, would apply the abandoned
// attempt's prefillPlan to a resume that had deliberately gated prefill off.
type chatRecordedMsg struct {
	session     *pb.Session
	chat        *pb.ClaudeChat
	launchID    uint64
	prefillPlan string
	resume      bool
	err         error
}

// attachLaunchSeq numbers launch attempts process-wide. It has to be shared
// rather than per-model: each attach builds a brand-new AttachModel, so a
// per-model counter would hand every attempt the same first value and defeat
// the point.
//
// Add returns the POST-increment value, so a real chatRecordedMsg never
// carries 0 and a model that has not staged a launch (launchID still the zero
// value) therefore matches nothing. Anything changing how the ticket is minted
// has to preserve that, or an unstaged model starts accepting messages.
var attachLaunchSeq atomic.Uint64

// attachErrMsg signals a fetch or launch error.
type attachErrMsg struct {
	err error
}

// AttachModel launches an interactive Claude Code TUI in the session's worktree.
type AttachModel struct {
	client         client.BossClient
	telemetry      telemetry.Client
	ctx            context.Context
	manager        *bosspty.Manager
	sessionID      string
	resumeID       string // Claude Code session UUID to resume (empty = new chat)
	agentSessionID string // The Claude Code session UUID actually launched

	session   *pb.Session
	launching bool  // true while fetching session
	returned  bool  // true after claude exits
	agentErr  error // error from claude process (if any)
	// launchInfo is the daemon's description of the launch command, fetched on
	// error so View can show a copy-pasteable reproduction command. nil until
	// the lookup returns (or stays nil if it failed — best-effort).
	launchInfo *pb.DescribeChatLaunchResponse
	detach     bool
	err        error
	width      int
	height     int

	// overrideAgent populates RecordChat's agent_name override at chat
	// creation time. Empty inherits the parent session's AgentName. Set
	// by the chat picker before pushing onto the attach view.
	overrideAgent string

	// launchStartedAt is captured in NewAttachModel so the minimum-display
	// budget covers the entire launch path, not just the post-RPC tail.
	launchStartedAt time.Time

	// launchID is this attempt's ticket from attachLaunchSeq, taken when the
	// off-loop RecordChat is staged. Only the chatRecordedMsg carrying the
	// same ticket may be applied here; see chatRecordedMsg.
	launchID uint64

	// pendingExec is set in the chatRecordedMsg handler and consumed by
	// the startExecMsg handler once the launching-display delay elapses.
	pendingExec *pendingExec

	effectiveTERM string // TERM actually handed to `tmux attach`, for diagnostics

	// tmuxName is the tmux session name this attach targets, stashed as soon
	// as it's known so the error screen can offer a reproduction command even
	// though pendingExec (which also carries it) is cleared before the pane
	// exits.
	tmuxName string
	// attachDestination is the --host ssh destination this attach went through,
	// or "" for a local tmux. Captured at attach time (not re-read at render
	// time) so the error screen reproduces the transport that actually ran.
	attachDestination string
	// tmuxTail is tmux's own startup output (e.g. a missing-terminfo error),
	// captured via Manager.RecentOutput on a failed attach — before the
	// process can be evicted — so the error screen can show the real reason
	// instead of a bare "exit status 1".
	tmuxTail string
	// pasteUploader rewrites a pasted local image path into one the remote agent
	// can open. Non-nil ONLY under --host, and held by pointer so the upload
	// goroutine's closure and the model copy that later runs cleanup share one
	// record of the remote directory. See attachpaste.go.
	pasteUploader *remotePasteUploader
}

// SetTelemetry installs a telemetry client for successful attach actions.
func (m *AttachModel) SetTelemetry(client telemetry.Client) {
	m.telemetry = client
}

// SetOverrideAgent supplies a per-chat agent override that's forwarded as
// RecordChat's agent_name. Empty inherits the parent session's AgentName.
func (m *AttachModel) SetOverrideAgent(name string) { m.overrideAgent = name }

// NewAttachModel creates an AttachModel for the given session.
// If resumeID is non-empty, Claude Code will be launched with --resume.
func NewAttachModel(c client.BossClient, parentCtx context.Context, manager *bosspty.Manager, sessionID, resumeID string) AttachModel {
	return AttachModel{
		client:          c,
		ctx:             parentCtx,
		manager:         manager,
		sessionID:       sessionID,
		resumeID:        resumeID,
		launching:       true,
		launchStartedAt: time.Now(),
	}
}

func (m AttachModel) Init() tea.Cmd {
	return m.fetchAttachState()
}

func (m AttachModel) fetchAttachState() tea.Cmd {
	return func() tea.Msg {
		sess, err := m.client.GetSession(m.ctx, m.sessionID, client.SessionReadOptions{})
		if err != nil {
			return attachErrMsg{err: err}
		}
		// Best-effort chat list — used only to gate first-attach prefill.
		// A failure here should not block attach; treat as "unknown count"
		// (i.e. assume non-empty so we don't accidentally prefill on a
		// resumed session).
		chats, _ := m.client.ListChats(m.ctx, m.sessionID)
		return attachReadyMsg{session: sess, chats: chats}
	}
}

func (m AttachModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case attachReadyMsg:
		m.session = msg.session

		// Decide whether this is a fresh first attach to a tracker-backed
		// session that should have its plan pasted into Claude's input box.
		// Mirrors the gating that used to live in shouldConsiderPrefill on
		// the daemon side (removed in PR #179 along with the per-chat tmux
		// indirection that hosted it).
		prefillPlan := ""
		if m.resumeID == "" &&
			msg.session.GetTrackerId() != "" &&
			msg.session.GetPlan() != "" &&
			len(msg.chats) == 0 {
			prefillPlan = msg.session.GetPlan()
		}

		// Resolve the Claude session UUID and ask the daemon to record the
		// chat AND ensure a tmux session is hosting it. Doing the create on
		// the daemon side means the tmux server outlives bossd (it
		// double-forks), so the user can kill bossd, restart it, and
		// reattach to the same chat without losing state.
		resume := m.resumeID != ""
		if resume {
			m.agentSessionID = m.resumeID
		} else {
			m.agentSessionID = uuid.New().String()
		}
		// Off the update loop, not on it: m.launching stays true so View()
		// keeps rendering the launching line and keys (esc included) keep
		// being processed while the RPC is in flight.
		agentSessionID := m.agentSessionID
		m.launchID = attachLaunchSeq.Add(1)
		launchID := m.launchID
		return m, func() tea.Msg {
			chat, err := m.client.RecordChat(m.ctx, m.sessionID, agentSessionID, "New chat", m.overrideAgent, resume)
			return chatRecordedMsg{
				session:     msg.session,
				chat:        chat,
				launchID:    launchID,
				prefillPlan: prefillPlan,
				resume:      resume,
				err:         err,
			}
		}

	case chatRecordedMsg:
		// Drop a result that is no longer ours. Now that RecordChat runs off
		// the update loop it can outlive the attempt that issued it: the user
		// may have pressed esc (m.detach), or backed out and opened a chat,
		// which builds a fresh AttachModel this abandoned message would
		// otherwise drive. Matching on the launch ticket rather than the chat
		// keeps that exact even when both attempts name the same chat.
		if m.detach || msg.launchID != m.launchID {
			return m, nil
		}
		resume := msg.resume
		if msg.err != nil {
			m.err = fmt.Errorf("record chat: %w", msg.err)
			return m, nil
		}
		tmuxName := msg.chat.GetTmuxSessionName()
		if tmuxName == "" {
			m.err = fmt.Errorf("daemon did not return a tmux session name; check that tmux is installed")
			return m, nil
		}
		if err := validateTmuxSessionName(tmuxName); err != nil {
			m.err = err
			m.launching = false
			return m, nil
		}
		// Stash on the model (not just pendingExec) so it survives to the
		// agentFinishedMsg handler, which fires after pendingExec has already
		// been cleared.
		m.tmuxName = tmuxName
		// Resume-attach guard: the chat record can outlive its tmux session
		// (interrupted setup, daemon restart, external teardown). If we'd be
		// resuming a session that no longer exists, don't hand the terminal
		// to `tmux attach` — it would dump a raw "can't find session" error
		// and leave the user stranded. Surface a dismissable error instead.
		// New chats (resumeID == "") get a freshly created session from the
		// daemon, so they are guaranteed alive and skip this check.
		//
		// Only a probe that positively answered "gone" may fire this. Under
		// --host the probe can also fail to reach the host at all, and
		// errChatSessionGone would then tell a user whose remote chat is
		// perfectly alive that it is unavailable and to start a new one —
		// exactly the silent-wrong-answer class this ticket exists to remove.
		// Letting the attach proceed instead surfaces ssh's own error in the
		// pane, and the diagnostic screen reproduces it with the real ssh
		// command.
		if resume && probeTmuxSession(tmuxName) == tmuxProbeGone {
			m.err = errChatSessionGone
			m.launching = false
			return m, nil
		}
		if !resume {
			captureViewTelemetry(m.ctx, m.telemetry, telemetry.EventChatCreated, map[string]any{
				"source": "tui",
			})
		}
		captureViewTelemetry(m.ctx, m.telemetry, telemetry.EventChatAttached, map[string]any{
			"source": "tui",
			"action": "attach",
			"resume": resume,
		})

		// Build the tmux-attach exec command and bosspty wrapper. We do
		// this synchronously so that, by the time the launching-display
		// delay elapses, the manager already knows about the session and
		// the user's Ctrl+X intercept has somewhere to land.
		m.attachDestination = remoteHostDestination()
		attach, err := buildAttachCommand(m.attachDestination, tmuxName, msg.session.GetWorktreePath())
		if err != nil {
			m.err = err
			m.launching = false
			return m, nil
		}
		// #nosec G204 -- tmux attach -t <name>, or the same under ssh; const argv
		// plus an app-generated session id and the user's own --host destination. owner=@recurser review-by=2027-02-07 issue=BOS-714
		// No local shell either way; the remote leg's shell gets the session name
		// shell-quoted by buildAttachCommand.
		agentCmd := exec.Command(attach.argv[0], attach.argv[1:]...)
		agentCmd.Dir = attach.dir
		// The destination captured above, not a second remoteHostDestination()
		// call: the TERM in this env has to be the one the machine running the
		// argv just built can actually resolve, so the two must name the same
		// transport (the same reason the paste-uploader gate below reuses it).
		env, effTERM := attachTmuxEnv(os.Environ(), m.attachDestination)
		agentCmd.Env = env
		m.effectiveTERM = effTERM
		m.manager.RegisterSession(m.agentSessionID, m.sessionID)
		ptycmd := bosspty.NewPTYCommand(m.manager, m.agentSessionID, agentCmd)

		// A dragged-in image is a LOCAL path, and under --host the agent reading
		// it is on another machine, so it has to be copied over before the path
		// means anything. Gated on the destination captured above rather than a
		// second isRemoteHost() call: the two must name the same transport as the
		// argv that was just built, and re-reading the package global here would
		// let a mid-attach change wire an uploader for a host the pane is not
		// attached to.
		//
		// In local mode SetPasteUploader is still not called AT ALL — not with
		// nil, not with a no-op. No uploader means no paste is ever swallowed
		// here and no ssh is ever put in the path of a file the agent can
		// already open, and that remains the local guarantee.
		//
		// What the else branch installs is NOT a second uploader. It is an
		// observation hook that can only ever decline (BOS-849). Without one,
		// a boss running on the far side of a plain ssh login inspected pasted
		// text not at all, so a screenshot pasted from the user's laptop
		// reached the agent as a path to a file on the wrong machine with
		// nothing anywhere to say so — silence that was indistinguishable from
		// boss eating the paste. Boss cannot fetch that file (there is no
		// reverse channel back to the client), so the hook forwards the bytes
		// unchanged and explains, naming `boss --host` as the route that does
		// work.
		//
		// The local guarantee is therefore now "never claims" rather than "no
		// machinery": internal/pty does keep paste state on this branch, and
		// byte-for-byte conservation rests on that hook's claim closure always
		// returning false. HasPasteUploader and HasImagePasteNotice make both
		// halves assertable from here.
		if m.attachDestination != "" {
			m.pasteUploader = newPasteUploader(m.attachDestination, m.agentSessionID)
			ptycmd.SetPasteUploader(m.pasteUploader.upload)
		} else {
			ptycmd.SetImagePasteNotice()
		}

		// Stash everything needed for the deferred exec and stay in the
		// launching state so View() keeps rendering the Ctrl+X hint.
		m.pendingExec = &pendingExec{
			ptycmd:      ptycmd,
			tmuxName:    tmuxName,
			cmd:         agentCmd,
			prefillPlan: msg.prefillPlan,
		}

		// Schedule the actual tea.Exec for whenever the rest of the
		// minimum-display budget runs out. If RPCs already took longer
		// than the budget, fire startExecMsg on the next Update cycle.
		remaining := minLaunchingDisplay - time.Since(m.launchStartedAt)
		if remaining <= 0 {
			return m, func() tea.Msg { return startExecMsg{} }
		}
		return m, tea.Tick(remaining, func(time.Time) tea.Msg {
			return startExecMsg{}
		})

	case startExecMsg:
		// Nothing to do if the user already bailed via esc (m.detach)
		// or if some duplicate startExecMsg snuck through after we
		// consumed the stash. Clear pendingExec in both cases so no
		// stale state hangs around.
		if m.detach || m.pendingExec == nil {
			m.pendingExec = nil
			return m, nil
		}
		pe := m.pendingExec
		m.pendingExec = nil
		m.launching = false

		// Kick the prefill goroutine off here (not in attachReadyMsg)
		// so its 3 s processWait window starts when the manager is
		// about to start the *Process, not 1.5 s earlier.
		if pe.prefillPlan != "" {
			go prefillClaudeInput(m.ctx, m.manager, m.agentSessionID, pe.prefillPlan)
		}

		return m, tea.Exec(pe.ptycmd, func(err error) tea.Msg {
			// Clear the primary screen buffer so Claude content doesn't
			// linger in scrollback when boss exits alt screen.
			fmt.Print("\033[2J\033[H\033[3J")
			// err is handed straight to the verdict, not just carried past it:
			// the pane's own exit status is the evidence that the tmux client
			// ran rather than died on startup, and without it a live remote
			// session alone would classify the failure below as a detach and
			// skip the diagnostic entirely. See detachedAfterExec (BOS-728).
			detached := detachedAfterExec(pe.ptycmd.Detached, err, pe.tmuxName)
			return agentFinishedMsg{err: err, detached: detached}
		})

	case agentFinishedMsg:
		// The remote upload directory is dead either way — the user left the
		// pane, or the pane died — so both outcomes below carry the removal.
		// It is best-effort and cannot change the outcome it rides along with:
		// see cleanupRemoteUploads.
		cleanupUploads := m.cleanupRemoteUploads()
		if msg.detached {
			// User pressed Ctrl+X or Ctrl+] — process is still running in
			// the background; drop back to the caller's view.
			m.detach = true
			return m, cleanupUploads
		}
		m.returned = true
		m.agentErr = msg.err
		// Clean exits auto-bounce back to the chat picker so the user
		// doesn't have to press esc to dismiss an empty "session ended"
		// screen. Error exits hold the screen so the failure is readable
		// — otherwise an immediately-failing tmux attach (e.g. session
		// torn down by another bossd) looks like the chat just refuses
		// to open, with no clue why.
		if msg.err == nil {
			m.detach = true
		}
		if msg.err != nil {
			// Capture tmux's own startup output (e.g. a missing-terminfo error)
			// before the process is evicted, so the error screen can show the
			// real reason instead of a bare "exit status 1".
			m.tmuxTail = string(m.manager.RecentOutput(m.agentSessionID, 2048))
			// The pane died on launch — ask the daemon what command it ran so
			// the error screen can show a copy-pasteable reproduction.
			// Do this before title cleanup because unused chats have no JSONL
			// title and updateChatTitle deletes that orphan chat row.
			return m, tea.Batch(
				m.reportStopped(),
				tea.Sequence(m.describeLaunch(), m.updateChatTitle()),
				cleanupUploads,
			)
		}
		// Best-effort: update chat title from JSONL and report stopped status.
		return m, tea.Batch(m.updateChatTitle(), m.reportStopped(), cleanupUploads)

	case chatTitleUpdatedMsg:
		// Ignored — fire-and-forget.
		return m, nil

	case launchDescribedMsg:
		// Enriches the already-rendered error screen with the reproduction
		// command once the daemon responds. nil info leaves the bare error.
		m.launchInfo = msg.info
		return m, nil

	case attachErrMsg:
		m.err = msg.err
		return m, nil

	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		return m, nil

	case tea.KeyMsg:
		switch msg.String() {
		case "esc":
			m.detach = true
			return m, nil
		}
	}

	return m, nil
}

// agentChatTitle and agentTranscriptAbsentOrEmpty read Claude Code's on-disk
// JSONL transcript under a session's worktree. They are indirected through
// package vars purely so tests can prove the --host gates SKIP them: a test that
// only asserts "nothing crashed" passes against the exact bug those gates fix,
// because reading a nonexistent path does not crash — it quietly answers "no
// title" and "transcript absent".
var (
	agentChatTitle               = agent.ChatTitle
	agentTranscriptAbsentOrEmpty = agent.TranscriptAbsentOrEmpty
)

// updateChatTitle reads the Claude JSONL file and updates the chat title via RPC.
// If the session was never used (no real user message), it deletes the orphan record.
func (m AttachModel) updateChatTitle() tea.Cmd {
	if m.agentSessionID == "" || m.session == nil {
		return nil
	}
	// Against a remote daemon the worktree — and the transcript inside it — are
	// on the other machine, so both reads below would answer about a local path
	// that does not exist. The title backfill would silently no-op forever
	// (harmless in itself: the remote daemon's tmux poller keeps refreshing
	// titles server-side, and only ever over "" / "New chat", so a user-set
	// title is never at risk). The orphan reap is the dangerous one: an absent
	// local transcript reads as "this chat was never used" and would DELETE the
	// record of a perfectly live remote chat. Skip both; a nil tea.Cmd is
	// compacted away by tea.Batch and tea.Sequence.
	if isRemoteHost() {
		return nil
	}
	agentSessionID := m.agentSessionID
	worktreePath := m.session.GetWorktreePath()
	return func() tea.Msg {
		title := agentChatTitle(worktreePath, agentSessionID)
		if title != "" {
			_ = m.client.UpdateChatTitle(m.ctx, agentSessionID, title)
		} else if agentTranscriptAbsentOrEmpty(worktreePath, agentSessionID) {
			// Reap a genuinely-unused orphan ONLY when its transcript is
			// confirmed absent or zero-length. A non-empty transcript whose
			// title won't parse — or any read failure — must never delete the
			// row: a mis-encoded project key once made rich chats look
			// title-less here and this branch destroyed them. See
			// agent.TranscriptAbsentOrEmpty.
			_ = m.client.DeleteChat(m.ctx, agentSessionID)
		}
		return chatTitleUpdatedMsg{}
	}
}

// reportStopped sends a fire-and-forget stopped status to the daemon so other
// clients see the change immediately.
func (m AttachModel) reportStopped() tea.Cmd {
	if m.agentSessionID == "" {
		return nil
	}
	agentSessionID := m.agentSessionID
	return func() tea.Msg {
		_ = m.client.ReportChatStatus(m.ctx, []*pb.ChatStatusReport{{
			AgentSessionId: agentSessionID,
			Status:         pb.ChatStatus_CHAT_STATUS_STOPPED,
			LastOutputAt:   timestamppb.Now(),
		}})
		return nil
	}
}

// describeLaunch asks the daemon for the exact command it used to launch this
// chat's agent so the error view can show a copy-pasteable reproduction. The
// command runs on the daemon host, which may differ from where boss runs, so
// the daemon also reports its hostname. Best-effort: any failure (including the
// remote-orchestrator client, which is local-only) yields a nil info and the
// view falls back to the bare exit error.
func (m AttachModel) describeLaunch() tea.Cmd {
	if m.agentSessionID == "" {
		return nil
	}
	agentSessionID := m.agentSessionID
	return func() tea.Msg {
		info, err := m.client.DescribeChatLaunch(m.ctx, agentSessionID)
		if err != nil {
			return launchDescribedMsg{info: nil}
		}
		return launchDescribedMsg{info: info}
	}
}

// prefillClaudeInput writes the session plan into Claude's TUI input box via
// a bracketed-paste sequence so the user lands on a ready-to-refine prompt
// instead of an empty input. Best-effort and non-fatal — a missing prefill
// is a UX degradation, not a reason to abort the attach.
//
// Flow:
//  1. Wait for the bosspty.Manager to register the *Process for agentSessionID
//     (tea.Exec calls Manager.GetOrStart on its own goroutine, so there is a
//     small race window between this goroutine starting and the entry being
//     in the map).
//  2. Wait for Claude Code's input-box prompt indicator (❯) to appear
//     in the PTY output. If the marker never shows up within the
//     deadline, paste anyway — same fail-open policy as the daemon-side
//     prefill that this replaces. We match on the prompt character
//     rather than the default footer so users with custom statuslines
//     (which replace the footer entirely) still get the prefill.
//  3. Write the plan to the PTY as a bracketed paste, via the shared
//     bosspty.BracketedPaste — the single construction site for that sequence,
//     so this and the upload path cannot drift into two spellings of it.
func prefillClaudeInput(ctx context.Context, manager *bosspty.Manager, agentSessionID, plan string) {
	const (
		readyMarker      = "❯"
		recentBufferSize = 4096
		processWait      = 3 * time.Second
		readyWait        = 2 * time.Second
		pollInterval     = 100 * time.Millisecond
	)

	// Step 1: wait for the process to appear in the manager.
	var proc *bosspty.Process
	procDeadline := time.Now().Add(processWait)
	for {
		if p, ok := manager.Get(agentSessionID); ok {
			proc = p
			break
		}
		if time.Now().After(procDeadline) {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(pollInterval):
		}
	}

	// Step 2: wait for Claude's input box to be ready, then paste.
	marker := []byte(readyMarker)
	readyDeadline := time.Now().Add(readyWait)
	for time.Now().Before(readyDeadline) {
		if bytes.Contains(proc.RecentOutput(recentBufferSize), marker) {
			break
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(pollInterval):
		}
	}

	// Step 3: paste. Errors are silently dropped — the process may have
	// exited or detached by now; either way the user can recover by typing
	// the prompt manually.
	_ = proc.WriteInput(bosspty.BracketedPaste(plan))
}

// Detached returns true if the user should return to the home screen.
func (m AttachModel) Detached() bool { return m.detach }

// SessionID returns the session ID for navigation after detach.
func (m AttachModel) SessionID() string { return m.sessionID }

// AgentSessionID returns the agent session UUID for navigation after detach.
func (m AttachModel) AgentSessionID() string { return m.agentSessionID }

func agentDisplayName(name string) string {
	switch strings.ToLower(name) {
	case "claude":
		return "Claude Code"
	case "codex":
		return "Codex"
	case "opencode":
		return "OpenCode"
	case "":
		return "agent"
	default:
		return name
	}
}

func (m AttachModel) displayAgentName() string {
	if m.overrideAgent != "" {
		return m.overrideAgent
	}
	if m.session != nil {
		return m.session.GetAgentName()
	}
	return ""
}

// renderLaunchDiagnostic formats a copy-pasteable reproduction command from the
// daemon's launch description, shown under the bare exit error so the user can
// run the agent themselves and see the underlying failure. The cause is NOT
// always a PATH/login-shell problem — the agent binary may resolve fine and then
// exit non-zero on its own (e.g. `--session-id <id>` colliding with an existing
// transcript: "Session ID already in use"), and bossd often can't capture that
// stderr before the pane dies. So the prose stays neutral and points the user at
// the reproduction command rather than asserting a single cause. Returns "" when
// no description is available so the caller renders just the bare error.
func renderLaunchDiagnostic(info *pb.DescribeChatLaunchResponse, agentLabel string) string {
	if info == nil || len(info.GetArgv()) == 0 {
		return ""
	}
	prose := lipgloss.NewStyle().Padding(0, 2)
	code := lipgloss.NewStyle().Padding(0, 4)

	where := "the daemon host"
	if host := info.GetHost(); host != "" {
		where = "host '" + host + "'"
	}

	cmd := shellJoin(info.GetArgv())
	if wt := info.GetWorktreePath(); wt != "" {
		cmd = "cd " + shellQuote(wt) + " && \\\n" + cmd
	}

	var b strings.Builder
	// When bossd captured the agent's own final output at pane death (BOS-477),
	// lead with it verbatim (sanitized like the tmux tail so control bytes can't
	// scramble the boss frame) — it is the real error. Otherwise fall back to the
	// neutral prose that points the user at the reproduction command.
	if captured := strings.TrimSpace(sanitizeTmuxTail(info.GetCapturedOutput())); captured != "" {
		b.WriteString(prose.Render(agentLabel + " reported:"))
		b.WriteString("\n\n")
		b.WriteString(code.Render(captured))
		b.WriteString("\n\n")
	} else {
		b.WriteString(prose.Render(fmt.Sprintf(
			"The %s CLI exited before its pane came up. This is often a PATH or\n"+
				"login-shell problem that only reproduces where the agent runs, but it\n"+
				"can also be the agent itself refusing to start. Run the command to see\n"+
				"the underlying error.", agentLabel)))
		b.WriteString("\n\n")
	}
	b.WriteString(prose.Render("bossd ran this on " + where + ":"))
	b.WriteString("\n\n")
	b.WriteString(code.Render(cmd))
	b.WriteString("\n\n")
	b.WriteString(prose.Render("Run it there to see the underlying error."))
	return b.String()
}

// renderTmuxAttachDiagnostic formats a copy-pasteable `tmux attach` reproduction
// plus the captured tmux startup output, shown under the bare exit error so a
// user can see (and reproduce) the underlying tmux failure — most commonly a
// missing terminfo entry for their TERM. Returns "" when there is no session
// name to reproduce with.
//
// destination is the --host ssh destination the attach actually went through, or
// "" for a local tmux. It is not cosmetic: a local `tmux attach -t <name>`
// printed under a --host failure is a command that cannot work on the machine
// the user would paste it into, and it points the terminfo fix at the wrong
// host. The reproduction therefore mirrors the transport that ran.
func renderTmuxAttachDiagnostic(destination, tmuxName, effectiveTERM, capturedTail string) string {
	if tmuxName == "" {
		return ""
	}
	prose := lipgloss.NewStyle().Padding(0, 2)
	code := lipgloss.NewStyle().Padding(0, 4)

	argv, err := buildAttachReproArgv(destination, tmuxName)
	if err != nil {
		return prose.Render(err.Error())
	}
	repro := shellJoin(argv)
	if effectiveTERM != "" {
		// ssh hands the local TERM to the remote pty, so prefixing the whole
		// invocation sets the value the remote tmux client will see too.
		repro = "TERM=" + effectiveTERM + " " + repro
	}

	var b strings.Builder
	b.WriteString(prose.Render("boss attached with:"))
	b.WriteString("\n\n")
	b.WriteString(code.Render(repro))
	b.WriteString("\n\n")
	// capturedTail is raw PTY output: sanitize it before rendering so a tmux
	// failure whose tail carries ANSI/cursor sequences or stray control bytes
	// can't scramble the boss error frame the view is meant to keep control of.
	if tail := strings.TrimSpace(sanitizeTmuxTail(capturedTail)); tail != "" {
		b.WriteString(prose.Render("tmux reported:"))
		b.WriteString("\n\n")
		b.WriteString(code.Render(tail))
		b.WriteString("\n\n")
	}
	terminfoHost := "this host"
	if destination != "" {
		// The tmux client is the remote one, so its terminfo database is the
		// one that has to know the TERM — installing it here fixes nothing.
		terminfoHost = destination
	}
	b.WriteString(prose.Render(
		"If this is a missing-terminfo error, install the entry on " + terminfoHost + "\n" +
			"(" + termnorm.InstallHint + ") or run: export TERM=" + termnorm.FallbackTERM))
	if destination != "" {
		// A --host attach can also die by losing the connection mid-pane, which
		// skips the deferred terminal cleanup and leaves mouse reporting and
		// bracketed paste set (BOS-650). The remote tmux session survives that,
		// so say both things: how to un-wedge the terminal, and that the work is
		// still there to reattach to.
		b.WriteString("\n\n")
		b.WriteString(prose.Render(
			"If the connection dropped mid-attach and this terminal now misbehaves,\n" +
				"run: boss fix-terminal — the remote tmux session survives the drop, so\n" +
				"attaching again returns you to the same pane."))
	}
	return b.String()
}

// sanitizeTmuxTail strips ANSI/CSI escape sequences and any remaining C0
// control bytes (keeping newlines and tabs) from captured PTY output, so a
// tmux startup-failure tail is safe to render inside the boss error frame. The
// common missing-terminfo error is plain stderr text and passes through
// unchanged.
func sanitizeTmuxTail(s string) string {
	s = ansi.Strip(s)
	return strings.Map(func(r rune) rune {
		if r == '\n' || r == '\t' {
			return r
		}
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, s)
}

// shellJoin renders argv as a single copy-pasteable shell command, quoting any
// argument that contains whitespace or shell-significant characters.
func shellJoin(argv []string) string {
	quoted := make([]string, len(argv))
	for i, a := range argv {
		quoted[i] = shellQuote(a)
	}
	return strings.Join(quoted, " ")
}

// shellQuote wraps s in single quotes when it contains characters the shell
// would otherwise interpret, escaping embedded single quotes the POSIX way
// ('\”). A safe-looking token is returned unquoted for readability.
func shellQuote(s string) string {
	if s == "" {
		return "''"
	}
	if safeShellToken(s) {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// safeShellToken reports whether s consists solely of characters that need no
// shell quoting.
func safeShellToken(s string) bool {
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case strings.ContainsRune("-_./:=@", r):
		default:
			return false
		}
	}
	return true
}

func (m AttachModel) View() tea.View {
	if m.err != nil {
		// Attach is where a --host session lives, so a dropped tunnel surfaces
		// here first; the raw dial error against the forwarded socket made that
		// read as a frozen session (BOS-724).
		return tea.NewView(
			renderError(rpcErrorMessage(m.err), m.width) + "\n" +
				styleActionBar.Render("[esc] back"),
		)
	}

	if m.launching {
		var b strings.Builder
		title := m.sessionID
		if m.session != nil {
			title = m.session.Title
		}
		b.WriteString(lipgloss.NewStyle().Padding(0, 2).Render(
			fmt.Sprintf("Launching %s for %s...  Press Ctrl+X to detach", agentDisplayName(m.displayAgentName()), title)))
		return tea.NewView(b.String())
	}

	if m.returned {
		var b strings.Builder
		agentLabel := agentDisplayName(m.displayAgentName())
		if m.agentErr != nil {
			b.WriteString(renderError(fmt.Sprintf("%s exited with error: %v", agentLabel, m.agentErr), m.width))
			if diag := renderLaunchDiagnostic(m.launchInfo, agentLabel); diag != "" {
				b.WriteString("\n")
				b.WriteString(diag)
			}
			if diag := renderTmuxAttachDiagnostic(m.attachDestination, m.tmuxName, m.effectiveTERM, m.tmuxTail); diag != "" {
				b.WriteString("\n")
				b.WriteString(diag)
			}
		} else {
			b.WriteString(lipgloss.NewStyle().Padding(0, 2).Render(fmt.Sprintf("%s session ended.", agentLabel)))
		}
		b.WriteString("\n")
		b.WriteString(actionBarWidth(m.width, []string{"[esc] back"}))
		return tea.NewView(b.String())
	}

	// Never render a completely empty frame: a blank view lets stray
	// terminal output (e.g. a child process's leftover stderr) masquerade as
	// the boss screen with no affordance. A minimal line guarantees the user
	// always sees boss is in control.
	return tea.NewView(lipgloss.NewStyle().Padding(0, 2).Render("Attaching…"))
}
