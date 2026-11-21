package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	bossanovav1 "github.com/recurser/bossalib/gen/bossanova/v1"
)

type fakeProcessFiles struct {
	files []string
	err   error
}

func (f fakeProcessFiles) OpenFiles(int) ([]string, error) { return f.files, f.err }

func userTurnAt(at string) string {
	return `{"type":"USER_INPUT","source":"USER_EXPLICIT","status":"DONE","created_at":"` + at + `","content":"<USER_REQUEST>\nhi\n</USER_REQUEST>"}` + "\n"
}

func TestResolveFromPaneProcess(t *testing.T) {
	s, home := newTestServer(t)
	conversations := filepath.Join(home, ".gemini", "antigravity-cli", "conversations")
	db := func(id string) string { return filepath.Join(conversations, id+".db") }
	installJSON(t, home, "last_conversations.json", map[string]string{"/work": conversationB})
	launchedAt := time.Date(2026, 11, 20, 10, 0, 0, 0, time.UTC)
	launched := timestamppb.New(launchedAt)
	installTranscript(t, home, conversationA, userTurnAt("2026-11-20T10:00:03Z"))
	installTranscript(t, home, conversationD, userTurnAt("2026-11-20T10:00:04Z"))
	stale := installTranscript(t, home, conversationB, userTurnAt("2026-11-20T09:00:00Z"))
	if err := os.Chtimes(stale, launchedAt.Add(-time.Hour), launchedAt.Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name      string
		procs     fakeProcessFiles
		backfill  bool
		wantID    string
		ambiguous bool
	}{
		{name: "the pane's own conversation", procs: fakeProcessFiles{files: []string{"/dev/null", db(conversationA), db(conversationA) + "-wal"}}, wantID: conversationA},
		{name: "two active conversations open", procs: fakeProcessFiles{files: []string{db(conversationA), db(conversationD)}}, ambiguous: true},
		{name: "a stale conversation opened in passing", procs: fakeProcessFiles{files: []string{db(conversationB)}}},
		{name: "an empty conversation opened in passing", procs: fakeProcessFiles{files: []string{db(conversationC)}}},
		{name: "the pane's own conversation beside a passing one", procs: fakeProcessFiles{files: []string{db(conversationC), db(conversationB), db(conversationA)}}, wantID: conversationA},
		{name: "nothing open yet, launch poll waits", procs: fakeProcessFiles{files: []string{"/dev/null"}}},
		{name: "a database outside agy's store", procs: fakeProcessFiles{files: []string{filepath.Join(home, "elsewhere", conversationA+".db")}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s.procs = tt.procs
			resp, err := s.ResolveInteractiveSessionID(context.Background(), &bossanovav1.ResolveInteractiveSessionIDRequest{
				WorkDir: "/work", PanePid: 4242, LaunchedAfter: launched, AllowLegacyBackfill: tt.backfill,
			})
			if err != nil {
				t.Fatal(err)
			}
			if resp.GetSessionId() != tt.wantID || resp.GetFound() != (tt.wantID != "") || resp.GetAmbiguous() != tt.ambiguous {
				t.Fatalf("resolve = %+v; want id %q ambiguous=%v", resp, tt.wantID, tt.ambiguous)
			}
			if !resp.GetFound() && resp.GetReason() == "" {
				t.Fatal("a miss must carry a reason")
			}
		})
	}
}

