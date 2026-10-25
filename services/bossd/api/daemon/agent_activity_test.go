package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/recurser/bossd/internal/domain"

	"connectrpc.com/connect"
	"github.com/rs/zerolog"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/recurser/bossalib/contract/daemon/alertstate"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/kit/vcs"
	"github.com/recurser/bossd/internal/db"
	"github.com/recurser/bossd/internal/domain/agenttelemetry"
	"github.com/recurser/bossd/internal/sessionattention"
	"github.com/recurser/bossd/internal/status"
)

// giveUpEvents is a lost login the daemon gave up on at 23:16, newest first.
func giveUpEvents() []*pb.RotationEvent {
	return []*pb.RotationEvent{{
		Trigger:   pb.RotationTrigger_ROTATION_TRIGGER_AUTH_INVALIDATED,
		Outcome:   pb.RotationOutcome_ROTATION_OUTCOME_RESPAWN_CAP_EXHAUSTED,
		CreatedAt: timestamppb.New(at(16)),
	}}
}

// fakeTally serves per-chat counts and records each window it was asked for.
type fakeTally struct {
	mu     sync.Mutex
	counts map[string]agenttelemetry.Counts
	errs   map[string]error
	since  []time.Time
}

func (f *fakeTally) tally(_ context.Context, _ string, chat *domain.AgentChat, since, _ time.Time) (agenttelemetry.Counts, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.since = append(f.since, since)
	if err := f.errs[chat.AgentSessionID]; err != nil {
		return agenttelemetry.Counts{}, err
	}
	return f.counts[chat.AgentSessionID], nil
}

func (f *fakeTally) calls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.since)
}

func runsOf(runs ...domain.AgentRun) func(context.Context, string) ([]domain.AgentRun, error) {
	return func(context.Context, string) ([]domain.AgentRun, error) { return runs, nil }
}

var twoChats = []*domain.AgentChat{{AgentSessionID: "a"}, {AgentSessionID: "b"}}

// restartProof asks w for session s1's activity, judging the give-up on
// events the way session hydration does.
func restartProof(w WedgeActivity, live time.Time, events []*pb.RotationEvent, chats []*domain.AgentChat) time.Time {
	q := WedgeQuery{SessionID: "s1", WorktreePath: "/wt", Live: live, Chats: chats}
	if giveUpAt, ok := sessionattention.NewestGiveUpAtProto(events); ok {
		q.GiveUpAt = giveUpAt
	}
	return w.RestartProofActivity(context.Background(), q)
}

// recovered is whether the activity RestartProofActivity returns shows the
// 23:16 give-up recovered, by the rule every reader applies.
func recovered(activity time.Time) bool {
	return alertstate.WedgeRecovery{LastAgentActivityAt: activity}.RecoveredSince(at(16))
}

// A productive run that started after the give-up is persisted recovery: a
// clean stop (headless) or output tokens (an interactive chat stops as
// "stopped"). A run with no output, the zero-token auth failure, a run still
// going or stopped abnormally, and a productive run that began before the
// give-up (the run that lost the login) prove nothing.
func TestRestartProofActivity_ProductiveRunsAfterTheGiveUp(t *testing.T) {
	tokens := func(n int64) *int64 { return &n }
	withTokens := func(r domain.AgentRun, calls int64, out *int64) domain.AgentRun {
		r.ParentModelCallCount = calls
		r.OutputTokenCount = out
		return r
	}
	for name, tc := range map[string]struct {
		run  domain.AgentRun
		want time.Time
	}{
		"clean stop after the give-up":       {run: buildRun(at(18), timePtr(at(24)), db.AgentRunStopClean, ""), want: at(24)},
		"stopped with output after it":       {run: withTokens(buildRun(at(18), timePtr(at(24)), db.AgentRunStopStopped, ""), 3, tokens(420)), want: at(24)},
		"open with output after it":          {run: withTokens(buildRun(at(18), nil, "", ""), 3, tokens(420)), want: at(18)},
		"no model call":                      {run: buildRun(at(18), timePtr(at(24)), db.AgentRunStopStopped, "")},
		"zero-token auth failure transcript": {run: withTokens(buildRun(at(18), timePtr(at(24)), db.AgentRunStopStopped, ""), 1, tokens(0))},
		"still running":                      {run: buildRun(at(18), nil, "", "")},
		"stopped abnormally":                 {run: buildRun(at(18), timePtr(at(19)), db.AgentRunStopUnknown, "")},
		"clean before the give-up":           {run: buildRun(at(10), timePtr(at(15)), db.AgentRunStopClean, "")},
		"clean, started before it":           {run: buildRun(at(4), timePtr(at(18)), db.AgentRunStopClean, "")},
		"output, started before it":          {run: withTokens(buildRun(at(10), timePtr(at(24)), db.AgentRunStopStopped, ""), 3, tokens(420))},
	} {
		t.Run(name, func(t *testing.T) {
			w := WedgeActivity{Runs: runsOf(tc.run), Tally: (&fakeTally{}).tally}
			got := restartProof(w, time.Time{}, giveUpEvents(), nil)
			if !got.Equal(tc.want) || recovered(got) != !tc.want.IsZero() {
				t.Fatalf("activity = %v, want %v", got, tc.want)
			}
		})
	}
}

// An in-pane /login continues the run that lost the login, and a restart
// forgets the live output, so the transcripts prove the recovery: the newest
// message with output tokens after the give-up is the activity. A zero-token
// message (the transcribed auth failure) and an unreadable transcript prove
// nothing.
func TestRestartProofActivity_TranscriptOutputAfterTheGiveUp(t *testing.T) {
	for name, tc := range map[string]struct {
		counts map[string]agenttelemetry.Counts
		errs   map[string]error
		want   time.Time
	}{
		"one chat wrote output": {
			counts: map[string]agenttelemetry.Counts{"a": {LastOutputAt: at(20)}, "b": {LastOutputAt: at(22)}},
			want:   at(22),
		},
		"only a zero-token auth failure": {
			counts: map[string]agenttelemetry.Counts{"a": {ParentModelCallCount: 1}},
		},
		"output at the give-up instant is not after it": {
			counts: map[string]agenttelemetry.Counts{"a": {LastOutputAt: at(16)}},
		},
		"unreadable transcripts prove nothing": {
			errs: map[string]error{"a": os.ErrNotExist, "b": errors.New("boom")},
		},
	} {
		t.Run(name, func(t *testing.T) {
			tally := &fakeTally{counts: tc.counts, errs: tc.errs}
			w := WedgeActivity{Tally: tally.tally}
			got := restartProof(w, time.Time{}, giveUpEvents(), twoChats)
			if !got.Equal(tc.want) {
				t.Fatalf("activity = %v, want %v", got, tc.want)
			}
			for _, since := range tally.since {
				if !since.Equal(at(16)) {
					t.Fatalf("tally window starts at %v, want the give-up %v", since, at(16))
				}
			}
		})
	}
}

