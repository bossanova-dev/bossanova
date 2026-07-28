package clitest_test

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/recurser/boss/internal/clitest"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// noteJSON mirrors the stable schema emitted by `boss notes add|ls|show|edit
// --json`. It is declared here rather than shared with the CLI's own struct on
// purpose: this copy is the wire contract scripts depend on, so a rename in the
// CLI must break this test rather than silently travel through it.
//
// Unlike a callback or broadcast message, a note body is NOT a secret — it is
// the payload — so the body is expected in output, and the assertions below
// check it is carried, not withheld.
type noteJSON struct {
	ID        string   `json:"id"`
	RepoID    string   `json:"repo_id"`
	SessionID string   `json:"session_id"`
	ChatID    string   `json:"chat_id"`
	Body      string   `json:"body"`
	Tags      []string `json:"tags"`
	CreatedAt string   `json:"created_at"`
	UpdatedAt string   `json:"updated_at"`
}

// noteBodyTail is the last word of testLongBody. `boss notes ls` truncates the
// body to a one-line preview, so this word must NOT appear in the table while
// `boss notes show` must print it.
const noteBodyTail = "TAIL-MARKER-VISIBLE-ONLY-IN-SHOW"

// testLongBody is comfortably longer than the 60-rune BODY column cap, so the
// preview/full-text difference is observable rather than incidental.
const testLongBody = "the retry loop drops the last error on the floor when the context " +
	"deadline fires first, which is why the sweep saw an empty reason " + noteBodyTail

func noteTime(offset int) *timestamppb.Timestamp {
	base := time.Date(2024, 3, 1, 12, 0, 0, 0, time.UTC)
	return timestamppb.New(base.Add(time.Duration(offset) * time.Minute))
}

// testNotes returns two seeded notes with disjoint tags, oldest first.
func testNotes() []*pb.Note {
	return []*pb.Note{
		{
			Id:        "note-aaa",
			RepoId:    "repo-1",
			SessionId: "sess-1",
			ChatId:    "chat-1",
			Body:      "the flake is in the timer, not the assertion",
			Tags:      []string{"flaky", "tech-debt"},
			CreatedAt: noteTime(0),
			UpdatedAt: noteTime(0),
		},
		{
			Id:        "note-bbb",
			RepoId:    "repo-1",
			Body:      testLongBody,
			Tags:      []string{"perf"},
			CreatedAt: noteTime(10),
			UpdatedAt: noteTime(10),
		},
	}
}

// testFilterNotes returns testNotes plus a note in a DIFFERENT repo, so every
// filter case has at least one row it is required to reject. note-ccc carries
// no session and no chat, which is what makes the SET-but-empty provenance
// filter case meaningful.
func testFilterNotes() []*pb.Note {
	return append(testNotes(), &pb.Note{
		Id:        "note-ccc",
		RepoId:    "repo-2",
		Body:      "the docs drifted from the flag names",
		Tags:      []string{"docs"},
		CreatedAt: noteTime(20),
		UpdatedAt: noteTime(20),
	})
}

func TestCLI_Notes_Add(t *testing.T) {
	h := clitest.New(t)
	const body = "the dependabot bump needs a matching lockfile refresh"
	res := h.Run("notes", "add", body, "--repo", "r1", "--tag", "tech-debt", "--tag", "flaky")

	if res.ExitCode != 0 {
		t.Fatalf("exit=%d stderr=%q", res.ExitCode, res.Stderr)
	}
	if !strings.Contains(res.Stdout, "Added note ") {
		t.Errorf("expected a confirmation line, got %q", res.Stdout)
	}

	calls := h.Daemon.CreateNoteCalls()
	if len(calls) != 1 {
		t.Fatalf("expected 1 CreateNote call, got %d", len(calls))
	}
	req := calls[0]
	if req.GetRepoId() != "r1" {
		t.Errorf("repo id = %q, want r1", req.GetRepoId())
	}
	if req.GetBody() != body {
		t.Errorf("body = %q, want %q", req.GetBody(), body)
	}
	if got := strings.Join(req.GetTags(), ","); got != "tech-debt,flaky" {
		t.Errorf("tags = %q, want both tags in flag order", got)
	}
}

