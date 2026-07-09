package server

import (
	"context"
	"errors"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/recurser/bossalib/models"
	"github.com/recurser/bossd/internal/db"
	"github.com/rs/zerolog"
)

func codexChat(agentSessionID, providerID string, created time.Time) *models.AgentChat {
	c := &models.AgentChat{
		AgentSessionID: agentSessionID,
		AgentName:      "codex",
		CreatedAt:      created,
	}
	if providerID != "" {
		c.ProviderSessionID = ptr(providerID)
	}
	return c
}

func TestCodexProviderDuplicatesClearsLaterCreatedKeepsEarliest(t *testing.T) {
	base := time.Date(2026, 7, 7, 3, 13, 40, 0, time.UTC)
	chats := []*models.AgentChat{
		codexChat("chatA", "shared-id", base),
		codexChat("chatB", "shared-id", base.Add(30*time.Second)),
	}
	got := codexProviderSessionIDDuplicatesToClear(chats)
	if len(got) != 1 || got[0] != "chatB" {
		t.Fatalf("cleared = %v, want [chatB] (later-created loses, earliest keeps its id)", got)
	}
}

func TestCodexProviderDuplicatesTieBreaksLexicographically(t *testing.T) {
	same := time.Date(2026, 7, 7, 3, 13, 40, 0, time.UTC)
	chats := []*models.AgentChat{
		codexChat("chatZ", "shared-id", same),
		codexChat("chatA", "shared-id", same),
	}
	got := codexProviderSessionIDDuplicatesToClear(chats)
	if len(got) != 1 || got[0] != "chatZ" {
		t.Fatalf("cleared = %v, want [chatZ] (equal created → keep lexicographically-first agentSessionID)", got)
	}
}

func TestCodexProviderDuplicatesThreeWayKeepsOne(t *testing.T) {
	base := time.Date(2026, 7, 7, 3, 13, 40, 0, time.UTC)
	chats := []*models.AgentChat{
		codexChat("c1", "shared-id", base.Add(2*time.Second)),
		codexChat("c2", "shared-id", base),
		codexChat("c3", "shared-id", base.Add(1*time.Second)),
	}
	got := codexProviderSessionIDDuplicatesToClear(chats)
	sort.Strings(got)
	if len(got) != 2 || got[0] != "c1" || got[1] != "c3" {
		t.Fatalf("cleared = %v, want [c1 c3] (keep earliest c2)", got)
	}
}

func TestCodexProviderDuplicatesNoDuplicatesClearsNothing(t *testing.T) {
	base := time.Date(2026, 7, 7, 3, 13, 40, 0, time.UTC)
	chats := []*models.AgentChat{
		codexChat("c1", "id-1", base),
		codexChat("c2", "id-2", base),
	}
	if got := codexProviderSessionIDDuplicatesToClear(chats); len(got) != 0 {
		t.Fatalf("cleared = %v, want none", got)
	}
}

// --- repair orchestration (repairDuplicateCodexProviderSessionIDs +
// duplicateCodexProviderSessionCheck) with minimal purpose-built store fakes ---

type repairRepoStoreFake struct {
	db.RepoStore
	repos []*models.Repo
}

func (f *repairRepoStoreFake) List(context.Context) ([]*models.Repo, error) { return f.repos, nil }

type repairSessionStoreFake struct {
	db.SessionStore
	byRepo map[string][]*models.Session
	err    error
}

func (f *repairSessionStoreFake) ListActive(_ context.Context, repoID string) ([]*models.Session, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.byRepo[repoID], nil
}

type repairChatStoreFake struct {
	db.AgentChatStore
	bySession map[string][]*models.AgentChat
	listErr   map[string]error
	cleared   []string
}

func (f *repairChatStoreFake) ListBySession(_ context.Context, sessionID string) ([]*models.AgentChat, error) {
	if err := f.listErr[sessionID]; err != nil {
		return nil, err
	}
	return f.bySession[sessionID], nil
}

func (f *repairChatStoreFake) UpdateProviderSessionID(_ context.Context, agentSessionID string, providerSessionID *string) error {
	if providerSessionID == nil {
		f.cleared = append(f.cleared, agentSessionID)
	}
	return nil
}

