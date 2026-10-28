package server

import (
	"context"
	"os"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/recurser/bossd/internal/domain"

	"connectrpc.com/connect"
	"github.com/recurser/bossalib/attention"
	"github.com/recurser/bossalib/contract/daemon/alertstate"
	"github.com/recurser/bossalib/displaystatus"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/kit/projectconfig"
	"github.com/recurser/bossalib/kit/vcs"
	"github.com/recurser/bossalib/machine"
	"github.com/recurser/bossalib/models"
	"github.com/recurser/bossalib/sessionreason"
	"github.com/recurser/bossd/internal/domain/resumepolicy"
	"github.com/recurser/bossd/internal/session"
	"github.com/recurser/bossd/internal/sessionattention"
	"github.com/recurser/bossd/internal/status"
	"github.com/recurser/bossd/internal/verifyadmission"
)

type attentionCache struct {
	mu        sync.Mutex
	factories map[string]factoryAttention
	questions map[string]questionAttention
}
type questionAttention struct {
	text    string
	askedAt time.Time
	at      time.Time
}
type factoryAttention struct {
	value         bool
	expires       time.Time
	admissionPath string
	admission     verifyadmission.Snapshot
}
type attentionDeps struct {
	row     *models.Session
	display *status.DisplayEntry
	chats   []*domain.AgentChat
}

// SessionAttention returns the current RPC attention for daemon policies.
func (s *Server) SessionAttention(ctx context.Context, sessionID string) (*pb.SessionAttention, error) {
	resp, err := s.GetSession(ctx, connect.NewRequest(&pb.GetSessionRequest{Id: sessionID}))
	if err != nil {
		return nil, err
	}
	return resp.Msg.Session.Attention, nil
}

// HydrateAttentionForStream shares the RPC producer with the reverse stream.
// Call after the legacy display and observability overlays on every snapshot/delta.
func (s *Server) HydrateAttentionForStream(ctx context.Context, p *pb.Session) {
	if s == nil || p == nil || s.sessions == nil {
		return
	}
	row, err := s.sessions.Get(ctx, p.Id)
	if err != nil || row == nil {
		return
	}
	var chats []*domain.AgentChat
	if s.agentChats != nil {
		chats, _ = s.agentChats.ListBySession(ctx, p.Id)
	}
	var display *status.DisplayEntry
	if s.displayTracker != nil {
		display = s.displayTracker.Get(p.Id)
	}
	s.hydrateAttention(ctx, p, attentionDeps{row: row, display: display, chats: chats})
}