// The persisted reads happen only when they decide something: the newest
// event gave up and live output is not after it.
func TestRestartProofActivity_ReadsOnlyOnTheGiveUpPath(t *testing.T) {
	resumed := []*pb.RotationEvent{{
		Trigger:   pb.RotationTrigger_ROTATION_TRIGGER_AUTH_INVALIDATED,
		Outcome:   pb.RotationOutcome_ROTATION_OUTCOME_RESPAWNED_SAME_ACCOUNT,
		CreatedAt: timestamppb.New(at(16)),
	}}
	for name, tc := range map[string]struct {
		live   time.Time
		events []*pb.RotationEvent
	}{
		"no rotation events":              {},
		"newest event resumed":            {events: resumed},
		"live output after the give-up":   {live: at(17), events: giveUpEvents()},
		"newest event gave up, live none": {events: giveUpEvents()},
	} {
		t.Run(name, func(t *testing.T) {
			tally := &fakeTally{counts: map[string]agenttelemetry.Counts{"a": {LastOutputAt: at(30)}}}
			runsRead := false
			w := WedgeActivity{Tally: tally.tally, Runs: func(context.Context, string) ([]domain.AgentRun, error) {
				runsRead = true
				return nil, nil
			}}
			got := restartProof(w, tc.live, tc.events, twoChats)
			onPath := name == "newest event gave up, live none"
			if (tally.calls() > 0) != onPath || runsRead != onPath {
				t.Fatalf("tally calls %d, runs read %v; want reads only on the give-up path", tally.calls(), runsRead)
			}
			want := tc.live
			if onPath {
				want = at(30)
			}
			if !got.Equal(want) {
				t.Fatalf("activity = %v, want %v", got, want)
			}
		})
	}
}

// The cache answers repeat reads of one give-up for wedgeActivityTTL, then
// reads again.
func TestRestartProofActivity_CacheBoundsRereads(t *testing.T) {
	now := at(30)
	cache := clockedWedgeCache(func() time.Time { return now })
	tally := &fakeTally{}
	w := WedgeActivity{Tally: tally.tally, Cache: cache}
	read := func() time.Time {
		return restartProof(w, time.Time{}, giveUpEvents(), twoChats[:1])
	}
	if got := read(); !got.IsZero() || tally.calls() != 1 {
		t.Fatalf("first read: activity %v, calls %d", got, tally.calls())
	}
	tally.mu.Lock()
	tally.counts = map[string]agenttelemetry.Counts{"a": {LastOutputAt: at(25)}}
	tally.mu.Unlock()
	now = now.Add(wedgeActivityTTL - time.Second)
	if got := read(); !got.IsZero() || tally.calls() != 1 {
		t.Fatalf("within the TTL: activity %v, calls %d; want the cached answer", got, tally.calls())
	}
	now = now.Add(time.Second)
	if got := read(); !got.Equal(at(25)) || tally.calls() != 2 {
		t.Fatalf("after the TTL: activity %v, calls %d; want a fresh read", got, tally.calls())
	}
}

// After a bossd restart the live tracker is empty. The session a client reads
// (GetSession, which feeds the TUI and, through the same hydrator, the web)
// and the timeline must then agree on whether a lost login recovered, because
// both judge it on the one restart-proof last_agent_activity_at.
func TestRestartProofActivity_HydratedSessionAndTimelineAgreeAfterARestart(t *testing.T) {
	for name, tc := range map[string]struct {
		counts    map[string]agenttelemetry.Counts
		recovered bool
	}{
		"in-pane /login wrote output after the give-up": {counts: map[string]agenttelemetry.Counts{"agent-1": {LastOutputAt: at(20)}}, recovered: true},
		"still wedged": {},
	} {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			tally := &fakeTally{counts: tc.counts}
			srv, sessID := wedgeFixture(t, tally, wedgeSetup{})
			// Each read computes on its own, so agreement is not a cache hit.
			srv.wedgeCache = nil

			got, err := srv.GetSession(ctx, connect.NewRequest(&pb.GetSessionRequest{Id: sessID}))
			if err != nil {
				t.Fatalf("GetSession: %v", err)
			}
			p := got.Msg.GetSession()
			hintWedged := wedgedPerHint(p)
			listed, err := srv.ListSessions(ctx, connect.NewRequest(&pb.ListSessionsRequest{}))
			if err != nil || len(listed.Msg.GetSessions()) != 1 {
				t.Fatalf("ListSessions = %v, %v; want the one session", listed, err)
			}
			if listWedged := wedgedPerHint(listed.Msg.GetSessions()[0]); listWedged != hintWedged {
				t.Fatalf("ListSessions hint wedged %v, GetSession %v", listWedged, hintWedged)
			}

			timelineWedged := wedgedPerTimeline(t, srv, sessID)

			if hintWedged == tc.recovered || timelineWedged == tc.recovered {
				t.Fatalf("hint wedged %v, timeline wedged %v; want both %v (activity %v)",
					hintWedged, timelineWedged, !tc.recovered, p.GetLastAgentActivityAt())
			}
		})
	}
}