// TestCLI_Notes_Add_NoRepo pins the actionable failure when the repository
// cannot be determined: no --repo, no ambient BOSS_REPO_ID (the harness strips
// it), and a daemon that resolves the working directory to nothing.
func TestCLI_Notes_Add_NoRepo(t *testing.T) {
	h := clitest.New(t)
	res := h.Run("notes", "add", "orphan note")

	if res.ExitCode == 0 {
		t.Fatalf("expected non-zero exit with no repo context; stdout=%q", res.Stdout)
	}
	if !strings.Contains(res.Stderr, "--repo") {
		t.Errorf("stderr should tell the user to pass --repo, got %q", res.Stderr)
	}
	if n := len(h.Daemon.CreateNoteCalls()); n != 0 {
		t.Errorf("no CreateNote call should be made on a validation failure, got %d", n)
	}
}

// TestCLI_Notes_Add_AmbientContext proves an agent inside a session records a
// note with full provenance and no ids to look up.
func TestCLI_Notes_Add_AmbientContext(t *testing.T) {
	h := clitest.New(t, clitest.WithEnv(
		"BOSS_REPO_ID=ambient-repo",
		"BOSS_SESSION_ID=ambient-session",
		"BOSS_AGENT_SESSION_ID=ambient-chat",
	))
	res := h.Run("notes", "add", "recorded from inside a run")

	if res.ExitCode != 0 {
		t.Fatalf("exit=%d stderr=%q", res.ExitCode, res.Stderr)
	}
	calls := h.Daemon.CreateNoteCalls()
	if len(calls) != 1 {
		t.Fatalf("expected 1 CreateNote call, got %d", len(calls))
	}
	req := calls[0]
	if req.GetRepoId() != "ambient-repo" {
		t.Errorf("repo id = %q, want ambient-repo", req.GetRepoId())
	}
	if req.GetSessionId() != "ambient-session" {
		t.Errorf("session id = %q, want ambient-session", req.GetSessionId())
	}
	if req.GetChatId() != "ambient-chat" {
		t.Errorf("chat id = %q, want ambient-chat", req.GetChatId())
	}
}

// TestCLI_Notes_Add_WorkingDirectoryContext covers the last fallback: no flag
// and no env, so the repo and session come from the daemon's view of the
// working directory.
func TestCLI_Notes_Add_WorkingDirectoryContext(t *testing.T) {
	h := clitest.New(t, clitest.WithResolvedContext(
		&pb.Repo{Id: "wd-repo"},
		&pb.Session{Id: "wd-session"},
	))
	res := h.Run("notes", "add", "resolved from the working directory")

	if res.ExitCode != 0 {
		t.Fatalf("exit=%d stderr=%q", res.ExitCode, res.Stderr)
	}
	calls := h.Daemon.CreateNoteCalls()
	if len(calls) != 1 {
		t.Fatalf("expected 1 CreateNote call, got %d", len(calls))
	}
	if calls[0].GetRepoId() != "wd-repo" {
		t.Errorf("repo id = %q, want wd-repo", calls[0].GetRepoId())
	}
	if calls[0].GetSessionId() != "wd-session" {
		t.Errorf("session id = %q, want wd-session", calls[0].GetSessionId())
	}
	// No chat fallback exists: guessing one would misattribute the note.
	if calls[0].GetChatId() != "" {
		t.Errorf("chat id = %q, want empty (no working-directory fallback)", calls[0].GetChatId())
	}
}

func TestCLI_Notes_List(t *testing.T) {
	h := clitest.New(t, clitest.WithNotes(testNotes()...))
	res := h.Run("notes", "ls")

	if res.ExitCode != 0 {
		t.Fatalf("exit=%d stderr=%q", res.ExitCode, res.Stderr)
	}
	for _, want := range []string{"note-aaa", "note-bbb", "flaky", "tech-debt", "perf", "2024-03-01T12:00:00Z"} {
		if !strings.Contains(res.Stdout, want) {
			t.Errorf("stdout missing %q\n%s", want, res.Stdout)
		}
	}
}

func TestCLI_Notes_List_Empty(t *testing.T) {
	h := clitest.New(t)
	res := h.Run("notes", "ls")

	if res.ExitCode != 0 {
		t.Fatalf("exit=%d stderr=%q", res.ExitCode, res.Stderr)
	}
	if !strings.Contains(res.Stdout, "No notes.") {
		t.Errorf("expected the empty-state line, got %q", res.Stdout)
	}
}