// hydrateAttention supplies Session.attention last in GetSession, ListSessions,
// and streamSessionHydrator.Hydrate; it never writes the legacy display fields.
// The additive field already has its compatibility transform (SessionAttentionChange).
func (s *Server) hydrateAttention(ctx context.Context, p *pb.Session, d attentionDeps) {
	row := d.row
	if row == nil {
		return
	}
	in := attention.Input{HasPR: row.PRNumber != nil, PRState: attention.PRStateOpen, DismissedAlertKey: row.DismissedAlertKey, StateSince: row.UpdatedAt, HasTrackerLink: p.GetTrackerUrl() != ""}
	if row.StateEnteredAt != nil {
		in.StateSince = *row.StateEnteredAt
	}
	if e := d.display; e != nil {
		var checks []vcs.CheckResult
		if s.checkSnapshots != nil {
			if snaps, err := s.checkSnapshots.ListBySession(ctx, row.ID, 1); err == nil && len(snaps) > 0 && snaps[0].HeadSHA == e.HeadSHA {
				checks = snapshotChecks(snaps[0])
			}
		}
		in = sessionattention.Derive(sessionattention.Facts{HasPR: in.HasPR, IsPartial: e.IsPartial, PR: &sessionattention.PR{HeadSHA: e.HeadSHA, Status: e.Status, Checks: checks}, DismissedAlertKey: row.DismissedAlertKey}).Input
		in.StateSince = row.UpdatedAt
		if row.StateEnteredAt != nil {
			in.StateSince = *row.StateEnteredAt
		}
		in.HasTrackerLink = p.GetTrackerUrl() != ""
		in.Mergeable = e.Mergeable
		if e.HasFailures && in.ChecksFailing == 0 {
			in.ChecksFailing = 1
		}
		if e.Status == vcs.DisplayStatusNeedsHuman {
			in.ParkCodes = e.ParkCodes()
			in.ParkLabel = e.VerifyLabel
			if s.evidence != nil && e.HeadSHA != "" {
				in.OpenItems = s.evidence.OpenItems(row.ID, e.HeadSHA)
			}
		}
		if s.evidence != nil && e.HeadSHA != "" {
			if entry, ok := s.evidence.Entry(row.ID); ok && strings.EqualFold(entry.Summary.Head, e.HeadSHA) {
				summary := entry.Summary
				in.Evidence = &attention.Evidence{URL: summary.VideoURL, VideoDuration: summary.VideoDuration, Proven: summary.Proven, Total: summary.Total}
			}
		}
	}
	// The start flag Lifecycle.StartSession holds is the same one the display
	// cascade reads as "initializing", so the board and the label agree.
	in.IsInitializing = d.display != nil && d.display.SettingUp
	in.IsFactoryRepo = s.attentionFactory(ctx, row.RepoID)
	// VerifyAdmissionLabelChange down-converts these admission labels for older clients;
	// WaitingForVerifyLabelChange carries them further back to stopped/idle.
	if e := d.display; in.IsFactoryRepo && e != nil && row.PRNumber != nil && e.HeadSHA != "" {
		in.VerifyAdmission = s.attentionVerifyAdmission(row.RepoID, *row.PRNumber, e.HeadSHA)
	}
	if e := d.display; e != nil && e.HeadSHA != "" && s.checkSnapshots != nil &&
		(e.Status == vcs.DisplayStatusNeedsHuman || (attention.VerifyHolds(in) && in.Verified)) {
		// Only a park or a factory repo's verified head reads the head's
		// snapshots: the park's own start dates it, and how long verify has
		// left a verified head unmerged decides whether it is the operator's.
		in.ParkSince, in.VerifiedSince = storedHeadSince(ctx, s.checkSnapshots, row.ID, e.HeadSHA)
		in.Now = time.Now()
		in.IsSentBack = s.evidence != nil && s.evidence.SentBack(row.ID, e.HeadSHA)
	}
	q := sessionattention.AskingChat(s.chatStatus, d.chats)
	s.attentionCache.mu.Lock()
	for _, chat := range d.chats {
		if s.chatStatus == nil {
			delete(s.attentionCache.questions, chat.AgentSessionID)
			continue
		}
		e := s.chatStatus.Get(chat.AgentSessionID)
		if e == nil || e.Status != pb.ChatStatus_CHAT_STATUS_QUESTION {
			delete(s.attentionCache.questions, chat.AgentSessionID)
		}
	}
	s.attentionCache.mu.Unlock()
	if q != nil {
		in.QuestionChatID = q.ChatID
		in.QuestionText, in.QuestionAt = s.attentionQuestion(ctx, row, d.chats, q.ChatID, q.At)
	}
	if s.chatStatus != nil {
		served, _, _, _ := s.chatStatusAndWaitingAggregate(ctx, d.chats, true)
		in.ChatStatus = served
		in.ProgressPhase = s.phaseForChats(d.chats, served)
	}
	in.Fix = s.attentionFix(ctx, p, d, in.StateSince, in.ChatStatus == pb.ChatStatus_CHAT_STATUS_WORKING)
	p.Attention = attention.Compute(in)
	p.Attention.ShouldShowDisplayLabel = p.Attention.GetKind() == pb.AttentionKind_ATTENTION_KIND_FIX && displaystatus.FixShowsLabel(p)
	if !session.CanResumeSessionRun(row) || (s.lifecycle != nil && !s.lifecycle.RunResumeAvailable(ctx, row, d.chats)) {
		actions := p.Attention.Actions[:0]
		for _, action := range p.Attention.Actions {
			if action != pb.AttentionAction_ATTENTION_ACTION_RESUME {
				actions = append(actions, action)
			}
		}
		p.Attention.Actions = actions
	}
}