// wedgedPerHint is the TUI/web auth-wedge rule over a hydrated session.
func wedgedPerHint(p *pb.Session) bool {
	rec := alertstate.WedgeRecovery{PRDone: p.GetDisplayStatus() == pb.DisplayStatus_DISPLAY_STATUS_MERGED ||
		p.GetDisplayStatus() == pb.DisplayStatus_DISPLAY_STATUS_CLOSED}
	if ts := p.GetLastAgentActivityAt(); ts != nil {
		rec.LastAgentActivityAt = ts.AsTime()
	}
	return alertstate.AuthWedgeCurrent(p.GetRotationEvents(), rec)
}

// wedgeFixture is a session whose chat agent-1 lost its login: its run began
// at 23:04 and the daemon gave up at 23:16. The server's tracker knows nothing,
// as after a restart, and its transcripts are read through tally.
// wedgeSetup varies wedgeFixture: extra rotation events stored after the
// give-up, whether the session's PR is merged, and how many more sessions
// (chats agent-2, agent-3, ...) lost their login the same way.
type wedgeSetup struct {
	events        []db.RotationEvent
	prMerged      bool
	extraSessions int
}

func wedgeFixture(t *testing.T, tally *fakeTally, setup wedgeSetup) (*Server, string) {
	t.Helper()
	ctx := context.Background()
	sqlDB := setupServerTestDB(t)
	repos := db.NewRepoStore(sqlDB)
	sessions := db.NewSessionStore(sqlDB)
	chats := db.NewAgentChatStore(sqlDB)
	runs := db.NewAgentRunStore(sqlDB)
	events := db.NewRotationEventStore(sqlDB)
	snapshots := db.NewCheckSnapshotStore(sqlDB)
	repo, err := repos.Create(ctx, db.CreateRepoParams{
		DisplayName: "r", LocalPath: "/tmp/r", OriginURL: "https://github.com/acme/r.git",
		DefaultBaseBranch: "main", WorktreeBaseDir: "/tmp/wt",
	})
	if err != nil {
		t.Fatalf("create repo: %v", err)
	}
	var first string
	for n := 1; n <= 1+setup.extraSessions; n++ {
		chatID := fmt.Sprintf("agent-%d", n)
		params := db.CreateSessionParams{RepoID: repo.ID, Title: "s", BranchName: fmt.Sprintf("feat/w%d", n), BaseBranch: "main"}
		if setup.prMerged {
			pr := 3033
			params.PRNumber = &pr
		}
		sess, err := sessions.Create(ctx, params)
		if err != nil {
			t.Fatalf("create session: %v", err)
		}
		if first == "" {
			first = sess.ID
		}
		if setup.prMerged {
			if err := snapshots.Create(ctx, db.CheckSnapshot{
				SessionID: sess.ID, HeadSHA: timelineHead, PolledAt: at(18), RawJSON: "[]", ComputedStatus: int(vcs.DisplayStatusMerged),
			}); err != nil {
				t.Fatalf("insert snapshot: %v", err)
			}
		}
		if _, err := chats.Create(ctx, db.CreateAgentChatParams{SessionID: sess.ID, AgentSessionID: chatID, AgentName: "claude"}); err != nil {
			t.Fatalf("create chat: %v", err)
		}
		// The run that lost the login is still the pane's run.
		if _, err := runs.Create(ctx, domain.AgentRun{SessionID: sess.ID, AgentSessionID: chatID, StartedAt: at(4)}); err != nil {
			t.Fatalf("start run: %v", err)
		}
		giveUp := db.RotationEvent{
			ChatID: chatID, Provider: "claude",
			Trigger:   pb.RotationTrigger_ROTATION_TRIGGER_AUTH_INVALIDATED.String(),
			Outcome:   pb.RotationOutcome_ROTATION_OUTCOME_RESPAWN_CAP_EXHAUSTED.String(),
			CreatedAt: at(16),
		}
		for i, ev := range append([]db.RotationEvent{giveUp}, setup.events...) {
			ev.ID = fmt.Sprintf("rot-%d-%d", n, i)
			ev.SessionID = sess.ID
			if err := events.Create(ctx, ev); err != nil {
				t.Fatalf("insert rotation event: %v", err)
			}
		}
	}
	srv := New(Config{
		Repos: repos, Sessions: sessions, AgentChats: chats, AgentRuns: runs, RotationEvents: events,
		CheckSnapshots: snapshots,
		// The restarted daemon's tracker knows nothing yet.
		ChatStatus: status.NewTracker(),
	})
	srv.transcriptTally = tally.tally
	// The read lands before the RPC answers, so one call shows its result.
	srv.wedgeCache.readWait = time.Minute
	return srv, first
}

// wedgedPerTimeline reports whether the session timeline waits on the lost
// login.
func wedgedPerTimeline(t *testing.T, srv *Server, sessionID string) bool {
	t.Helper()
	resp, err := srv.GetSessionTimeline(context.Background(), connect.NewRequest(&pb.GetSessionTimelineRequest{SessionId: sessionID}))
	if err != nil {
		t.Fatalf("GetSessionTimeline: %v", err)
	}
	for _, st := range resp.Msg.GetSteps() {
		if st.GetState() == current && st.GetTitle() == "Lost its login" {
			return true
		}
	}
	return false
}

// The timeline opens the incident over every rotation event it read, so it
// must judge the give-up's recovery on that same give-up — not on the newest
// rotationEventsCap events a hydrated session carries. Here more than that
// many manual switches followed the give-up, and the transcript shows output
// after it: the timeline must read it and find the session recovered.
func TestRestartProofActivity_TimelineJudgesTheGiveUpItOpensTheIncidentOn(t *testing.T) {
	manual := make([]db.RotationEvent, 0, rotationEventsCap)
	for i := range rotationEventsCap {
		manual = append(manual, db.RotationEvent{
			ChatID: "agent-1", Provider: "claude",
			Trigger:   pb.RotationTrigger_ROTATION_TRIGGER_MANUAL.String(),
			Outcome:   pb.RotationOutcome_ROTATION_OUTCOME_ROTATED.String(),
			CreatedAt: at(17).Add(time.Duration(i) * time.Second),
		})
	}
	tally := &fakeTally{counts: map[string]agenttelemetry.Counts{"agent-1": {LastOutputAt: at(22)}}}
	srv, sessID := wedgeFixture(t, tally, wedgeSetup{events: manual})
	if wedgedPerTimeline(t, srv, sessID) {
		t.Fatalf("timeline waits on the lost login; want it recovered by the transcript output at %v", at(22))
	}
	if tally.calls() == 0 {
		t.Fatal("timeline never read the transcripts after the give-up")
	}
}