func TestResolveFromLastConversations(t *testing.T) {
	workDir := t.TempDir()
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(workDir, link); err != nil {
		t.Fatal(err)
	}
	realDir, err := filepath.EvalSymlinks(workDir)
	if err != nil {
		t.Fatal(err)
	}
	launched := time.Date(2026, 11, 20, 10, 0, 0, 500_000_000, time.UTC)
	tests := []struct {
		name     string
		key      string
		firstAt  string
		procsErr error
		want     string
	}{
		{name: "conversation started after launch", key: link, firstAt: "2026-11-20T10:00:03Z", want: conversationA},
		{name: "same second as launch", key: link, firstAt: "2026-11-20T10:00:00Z", want: conversationA},
		{name: "keyed by the resolved path", key: realDir, firstAt: "2026-11-20T10:00:03Z", want: conversationA},
		{name: "an earlier conversation in the same worktree", key: link, firstAt: "2026-11-20T09:59:59Z"},
		{name: "pane probe failed falls back", key: link, firstAt: "2026-11-20T10:00:03Z", procsErr: errors.New("lsof missing"), want: conversationA},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s, home := newTestServer(t)
			s.procs = fakeProcessFiles{err: tt.procsErr}
			installJSON(t, home, "last_conversations.json", map[string]string{tt.key: conversationA})
			installTranscript(t, home, conversationA, userTurnAt(tt.firstAt))
			req := &bossanovav1.ResolveInteractiveSessionIDRequest{WorkDir: link, LaunchedAfter: timestamppb.New(launched)}
			if tt.procsErr != nil {
				req.PanePid = 4242
			}
			resp, err := s.ResolveInteractiveSessionID(context.Background(), req)
			if err != nil {
				t.Fatal(err)
			}
			if resp.GetSessionId() != tt.want || resp.GetFound() != (tt.want != "") {
				t.Fatalf("resolve = %+v; want %q", resp, tt.want)
			}
		})
	}
}

func TestResolveWithoutLaunchTimeNeverGuesses(t *testing.T) {
	s, home := newTestServer(t)
	installJSON(t, home, "last_conversations.json", map[string]string{"/work": conversationA})
	installTranscript(t, home, conversationA, userTurnAt("2026-11-20T10:00:03Z"))
	resp, err := s.ResolveInteractiveSessionID(context.Background(), &bossanovav1.ResolveInteractiveSessionIDRequest{WorkDir: "/work", RequestedSessionId: conversationB})
	if err != nil || resp.GetFound() {
		t.Fatalf("resolve = %+v, %v; want no binding without a launch time", resp, err)
	}
}

func TestResolveBackfillOnLivePaneNeverBorrowsASiblingsConversation(t *testing.T) {
	s, home := newTestServer(t)
	s.procs = fakeProcessFiles{files: []string{"/dev/null"}}
	installJSON(t, home, "last_conversations.json", map[string]string{"/work": conversationA})
	installTranscript(t, home, conversationA, userTurnAt("2026-11-20T10:10:00Z"))
	created := time.Date(2026, 11, 20, 10, 0, 0, 0, time.UTC)
	resp, err := s.ResolveInteractiveSessionID(context.Background(), &bossanovav1.ResolveInteractiveSessionIDRequest{
		WorkDir: "/work", PanePid: 4242, AllowLegacyBackfill: true,
		LaunchedAfter: timestamppb.New(created.Add(-5 * time.Minute)), ChatCreatedAt: timestamppb.New(created),
	})
	if err != nil || resp.GetFound() {
		t.Fatalf("resolve = %+v, %v; a live pane holding no conversation has none", resp, err)
	}
}

func TestResolveFallbackRejectsAConversationStartedLongAfterLaunch(t *testing.T) {
	s, home := newTestServer(t)
	installJSON(t, home, "last_conversations.json", map[string]string{"/work": conversationA})
	installTranscript(t, home, conversationA, userTurnAt("2026-11-20T10:10:00Z"))
	resp, err := s.ResolveInteractiveSessionID(context.Background(), &bossanovav1.ResolveInteractiveSessionIDRequest{
		WorkDir: "/work", LaunchedAfter: timestamppb.New(time.Date(2026, 11, 20, 10, 0, 0, 0, time.UTC)),
	})
	if err != nil || resp.GetFound() {
		t.Fatalf("resolve = %+v, %v; a conversation ten minutes after launch belongs to a later chat", resp, err)
	}
}