// attentionFactory reports whether the repo has an enabled verify job on this
// daemon: a cron job that invokes boss-verify by prompt or runs its cron gate.
// A disabled one does not count; it will not reach a head. It costs one
// cron-job read per repo a minute; a failed read is false, uncached.
func (s *Server) attentionFactory(ctx context.Context, repoID string) bool {
	if s.cronJobs == nil {
		return false
	}
	c := &s.attentionCache
	c.mu.Lock()
	defer c.mu.Unlock()
	if cached, ok := c.factories[repoID]; ok && time.Now().Before(cached.expires) {
		return cached.value
	}
	jobs, err := s.cronJobs.ListByRepo(ctx, repoID)
	if err != nil {
		return false
	}
	verify, _ := projectconfig.DefaultFactoryStages().Lookup("verify")
	value := false
	// No dated apiversion bump for the MatchesGate leg, and why: it moves a
	// repo whose only enabled verify job matches by gate from merge-ready to
	// in progress for every pin. Session.attention reaches the boss TUI, which
	// stamps its built Current but can point at a remote daemon of another
	// version, and the web through bosso, which is version-skewed. Neither can
	// get the old answer back: the in-progress row carries nothing that tells
	// it apart from any other in-progress row, and restoring it would need this
	// daemon's cron jobs, which no response carries. See
	// docs/structure/contracts.md#skipping-a-bump-justify-it-in-the-diff.
	for _, job := range jobs {
		if job != nil && job.IsEnabled && (verify.MatchesPrompt(job.Prompt) || verify.MatchesGate(job.GateCommand)) {
			value = true
			break
		}
	}
	if c.factories == nil {
		c.factories = make(map[string]factoryAttention)
	}
	entry := factoryAttention{value: value, expires: time.Now().Add(time.Minute)}
	if value {
		entry.admissionPath, entry.admission = s.readVerifyAdmission(ctx, repoID, c.factories[repoID].admissionPath)
	}
	c.factories[repoID] = entry
	return value
}

// attentionQuestion returns the words of the question chatID is asking and
// when it asked. at is when the chat tracker last saw the pane change; it keys
// the cache and is never served, since a restarted daemon re-seeds it.
func (s *Server) attentionQuestion(ctx context.Context, row *models.Session, chats []*domain.AgentChat, chatID string, at time.Time) (string, time.Time) {
	c := &s.attentionCache
	c.mu.Lock()
	defer c.mu.Unlock()
	if entry, ok := c.questions[chatID]; ok && entry.at.Equal(at) {
		return entry.text, entry.askedAt
	}
	// A single bounded transcript read per question, including unavailable reads.
	readCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	askedAt := s.questionAskedAt(readCtx, row, chats, chatID)
	text := ""
	resp, err := s.GetChatTranscript(readCtx, connect.NewRequest(&pb.GetChatTranscriptRequest{SessionId: row.ID, AgentSessionId: chatID, MaxMessages: 1}))
	if err == nil {
		text = resp.Msg.GetFinalAssistantText()
		if msgs := resp.Msg.GetMessages(); askedAt.IsZero() && len(msgs) > 0 && msgs[len(msgs)-1].GetRole() == "assistant" {
			askedAt, _ = time.Parse(time.RFC3339Nano, msgs[len(msgs)-1].GetTimestamp())
		}
	}
	if c.questions == nil {
		c.questions = make(map[string]questionAttention)
	}
	c.questions[chatID] = questionAttention{text: text, askedAt: askedAt, at: at}
	return text, askedAt
}

// questionAskedAt is when the asking chat's agent wrote its newest message,
// the turn that asked, read from the transcript a restart does not touch.
// Bookkeeping the agent CLI writes while the question waits never moves it.
// Zero when the runner cannot tell, as codex's cannot; the newest assistant
// message then dates it.
func (s *Server) questionAskedAt(ctx context.Context, row *models.Session, chats []*domain.AgentChat, chatID string) time.Time {
	i := slices.IndexFunc(chats, func(c *domain.AgentChat) bool { return c != nil && c.AgentSessionID == chatID })
	if i < 0 || row.WorktreePath == "" {
		return time.Time{}
	}
	chat := chats[i]
	agentName := chat.AgentName
	if agentName == "" {
		agentName = defaultLegacyAgent
	}
	prober, ok := s.agentClients[agentName].(interface {
		ProbeProgressLiveness(context.Context, *pb.ProbeProgressLivenessRequest) (*pb.ProbeProgressLivenessResponse, error)
	})
	if !ok {
		return time.Time{}
	}
	id := chat.AgentSessionID
	if chat.ProviderSessionID != nil && *chat.ProviderSessionID != "" {
		id = *chat.ProviderSessionID
	}
	resp, err := prober.ProbeProgressLiveness(ctx, &pb.ProbeProgressLivenessRequest{WorkDir: row.WorktreePath, AgentSessionId: id})
	if err != nil || !resp.GetIsKnown() || resp.GetLastAssistantMessageAt() == nil {
		return time.Time{}
	}
	return resp.GetLastAssistantMessageAt().AsTime()
}

// deadBootstrapSentence explains a run reaped before setup finished. Its
// worktree, branch and chat are gone, so it names the only remedy left; a cron
// run is rerun by its schedule, so archiving loses nothing.
func deadBootstrapSentence(row *models.Session) string {
	if row.CronJobID != nil && *row.CronJobID != "" {
		return "Run died before setup finished and left no worktree or chat"
	}
	return "Session died before setup finished and left no worktree or chat"
}