// A read that fails (a transcript or the run list could not be read) backs
// off for wedgeActivityTTL instead of re-reading on every poll. A completed
// read is cached, a missing transcript included (there is nothing to find).
func TestRestartProofActivity_FailedReadsBackOff(t *testing.T) {
	for name, tc := range map[string]struct {
		tallyErr error
		runsErr  error
	}{
		"transcript read failed":    {tallyErr: errors.New("read: input/output error")},
		"run list failed":           {runsErr: errors.New("database is locked")},
		"no transcript to read":     {tallyErr: os.ErrNotExist},
		"transcript read, none new": {},
	} {
		t.Run(name, func(t *testing.T) {
			now := at(30)
			cache := clockedWedgeCache(func() time.Time { return now })
			tally := &fakeTally{errs: map[string]error{"a": tc.tallyErr}}
			runsErr := tc.runsErr
			w := WedgeActivity{Tally: tally.tally, Cache: cache, Runs: func(context.Context, string) ([]domain.AgentRun, error) {
				return nil, runsErr
			}}
			q := WedgeQuery{SessionID: "s1", WorktreePath: "/wt", GiveUpAt: at(16), Chats: twoChats[:1]}
			if got := w.RestartProofActivity(context.Background(), q); !got.IsZero() {
				t.Fatalf("first read: activity %v, want none", got)
			}
			// The session recovers; a later reader with a live context asks.
			tally.mu.Lock()
			tally.errs = nil
			tally.counts = map[string]agenttelemetry.Counts{"a": {LastOutputAt: at(25)}}
			tally.mu.Unlock()
			runsErr = nil
			before := tally.calls()
			now = now.Add(wedgeActivityTTL - time.Second)
			if got := w.RestartProofActivity(context.Background(), q); !got.IsZero() || tally.calls() != before {
				t.Fatalf("within the backoff: activity %v, re-read %v; want no re-read", got, tally.calls() > before)
			}
			now = now.Add(time.Second)
			if got := w.RestartProofActivity(context.Background(), q); !got.Equal(at(25)) || tally.calls() == before {
				t.Fatalf("after the backoff: activity %v, re-read %v; want a fresh read finding %v", got, tally.calls() > before, at(25))
			}
		})
	}
}

// Output known for a give-up is never lowered: a re-read that fails, or that
// completes but finds less (a transcript pruned), still serves it — however
// long ago it was found, and whatever other sessions the cache stored since.
func TestRestartProofActivity_KnownOutputSurvivesLaterReads(t *testing.T) {
	for name, laterErr := range map[string]error{
		"a failed re-read":          errors.New("read: input/output error"),
		"a re-read that finds less": nil,
	} {
		t.Run(name, func(t *testing.T) {
			now := at(30)
			cache := clockedWedgeCache(func() time.Time { return now })
			tally := &fakeTally{counts: map[string]agenttelemetry.Counts{"recovered": {LastOutputAt: at(25)}}}
			w := WedgeActivity{Tally: tally.tally, Cache: cache}
			read := func(sessionID, chat string) time.Time {
				return w.RestartProofActivity(context.Background(), WedgeQuery{
					SessionID: sessionID, WorktreePath: "/wt", GiveUpAt: at(16),
					Chats: []*domain.AgentChat{{AgentSessionID: chat}},
				})
			}
			if got := read("s-recovered", "recovered"); !got.Equal(at(25)) {
				t.Fatalf("first read: activity %v, want %v", got, at(25))
			}
			// Past the recovery TTL, another session stores an entry, then
			// the recovered session's re-read goes wrong.
			now = now.Add(wedgeRecoveryTTL + time.Minute)
			read("s-other", "wedged")
			tally.mu.Lock()
			tally.counts = nil
			if laterErr != nil {
				tally.errs = map[string]error{"recovered": laterErr}
			}
			tally.mu.Unlock()
			before := tally.calls()
			if got := read("s-recovered", "recovered"); !got.Equal(at(25)) || tally.calls() == before {
				t.Fatalf("after %s: activity %v, re-read %v; want the known %v kept", name, got, tally.calls() > before, at(25))
			}
			// The cache itself still holds it, not just this one answer.
			if got := read("s-recovered", "recovered"); !got.Equal(at(25)) {
				t.Fatalf("next query after %s: activity %v; want the known %v kept", name, got, at(25))
			}
		})
	}
}

// A failed persisted read is logged rather than dropped: a warning, at most
// once per session per wedgeActivityTTL so a poll loop cannot flood the log.
func TestRestartProofActivity_LogsFailedReads(t *testing.T) {
	now := at(30)
	cache := clockedWedgeCache(func() time.Time { return now })
	var buf bytes.Buffer
	tally := &fakeTally{errs: map[string]error{"a": errors.New("read: input/output error")}}
	w := WedgeActivity{Tally: tally.tally, Cache: cache, Logger: zerolog.New(&buf).Level(zerolog.DebugLevel)}
	q := WedgeQuery{SessionID: "s1", WorktreePath: "/wt", GiveUpAt: at(16), Chats: twoChats[:1]}
	levels := func() []string {
		var out []string
		for line := range strings.SplitSeq(strings.TrimSpace(buf.String()), "\n") {
			if line == "" {
				continue
			}
			var rec struct {
				Level   string `json:"level"`
				Session string `json:"session_id"`
				Error   string `json:"error"`
			}
			if err := json.Unmarshal([]byte(line), &rec); err != nil {
				t.Fatalf("log line %q: %v", line, err)
			}
			if rec.Session != "s1" || rec.Error == "" {
				t.Fatalf("log line %q lacks the session or the error", line)
			}
			out = append(out, rec.Level)
		}
		return out
	}

	w.RestartProofActivity(context.Background(), q)
	w.RestartProofActivity(context.Background(), q)
	if got := levels(); !slices.Equal(got, []string{"warn"}) {
		t.Fatalf("two failed reads within the TTL logged %v, want one warning", got)
	}
	now = now.Add(wedgeActivityTTL)
	w.RestartProofActivity(context.Background(), q)
	if got := levels(); !slices.Equal(got, []string{"warn", "warn"}) {
		t.Fatalf("a failed read after the TTL logged %v, want a second warning", got)
	}
}