// TestCLI_Notes_List_TagFilter asserts both halves of the filter contract: the
// visible rows, and that the filter was actually sent to the daemon. A CLI that
// filtered client-side would pass the first assertion and fail the second.
func TestCLI_Notes_List_TagFilter(t *testing.T) {
	h := clitest.New(t, clitest.WithNotes(testNotes()...))
	res := h.Run("notes", "ls", "--tag", "perf")

	if res.ExitCode != 0 {
		t.Fatalf("exit=%d stderr=%q", res.ExitCode, res.Stderr)
	}
	if !strings.Contains(res.Stdout, "note-bbb") {
		t.Errorf("stdout should contain the perf note, got %q", res.Stdout)
	}
	if strings.Contains(res.Stdout, "note-aaa") {
		t.Errorf("stdout should NOT contain the untagged-for-perf note, got %q", res.Stdout)
	}
	calls := h.Daemon.ListNoteCalls()
	if len(calls) != 1 {
		t.Fatalf("expected 1 ListNotes call, got %d", len(calls))
	}
	if got := strings.Join(calls[0].GetTags(), ","); got != "perf" {
		t.Errorf("ListNotes tag filter = %q, want perf", got)
	}
}

// TestCLI_Notes_List_FilterExclusions gives every non-tag filter a row it MUST
// reject. A filter is only proven by what it leaves out: a listing that ignored
// --repo, --session, --search or --limit entirely would still satisfy any
// assertion that only checks the wanted note is present.
//
// Each case also inspects the recorded ListNotesRequest, so a CLI that filtered
// client-side (or a mock that filtered on the wrong field) cannot pass.
func TestCLI_Notes_List_FilterExclusions(t *testing.T) {
	cases := []struct {
		name      string
		args      []string
		wantIDs   []string
		assertReq func(t *testing.T, req *pb.ListNotesRequest)
	}{
		{
			name:    "repo filter excludes other repos",
			args:    []string{"--repo", "repo-1"},
			wantIDs: []string{"note-aaa", "note-bbb"},
			assertReq: func(t *testing.T, req *pb.ListNotesRequest) {
				if req.RepoId == nil || req.GetRepoId() != "repo-1" {
					t.Errorf("repo filter = %v, want repo-1", req.RepoId)
				}
			},
		},
		{
			// sess-1 is note-aaa's; note-bbb and note-ccc carry NO session, and a
			// note with no session is not "the session you asked for".
			name:    "session filter excludes other and absent sessions",
			args:    []string{"--session", "sess-1"},
			wantIDs: []string{"note-aaa"},
			assertReq: func(t *testing.T, req *pb.ListNotesRequest) {
				if req.SessionId == nil || req.GetSessionId() != "sess-1" {
					t.Errorf("session filter = %v, want sess-1", req.SessionId)
				}
			},
		},
		{
			// The daemon stores an absent session as SQL NULL and `session_id = ''`
			// matches no row, so a SET-but-empty filter returns NOTHING — it must
			// NOT be read as "the notes that have no session".
			name:    "empty session filter matches nothing",
			args:    []string{"--session", ""},
			wantIDs: nil,
			assertReq: func(t *testing.T, req *pb.ListNotesRequest) {
				if req.SessionId == nil || req.GetSessionId() != "" {
					t.Errorf("session filter = %v, want a SET empty string", req.SessionId)
				}
			},
		},
		{
			// The daemon trims the filter value before binding it, so surrounding
			// whitespace must not silently empty the result set.
			name:    "session filter is trimmed",
			args:    []string{"--session", " sess-1 "},
			wantIDs: []string{"note-aaa"},
			assertReq: func(t *testing.T, req *pb.ListNotesRequest) {
				// Deliberately asserts the UNTRIMMED value reached the daemon:
				// trimming is the daemon's job, and a CLI that started doing it
				// client-side would keep the row assertion green while silently
				// no longer exercising the server-side trim at all.
				if req.SessionId == nil || req.GetSessionId() != " sess-1 " {
					t.Errorf("session filter = %v, want the untrimmed \" sess-1 \"", req.SessionId)
				}
			},
		},
		{
			name:    "search excludes non-matching bodies",
			args:    []string{"--search", "timer"},
			wantIDs: []string{"note-aaa"},
			assertReq: func(t *testing.T, req *pb.ListNotesRequest) {
				if req.Search == nil || req.GetSearch() != "timer" {
					t.Errorf("search filter = %v, want timer", req.Search)
				}
			},
		},
		{
			name:    "limit caps the oldest-first listing",
			args:    []string{"--limit", "1"},
			wantIDs: []string{"note-aaa"},
			assertReq: func(t *testing.T, req *pb.ListNotesRequest) {
				if req.GetLimit() != 1 {
					t.Errorf("limit = %d, want 1", req.GetLimit())
				}
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := clitest.New(t, clitest.WithNotes(testFilterNotes()...))
			res := h.Run(append([]string{"notes", "ls", "--json"}, tc.args...)...)
			if res.ExitCode != 0 {
				t.Fatalf("exit=%d stderr=%q", res.ExitCode, res.Stderr)
			}
			var listed []noteJSON
			if err := json.Unmarshal([]byte(res.Stdout), &listed); err != nil {
				t.Fatalf("unmarshal: %v\n%s", err, res.Stdout)
			}
			got := make([]string, len(listed))
			for i, n := range listed {
				got[i] = n.ID
			}
			if strings.Join(got, ",") != strings.Join(tc.wantIDs, ",") {
				t.Errorf("ids = %v, want %v", got, tc.wantIDs)
			}
			calls := h.Daemon.ListNoteCalls()
			if len(calls) != 1 {
				t.Fatalf("expected 1 ListNotes call, got %d", len(calls))
			}
			if tc.assertReq != nil {
				tc.assertReq(t, calls[0])
			}
		})
	}
}