// fixTargetChat is the chat a FIX opens: the run's own chat while it is still
// listed, else the newest chat the session has, else none.
//
// No dated apiversion bump, and why: the old answer named the run chat even
// after it was deleted, or named none, and clients older than #3238 attach
// target_chat_id on [enter]. bossd treated that attach as a request for a
// brand-new chat, so selecting the session launched the operator into one.
// A down-convert restoring the dead id would restore that defect. See
// docs/structure/contracts.md#skipping-a-bump-justify-it-in-the-diff.
func fixTargetChat(runChatID string, chats []*domain.AgentChat) string {
	if primaryChatFromSlice(chats, runChatID) != nil {
		return runChatID
	}
	var newest *domain.AgentChat
	for _, c := range chats {
		if c != nil && (newest == nil || c.CreatedAt.After(newest.CreatedAt) || c.CreatedAt.Equal(newest.CreatedAt) && c.Seq > newest.Seq) {
			newest = c
		}
	}
	if newest == nil {
		return ""
	}
	return newest.AgentSessionID
}

func (s *Server) attentionFix(ctx context.Context, p *pb.Session, d attentionDeps, since time.Time, agentWorking bool) *attention.FixFact {
	row := d.row
	chatID := p.GetAgentSessionId()
	target := fixTargetChat(chatID, d.chats)
	fix := func(key, sentence string, at time.Time) *attention.FixFact {
		return &attention.FixFact{AlertKey: key, Sentence: sentence, ChatID: target, Since: at}
	}
	blockedFacts := session.BlockedAlertFacts(row, d.display)
	// A Blocked run whose own agent is at work on a turn the block did not see,
	// or parked on a callback it armed in this block, has nothing for the
	// operator until that turn ends or the callback fires, so no FIX of any kind:
	// the repair-exhausted leg below would otherwise re-raise the cap the block
	// already reported.
	//
	// No dated apiversion bump for this leg: Session.attention reaches the boss
	// TUI, which stamps its built Current but can point at a remote daemon of
	// another version, and the web through version-skewed bosso. A response
	// carries no transcript time and no head, so a down-convert cannot tell this
	// leg from the green one below, and restoring FIX would re-show a "needs
	// you" on a row whose agent is working. See
	// docs/structure/contracts.md#skipping-a-bump-justify-it-in-the-diff.
	if row.State == machine.Blocked && blockedFacts.RunWorking {
		return nil
	}
	// The ledger's documented run key is the original prompt chat. A new chat
	// starts a fresh budget; old attempts cannot poison a later run.
	if s.resumeAttempts != nil && chatID != "" {
		rows, err := s.resumeAttempts.ListByBudget(ctx, row.ID, session.ResumeBudgetKey(row, d.chats))
		if err == nil && len(rows) > 0 {
			attempts := make([]resumepolicy.Attempt, 0, len(rows))
			for _, a := range rows {
				class, ok := resumepolicy.ParseFailureClass(a.FailureClass)
				if ok {
					attempts = append(attempts, resumepolicy.Attempt{At: a.AttemptedAt, Class: class})
				}
			}
			if row.State == machine.Blocked && len(attempts) > 0 && resumepolicy.Next(since, attempts, time.Now()).Action == resumepolicy.ActionGiveUp {
				host, _ := os.Hostname()
				return fix(resumepolicy.AlertKey(attempts[len(attempts)-1].Class), resumepolicy.Sentence(attempts, host, time.Local), attempts[0].At)
			}
		}
	}
	// No dated apiversion bump for the green leg, and why: a blocked run whose
	// PR is green and mergeable on the attempt head moves from FIX to in
	// progress or merge for every pin. Session.attention reaches the boss TUI,
	// which stamps its built Current but can point at a remote daemon of
	// another version, and the web through bosso, which is version-skewed.
	// Neither can get the old answer back: responses omit last_attempt_head_sha,
	// so a down-convert cannot tell the old head-moved rule from the green leg,
	// and restoring it would re-show the false FIX this corrects. The FIX
	// sentence's single "Run blocked:" label is versioned by
	// apiversion.RunBlockedLabelChange. The same holds for a blocked run whose
	// checks are still running with nothing failed (alertstate.ClearCIPending):
	// it reads in progress until they settle, and a down-convert restoring FIX
	// would re-show "needs you" on a row with nothing yet to act on, the defect
	// this corrects, as with the run-working leg above. See
	// docs/structure/contracts.md#skipping-a-bump-justify-it-in-the-diff.
	staleBlocked := !alertstate.BlockedAlertCurrent(blockedFacts)
	if row.State == machine.Blocked && sessionreason.IsBootstrapStranded(row.BlockedReason) {
		return &attention.FixFact{
			AlertKey:     "blocked:" + row.State.String(),
			Sentence:     deadBootstrapSentence(row),
			Since:        since,
			WorktreeGone: true,
		}
	}
	if (row.State == machine.Blocked && !staleBlocked) || row.State == machine.Orphaned {
		sentence := "Run stopped"
		if row.State == machine.Orphaned {
			sentence = "Run lost its agent"
		}
		if row.BlockedReason != nil && strings.TrimSpace(*row.BlockedReason) != "" {
			sentence = "Run blocked: " + sessionreason.WithoutBlockedLabel(*row.BlockedReason)
		}
		return fix("blocked:"+row.State.String(), sentence, since)
	}
	_, stalled := s.repairLease.StalledAt(row.ID)
	if row.LastRepairAttemptCount >= 5 && alertstate.RepairAlertCurrent(session.RepairAlertFacts(row, d.display, stalled)) {
		return fix("repair-exhausted", "Automatic repair exhausted its attempts", since)
	}
	recovered := alertstate.WedgeRecovery{LastAgentActivityAt: protoTime(p.LastAgentActivityAt), PRDone: p.DisplayStatus == pb.DisplayStatus_DISPLAY_STATUS_MERGED || p.DisplayStatus == pb.DisplayStatus_DISPLAY_STATUS_CLOSED}
	if alertstate.AuthWedgeCurrent(p.RotationEvents, recovered) {
		return fix("auth-wedge", "Agent authentication failed", since)
	}
	if p.DisplayStatus == pb.DisplayStatus_DISPLAY_STATUS_CONFLICT && s.repos != nil {
		if repo, err := s.repos.Get(ctx, row.RepoID); err == nil && repo != nil && !repo.CanAutoRepair {
			return fix("merge-conflict", "Merge conflict", since)
		}
	}
	// A conflict in a state the repair plugin never acts on (madverts-core#6672:
	// an attended session still in ImplementingPlan) has no one coming for it,
	// so it is the operator's once the session's agent is not at work on it.
	//
	// No dated apiversion bump: Session.attention reaches the boss TUI, which
	// stamps its built Current but can point at a remote daemon of another
	// version, and the web through version-skewed bosso. A down-convert back to
	// IN_PROGRESS would restore the "idle" row that hid this conflict, which is
	// the defect, not a contract. See
	// docs/structure/contracts.md#skipping-a-bump-justify-it-in-the-diff.
	if p.DisplayStatus == pb.DisplayStatus_DISPLAY_STATUS_CONFLICT && !agentWorking && !repairActsOn(row.State) {
		return fix("merge-conflict", "Merge conflict", since)
	}
	return nil
}

