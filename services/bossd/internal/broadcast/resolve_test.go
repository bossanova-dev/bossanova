package broadcast

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/rs/zerolog"

	bcast "github.com/recurser/bossalib/broadcast"
	"github.com/recurser/bossalib/machine"
	"github.com/recurser/bossalib/models"
)

// fakeChats is a chatLister returning a canned chat list (or an error).
type fakeChats struct {
	chats []*models.AgentChat
	err   error
}

func (f *fakeChats) ListRoutableChats(context.Context) ([]*models.AgentChat, error) {
	return f.chats, f.err
}

// fakeSessions is a sessionGetter backed by an in-memory map; an unknown id
// reports sql.ErrNoRows exactly as the SQLite store does.
type fakeSessions struct {
	sessions map[string]*models.Session
	err      error
	getCalls int
}

func (f *fakeSessions) Get(_ context.Context, id string) (*models.Session, error) {
	f.getCalls++
	if f.err != nil {
		return nil, f.err
	}
	s, ok := f.sessions[id]
	if !ok {
		return nil, sql.ErrNoRows
	}
	return s, nil
}

func strPtr(s string) *string { return &s }

// chat builds a routable chat row for the fixture set.
func chat(chatID, sessionID, agentName, accountID, daemonID string) *models.AgentChat {
	c := &models.AgentChat{
		AgentSessionID: chatID,
		SessionID:      sessionID,
		AgentName:      agentName,
		DaemonID:       daemonID,
	}
	if accountID != "" {
		c.AccountID = strPtr(accountID)
	}
	return c
}

// fixtureResolver wires a resolver over three chats spread across two sessions,
// two repos, two agents, two accounts and two daemons, so every dimension can be
// isolated by a single-term selector.
func fixtureResolver() *Resolver {
	chats := []*models.AgentChat{
		chat("chat-a", "sess-1", "claude", "acct-1", ""),
		chat("chat-b", "sess-1", "codex", "acct-2", ""),
		chat("chat-c", "sess-2", "claude", "acct-1", "daemon-remote"),
	}
	sessions := map[string]*models.Session{
		"sess-1": {ID: "sess-1", RepoID: "repo-1"},
		"sess-2": {ID: "sess-2", RepoID: "repo-2"},
	}
	return NewResolver(&fakeChats{chats: chats}, &fakeSessions{sessions: sessions}, zerolog.Nop())
}

func mustParse(t *testing.T, s string) bcast.Selector {
	t.Helper()
	sel, err := bcast.Parse(s)
	if err != nil {
		t.Fatalf("parse selector %q: %v", s, err)
	}
	return sel
}

func chatIDs(targets []bcast.Target) []string {
	out := make([]string, 0, len(targets))
	for _, t := range targets {
		out = append(out, t.ChatID)
	}
	return out
}

func assertChatIDs(t *testing.T, got []bcast.Target, want ...string) {
	t.Helper()
	gotIDs := chatIDs(got)
	if len(gotIDs) != len(want) {
		t.Fatalf("targets = %v, want %v", gotIDs, want)
	}
	seen := make(map[string]int, len(gotIDs))
	for _, id := range gotIDs {
		seen[id]++
	}
	for _, w := range want {
		if seen[w] == 0 {
			t.Fatalf("targets = %v, want %v", gotIDs, want)
		}
		seen[w]--
	}
}

// TestResolve_Dimensions covers each of the six addressable dimensions in
// isolation plus AND-within-a-clause and OR-across-clauses combinations.
func TestResolve_Dimensions(t *testing.T) {
	cases := []struct {
		name     string
		selector string
		want     []string
	}{
		{"chat", "chat:chat-b", []string{"chat-b"}},
		{"session", "session:sess-1", []string{"chat-a", "chat-b"}},
		{"repo", "repo:repo-2", []string{"chat-c"}},
		{"agent", "agent:claude", []string{"chat-a", "chat-c"}},
		{"account", "account:acct-2", []string{"chat-b"}},
		{"daemon", "daemon:daemon-remote", []string{"chat-c"}},
		{"and within clause", "agent:claude,repo:repo-1", []string{"chat-a"}},
		{"and within clause no match", "agent:codex,repo:repo-2", nil},
		{"or across clauses", "chat:chat-b+repo:repo-2", []string{"chat-b", "chat-c"}},
		{"or within a field", "agent:claude,agent:codex,repo:repo-1", []string{"chat-a", "chat-b"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := fixtureResolver()
			got, err := r.Resolve(context.Background(), mustParse(t, tc.selector), "", false)
			if err != nil {
				t.Fatalf("Resolve: %v", err)
			}
			assertChatIDs(t, got, tc.want...)
		})
	}
}