// TestCLI_Notes_List_ExplicitEmptyRepoOverridesAmbientRepoID drives the
// documented cross-repo escape hatch through real argv, in the environment that
// makes it necessary: a boss-managed agent pane exports BOSS_REPO_ID, so `cd`
// out of the repo cannot widen the listing and only an explicit `--repo ""`
// can. The paired no-flag case proves the ambient var really was scoping the
// listing, so this cannot pass vacuously.
func TestCLI_Notes_List_ExplicitEmptyRepoOverridesAmbientRepoID(t *testing.T) {
	listIDs := func(t *testing.T, h *clitest.Harness, args ...string) []string {
		t.Helper()
		res := h.Run(append([]string{"notes", "ls", "--json"}, args...)...)
		if res.ExitCode != 0 {
			t.Fatalf("exit=%d stderr=%q", res.ExitCode, res.Stderr)
		}
		var listed []noteJSON
		if err := json.Unmarshal([]byte(res.Stdout), &listed); err != nil {
			t.Fatalf("unmarshal: %v\n%s", err, res.Stdout)
		}
		ids := make([]string, len(listed))
		for i, n := range listed {
			ids[i] = n.ID
		}
		return ids
	}

	scoped := clitest.New(t, clitest.WithNotes(testFilterNotes()...), clitest.WithEnv("BOSS_REPO_ID=repo-1"))
	if got := strings.Join(listIDs(t, scoped), ","); got != "note-aaa,note-bbb" {
		t.Fatalf("ids = %v, want repo-1's notes — $BOSS_REPO_ID must scope the listing", got)
	}

	every := clitest.New(t, clitest.WithNotes(testFilterNotes()...), clitest.WithEnv("BOSS_REPO_ID=repo-1"))
	if got := strings.Join(listIDs(t, every, "--repo", ""), ","); got != "note-aaa,note-bbb,note-ccc" {
		t.Fatalf(`ids = %v, want every repo — an explicit --repo "" must beat $BOSS_REPO_ID`, got)
	}
	calls := every.Daemon.ListNoteCalls()
	if len(calls) != 1 {
		t.Fatalf("expected 1 ListNotes call, got %d", len(calls))
	}
	if calls[0].RepoId != nil {
		t.Errorf("repo filter = %v, want unset on the wire", calls[0].RepoId)
	}
}