// repairActsOn mirrors bossd-plugin-repair's isRepairableState for a
// conflicted PR; the plugin is a separate module, so the set is duplicated.
func repairActsOn(state machine.State) bool {
	switch state {
	case machine.AwaitingChecks, machine.FixingChecks, machine.GreenDraft, machine.ReadyForReview, machine.Finalizing:
		return true
	default:
		return false
	}
}

func (s *Server) attentionVerifyAdmission(repoID string, prNumber int, head string) attention.Admission {
	s.attentionCache.mu.Lock()
	defer s.attentionCache.mu.Unlock()
	answer := s.attentionCache.factories[repoID].admission.Lookup(prNumber, head)
	var state attention.AdmissionState
	switch answer.State {
	case verifyadmission.StateAdmitted:
		state = attention.AdmissionAdmitted
	case verifyadmission.StatePending:
		state = attention.AdmissionPending
	case verifyadmission.StateExcluded:
		state = attention.AdmissionExcluded
	}
	return attention.Admission{State: state, Reason: answer.Reason}
}

func (s *Server) readVerifyAdmission(ctx context.Context, repoID, path string) (string, verifyadmission.Snapshot) {
	if s.repos == nil {
		return "", verifyadmission.Snapshot{}
	}
	if path == "" {
		repo, err := s.repos.Get(ctx, repoID)
		if err != nil || repo == nil || repo.LocalPath == "" {
			return "", verifyadmission.Snapshot{}
		}
		path, err = verifyadmission.Path(ctx, repo.LocalPath)
		if err != nil {
			s.logger.Debug().Err(err).Str("repo_id", repoID).Msg("resolve verify admission path")
			return "", verifyadmission.Snapshot{}
		}
	}
	snapshot, err := verifyadmission.Read(path)
	if err != nil {
		s.logger.Debug().Err(err).Str("repo_id", repoID).Msg("read verify admission")
	}
	return path, snapshot
}