// TestResolve_SelfExclusion verifies the origin chat is dropped from its own
// audience by default and included only on an explicit opt-in.
func TestResolve_SelfExclusion(t *testing.T) {
	sel := mustParse(t, "session:sess-1")

	r := fixtureResolver()
	got, err := r.Resolve(context.Background(), sel, "chat-a", false)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	assertChatIDs(t, got, "chat-b")

	r = fixtureResolver()
	got, err = r.Resolve(context.Background(), sel, "chat-a", true)
	if err != nil {
		t.Fatalf("Resolve(includeOrigin): %v", err)
	}
	assertChatIDs(t, got, "chat-a", "chat-b")
}

// TestResolve_StartErrorChatsExcluded verifies a chat that never came up is
// never a target, even when the selector names it directly.
func TestResolve_StartErrorChatsExcluded(t *testing.T) {
	chats := []*models.AgentChat{
		chat("chat-a", "sess-1", "claude", "", ""),
		chat("chat-dead", "sess-1", "claude", "", ""),
	}
	chats[1].StartError = strPtr("SendPlan timed out")
	sessions := map[string]*models.Session{"sess-1": {ID: "sess-1", RepoID: "repo-1"}}
	r := NewResolver(&fakeChats{chats: chats}, &fakeSessions{sessions: sessions}, zerolog.Nop())

	got, err := r.Resolve(context.Background(), mustParse(t, "session:sess-1"), "", false)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	assertChatIDs(t, got, "chat-a")

	// Naming the dead chat directly must not resurrect it.
	got, err = r.Resolve(context.Background(), mustParse(t, "chat:chat-dead"), "", false)
	if err != nil {
		t.Fatalf("Resolve(direct): %v", err)
	}
	if len(got) != 0 {
		t.Errorf("targets = %v, want none (start_error chat is unreachable)", chatIDs(got))
	}
}

// TestResolve_SkipsChatsWithMissingSession verifies a chat whose session cannot
// be loaded is skipped rather than failing the whole resolve.
func TestResolve_SkipsChatsWithMissingSession(t *testing.T) {
	chats := []*models.AgentChat{
		chat("chat-a", "sess-1", "claude", "", ""),
		chat("chat-orphan", "sess-gone", "claude", "", ""),
	}
	sessions := map[string]*models.Session{"sess-1": {ID: "sess-1", RepoID: "repo-1"}}
	r := NewResolver(&fakeChats{chats: chats}, &fakeSessions{sessions: sessions}, zerolog.Nop())

	got, err := r.Resolve(context.Background(), mustParse(t, "agent:claude"), "", false)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	assertChatIDs(t, got, "chat-a")
}

// TestResolve_EmptyAudienceIsNotAnError verifies "nobody matched" is a
// legitimate outcome: an empty slice and a nil error.
func TestResolve_EmptyAudienceIsNotAnError(t *testing.T) {
	r := fixtureResolver()
	got, err := r.Resolve(context.Background(), mustParse(t, "repo:repo-nope"), "", false)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("targets = %v, want empty", chatIDs(got))
	}
}