func TestCLI_Notes_List_JSON(t *testing.T) {
	h := clitest.New(t, clitest.WithNotes(testNotes()...))
	res := h.Run("notes", "ls", "--json")

	if res.ExitCode != 0 {
		t.Fatalf("exit=%d stderr=%q", res.ExitCode, res.Stderr)
	}
	var notes []noteJSON
	if err := json.Unmarshal([]byte(res.Stdout), &notes); err != nil {
		t.Fatalf("unmarshal: %v\n%s", err, res.Stdout)
	}
	if len(notes) != 2 {
		t.Fatalf("expected 2 notes, got %d", len(notes))
	}
	// Notes list oldest first.
	first := notes[0]
	if first.ID != "note-aaa" || first.RepoID != "repo-1" {
		t.Errorf("unexpected first note: %+v", first)
	}
	if first.SessionID != "sess-1" || first.ChatID != "chat-1" {
		t.Errorf("unexpected provenance: %+v", first)
	}
	if first.Body != "the flake is in the timer, not the assertion" {
		t.Errorf("body should be carried verbatim, got %q", first.Body)
	}
	if got := strings.Join(first.Tags, ","); got != "flaky,tech-debt" {
		t.Errorf("tags = %q, want flaky,tech-debt", got)
	}
	if first.CreatedAt != "2024-03-01T12:00:00Z" || first.UpdatedAt != "2024-03-01T12:00:00Z" {
		t.Errorf("unexpected RFC3339 timestamps: %+v", first)
	}
	// The JSON body is the FULL text, not the table's preview.
	if !strings.Contains(notes[1].Body, noteBodyTail) {
		t.Errorf("json body should carry the full text, got %q", notes[1].Body)
	}
}

// TestCLI_Notes_List_JSON_Empty pins the machine contract for no results: an
// empty array, never `null`, so `jq length` works without a guard.
func TestCLI_Notes_List_JSON_Empty(t *testing.T) {
	h := clitest.New(t)
	res := h.Run("notes", "ls", "--json")

	if res.ExitCode != 0 {
		t.Fatalf("exit=%d stderr=%q", res.ExitCode, res.Stderr)
	}
	if strings.TrimSpace(res.Stdout) != "[]" {
		t.Errorf("expected [] for no results, got %q", res.Stdout)
	}
	var notes []noteJSON
	if err := json.Unmarshal([]byte(res.Stdout), &notes); err != nil {
		t.Fatalf("unmarshal: %v\n%s", err, res.Stdout)
	}
	if notes == nil {
		t.Errorf("expected a non-nil empty slice, got null")
	}
}

// TestCLI_Notes_Show_FullBody is the reason `show` exists: the list truncates
// the body to a one-line preview, and only `show` prints the tail.
func TestCLI_Notes_Show_FullBody(t *testing.T) {
	h := clitest.New(t, clitest.WithNotes(testNotes()...))

	list := h.Run("notes", "ls")
	if list.ExitCode != 0 {
		t.Fatalf("ls exit=%d stderr=%q", list.ExitCode, list.Stderr)
	}
	if strings.Contains(list.Stdout, noteBodyTail) {
		t.Errorf("ls should show a truncated preview, but printed the body tail:\n%s", list.Stdout)
	}

	show := h.Run("notes", "show", "note-bbb")
	if show.ExitCode != 0 {
		t.Fatalf("show exit=%d stderr=%q", show.ExitCode, show.Stderr)
	}
	if !strings.Contains(show.Stdout, testLongBody) {
		t.Errorf("show should print the full body, got %q", show.Stdout)
	}
	for _, want := range []string{"note-bbb", "repo-1", "perf", "2024-03-01T12:10:00Z"} {
		if !strings.Contains(show.Stdout, want) {
			t.Errorf("show output missing %q\n%s", want, show.Stdout)
		}
	}
}

func TestCLI_Notes_Show_Unknown(t *testing.T) {
	h := clitest.New(t, clitest.WithNotes(testNotes()...))
	res := h.Run("notes", "show", "note-missing")

	if res.ExitCode == 0 {
		t.Fatalf("expected non-zero exit for an unknown note; stdout=%q", res.Stdout)
	}
	if !strings.Contains(res.Stderr, "note-missing") {
		t.Errorf("stderr should name the missing note, got %q", res.Stderr)
	}
}

