package server

import (
	"context"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/recurser/bossd/internal/domain"

	"connectrpc.com/connect"
	"github.com/recurser/bossalib/attention"
	"github.com/recurser/bossalib/contract/daemon/alertstate"
	sharedplugin "github.com/recurser/bossalib/contract/plugin/plugin"
	"github.com/recurser/bossalib/displaystatus"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/kit/safego"
	"github.com/recurser/bossalib/kit/vcs"
	"github.com/recurser/bossalib/machine"
	"github.com/recurser/bossalib/models"
	"github.com/recurser/bossalib/sessionreason"
	"github.com/recurser/bossd/internal/domain/resumepolicy"
	"github.com/recurser/bossd/internal/session"
	"github.com/recurser/bossd/internal/sessionattention"
	"github.com/recurser/bossd/internal/status"
	"github.com/recurser/bossd/internal/verifyadmission"
	"github.com/recurser/bossd/internal/verifyjob"
)

type attentionCache struct {
	mu        sync.Mutex
	factories map[string]factoryAttention
	questions map[string]questionAttention
	blockers  blockerCache
}
type questionAttention struct {
	text    string
	askedAt time.Time
	at      time.Time
}
type factoryAttention struct {
	expires       time.Time
	admissionPath string
	admission     verifyadmission.Snapshot
}
type attentionDeps struct {
	row          *models.Session
	display      *status.DisplayEntry
	repo         *models.Repo
	repairLoaded bool
	chats        []*domain.AgentChat
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

// hydrateAttention supplies Session.attention, and the PR's base and stack,
// last in GetSession, ListSessions, and streamSessionHydrator.Hydrate; it
// never writes the legacy display fields.
// The additive field already has its compatibility transform (SessionAttentionChange).
func (s *Server) hydrateAttention(ctx context.Context, p *pb.Session, d attentionDeps) {
	p.PrProofDigest, p.PrProofUrl, p.PrProofHeadSha = "", "", ""
	row := d.row
	s.hydratePRStack(p, row)
	if row == nil {
		return
	}
	if s.evidence != nil && row.PRNumber != nil {
		if proof, ok := s.evidence.LatestProof(row.ID); ok && proof.Page != "" {
			p.PrProofUrl, p.PrProofHeadSha = proof.Page, proof.Head
		}
	}
	in := attention.Input{HasPR: row.PRNumber != nil, PRState: attention.PRStateOpen, DismissedAlertKey: row.DismissedAlertKey, StateSince: row.UpdatedAt, HasTrackerLink: p.GetTrackerUrl() != ""}
	if row.StateEnteredAt != nil {
		in.StateSince = *row.StateEnteredAt
	}
	if e := d.display; e != nil {
		p.PrProofDigest = s.proofDigest(row.ID, e.HeadSHA)
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
		// The tracker entry is the one the view's composite reads, so the two
		// labels agree even when the newest snapshot's head lags the entry.
		in.VerifyWaitReason = e.VerifyWaitReason()
		if e.Status == vcs.DisplayStatusNeedsHuman {
			in.ParkCodes = e.ParkCodes()
			in.ParkLabel = e.VerifyLabel
			if s.evidence != nil && e.HeadSHA != "" {
				in.OpenItems = s.evidence.OpenItems(row.ID, e.HeadSHA)
				in.ParkAsks = s.evidence.ParkAsks(row.ID, e.HeadSHA)
			}
		}
		s.hydrateAttentionEvidence(row.ID, e.HeadSHA, &in)
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
		served, reason, _, _ := s.chatStatusAndWaitingAggregate(ctx, d.chats, true)
		in.Wait = callbackWaitForSession(p, reason)
		in.ChatStatus = served
		in.ProgressPhase = s.phaseForChats(d.chats, served)
		in.ChatAtWork = served == pb.ChatStatus_CHAT_STATUS_WORKING && s.chatAtWork(d, p.GetAgentSessionId())
	}
	s.hydrateRepairAttention(ctx, p, d, &in)
	in.BlockerTallies, in.BlockerFiles = s.attentionBlockers(p, d, in)
	p.Attention = attention.Compute(in)
	p.VerifyPark = attention.SignOffPark(in)
	s.hydrateMergedConfidence(ctx, p, d)
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

// chatAtWork reports a chat mid-turn (attention.Input.ChatAtWork). A blocked or
// orphaned run's own chat is left out: one wedged mid-turn still reports
// working, and a turn the block did not see already clears the block's FIX
// (alertstate.ClearRunWorking).
func (s *Server) chatAtWork(d attentionDeps, runChatID string) bool {
	runStuck := d.row.State == machine.Blocked || d.row.State == machine.Orphaned
	for _, chat := range d.chats {
		if chat == nil || runStuck && chat.AgentSessionID == runChatID {
			continue
		}
		e := s.chatStatus.Get(chat.AgentSessionID)
		if e == nil {
			continue
		}
		if st, _ := status.PromoteWaiting(e.Status, s.chatStatus.Waiting(chat.AgentSessionID)); st == pb.ChatStatus_CHAT_STATUS_WORKING {
			return true
		}
	}
	return false
}

// callbackWaitForSession converts the served reason into plain attention facts.
func callbackWaitForSession(p *pb.Session, reason string) attention.CallbackWait {
	wait := attention.CallbackWait{Trigger: displaystatus.CallbackWaitingTrigger(reason)}
	nwo := vcs.GitHubNWO(p.GetPrUrl())
	if nwo == "" {
		nwo = vcs.GitHubNWO(p.GetRepoOriginUrl())
	}
	if !displaystatus.IsOwnPRWaitingReason(reason, nwo, int(p.GetPrNumber())) {
		_, _, wait.OtherPR, _ = displaystatus.CallbackWaitingTarget(reason)
	}
	return wait
}

// attentionFactory reports whether the repo has an enabled verify job on this
// daemon: a cron job that invokes boss-verify by prompt or runs its cron gate.
// A disabled one does not count; it will not reach a head. It costs one
// cron-job read per repo a minute; a failed read is false, uncached.
func (s *Server) attentionFactory(ctx context.Context, repoID string) bool {
	c := &s.attentionCache
	c.mu.Lock()
	if s.factoryRepoChecker == nil {
		s.factoryRepoChecker = verifyjob.New(s.cronJobs)
	}
	checker := s.factoryRepoChecker
	c.mu.Unlock()
	value := checker.IsFactoryRepo(ctx, repoID)
	if !value {
		return false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if cached, ok := c.factories[repoID]; ok && time.Now().Before(cached.expires) {
		return value
	}
	// No dated apiversion bump for the MatchesGate leg, and why: it moves a
	// repo whose only enabled verify job matches by gate from merge-ready to
	// in progress for every pin. Session.attention reaches the boss TUI, which
	// stamps its built Current but can point at a remote daemon of another
	// version, and the web through bosso, which is version-skewed. Neither can
	// get the old answer back: the in-progress row carries nothing that tells
	// it apart from any other in-progress row, and restoring it would need this
	// daemon's cron jobs, which no response carries. See
	// docs/structure/contracts.md#skipping-a-bump-justify-it-in-the-diff.
	if c.factories == nil {
		c.factories = make(map[string]factoryAttention)
	}
	entry := factoryAttention{expires: time.Now().Add(time.Minute)}
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
	// PR is green and mergeable moves from FIX to in
	// progress or merge for every pin. Session.attention reaches the boss TUI,
	// which stamps its built Current but can point at a remote daemon of
	// another version, and the web through bosso, which is version-skewed.
	// Neither can get the old answer back: responses omit last_attempt_head_sha,
	// so a down-convert cannot recover the old head-moved rule; a standing
	// block now stays FIX on a moved head until green and mergeable,
	// restoring it would hide a block that still needs action. The FIX
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
	if row.LastRepairAttemptCount >= repairExhaustedAttempts && alertstate.RepairAlertCurrent(session.RepairAlertFacts(row, d.display, stalled)) {
		return fix("repair-exhausted", "Automatic repair exhausted its attempts", since)
	}
	recovered := alertstate.WedgeRecovery{LastAgentActivityAt: protoTime(p.LastAgentActivityAt), PRDone: p.DisplayStatus == pb.DisplayStatus_DISPLAY_STATUS_MERGED || p.DisplayStatus == pb.DisplayStatus_DISPLAY_STATUS_CLOSED}
	if alertstate.AuthWedgeCurrent(p.RotationEvents, recovered) {
		return fix("auth-wedge", "Agent authentication failed", since)
	}
	// No dated apiversion bump: the version-skewed TUI and web must not
	// promise a rebase when no repair workflow is loaded. A down-convert would
	// restore that false promise, rather than a useful compatibility contract.
	// See docs/structure/contracts.md#skipping-a-bump-justify-it-in-the-diff.
	if p.DisplayStatus == pb.DisplayStatus_DISPLAY_STATUS_CONFLICT && d.repo != nil && (!d.repo.CanAutoRepair || !d.repairLoaded) {
		return fix("merge-conflict", "Merge conflict", since)
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
	// No dated apiversion bump: attention is consumed by the version-skewed
	// TUI and web through bosso. Down-converting FIX back to IN_PROGRESS would
	// restore the false waiting row this correction removes. See
	// docs/structure/contracts.md#skipping-a-bump-justify-it-in-the-diff.
	return s.stuckWaitFix(ctx, p, d, agentWorking)
}

func (s *Server) hydrateRepairAttention(ctx context.Context, p *pb.Session, d attentionDeps, in *attention.Input) {
	d.repairLoaded = s.repairWorkflowLoaded == nil || s.repairWorkflowLoaded()
	// Share one authoritative repository read across conflict FIX, conflict
	// rebase and armed repair ownership.
	if (p.DisplayStatus == pb.DisplayStatus_DISPLAY_STATUS_CONFLICT || repairMayArm(d)) && s.repos != nil {
		d.repo, _ = s.repos.Get(ctx, d.row.RepoID)
	}
	in.RepairOwned = s.repairOwned(p, d)
	in.Fix = s.attentionFix(ctx, p, d, in.StateSince, in.ChatStatus == pb.ChatStatus_CHAT_STATUS_WORKING)
	if in.Fix != nil || !conflictRebaseOwned(d) {
		return
	}
	// A completed conflict attempt on this head is deduplicated by repair.
	// Display snapshots have no current base SHA: a base-move retry owns the
	// conflict once it takes a live lease, not merely because repair is enabled.
	// Until then the existing conflict FIX is truthful, rather than promising
	// a rebase that may never run. A new head is eligible again immediately.
	if !s.repairLease.Active(d.row.ID) && repairAttemptFinishedOn(d.row, d.display) {
		in.Fix = &attention.FixFact{AlertKey: "merge-conflict", Sentence: "Merge conflict", Since: in.StateSince}
		return
	}
	in.IsRebasing = true
}

// repairExhaustedAttempts is the consecutive failed repair attempts after which
// automatic repair has given up on a PR: its FIX reads "repair exhausted" and
// nothing arms another attempt.
const repairExhaustedAttempts = 5

// repairOwned reports whether automatic repair runs on the session's PR, or is
// armed to (repairArmed): a live repair lease or a repair the display reports.
// attention.Compute moves a fault repair owns from FIX to in progress.
func (s *Server) repairOwned(p *pb.Session, d attentionDeps) bool {
	if s.repairLease.Active(d.row.ID) || p.GetDisplayIsRepairing() || d.display != nil && d.display.IsRepairing {
		return true
	}
	return s.repairArmed(d)
}

// repairArmed reports whether the repair plugin will take the PR on its next
// trigger: repairMayArm holds, the repository allows automatic repair and no
// stalled lease is recorded.
func (s *Server) repairArmed(d attentionDeps) bool {
	if !repairMayArm(d) || d.repo == nil || !d.repo.CanAutoRepair {
		return false
	}
	_, stalled := s.repairLease.StalledAt(d.row.ID)
	return !stalled
}

// repairMayArm is repairArmed's session half, checked before the repository
// read: a failing or changes-requested PR, which are the plugin's repair
// triggers besides a conflict (IsRebasing covers that), on a session it
// repairs in a state it acts on, with attempts left, no refusal recorded and
// no finished attempt on this head and status. The plugin skips that repeated
// input; a moved base or new review feedback can unlock it, but display
// snapshots carry neither, so such a retry owns the PR once it takes a lease.
func repairMayArm(d attentionDeps) bool {
	e := d.display
	return e != nil && (e.Status == vcs.DisplayStatusFailing || e.Status == vcs.DisplayStatusRejected) &&
		d.row.PRNumber != nil && !d.row.IsVerifyOnly && repairArmsOn(d.row.State) &&
		d.row.LastRepairAttemptCount < repairExhaustedAttempts && d.row.LastRepairBlockedAt == nil &&
		!repairAttemptFinishedOn(d.row, e)
}

// repairArmsOn mirrors bossd-plugin-repair's isRepairableState for a failing
// or changes-requested PR, which excludes Finalizing; the plugin is a separate
// module, so the set is duplicated.
func repairArmsOn(state machine.State) bool {
	return state != machine.Finalizing && repairActsOn(state)
}

// repairAttemptFinishedOn reports whether the last repair attempt acted on the
// display's head and status and finished, mirroring the repair plugin's
// duplicate-input gate: runner failures and incomplete interactive chats are
// still retryable on the same input, so those outcomes do not count.
// vcs.DisplayStatus and the recorded proto status share their numbering.
func repairAttemptFinishedOn(row *models.Session, e *status.DisplayEntry) bool {
	return e.HeadSHA != "" && row.LastRepairHeadSHA == e.HeadSHA &&
		vcs.DisplayStatus(row.LastRepairDisplayStatus) == e.Status &&
		row.LastRepairRunnerError == "" &&
		!sharedplugin.ChatRunRetryable(row.LastRepairExitError)
}

// conflictRebaseOwned reports whether automatic repair owns this conflict.
// List and stream snapshots may omit RepoCanAutoRepair, so use the repository read.
func conflictRebaseOwned(d attentionDeps) bool {
	return d.display != nil && d.display.Status == vcs.DisplayStatusConflict &&
		!d.row.IsVerifyOnly && repairActsOn(d.row.State) && d.repo != nil && d.repo.CanAutoRepair && d.repairLoaded
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
	return attention.Admission{State: state, Reason: answer.Reason, Rule: answer.Rule}
}

// readVerifyAdmission is loadVerifyAdmission for attention, where an
// unreadable file only leaves admission unknown.
func (s *Server) readVerifyAdmission(ctx context.Context, repoID, path string) (string, verifyadmission.Snapshot) {
	if s.repos == nil {
		return "", verifyadmission.Snapshot{}
	}
	path, snapshot, err := s.loadVerifyAdmission(ctx, repoID, path)
	if err != nil {
		s.logger.Debug().Err(err).Str("repo_id", repoID).Msg("read verify admission")
	}
	return path, snapshot
}

// loadVerifyAdmission reads repoID's verify admission file at path, resolving
// the path from the repository's checkout when it is empty. A missing file is
// an empty snapshot; an unresolvable path or an unreadable, unparseable or
// unsupported file is an error, so a caller can tell "could not look" from
// "nothing recorded".
func (s *Server) loadVerifyAdmission(ctx context.Context, repoID, path string) (string, verifyadmission.Snapshot, error) {
	if path == "" {
		if s.repos == nil {
			return "", verifyadmission.Snapshot{}, errors.New("no repository store")
		}
		repo, err := s.repos.Get(ctx, repoID)
		if err != nil {
			return "", verifyadmission.Snapshot{}, fmt.Errorf("get repository: %w", err)
		}
		if repo == nil || repo.LocalPath == "" {
			return "", verifyadmission.Snapshot{}, errors.New("repository has no local path")
		}
		if path, err = verifyadmission.Path(ctx, repo.LocalPath); err != nil {
			return "", verifyadmission.Snapshot{}, err
		}
	}
	snapshot, err := verifyadmission.Read(path)
	return path, snapshot, err
}

// blockerRetryBackoff is how long a failed blockers read waits before a later
// hydrate may read the same session's head again.
const blockerRetryBackoff = 30 * time.Second

// blockerAbsentTTL is how long an absent blockers file is served as no
// blockers. Absent means verify has not posted for the head yet, and it may
// park the same head later, so the empty entry expires and is read again.
const blockerAbsentTTL = 2 * time.Minute

// blockerCache holds, per session, the standing verify blockers bossd last
// read for one PR head. Hydrate serves an entry only for the head the display
// tracker holds and never waits on a read: a miss starts at most one
// background read per session. An absent blockers file caches as no blockers
// for blockerAbsentTTL; an unreadable file or a failed read is not cached and
// waits blockerRetryBackoff.
type blockerCache struct {
	mu       sync.Mutex
	entries  map[string]blockerEntry
	inFlight map[string]bool
	failures map[string]blockerFailure
	// now and refreshed are test seams: the clock the backoff reads, and a
	// receiver for each background read's done channel.
	now       func() time.Time
	refreshed func(<-chan struct{})
}

type blockerEntry struct {
	head    string
	tallies []attention.BlockerTally
	files   []string
	// readAt is when the read behind the entry started; an older read never
	// replaces it. expiresAt, when set, ends an absent entry.
	readAt    time.Time
	expiresAt time.Time
}

func (e blockerEntry) servable(head string, now time.Time) bool {
	return strings.EqualFold(e.head, head) && (e.expiresAt.IsZero() || now.Before(e.expiresAt))
}

type blockerFailure struct {
	head    string
	retryAt time.Time
}

func (c *blockerCache) clock() time.Time {
	if c.now != nil {
		return c.now()
	}
	return time.Now()
}

// attentionBlockers returns the blocker tallies and files hydrate serves for
// the session's displayed head. On a miss for a candidate it starts the
// session's one background read and serves none until that read lands.
func (s *Server) attentionBlockers(p *pb.Session, d attentionDeps, in attention.Input) ([]attention.BlockerTally, []string) {
	c := &s.attentionCache.blockers
	id := d.row.ID
	candidate := s.blockerCandidate(p, d, in)
	c.mu.Lock()
	if !candidate {
		delete(c.entries, id)
		delete(c.failures, id)
		c.mu.Unlock()
		return nil, nil
	}
	head := d.display.HeadSHA
	if e, ok := c.entries[id]; ok && e.servable(head, c.clock()) {
		c.mu.Unlock()
		return e.tallies, e.files
	}
	f, failed := c.failures[id]
	if c.inFlight[id] || failed && strings.EqualFold(f.head, head) && c.clock().Before(f.retryAt) {
		c.mu.Unlock()
		return nil, nil
	}
	if c.inFlight == nil {
		c.inFlight = make(map[string]bool)
	}
	c.inFlight[id] = true
	refreshed := c.refreshed
	c.mu.Unlock()
	// The list must not wait, so only the test seam joins done; the read
	// itself ends within domain.VerifyVerbTimeout.
	done := safego.Go(s.logger, func() { s.refreshBlockers(id, head) })
	if refreshed != nil {
		refreshed(done)
	}
	return nil, nil
}

// blockerCandidate reports whether a session's blockers are worth a read: an
// open PR at a known head with a verify park, a live repair lease, a repair
// the display reports, or a recorded repair attempt. Other sessions, and a
// server wired without verify verbs, never read.
func (s *Server) blockerCandidate(p *pb.Session, d attentionDeps, in attention.Input) bool {
	e := d.display
	if s.newVerifyVerbs == nil && s.verifyVerbsFactory == nil || e == nil || e.HeadSHA == "" || d.row.PRNumber == nil || !in.HasPR || in.PRState != attention.PRStateOpen {
		return false
	}
	return len(e.ParkCodes()) > 0 || s.repairLease.Active(d.row.ID) || p.GetDisplayIsRepairing() || d.row.LastRepairAttemptCount > 0
}

// refreshBlockers is the background read attentionBlockers starts: the verify
// bridge's blockers verb for head, bounded by domain.VerifyVerbTimeout. A read
// that fails, or panics, frees the session for a retry after the backoff.
func (s *Server) refreshBlockers(id, head string) {
	c := &s.attentionCache.blockers
	failed := true
	defer func() {
		c.mu.Lock()
		defer c.mu.Unlock()
		delete(c.inFlight, id)
		if failed {
			if c.failures == nil {
				c.failures = make(map[string]blockerFailure)
			}
			c.failures[id] = blockerFailure{head: head, retryAt: c.clock().Add(blockerRetryBackoff)}
		}
	}()
	readAt := c.clock()
	ctx, cancel := context.WithTimeout(context.Background(), domain.VerifyVerbTimeout)
	defer cancel()
	result, err := s.readSessionBlockers(ctx, id, head)
	if err != nil || !blockerOutcomeCacheable(result.Outcome) {
		s.logger.Debug().Err(err).Str("session", id).Str("outcome", result.Outcome).Msg("attention blockers: read failed; retrying after backoff")
		return
	}
	failed = false
	s.recordBlockers(id, head, readAt, result)
}

func (s *Server) readSessionBlockers(ctx context.Context, id, head string) (domain.VerifyBlockers, error) {
	target, env, err := s.verifyTarget(ctx, id, false)
	if err != nil {
		return domain.VerifyBlockers{}, err
	}
	return s.verifyVerbsFor(env).Blockers(ctx, target, head)
}

// blockerOutcomeCacheable reports whether a blockers read answered for the
// head. Unreadable is a failed comments or permission read, so it retries.
func blockerOutcomeCacheable(outcome string) bool {
	return outcome == "absent" || outcome == "written"
}

// recordBlockers caches a blockers read for head that started at readAt,
// unless the displayed head has moved since, when the entry could never be
// served, or a read that started later already filled the entry.
func (s *Server) recordBlockers(id, head string, readAt time.Time, result domain.VerifyBlockers) {
	if !blockerOutcomeCacheable(result.Outcome) || !strings.EqualFold(s.displayedPRHead(id), head) {
		return
	}
	blockers := make([]attention.Blocker, 0, len(result.Blockers))
	for _, b := range result.Blockers {
		blockers = append(blockers, attention.Blocker{Kind: b.Kind, Severity: b.Severity, File: b.File})
	}
	entry := blockerEntry{head: head, readAt: readAt}
	if result.Outcome == "written" {
		entry.tallies, entry.files = attention.BlockerTallies(blockers)
	} else {
		entry.expiresAt = readAt.Add(blockerAbsentTTL)
	}
	c := &s.attentionCache.blockers
	c.mu.Lock()
	defer c.mu.Unlock()
	if prev, ok := c.entries[id]; ok && strings.EqualFold(prev.head, head) && prev.readAt.After(readAt) {
		return
	}
	if c.entries == nil {
		c.entries = make(map[string]blockerEntry)
	}
	c.entries[id] = entry
	delete(c.failures, id)
}

func (s *Server) proofDigest(sessionID, head string) string {
	if s.evidence == nil || head == "" {
		return ""
	}
	proof, ok := s.evidence.Proof(sessionID, head)
	if !ok || proof.Legacy || !strings.EqualFold(proof.Head, head) {
		return ""
	}
	return proof.Digest
}