// TestResolve_FanOutCapRefuses verifies exceeding MaxTargets is a loud refusal
// naming both the resolved count and the cap — never a silent truncation.
func TestResolve_FanOutCapRefuses(t *testing.T) {
	const n = MaxTargets + 1
	chats := make([]*models.AgentChat, 0, n)
	for i := range n {
		chats = append(chats, chat(fmt.Sprintf("chat-%d", i), "sess-1", "claude", "", ""))
	}
	sessions := map[string]*models.Session{"sess-1": {ID: "sess-1", RepoID: "repo-1"}}
	r := NewResolver(&fakeChats{chats: chats}, &fakeSessions{sessions: sessions}, zerolog.Nop())

	got, err := r.Resolve(context.Background(), mustParse(t, "session:sess-1"), "", false)
	if err == nil {
		t.Fatalf("Resolve returned %d targets, want an error", len(got))
	}
	if got != nil {
		t.Errorf("Resolve returned %d targets alongside the error, want no partial audience", len(got))
	}
	if !errors.Is(err, ErrTooManyTargets) {
		t.Errorf("error %v does not match ErrTooManyTargets", err)
	}
	msg := err.Error()
	if !strings.Contains(msg, fmt.Sprint(n)) {
		t.Errorf("error %q does not name the resolved count %d", msg, n)
	}
	if !strings.Contains(msg, fmt.Sprint(MaxTargets)) {
		t.Errorf("error %q does not name the cap %d", msg, MaxTargets)
	}

	// Exactly at the cap is allowed.
	r = NewResolver(&fakeChats{chats: chats[:MaxTargets]}, &fakeSessions{sessions: sessions}, zerolog.Nop())
	atCap, err := r.Resolve(context.Background(), mustParse(t, "session:sess-1"), "", false)
	if err != nil {
		t.Fatalf("Resolve at cap: %v", err)
	}
	if len(atCap) != MaxTargets {
		t.Errorf("targets = %d, want %d", len(atCap), MaxTargets)
	}
}

// TestResolve_ListError propagates a chat-store failure: unlike a single
// unloadable session, it means the candidate set itself is unknown.
func TestResolve_ListError(t *testing.T) {
	boom := errors.New("db down")
	r := NewResolver(&fakeChats{err: boom}, &fakeSessions{}, zerolog.Nop())
	if _, err := r.Resolve(context.Background(), mustParse(t, "agent:claude"), "", false); !errors.Is(err, boom) {
		t.Errorf("err = %v, want it to wrap %v", err, boom)
	}
}

// TestResolve_DeduplicatesByChatID verifies a chat listed twice by the candidate
// source yields one target. CreateDeliveries rejects the WHOLE batch when a chat
// appears twice, so a duplicate here would fail the send outright and strand the
// created broadcast in pending — and it would also inflate the count checked
// against MaxTargets.
func TestResolve_DeduplicatesByChatID(t *testing.T) {
	chats := []*models.AgentChat{
		chat("chat-a", "sess-1", "claude", "acct-1", ""),
		chat("chat-a", "sess-1", "claude", "acct-1", ""),
	}
	sessions := map[string]*models.Session{"sess-1": {ID: "sess-1", RepoID: "repo-1"}}
	r := NewResolver(&fakeChats{chats: chats}, &fakeSessions{sessions: sessions}, zerolog.Nop())

	got, err := r.Resolve(context.Background(), mustParse(t, "repo:repo-1"), "", false)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	assertChatIDs(t, got, "chat-a")
}

// TestResolve_TerminalSessionChatsExcluded verifies a chat whose session a human
// deliberately ended (archived/merged/closed) or that was orphaned mid-flight is
// never a target: delivery wakes a chat, so including one would resurrect an
// agent in a finished worktree, and these rows accumulate until they push
// ordinary selectors over the fan-out cap.
func TestResolve_TerminalSessionChatsExcluded(t *testing.T) {
	archived := time.Now().UTC()
	cases := map[string]*models.Session{
		"archived": {ID: "sess-x", RepoID: "repo-1", ArchivedAt: &archived},
		"merged":   {ID: "sess-x", RepoID: "repo-1", State: machine.Merged},
		"closed":   {ID: "sess-x", RepoID: "repo-1", State: machine.Closed},
		"orphaned": {ID: "sess-x", RepoID: "repo-1", State: machine.Orphaned},
	}
	for name, session := range cases {
		t.Run(name, func(t *testing.T) {
			chats := []*models.AgentChat{chat("chat-x", "sess-x", "claude", "acct-1", "")}
			r := NewResolver(&fakeChats{chats: chats},
				&fakeSessions{sessions: map[string]*models.Session{"sess-x": session}}, zerolog.Nop())

			// Even naming the chat directly must not reach it.
			for _, sel := range []string{"repo:repo-1", "chat:chat-x"} {
				got, err := r.Resolve(context.Background(), mustParse(t, sel), "", false)
				if err != nil {
					t.Fatalf("Resolve(%s): %v", sel, err)
				}
				if len(got) != 0 {
					t.Errorf("Resolve(%s) = %v, want none (%s session)", sel, chatIDs(got), name)
				}
			}
		})
	}
}