func TestRepairDuplicateCodexProviderSessionIDs_ClearsCollisionKeepsEarliest(t *testing.T) {
	base := time.Date(2026, 7, 7, 3, 13, 40, 0, time.UTC)
	chats := &repairChatStoreFake{bySession: map[string][]*models.AgentChat{
		"sess-1": {codexChat("chatA", "shared", base), codexChat("chatB", "shared", base.Add(time.Second))},
	}}
	s := &Server{
		logger:     zerolog.Nop(),
		repos:      &repairRepoStoreFake{repos: []*models.Repo{{ID: "repo-1"}}},
		sessions:   &repairSessionStoreFake{byRepo: map[string][]*models.Session{"repo-1": {{ID: "sess-1"}}}},
		agentChats: chats,
	}

	check := s.duplicateCodexProviderSessionCheck(context.Background())
	if check.Ok {
		t.Errorf("Ok = true, want false (a collision was cleared)")
	}
	if len(chats.cleared) != 1 || chats.cleared[0] != "chatB" {
		t.Fatalf("cleared = %v, want [chatB] (keep earliest chatA)", chats.cleared)
	}
	if !strings.Contains(check.Detail, "cleared 1") {
		t.Errorf("Detail = %q, want it to report the clear", check.Detail)
	}
}

func TestRepairDuplicateCodexProviderSessionIDs_UnreadableSessionNotReportedGreen(t *testing.T) {
	chats := &repairChatStoreFake{
		bySession: map[string][]*models.AgentChat{},
		listErr:   map[string]error{"sess-1": errors.New("db locked")},
	}
	s := &Server{
		logger:     zerolog.Nop(),
		repos:      &repairRepoStoreFake{repos: []*models.Repo{{ID: "repo-1"}}},
		sessions:   &repairSessionStoreFake{byRepo: map[string][]*models.Session{"repo-1": {{ID: "sess-1"}}}},
		agentChats: chats,
	}

	check := s.duplicateCodexProviderSessionCheck(context.Background())
	// A session whose chats can't be listed may hide a collision; the check must
	// not present that as a clean bill of health.
	if check.Ok {
		t.Errorf("Ok = true, want false (a session was unreadable)")
	}
	if !strings.Contains(check.Detail, "unreadable") {
		t.Errorf("Detail = %q, want it to note the unreadable session", check.Detail)
	}
	if len(chats.cleared) != 0 {
		t.Errorf("cleared = %v, want none", chats.cleared)
	}
}

func TestRepairDuplicateCodexProviderSessionIDs_CleanScanIsGreen(t *testing.T) {
	base := time.Date(2026, 7, 7, 3, 13, 40, 0, time.UTC)
	chats := &repairChatStoreFake{bySession: map[string][]*models.AgentChat{
		"sess-1": {codexChat("chatA", "id-1", base), codexChat("chatB", "id-2", base)},
	}}
	s := &Server{
		logger:     zerolog.Nop(),
		repos:      &repairRepoStoreFake{repos: []*models.Repo{{ID: "repo-1"}}},
		sessions:   &repairSessionStoreFake{byRepo: map[string][]*models.Session{"repo-1": {{ID: "sess-1"}}}},
		agentChats: chats,
	}

	check := s.duplicateCodexProviderSessionCheck(context.Background())
	if !check.Ok {
		t.Errorf("Ok = false, want true (no collisions, all readable): %s", check.Detail)
	}
	if len(chats.cleared) != 0 {
		t.Errorf("cleared = %v, want none", chats.cleared)
	}
}

func TestCodexProviderDuplicatesIgnoresNilAndNonCodex(t *testing.T) {
	base := time.Date(2026, 7, 7, 3, 13, 40, 0, time.UTC)
	claudeA := &models.AgentChat{AgentSessionID: "claudeA", AgentName: "claude", ProviderSessionID: ptr("shared-id"), CreatedAt: base}
	claudeB := &models.AgentChat{AgentSessionID: "claudeB", AgentName: "claude", ProviderSessionID: ptr("shared-id"), CreatedAt: base.Add(time.Second)}
	chats := []*models.AgentChat{
		claudeA,
		claudeB,
		codexChat("codexNil", "", base), // nil provider id — ignored
	}
	if got := codexProviderSessionIDDuplicatesToClear(chats); len(got) != 0 {
		t.Fatalf("cleared = %v, want none (claude chats and nil ids are untouched)", got)
	}
}