// TestCLI_Notes_Edit_TagsReplace is the surprising-semantics regression test:
// --tag REPLACES the whole set, and an edit without --tag leaves the tags
// alone. Both directions are asserted against the daemon's stored note, so a
// CLI that merely printed the right thing cannot pass.
func TestCLI_Notes_Edit_TagsReplace(t *testing.T) {
	h := clitest.New(t, clitest.WithNotes(testNotes()...))

	res := h.Run("notes", "edit", "note-aaa", "--tag", "only")
	if res.ExitCode != 0 {
		t.Fatalf("exit=%d stderr=%q", res.ExitCode, res.Stderr)
	}
	if !strings.Contains(res.Stdout, "Updated note note-aaa") {
		t.Errorf("expected a confirmation line, got %q", res.Stdout)
	}
	stored := findNote(t, h, "note-aaa")
	if got := strings.Join(stored.GetTags(), ","); got != "only" {
		t.Errorf("tags after --tag only = %q, want the seeded set REPLACED by [only]", got)
	}
	if stored.GetBody() != "the flake is in the timer, not the assertion" {
		t.Errorf("a tag-only edit must not touch the body, got %q", stored.GetBody())
	}

	// A body-only edit must leave the (now single) tag set alone.
	res = h.Run("notes", "edit", "note-aaa", "--body", "rewritten body")
	if res.ExitCode != 0 {
		t.Fatalf("exit=%d stderr=%q", res.ExitCode, res.Stderr)
	}
	stored = findNote(t, h, "note-aaa")
	if stored.GetBody() != "rewritten body" {
		t.Errorf("body after --body = %q", stored.GetBody())
	}
	if got := strings.Join(stored.GetTags(), ","); got != "only" {
		t.Errorf("tags after a body-only edit = %q, want them untouched", got)
	}

	// The daemon must have seen the difference on the wire, not just in effect.
	calls := h.Daemon.UpdateNoteCalls()
	if len(calls) != 2 {
		t.Fatalf("expected 2 UpdateNote calls, got %d", len(calls))
	}
	if calls[0].Body != nil {
		t.Errorf("tag-only edit should leave body unset, got %q", calls[0].GetBody())
	}
	if calls[0].GetTags() == nil {
		t.Errorf("tag-only edit should SET the tag set")
	}
	if calls[1].Tags != nil {
		t.Errorf("body-only edit should leave the tag set unset, got %v", calls[1].GetTags().GetTags())
	}
}

func TestCLI_Notes_Edit_NothingToChange(t *testing.T) {
	h := clitest.New(t, clitest.WithNotes(testNotes()...))
	res := h.Run("notes", "edit", "note-aaa")

	if res.ExitCode == 0 {
		t.Fatalf("expected non-zero exit with neither --body nor --tag; stdout=%q", res.Stdout)
	}
	if !strings.Contains(res.Stderr, "--body") {
		t.Errorf("stderr should name the flags to pass, got %q", res.Stderr)
	}
	if n := len(h.Daemon.UpdateNoteCalls()); n != 0 {
		t.Errorf("no UpdateNote call should be made, got %d", n)
	}
}

// TestCLI_Notes_Remove_Idempotent mirrors the daemon's contract: removing a
// note that is already gone succeeds rather than erroring, so a retrying agent
// is not punished for finishing the job twice.
func TestCLI_Notes_Remove_Idempotent(t *testing.T) {
	h := clitest.New(t, clitest.WithNotes(testNotes()...))

	res := h.Run("notes", "rm", "note-aaa")
	if res.ExitCode != 0 {
		t.Fatalf("exit=%d stderr=%q", res.ExitCode, res.Stderr)
	}
	if !strings.Contains(res.Stdout, "Removed note note-aaa") {
		t.Errorf("unexpected output: %q", res.Stdout)
	}
	if remaining := h.Daemon.Notes(); len(remaining) != 1 || remaining[0].GetId() != "note-bbb" {
		t.Errorf("expected only note-bbb to remain, got %v", noteIDs(remaining))
	}

	again := h.Run("notes", "rm", "note-aaa")
	if again.ExitCode != 0 {
		t.Fatalf("second rm should be idempotent; exit=%d stderr=%q", again.ExitCode, again.Stderr)
	}
	calls := h.Daemon.DeleteNoteCalls()
	if len(calls) != 2 || calls[0] != "note-aaa" || calls[1] != "note-aaa" {
		t.Errorf("expected two deletes of note-aaa, got %v", calls)
	}
}

