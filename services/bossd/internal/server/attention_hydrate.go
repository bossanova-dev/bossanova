package server

import (
	"context"
	"os"
	"strings"
	"sync"
	"time"

	"connectrpc.com/connect"
	"github.com/recurser/bossalib/alertstate"
	"github.com/recurser/bossalib/attention"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/machine"
	"github.com/recurser/bossalib/models"
	"github.com/recurser/bossalib/projectconfig"
	"github.com/recurser/bossalib/resumepolicy"
	"github.com/recurser/bossalib/vcs"
	"github.com/recurser/bossd/internal/session"
	"github.com/recurser/bossd/internal/sessionattention"
	"github.com/recurser/bossd/internal/status"
)

type attentionCache struct {
	mu        sync.Mutex
	factories map[string]factoryAttention
	questions map[string]questionAttention
}
type questionAttention struct {
	text string
	at   time.Time
}
type factoryAttention struct {
	value   bool
	expires time.Time
}
type attentionDeps struct {
	row     *models.Session
	display *status.DisplayEntry
	chats   []*models.AgentChat
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
	var chats []*models.AgentChat
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
			if snaps, err := s.checkSnapshots.RecentBySession(ctx, row.ID, 1); err == nil && len(snaps) > 0 && snaps[0].HeadSHA == e.HeadSHA {
				checks = snapshotChecks(snaps[0])
			}
		}
		in = sessionattention.Derive(sessionattention.Facts{HasPR: in.HasPR, PR: &sessionattention.PR{HeadSHA: e.HeadSHA, Status: e.Status, Checks: checks}, DismissedAlertKey: row.DismissedAlertKey}).Input
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
			in.ParkCodes = strings.Split(e.VerifyReason, ",")
			in.ParkLabel = e.VerifyLabel
		}
		if s.evidence != nil && e.HeadSHA != "" {
			if entry, ok := s.evidence.Entry(row.ID); ok && strings.EqualFold(entry.Summary.Head, e.HeadSHA) {
				summary := entry.Summary
				in.Evidence = &attention.Evidence{URL: summary.VideoURL, VideoDuration: summary.VideoDuration, Proven: summary.Proven, Total: summary.Total}
			}
		}
	}
	in.IsFactoryRepo = s.attentionFactory(ctx, row.RepoID)
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
		in.QuestionText = s.attentionQuestion(ctx, row.ID, q.ChatID, q.At)
	}
	if s.chatStatus != nil {
		served, _, _, _ := s.chatStatusAndWaitingAggregate(ctx, d.chats, true)
		in.ChatWorking = served == pb.ChatStatus_CHAT_STATUS_WORKING
		in.ProgressPhase = s.phaseForChats(d.chats, served)
	}
	in.Fix = s.attentionFix(ctx, p, d, in.StateSince)
	p.Attention = attention.Compute(in)
}

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
	for _, job := range jobs {
		if job != nil && job.IsEnabled && verify.MatchesPrompt(job.Prompt) {
			value = true
			break
		}
	}
	if c.factories == nil {
		c.factories = make(map[string]factoryAttention)
	}
	c.factories[repoID] = factoryAttention{value: value, expires: time.Now().Add(time.Minute)}
	return value
}

func (s *Server) attentionQuestion(ctx context.Context, sessionID, chatID string, at time.Time) string {
	c := &s.attentionCache
	c.mu.Lock()
	defer c.mu.Unlock()
	if entry, ok := c.questions[chatID]; ok && entry.at.Equal(at) {
		return entry.text
	}
	// A single bounded transcript read per question, including unavailable reads.
	readCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	text := ""
	resp, err := s.GetChatTranscript(readCtx, connect.NewRequest(&pb.GetChatTranscriptRequest{SessionId: sessionID, AgentSessionId: chatID, MaxMessages: 1}))
	if err == nil {
		text = resp.Msg.GetFinalAssistantText()
	}
	if c.questions == nil {
		c.questions = make(map[string]questionAttention)
	}
	c.questions[chatID] = questionAttention{text: text, at: at}
	return text
}

func (s *Server) attentionFix(ctx context.Context, p *pb.Session, d attentionDeps, since time.Time) *attention.FixFact {
	row := d.row
	chatID := p.GetAgentSessionId()
	fix := func(key, sentence string, at time.Time) *attention.FixFact {
		return &attention.FixFact{AlertKey: key, Sentence: sentence, ChatID: chatID, Since: at}
	}
	// The ledger's documented run key is the original prompt chat. A new chat
	// starts a fresh budget; old attempts cannot poison a later run.
	if s.resumeAttempts != nil && chatID != "" {
		rows, err := s.resumeAttempts.ListForRun(ctx, row.ID, chatID)
		if err == nil && len(rows) > 0 {
			attempts := make([]resumepolicy.Attempt, 0, len(rows))
			for _, a := range rows {
				class, ok := resumepolicy.ParseFailureClass(a.FailureClass)
				if ok {
					attempts = append(attempts, resumepolicy.Attempt{At: a.AttemptedAt, Class: class})
				}
			}
			if len(attempts) > 0 && resumepolicy.Next(since, attempts, time.Now()).Action == resumepolicy.ActionGiveUp && !protoTime(p.LastAgentActivityAt).After(attempts[len(attempts)-1].At) {
				host, _ := os.Hostname()
				return fix(resumepolicy.AlertKey(attempts[len(attempts)-1].Class), resumepolicy.Sentence(attempts, host, time.Local), attempts[0].At)
			}
		}
	}
	staleBlocked := d.display != nil && d.display.HeadSHA != "" && row.LastAttemptHeadSHA != nil && *row.LastAttemptHeadSHA != "" && !strings.EqualFold(*row.LastAttemptHeadSHA, d.display.HeadSHA)
	if (row.State == machine.Blocked && !staleBlocked) || row.State == machine.Orphaned {
		sentence := "Run stopped · open the chat, then resume"
		if row.State == machine.Orphaned {
			sentence = "Run lost its agent · open the chat, then resume"
		}
		if row.BlockedReason != nil && strings.TrimSpace(*row.BlockedReason) != "" {
			sentence = "Run blocked: " + *row.BlockedReason + " · open the chat, then resume"
		}
		return fix("blocked:"+row.State.String(), sentence, since)
	}
	_, stalled := s.repairLease.StalledAt(row.ID)
	if row.LastRepairAttemptCount >= 5 && alertstate.RepairAlertCurrent(session.RepairAlertFacts(row, d.display, stalled)) {
		return fix("repair-exhausted", "Automatic repair exhausted its attempts · open the chat, then resume", since)
	}
	recovered := alertstate.WedgeRecovery{LastAgentActivityAt: protoTime(p.LastAgentActivityAt), PRDone: p.DisplayStatus == pb.DisplayStatus_DISPLAY_STATUS_MERGED || p.DisplayStatus == pb.DisplayStatus_DISPLAY_STATUS_CLOSED}
	if alertstate.AuthWedgeCurrent(p.RotationEvents, recovered) {
		return fix("auth-wedge", "Agent authentication failed · log in, then resume", since)
	}
	if p.DisplayStatus == pb.DisplayStatus_DISPLAY_STATUS_CONFLICT && s.repos != nil {
		if repo, err := s.repos.Get(ctx, row.RepoID); err == nil && repo != nil && !repo.CanAutoRepair {
			return fix("merge-conflict", "Merge conflict needs repair · resolve the conflict, then resume", since)
		}
	}
	return nil
}