// TestResolve_LiveSessionStatesAreStillTargets guards the exclusion from being
// over-broad: a blocked or in-flight session still has a pane a delivery reaches.
func TestResolve_LiveSessionStatesAreStillTargets(t *testing.T) {
	for _, state := range []machine.State{machine.ImplementingPlan, machine.Blocked, machine.ReadyForReview} {
		t.Run(state.String(), func(t *testing.T) {
			chats := []*models.AgentChat{chat("chat-x", "sess-x", "claude", "acct-1", "")}
			r := NewResolver(&fakeChats{chats: chats},
				&fakeSessions{sessions: map[string]*models.Session{
					"sess-x": {ID: "sess-x", RepoID: "repo-1", State: state},
				}}, zerolog.Nop())

			got, err := r.Resolve(context.Background(), mustParse(t, "repo:repo-1"), "", false)
			if err != nil {
				t.Fatalf("Resolve: %v", err)
			}
			assertChatIDs(t, got, "chat-x")
		})
	}
}

// TestResolve_TransientSessionErrorIsFatal verifies a session read that FAILED
// (as opposed to one that is absent) aborts the resolve rather than silently
// freezing a narrower audience: the RPC contract forbids a partial audience the
// caller believes is complete.
func TestResolve_TransientSessionErrorIsFatal(t *testing.T) {
	boom := errors.New("database is locked")
	chats := []*models.AgentChat{chat("chat-x", "sess-x", "claude", "acct-1", "")}
	r := NewResolver(&fakeChats{chats: chats}, &fakeSessions{err: boom}, zerolog.Nop())

	if _, err := r.Resolve(context.Background(), mustParse(t, "repo:repo-1"), "", false); !errors.Is(err, boom) {
		t.Fatalf("Resolve error = %v, want the underlying read error", err)
	}
}

// TestResolve_BlankChatIDIsSkipped verifies a chat row with a blank
// agent_session_id costs one unreachable target rather than failing the whole
// send: CreateDeliveries rejects the ENTIRE batch on a blank target chat id, so
// keeping it would make one malformed row break every broadcast.
func TestResolve_BlankChatIDIsSkipped(t *testing.T) {
	for _, blank := range []string{"", "   "} {
		chats := []*models.AgentChat{
			chat(blank, "sess-1", "claude", "acct-1", ""),
			chat("chat-a", "sess-1", "claude", "acct-1", ""),
		}
		sessions := map[string]*models.Session{"sess-1": {ID: "sess-1", RepoID: "repo-1"}}
		r := NewResolver(&fakeChats{chats: chats}, &fakeSessions{sessions: sessions}, zerolog.Nop())

		got, err := r.Resolve(context.Background(), mustParse(t, "repo:repo-1"), "", false)
		if err != nil {
			t.Fatalf("Resolve: %v", err)
		}
		assertChatIDs(t, got, "chat-a")
	}
}

// TestResolve_ReadsEachSessionOnce verifies the per-resolve memoization: sibling
// chats of one session must not each cost a synchronous point read, since
// ListRoutableChats is unbounded in the daemon's chat history.
func TestResolve_ReadsEachSessionOnce(t *testing.T) {
	chats := []*models.AgentChat{
		chat("chat-a", "sess-1", "claude", "acct-1", ""),
		chat("chat-b", "sess-1", "codex", "acct-2", ""),
		chat("chat-c", "sess-1", "claude", "acct-1", ""),
		chat("chat-d", "sess-missing", "claude", "acct-1", ""),
		chat("chat-e", "sess-missing", "claude", "acct-1", ""),
	}
	counting := &fakeSessions{sessions: map[string]*models.Session{
		"sess-1": {ID: "sess-1", RepoID: "repo-1"},
	}}
	r := NewResolver(&fakeChats{chats: chats}, counting, zerolog.Nop())

	got, err := r.Resolve(context.Background(), mustParse(t, "repo:repo-1"), "", false)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	assertChatIDs(t, got, "chat-a", "chat-b", "chat-c")
	// Two distinct session ids, so two reads — the absent one is cached too.
	if counting.getCalls != 2 {
		t.Errorf("sessions.Get called %d times, want 2 (one per distinct session)", counting.getCalls)
	}
}