// A caller that goes away (a superseded ListSessions poll, a reverse-stream
// delta past its deadline) neither cancels the read nor is answered by a
// re-read: the one read finishes in the background, logs nothing, and is
// cached for the next caller. Abandoned reads used to restart on every poll
// and log a line each, thousands of them, without ever finishing.
func TestRestartProofActivity_AnAbandonedCallerLeavesTheReadToFinish(t *testing.T) {
	cache := NewWedgeActivityCache()
	var buf bytes.Buffer
	tally := &fakeTally{counts: map[string]agenttelemetry.Counts{"a": {LastOutputAt: at(25)}}}
	w := WedgeActivity{Tally: tally.tally, Cache: cache, Logger: zerolog.New(&buf).Level(zerolog.DebugLevel)}
	q := WedgeQuery{SessionID: "s1", WorktreePath: "/wt", GiveUpAt: at(16), Chats: twoChats[:1]}
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	for range 5 {
		w.RestartProofActivity(cancelled, q)
	}
	settle(t, cache, "s1")
	if got := w.RestartProofActivity(context.Background(), q); !got.Equal(at(25)) || tally.calls() != 1 {
		t.Fatalf("after abandoned callers: activity %v, reads %d; want the one read's %v", got, tally.calls(), at(25))
	}
	if buf.Len() != 0 {
		t.Fatalf("abandoned callers logged %q; want nothing", buf.String())
	}
}

// Output after a give-up is proof the give-up recovered, so a found output is
// served for wedgeRecoveryTTL rather than re-read every wedgeActivityTTL. It
// still expires: a re-read refreshes the activity time (a stopped chat reaches
// no live tracker), so the value does not freeze at the first output found. A
// newer give-up asks a new question and reads at once.
func TestRestartProofActivity_CachesFoundOutputForTheRecoveryTTL(t *testing.T) {
	now := at(30)
	cache := clockedWedgeCache(func() time.Time { return now })
	tally := &fakeTally{counts: map[string]agenttelemetry.Counts{"a": {LastOutputAt: at(25)}}}
	w := WedgeActivity{Tally: tally.tally, Cache: cache}
	read := func(giveUpAt time.Time) time.Time {
		return w.RestartProofActivity(context.Background(), WedgeQuery{SessionID: "s1", WorktreePath: "/wt", GiveUpAt: giveUpAt, Chats: twoChats[:1]})
	}
	if got := read(at(16)); !got.Equal(at(25)) || tally.calls() != 1 {
		t.Fatalf("first read: activity %v, calls %d", got, tally.calls())
	}
	tally.mu.Lock()
	tally.counts = map[string]agenttelemetry.Counts{"a": {LastOutputAt: at(29)}}
	tally.mu.Unlock()
	now = now.Add(wedgeRecoveryTTL - time.Second)
	if got := read(at(16)); !got.Equal(at(25)) || tally.calls() != 1 {
		t.Fatalf("within the recovery TTL: activity %v, calls %d; want the cached recovery, no re-read", got, tally.calls())
	}
	now = now.Add(time.Second)
	if got := read(at(16)); !got.Equal(at(29)) || tally.calls() != 2 {
		t.Fatalf("after the recovery TTL: activity %v, calls %d; want a re-read refreshing it to %v", got, tally.calls(), at(29))
	}
	// A newer give-up asks a new question: output must follow it.
	tally.mu.Lock()
	tally.counts = map[string]agenttelemetry.Counts{"a": {LastOutputAt: at(32)}}
	tally.mu.Unlock()
	if got := read(at(31)); !got.Equal(at(32)) || tally.calls() != 3 {
		t.Fatalf("newer give-up: activity %v, calls %d; want a fresh read", got, tally.calls())
	}
}

// An entry outlives its re-read interval while the session is still queried
// (its known output must survive), and is dropped once nothing has queried it
// for wedgeIdleTTL, so the cache holds only sessions polled recently, not
// every session that ever gave up.
func TestRestartProofActivity_DropsOnlyIdleSessions(t *testing.T) {
	now := at(30)
	cache := clockedWedgeCache(func() time.Time { return now })
	tally := &fakeTally{counts: map[string]agenttelemetry.Counts{"recovered": {LastOutputAt: at(25)}}}
	w := WedgeActivity{Tally: tally.tally, Cache: cache}
	read := func(sessionID, chat string) {
		w.RestartProofActivity(context.Background(), WedgeQuery{
			SessionID: sessionID, WorktreePath: "/wt", GiveUpAt: at(16),
			Chats: []*domain.AgentChat{{AgentSessionID: chat}},
		})
	}
	read("s-polled", "recovered")
	read("s-idle", "wedged")
	// s-polled keeps being queried; s-idle is never asked about again.
	for range 4 {
		now = now.Add(wedgeIdleTTL / 4)
		read("s-polled", "recovered")
	}
	if _, ok := cache.entries["s-polled"]; !ok {
		t.Fatal("a session still being queried was dropped")
	}
	if _, ok := cache.entries["s-idle"]; ok || len(cache.entries) != 1 {
		t.Fatalf("cache holds %v; want only s-polled once s-idle sat idle for %v", cache.entries, wedgeIdleTTL)
	}
}