// TestCLI_Notes_RoundTrip drives the whole command group in the order an agent
// would: record a note, find it by tag, read it in full, change it, and remove
// it. Each step is asserted against the previous step's output, so a break
// anywhere in the client/daemon chain surfaces here.
func TestCLI_Notes_RoundTrip(t *testing.T) {
	h := clitest.New(t, clitest.WithEnv("BOSS_REPO_ID=rt-repo"))

	add := h.Run("notes", "add", testLongBody, "--tag", "Sweep", "--tag", " sweep ", "--tag", "perf", "--json")
	if add.ExitCode != 0 {
		t.Fatalf("add exit=%d stderr=%q", add.ExitCode, add.Stderr)
	}
	var created noteJSON
	if err := json.Unmarshal([]byte(add.Stdout), &created); err != nil {
		t.Fatalf("unmarshal add: %v\n%s", err, add.Stdout)
	}
	if created.ID == "" {
		t.Fatalf("created note has no id: %+v", created)
	}
	if created.RepoID != "rt-repo" {
		t.Errorf("repo id = %q, want rt-repo", created.RepoID)
	}
	// Tags come back normalised: trimmed, lowercased, de-duplicated, sorted.
	if got := strings.Join(created.Tags, ","); got != "perf,sweep" {
		t.Errorf("tags = %q, want perf,sweep", got)
	}

	found := h.Run("notes", "ls", "--tag", "sweep", "--json")
	if found.ExitCode != 0 {
		t.Fatalf("ls exit=%d stderr=%q", found.ExitCode, found.Stderr)
	}
	var listed []noteJSON
	if err := json.Unmarshal([]byte(found.Stdout), &listed); err != nil {
		t.Fatalf("unmarshal ls: %v\n%s", err, found.Stdout)
	}
	if len(listed) != 1 || listed[0].ID != created.ID {
		t.Fatalf("expected the new note to be found by tag, got %+v", listed)
	}

	show := h.Run("notes", "show", created.ID)
	if show.ExitCode != 0 {
		t.Fatalf("show exit=%d stderr=%q", show.ExitCode, show.Stderr)
	}
	if !strings.Contains(show.Stdout, testLongBody) {
		t.Errorf("show should print the full body, got %q", show.Stdout)
	}

	edit := h.Run("notes", "edit", created.ID, "--body", "resolved: the deadline masks the error", "--tag", "done", "--json")
	if edit.ExitCode != 0 {
		t.Fatalf("edit exit=%d stderr=%q", edit.ExitCode, edit.Stderr)
	}
	var edited noteJSON
	if err := json.Unmarshal([]byte(edit.Stdout), &edited); err != nil {
		t.Fatalf("unmarshal edit: %v\n%s", err, edit.Stdout)
	}
	if edited.Body != "resolved: the deadline masks the error" {
		t.Errorf("edited body = %q", edited.Body)
	}
	if got := strings.Join(edited.Tags, ","); got != "done" {
		t.Errorf("edited tags = %q, want the set replaced by [done]", got)
	}

	rm := h.Run("notes", "rm", created.ID)
	if rm.ExitCode != 0 {
		t.Fatalf("rm exit=%d stderr=%q", rm.ExitCode, rm.Stderr)
	}
	after := h.Run("notes", "ls", "--json")
	if after.ExitCode != 0 {
		t.Fatalf("ls exit=%d stderr=%q", after.ExitCode, after.Stderr)
	}
	if strings.TrimSpace(after.Stdout) != "[]" {
		t.Errorf("expected no notes after rm, got %q", after.Stdout)
	}
}

// findNote fetches one stored note from the mock daemon by id, failing the test
// when it is absent.
func findNote(t *testing.T, h *clitest.Harness, id string) *pb.Note {
	t.Helper()
	for _, n := range h.Daemon.Notes() {
		if n.GetId() == id {
			return n
		}
	}
	t.Fatalf("note %q not stored in the mock daemon", id)
	return nil
}

func noteIDs(notes []*pb.Note) []string {
	out := make([]string, len(notes))
	for i, n := range notes {
		out[i] = n.GetId()
	}
	return out
}