// A merged or closed PR already recovers every give-up (WedgeRecovery.PRDone,
// which every reader sets from the display status), so the persisted read
// decides nothing there and is skipped on every path: session hydration and
// the timeline.
func TestRestartProofActivity_SkipsTheReadOnceThePRIsDone(t *testing.T) {
	for _, st := range []pb.DisplayStatus{pb.DisplayStatus_DISPLAY_STATUS_MERGED, pb.DisplayStatus_DISPLAY_STATUS_CLOSED} {
		t.Run(st.String(), func(t *testing.T) {
			tally := &fakeTally{counts: map[string]agenttelemetry.Counts{"a": {LastOutputAt: at(25)}}}
			runsRead := false
			w := WedgeActivity{Tally: tally.tally, Runs: func(context.Context, string) ([]domain.AgentRun, error) {
				runsRead = true
				return nil, nil
			}}
			p := &pb.Session{Id: "s1", WorktreePath: "/wt", DisplayStatus: st, RotationEvents: giveUpEvents()}
			HydrateRestartProofActivity(context.Background(), p, twoChats, w)
			if tally.calls() != 0 || runsRead {
				t.Fatalf("tally calls %d, runs read %v; want no persisted read for a done PR", tally.calls(), runsRead)
			}
			if p.GetLastAgentActivityAt() != nil {
				t.Fatalf("last_agent_activity_at = %v, want the live value (none)", p.GetLastAgentActivityAt())
			}
			if wedgedPerHint(p) {
				t.Fatal("hint reads a done PR as wedged")
			}
		})
	}
	t.Run("an open PR still reads", func(t *testing.T) {
		tally := &fakeTally{counts: map[string]agenttelemetry.Counts{"a": {LastOutputAt: at(25)}}}
		p := &pb.Session{Id: "s1", WorktreePath: "/wt", DisplayStatus: pb.DisplayStatus_DISPLAY_STATUS_FAILING, RotationEvents: giveUpEvents()}
		HydrateRestartProofActivity(context.Background(), p, twoChats, WedgeActivity{Tally: tally.tally})
		if tally.calls() == 0 || p.GetLastAgentActivityAt() == nil || !p.GetLastAgentActivityAt().AsTime().Equal(at(25)) {
			t.Fatalf("tally calls %d, activity %v; want the transcript output read", tally.calls(), p.GetLastAgentActivityAt())
		}
	})
	t.Run("timeline", func(t *testing.T) {
		tally := &fakeTally{}
		srv, sessID := wedgeFixture(t, tally, wedgeSetup{prMerged: true})
		if wedgedPerTimeline(t, srv, sessID) {
			t.Fatal("timeline waits on the lost login of a merged PR")
		}
		if tally.calls() != 0 {
			t.Fatalf("timeline read %d transcripts for a merged PR; want none", tally.calls())
		}
	})
}

// A run still open when bossd restarted is closed by ReconcileOpen with the
// restart time as its stop (stop reason daemon_restart). That time is the
// daemon's, not the agent's: reporting it as output would make a dead session
// read as freshly active. Its output came after its start, so the start is
// the evidence it gives; a transcript's own output time, when readable, is
// newer evidence and wins.
func TestRestartProofActivity_ARestartStopIsNotOutput(t *testing.T) {
	out := int64(420)
	run := buildRun(at(18), timePtr(at(40)), db.AgentRunStopDaemonRestart, "")
	run.ParentModelCallCount = 3
	run.OutputTokenCount = &out
	for name, tc := range map[string]struct {
		counts map[string]agenttelemetry.Counts
		want   time.Time
	}{
		"no readable transcript": {want: at(18)},
		"transcript output":      {counts: map[string]agenttelemetry.Counts{"a": {LastOutputAt: at(22)}}, want: at(22)},
	} {
		t.Run(name, func(t *testing.T) {
			w := WedgeActivity{Runs: runsOf(run), Tally: (&fakeTally{counts: tc.counts}).tally}
			got := restartProof(w, time.Time{}, giveUpEvents(), twoChats[:1])
			if !got.Equal(tc.want) || !recovered(got) {
				t.Fatalf("activity = %v, want %v (never the restart time %v)", got, tc.want, at(40))
			}
		})
	}
}

// codexRollout writes a codex rollout for chat id under a fresh CODEX_HOME:
// fillerMB one-megabyte tool-output records (what makes a real rollout
// gigabytes long), then a turn with output tokens at each of outputs, and
// stamps the file's mtime at modTime.
func codexRollout(t *testing.T, id string, fillerMB int, modTime time.Time, outputs ...time.Time) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("CODEX_HOME", home)
	dir := filepath.Join(home, "sessions", "2026", "10", "13")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "rollout-2026-10-13T23-00-00-"+id+".jsonl")
	var b strings.Builder
	filler := strings.Repeat("x", 1<<20)
	for range fillerMB {
		fmt.Fprintf(&b, `{"timestamp":%q,"type":"response_item","payload":{"type":"function_call_output","output":%q}}`+"\n", at(17).Format(time.RFC3339Nano), filler)
	}
	if err := os.WriteFile(path, []byte(b.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	appendCodexOutput(t, path, modTime, outputs...)
	return path
}

// appendCodexOutput appends a turn with output tokens at each of outputs and
// stamps the file's mtime at modTime.
func appendCodexOutput(t *testing.T, path string, modTime time.Time, outputs ...time.Time) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	for _, ts := range outputs {
		if _, err := fmt.Fprintf(f, `{"timestamp":%q,"type":"event_msg","payload":{"type":"token_count","info":{"last_token_usage":{"output_tokens":42}}}}`+"\n", ts.Format(time.RFC3339Nano)); err != nil {
			t.Fatal(err)
		}
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, modTime, modTime); err != nil {
		t.Fatal(err)
	}
}

// gatedTally is the real transcript tally behind a gate: each call is counted
// and waits for release (or its context), standing in for the 25s it takes to
// parse the operator's 5.7 GB codex rollout.
type gatedTally struct {
	release chan struct{}
	calls   atomic.Int32
}

func (g *gatedTally) tally(ctx context.Context, worktreePath string, chat *domain.AgentChat, since, until time.Time) (agenttelemetry.Counts, error) {
	g.calls.Add(1)
	select {
	case <-g.release:
	case <-ctx.Done():
		return agenttelemetry.Counts{}, ctx.Err()
	}
	return tallyInteractiveAgentRunForServer(ctx, worktreePath, chat, since, until)
}

// clockedWedgeCache is a cache reading the time from now whose callers wait
// for the background read they start, so each call shows that read's result.
func clockedWedgeCache(now func() time.Time) *WedgeActivityCache {
	c := NewWedgeActivityCache()
	c.now = now
	c.readWait = time.Minute
	return c
}

// settle waits for sessionID's background persisted read, if one is running.
func settle(t *testing.T, c *WedgeActivityCache, sessionID string) {
	t.Helper()
	c.mu.Lock()
	r, ok := c.inflight[sessionID]
	c.mu.Unlock()
	if !ok {
		return
	}
	select {
	case <-r.done:
	case <-time.After(10 * time.Second):
		t.Fatal("background persisted read never finished")
	}
}

// A wedged session whose transcript is huge must not stall the RPCs that
// hydrate it (ListSessions polls, reverse-stream deltas, GetSession): every
// caller returns at once while one background read parses the rollout, and a
// transcript is parsed once per version, not once per caller or per TTL.
func TestRestartProofActivity_ASlowTranscriptReadNeverBlocksTheHotPath(t *testing.T) {
	const id = "01a0bd42-3649-7122-8aee-84b8170b83d7"
	path := codexRollout(t, id, 32, at(21), at(20))
	chats := []*domain.AgentChat{{AgentSessionID: id, AgentName: "codex"}}
	var clock sync.Mutex
	now := at(30)
	advance := func(d time.Duration) {
		clock.Lock()
		defer clock.Unlock()
		now = now.Add(d)
	}
	cache := NewWedgeActivityCache()
	cache.now = func() time.Time {
		clock.Lock()
		defer clock.Unlock()
		return now
	}
	tally := &gatedTally{release: make(chan struct{})}
	w := WedgeActivity{Tally: tally.tally, Cache: cache}
	hydrate := func(ctx context.Context) *pb.Session {
		p := &pb.Session{Id: "s1", WorktreePath: "/wt", DisplayStatus: pb.DisplayStatus_DISPLAY_STATUS_FAILING, RotationEvents: giveUpEvents()}
		HydrateRestartProofActivity(ctx, p, chats, w)
		return p
	}
	activity := func() time.Time {
		if ts := hydrate(context.Background()).GetLastAgentActivityAt(); ts != nil {
			return ts.AsTime()
		}
		return time.Time{}
	}

	const callers = 32
	var wg sync.WaitGroup
	took := make(chan time.Duration, callers)
	for range callers {
		wg.Go(func() {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			start := time.Now()
			hydrate(ctx)
			took <- time.Since(start)
		})
	}
	wg.Wait()
	close(took)
	var slowest time.Duration
	for d := range took {
		slowest = max(slowest, d)
	}
	if slowest > time.Second || tally.calls.Load() != 1 {
		t.Fatalf("%d concurrent hydrations: slowest took %v, transcript parsed %d times; want each back in under 1s and one parse",
			callers, slowest, tally.calls.Load())
	}

	close(tally.release)
	settle(t, cache, "s1")
	if got := activity(); !got.Equal(at(20)) || tally.calls.Load() != 1 {
		t.Fatalf("after the read: activity %v, parses %d; want the rollout's output at %v from the one parse", got, tally.calls.Load(), at(20))
	}

	// The re-read after the recovery TTL finds the rollout unchanged.
	advance(wedgeRecoveryTTL)
	activity()
	settle(t, cache, "s1")
	if got := activity(); !got.Equal(at(20)) || tally.calls.Load() != 1 {
		t.Fatalf("unchanged rollout re-read: activity %v, parses %d; want %v without a second parse", got, tally.calls.Load(), at(20))
	}

	// A new version of the rollout is parsed once more.
	appendCodexOutput(t, path, at(24), at(23))
	advance(wedgeRecoveryTTL)
	activity()
	settle(t, cache, "s1")
	if got := activity(); !got.Equal(at(23)) || tally.calls.Load() != 2 {
		t.Fatalf("grown rollout re-read: activity %v, parses %d; want %v from a second parse", got, tally.calls.Load(), at(23))
	}
}

// A transcript not written since the give-up cannot hold output after it, so
// it is not parsed: the operator's 5.7 GB rollout was last written ten days
// before the give-up it was searched for on every poll.
func TestRestartProofActivity_ATranscriptUntouchedSinceTheGiveUpIsNotParsed(t *testing.T) {
	const id = "01a0bd42-3649-7122-8aee-84b8170b83d7"
	codexRollout(t, id, 1, at(10), at(9))
	tally := &gatedTally{release: make(chan struct{})}
	close(tally.release)
	w := WedgeActivity{Tally: tally.tally}
	got := restartProof(w, time.Time{}, giveUpEvents(), []*domain.AgentChat{{AgentSessionID: id, AgentName: "codex"}})
	if !got.IsZero() || tally.calls.Load() != 0 {
		t.Fatalf("activity %v, parses %d; want none and no parse", got, tally.calls.Load())
	}
}

// A transcript rewritten to the same size is a new version when its mtime
// moved, so it is parsed again rather than served from the old parse.
func TestRestartProofActivity_ASameSizeRewriteWithANewMtimeIsParsedAgain(t *testing.T) {
	const id = "01a0bd42-3649-7122-8aee-84b8170b83d7"
	path := codexRollout(t, id, 0, at(21), at(20))
	tally := &gatedTally{release: make(chan struct{})}
	close(tally.release)
	now := at(30)
	w := WedgeActivity{Tally: tally.tally, Cache: clockedWedgeCache(func() time.Time { return now })}
	chats := []*domain.AgentChat{{AgentSessionID: id, AgentName: "codex"}}
	if got := restartProof(w, time.Time{}, giveUpEvents(), chats); !got.Equal(at(20)) || tally.calls.Load() != 1 {
		t.Fatalf("first read: activity %v, parses %d; want %v from one parse", got, tally.calls.Load(), at(20))
	}

	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	after := bytes.ReplaceAll(before, []byte(at(20).Format(time.RFC3339Nano)), []byte(at(23).Format(time.RFC3339Nano)))
	if len(after) != len(before) || bytes.Equal(after, before) {
		t.Fatalf("rewrite changed the size (%d -> %d) or nothing", len(before), len(after))
	}
	if err := os.WriteFile(path, after, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, at(24), at(24)); err != nil {
		t.Fatal(err)
	}
	now = now.Add(wedgeRecoveryTTL)
	if got := restartProof(w, time.Time{}, giveUpEvents(), chats); !got.Equal(at(23)) || tally.calls.Load() != 2 {
		t.Fatalf("same-size rewrite: activity %v, parses %d; want %v from a second parse", got, tally.calls.Load(), at(23))
	}
}

// ListSessions hydrates every session one after another. Twenty wedged
// sessions whose reads are slow must cost the list one shared wait, not one
// wait each (4s on a cold cache before the wait was shared).
func TestRestartProofActivity_AListWaitsOnceForAllItsWedgedSessions(t *testing.T) {
	const wedged = 20
	ctx := context.Background()
	srv, _ := wedgeFixture(t, &fakeTally{}, wedgeSetup{extraSessions: wedged - 1})
	release := make(chan struct{})
	srv.transcriptTally = func(ctx context.Context, _ string, _ *domain.AgentChat, _, _ time.Time) (agenttelemetry.Counts, error) {
		select {
		case <-release:
			return agenttelemetry.Counts{LastOutputAt: at(20)}, nil
		case <-ctx.Done():
			return agenttelemetry.Counts{}, ctx.Err()
		}
	}
	srv.wedgeCache.readWait = wedgeReadWait

	start := time.Now()
	listed, err := srv.ListSessions(ctx, connect.NewRequest(&pb.ListSessionsRequest{}))
	took := time.Since(start)
	t.Logf("cold ListSessions over %d wedged sessions with slow reads took %v", wedged, took)
	close(release)
	if err != nil || len(listed.Msg.GetSessions()) != wedged {
		t.Fatalf("ListSessions = %v, %v; want the %d sessions", listed, err, wedged)
	}
	for _, p := range listed.Msg.GetSessions() {
		settle(t, srv.wedgeCache, p.GetId())
	}
	// A wait per session would take wedged x wedgeReadWait; five waits leave a
	// loaded runner headroom for the rest of the list.
	if took > 5*wedgeReadWait {
		t.Fatalf("cold ListSessions took %v; want one shared wait of %v for the whole list", took, wedgeReadWait)
	}
}

// A read that outlasts its caller's wait lands after that caller answered
// with the older value, so the cache reports the session once the read
// advances its known output, and not when a later read finds nothing new.
func TestRestartProofActivity_AReadThatOutlastsItsWaitReportsTheChange(t *testing.T) {
	now := at(30)
	cache := NewWedgeActivityCache()
	cache.now = func() time.Time { return now }
	cache.readWait = time.Millisecond
	var reported []string
	var mu sync.Mutex
	cache.SetOnChange(func(sessionID string) {
		mu.Lock()
		defer mu.Unlock()
		reported = append(reported, sessionID)
	})
	release := make(chan struct{})
	w := WedgeActivity{Cache: cache, Tally: func(ctx context.Context, _ string, _ *domain.AgentChat, _, _ time.Time) (agenttelemetry.Counts, error) {
		<-release
		return agenttelemetry.Counts{LastOutputAt: at(20)}, nil
	}}
	reports := func() []string {
		mu.Lock()
		defer mu.Unlock()
		return slices.Clone(reported)
	}

	if got := restartProof(w, time.Time{}, giveUpEvents(), twoChats[:1]); !got.IsZero() {
		t.Fatalf("caller answered %v before the read landed; want the cold value", got)
	}
	if got := reports(); len(got) != 0 {
		t.Fatalf("reported %v before the read landed", got)
	}
	close(release)
	settle(t, cache, "s1")
	if got := reports(); !slices.Equal(got, []string{"s1"}) {
		t.Fatalf("reported %v once the read found output; want [s1]", got)
	}

	now = now.Add(wedgeRecoveryTTL)
	restartProof(w, time.Time{}, giveUpEvents(), twoChats[:1])
	settle(t, cache, "s1")
	if got := reports(); !slices.Equal(got, []string{"s1"}) {
		t.Fatalf("reported %v after a re-read found the same output; want only the first [s1]", got)
	}
}

// Each read can keep a core busy for its whole budget, so however many
// sessions are due at once, only wedgeConcurrentReads parse at a time, and the
// rest still run once a slot frees.
func TestRestartProofActivity_CapsConcurrentReads(t *testing.T) {
	const sessions = 3 * wedgeConcurrentReads
	cache := NewWedgeActivityCache()
	cache.readWait = time.Millisecond
	var (
		mu                      sync.Mutex
		running, peak, finished int
	)
	counts := func() (int, int, int) {
		mu.Lock()
		defer mu.Unlock()
		return running, peak, finished
	}
	release := make(chan struct{})
	w := WedgeActivity{Cache: cache, Tally: func(ctx context.Context, _ string, _ *domain.AgentChat, _, _ time.Time) (agenttelemetry.Counts, error) {
		mu.Lock()
		running++
		peak = max(peak, running)
		mu.Unlock()
		<-release
		mu.Lock()
		running--
		finished++
		mu.Unlock()
		return agenttelemetry.Counts{}, nil
	}}
	q := WedgeQuery{WorktreePath: "/wt", Chats: twoChats[:1], GiveUpAt: at(16)}
	for i := range sessions {
		q.SessionID = fmt.Sprintf("s%d", i)
		w.RestartProofActivity(context.Background(), q)
	}
	deadline := time.Now().Add(5 * time.Second)
	for r, _, _ := counts(); r < wedgeConcurrentReads && time.Now().Before(deadline); r, _, _ = counts() {
		time.Sleep(time.Millisecond)
	}
	time.Sleep(50 * time.Millisecond)
	if _, got, _ := counts(); got != wedgeConcurrentReads {
		t.Fatalf("%d reads parsed at once; want %d", got, wedgeConcurrentReads)
	}
	close(release)
	for i := range sessions {
		settle(t, cache, fmt.Sprintf("s%d", i))
	}
	if _, _, got := counts(); got != sessions {
		t.Fatalf("%d of %d reads ran; want every queued read to run once a slot freed", got, sessions)
	}
}
