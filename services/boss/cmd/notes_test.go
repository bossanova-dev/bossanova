package main

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/spf13/cobra"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/recurser/boss/internal/client"
	"github.com/recurser/boss/internal/views"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

// fakeNotesClient embeds client.BossClient so it satisfies the full interface
// with nil method bodies, overriding only the notes RPCs and ResolveContext.
// Any other call nil-panics, which is exactly the contract we want to pin: the
// notes commands must not reach for anything else.
type fakeNotesClient struct {
	client.BossClient

	created      *pb.CreateNoteRequest
	listed       *pb.ListNotesRequest
	updated      *pb.UpdateNoteRequest
	updatedRepo  string
	gotRepo      string
	gotID        string
	deletedRepo  string
	deletedID    string
	note         *pb.Note
	notes        []*pb.Note
	err          error
	resolved     *pb.ResolveContextResponse
	resolveErr   error
	resolveCalls int
}

func (f *fakeNotesClient) ResolveContext(_ context.Context, _ string) (*pb.ResolveContextResponse, error) {
	f.resolveCalls++
	return f.resolved, f.resolveErr
}

func (f *fakeNotesClient) CreateNote(_ context.Context, req *pb.CreateNoteRequest) (*pb.Note, error) {
	f.created = req
	return f.note, f.err
}

func (f *fakeNotesClient) GetNote(_ context.Context, repoID, id string) (*pb.Note, error) {
	f.gotRepo, f.gotID = repoID, id
	return f.note, f.err
}

func (f *fakeNotesClient) ListNotes(_ context.Context, req *pb.ListNotesRequest) ([]*pb.Note, error) {
	f.listed = req
	return f.notes, f.err
}

func (f *fakeNotesClient) UpdateNote(_ context.Context, repoID string, req *pb.UpdateNoteRequest) (*pb.Note, error) {
	f.updatedRepo, f.updated = repoID, req
	return f.note, f.err
}

func (f *fakeNotesClient) DeleteNote(_ context.Context, repoID, id string) error {
	f.deletedRepo, f.deletedID = repoID, id
	return f.err
}

// notesSubCmd builds the real notes command tree and returns one subcommand
// with its output captured. Building from notesCmd() means the tests exercise
// the actual flag definitions rather than a duplicate flag set that could drift.
func notesSubCmd(t *testing.T, name string) (*cobra.Command, *bytes.Buffer) {
	t.Helper()
	for _, sub := range notesCmd().Commands() {
		if sub.Name() == name {
			out := &bytes.Buffer{}
			sub.SetOut(out)
			sub.SetErr(out)
			sub.SetContext(context.Background())
			return sub, out
		}
	}
	t.Fatalf("notesCmd() has no %q subcommand", name)
	return nil, nil
}

// setFlag fails the test rather than silently ignoring an unknown flag, so a
// renamed flag surfaces here instead of as a mysteriously empty request field.
func setFlag(t *testing.T, cmd *cobra.Command, name, value string) {
	t.Helper()
	if err := cmd.Flags().Set(name, value); err != nil {
		t.Fatalf("set --%s=%q: %v", name, value, err)
	}
}

func TestNoteToJSONMapsEveryField(t *testing.T) {
	created := timestamppb.New(time.Unix(1_700_000_000, 0).UTC())
	got := noteToJSON(&pb.Note{
		Id:        "note-1",
		RepoId:    "repo-1",
		SessionId: "sess-1",
		ChatId:    "chat-1",
		Body:      "remember the milk",
		Tags:      []string{"a", "b"},
		CreatedAt: created,
	})
	want := noteJSON{
		ID:        "note-1",
		RepoID:    "repo-1",
		SessionID: "sess-1",
		ChatID:    "chat-1",
		Body:      "remember the milk",
		Tags:      []string{"a", "b"},
		CreatedAt: created.AsTime().UTC().Format(time.RFC3339),
		// UpdatedAt is deliberately nil above: a nil timestamp must render as
		// the empty string, never as a zero-time literal.
		UpdatedAt: "",
	}
	if got.ID != want.ID || got.RepoID != want.RepoID || got.SessionID != want.SessionID ||
		got.ChatID != want.ChatID || got.Body != want.Body || got.CreatedAt != want.CreatedAt ||
		got.UpdatedAt != want.UpdatedAt {
		t.Fatalf("noteToJSON() = %+v, want %+v", got, want)
	}
	if strings.Join(got.Tags, ",") != "a,b" {
		t.Fatalf("noteToJSON() tags = %v, want [a b]", got.Tags)
	}
}

// TestNoteJSONWireKeysArePinned pins the raw wire keys rather than the Go
// struct. Every other --json test unmarshals into noteJSON itself, so a renamed
// `json:"..."` tag would be symmetric and invisible; this decodes into a generic
// map so the documented machine contract cannot drift silently.
func TestNoteJSONWireKeysArePinned(t *testing.T) {
	stubEnv(t, nil)
	fake := &fakeNotesClient{note: &pb.Note{Id: "note-1", RepoId: "repo-1", Body: "body"}}
	cmd, out := notesSubCmd(t, "show")
	setFlag(t, cmd, "json", "true")

	if err := runNotesShow(cmd, fake, "note-1"); err != nil {
		t.Fatalf("runNotesShow() error = %v", err)
	}
	var raw map[string]any
	if err := json.Unmarshal(out.Bytes(), &raw); err != nil {
		t.Fatalf("show --json emitted invalid JSON %q: %v", out.String(), err)
	}
	want := []string{"id", "repo_id", "session_id", "chat_id", "body", "tags", "created_at", "updated_at"}
	for _, key := range want {
		if _, ok := raw[key]; !ok {
			t.Errorf("--json output is missing the contract key %q: %v", key, raw)
		}
	}
	if len(raw) != len(want) {
		t.Errorf("--json emitted %d keys %v, want exactly %v", len(raw), raw, want)
	}
	// An untagged note must still emit a list. `null` would force every
	// consumer to guard before iterating, and the key-presence check above
	// passes either way — so this is the assertion that actually pins it.
	if tags, ok := raw["tags"].([]any); !ok || len(tags) != 0 {
		t.Errorf("tags = %#v, want an empty list — an untagged note must not emit null", raw["tags"])
	}
}

func TestNotesAddSendsBodyRepoAndTags(t *testing.T) {
	stubEnv(t, nil)
	fake := &fakeNotesClient{note: &pb.Note{Id: "note-1"}}
	cmd, out := notesSubCmd(t, "add")
	setFlag(t, cmd, "repo", "repo-1")
	setFlag(t, cmd, "tag", "ops")

	if err := runNotesAdd(cmd, fake, "remember the milk"); err != nil {
		t.Fatalf("runNotesAdd() error = %v", err)
	}
	if fake.created == nil {
		t.Fatal("runNotesAdd() sent no CreateNoteRequest")
	}
	if fake.created.GetRepoId() != "repo-1" {
		t.Errorf("repo_id = %q, want repo-1", fake.created.GetRepoId())
	}
	if fake.created.GetBody() != "remember the milk" {
		t.Errorf("body = %q, want the note body", fake.created.GetBody())
	}
	if strings.Join(fake.created.GetTags(), ",") != "ops" {
		t.Errorf("tags = %v, want [ops]", fake.created.GetTags())
	}
	if !strings.Contains(out.String(), "note-1") {
		t.Errorf("confirmation %q does not name the note id", out.String())
	}
}

func TestNotesAddPassesIdempotencyKey(t *testing.T) {
	stubEnv(t, nil)
	fake := &fakeNotesClient{note: &pb.Note{Id: "note-1"}}
	cmd, _ := notesSubCmd(t, "add")
	setFlag(t, cmd, "repo", "repo-1")
	setFlag(t, cmd, "idempotency-key", "Release review: v2:YS5nb0Ax")

	if err := runNotesAdd(cmd, fake, "body"); err != nil {
		t.Fatalf("runNotesAdd() error = %v", err)
	}
	if got := fake.created.GetIdempotencyKey(); got != "Release review: v2:YS5nb0Ax" {
		t.Errorf("idempotency_key = %q", got)
	}
}

func TestNotesAddRepeatedTagsAccumulate(t *testing.T) {
	stubEnv(t, nil)
	fake := &fakeNotesClient{note: &pb.Note{Id: "note-1"}}
	cmd, _ := notesSubCmd(t, "add")
	setFlag(t, cmd, "repo", "repo-1")
	setFlag(t, cmd, "tag", "a")
	setFlag(t, cmd, "tag", "b")

	if err := runNotesAdd(cmd, fake, "body"); err != nil {
		t.Fatalf("runNotesAdd() error = %v", err)
	}
	if got := strings.Join(fake.created.GetTags(), ","); got != "a,b" {
		t.Fatalf("tags = %v, want [a b] — --tag must be repeatable, not comma-joined", fake.created.GetTags())
	}
}

func TestNotesAddWithoutResolvableRepoNamesTheFlag(t *testing.T) {
	stubEnv(t, nil)
	// No flag, no ambient env, and a context that resolves nothing.
	fake := &fakeNotesClient{resolved: &pb.ResolveContextResponse{}}
	cmd, _ := notesSubCmd(t, "add")

	err := runNotesAdd(cmd, fake, "body")
	if err == nil {
		t.Fatal("runNotesAdd() with no resolvable repo succeeded, want an actionable error")
	}
	if !strings.Contains(err.Error(), "--repo") {
		t.Fatalf("error %q does not name --repo", err)
	}
	if fake.created != nil {
		t.Error("runNotesAdd() sent a request despite having no repo")
	}
}

func TestNotesAddResolvesRepoSessionAndChatFromContext(t *testing.T) {
	stubEnv(t, map[string]string{"BOSS_AGENT_SESSION_ID": "chat-1"})
	fake := &fakeNotesClient{
		note: &pb.Note{Id: "note-1"},
		resolved: &pb.ResolveContextResponse{
			Repo:    &pb.Repo{Id: "repo-1"},
			Session: &pb.Session{Id: "sess-1"},
		},
	}
	cmd, _ := notesSubCmd(t, "add")

	if err := runNotesAdd(cmd, fake, "body"); err != nil {
		t.Fatalf("runNotesAdd() error = %v", err)
	}
	if fake.created.GetRepoId() != "repo-1" {
		t.Errorf("repo_id = %q, want repo-1 from the resolved context", fake.created.GetRepoId())
	}
	if fake.created.GetSessionId() != "sess-1" {
		t.Errorf("session_id = %q, want sess-1 from the resolved context", fake.created.GetSessionId())
	}
	if fake.created.GetChatId() != "chat-1" {
		t.Errorf("chat_id = %q, want chat-1 from the ambient chat", fake.created.GetChatId())
	}
	if fake.resolveCalls > 1 {
		t.Errorf("ResolveContext called %d times, want at most 1 per invocation", fake.resolveCalls)
	}
}

// TestNotesAddLeavesSessionAndChatUnsetWithoutContext is the negative half of
// the optional-pointer discipline: with nothing to stamp, the pointers must stay
// nil rather than carrying an empty string, which the daemon would store as a
// present-but-blank provenance.
func TestNotesAddLeavesSessionAndChatUnsetWithoutContext(t *testing.T) {
	stubEnv(t, nil)
	fake := &fakeNotesClient{
		note:     &pb.Note{Id: "note-1"},
		resolved: &pb.ResolveContextResponse{Repo: &pb.Repo{Id: "repo-1"}},
	}
	cmd, _ := notesSubCmd(t, "add")

	if err := runNotesAdd(cmd, fake, "body"); err != nil {
		t.Fatalf("runNotesAdd() error = %v", err)
	}
	if fake.created.SessionId != nil {
		t.Errorf("session_id = %q, want nil when nothing resolves a session", fake.created.GetSessionId())
	}
	if fake.created.ChatId != nil {
		t.Errorf("chat_id = %q, want nil when nothing resolves a chat", fake.created.GetChatId())
	}
}

func TestNotesAddPrefersFlagsOverEnvAndContext(t *testing.T) {
	stubEnv(t, map[string]string{
		"BOSS_REPO_ID":          "repo-env",
		"BOSS_SESSION_ID":       "sess-env",
		"BOSS_AGENT_SESSION_ID": "chat-env",
	})
	fake := &fakeNotesClient{
		note:     &pb.Note{Id: "note-1"},
		resolved: &pb.ResolveContextResponse{Repo: &pb.Repo{Id: "repo-ctx"}},
	}
	cmd, _ := notesSubCmd(t, "add")
	setFlag(t, cmd, "repo", "repo-flag")
	setFlag(t, cmd, "session", "sess-flag")
	setFlag(t, cmd, "chat", "chat-flag")

	if err := runNotesAdd(cmd, fake, "body"); err != nil {
		t.Fatalf("runNotesAdd() error = %v", err)
	}
	if fake.created.GetRepoId() != "repo-flag" ||
		fake.created.GetSessionId() != "sess-flag" ||
		fake.created.GetChatId() != "chat-flag" {
		t.Fatalf("flags lost to env/context: %+v", fake.created)
	}
}

func TestNotesAddFallsBackToAmbientEnv(t *testing.T) {
	stubEnv(t, map[string]string{
		"BOSS_REPO_ID":          "repo-env",
		"BOSS_SESSION_ID":       "sess-env",
		"BOSS_AGENT_SESSION_ID": "chat-env",
	})
	// A resolvable context is present but must lose to the ambient env.
	fake := &fakeNotesClient{
		note:     &pb.Note{Id: "note-1"},
		resolved: &pb.ResolveContextResponse{Repo: &pb.Repo{Id: "repo-ctx"}, Session: &pb.Session{Id: "sess-ctx"}},
	}
	cmd, _ := notesSubCmd(t, "add")

	if err := runNotesAdd(cmd, fake, "body"); err != nil {
		t.Fatalf("runNotesAdd() error = %v", err)
	}
	if fake.created.GetRepoId() != "repo-env" ||
		fake.created.GetSessionId() != "sess-env" ||
		fake.created.GetChatId() != "chat-env" {
		t.Fatalf("env lost to context: %+v", fake.created)
	}
}

// TestNotesAddSurvivesResolveContextError pins that a remote client — which
// cannot resolve local context and returns an error — is treated as "no
// context", never as a fatal error.
func TestNotesAddSurvivesResolveContextError(t *testing.T) {
	stubEnv(t, nil)
	fake := &fakeNotesClient{note: &pb.Note{Id: "note-1"}, resolveErr: context.DeadlineExceeded}
	cmd, _ := notesSubCmd(t, "add")
	setFlag(t, cmd, "repo", "repo-1")

	if err := runNotesAdd(cmd, fake, "body"); err != nil {
		t.Fatalf("runNotesAdd() error = %v, want the ResolveContext failure to be non-fatal", err)
	}
	if fake.created.GetRepoId() != "repo-1" {
		t.Errorf("repo_id = %q, want repo-1", fake.created.GetRepoId())
	}
}

func TestNotesAddJSONEmitsTheStableSchema(t *testing.T) {
	stubEnv(t, nil)
	fake := &fakeNotesClient{note: &pb.Note{Id: "note-1", RepoId: "repo-1", Body: "body", Tags: []string{"a"}}}
	cmd, out := notesSubCmd(t, "add")
	setFlag(t, cmd, "repo", "repo-1")
	setFlag(t, cmd, "json", "true")

	if err := runNotesAdd(cmd, fake, "body"); err != nil {
		t.Fatalf("runNotesAdd() error = %v", err)
	}
	var got noteJSON
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatalf("add --json emitted invalid JSON %q: %v", out.String(), err)
	}
	if got.ID != "note-1" || got.RepoID != "repo-1" || got.Body != "body" {
		t.Fatalf("add --json = %+v, want the created note", got)
	}
}

// longBody is long enough that the table preview must truncate it. The tail
// marker is what the truncation assertion greps for: if it reaches the table,
// the body was not truncated.
const longBody = "the quick brown fox jumps over the lazy dog and keeps on running well past any sane column width TAILMARKER"

func TestNotesListRendersTruncatedBodies(t *testing.T) {
	stubEnv(t, nil)
	created := timestamppb.New(time.Unix(1_700_000_000, 0).UTC())
	fake := &fakeNotesClient{notes: []*pb.Note{
		{Id: "note-1", Body: longBody, Tags: []string{"ops"}, CreatedAt: created},
	}}
	cmd, out := notesSubCmd(t, "ls")

	if err := runNotesList(cmd, fake); err != nil {
		t.Fatalf("runNotesList() error = %v", err)
	}
	got := out.String()
	for _, want := range []string{"ID", "CREATED", "TAGS", "BODY", "note-1", "ops"} {
		if !strings.Contains(got, want) {
			t.Errorf("table %q missing %q", got, want)
		}
	}
	if strings.Contains(got, "TAILMARKER") {
		t.Errorf("table rendered the full body; it must be truncated:\n%s", got)
	}
	if !strings.Contains(got, "the quick brown fox") {
		t.Errorf("table dropped the body head entirely:\n%s", got)
	}
}

// TestNotesListShowsRepoOnlyWhenUnscoped pins the attribution rule for the
// table: a cross-repo listing (the `--repo ""` recipe the reference documents)
// interleaves repos by creation time, so each row must name its repo; a
// repo-scoped listing must not spend a column repeating one value.
func TestNotesListShowsRepoOnlyWhenUnscoped(t *testing.T) {
	notes := []*pb.Note{
		{Id: "note-1", RepoId: "repo-alpha", Body: "alpha note"},
		{Id: "note-2", RepoId: "repo-beta", Body: "beta note"},
	}

	stubEnv(t, nil)
	unscoped, unscopedOut := notesSubCmd(t, "ls")
	setFlag(t, unscoped, "repo", "")
	if err := runNotesList(unscoped, &fakeNotesClient{notes: notes}); err != nil {
		t.Fatalf("runNotesList() error = %v", err)
	}
	for _, want := range []string{"REPO", "repo-alpha", "repo-beta"} {
		if !strings.Contains(unscopedOut.String(), want) {
			t.Errorf("cross-repo table is missing %q — rows are unattributable:\n%s", want, unscopedOut.String())
		}
	}

	scoped, scopedOut := notesSubCmd(t, "ls")
	setFlag(t, scoped, "repo", "repo-alpha")
	if err := runNotesList(scoped, &fakeNotesClient{notes: notes[:1]}); err != nil {
		t.Fatalf("runNotesList() error = %v", err)
	}
	if strings.Contains(scopedOut.String(), "REPO") {
		t.Errorf("scoped table spends a column on the repo it was filtered to:\n%s", scopedOut.String())
	}
}

// TestNotesListFlattensMultiLineBodies pins that a multi-line body cannot break
// the table's row-per-note layout.
func TestNotesListFlattensMultiLineBodies(t *testing.T) {
	stubEnv(t, nil)
	fake := &fakeNotesClient{notes: []*pb.Note{{Id: "note-1", Body: "first\nsecond"}}}
	cmd, out := notesSubCmd(t, "ls")

	if err := runNotesList(cmd, fake); err != nil {
		t.Fatalf("runNotesList() error = %v", err)
	}
	// Flattened onto one line, both words survive on the note's own row. Left
	// unflattened the renderer silently drops everything after the newline, so
	// this also pins that no content is lost.
	if !strings.Contains(out.String(), "first second") {
		t.Fatalf("multi-line body was not flattened onto the note's row:\n%s", out.String())
	}
	if strings.Contains(noteBodyPreview("first\nsecond"), "\n") {
		t.Error("noteBodyPreview kept a newline; it would split one note across table rows")
	}
}

// TestNoteBodyPreviewHandlesMultiByteBodies pins that the preview truncates on
// rune boundaries, not byte offsets. Forty é characters are eighty bytes but
// only forty display cells — comfortably inside the column — so a byte-based cut
// both truncates a body that fits and severs a rune, emitting invalid UTF-8.
func TestNoteBodyPreviewHandlesMultiByteBodies(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
	}{
		{name: "fits the column", body: strings.Repeat("é", 40)},
		{name: "overflows the column", body: strings.Repeat("é", 200)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := noteBodyPreview(tc.body)
			if !utf8.ValidString(got) {
				t.Errorf("noteBodyPreview(%d×é) = %q, which is not valid UTF-8", utf8.RuneCountInString(tc.body), got)
			}
			// noteBodyPreview truncates by RUNE, not by display cell, so this
			// bound only holds for the single-cell runes used here. It pins
			// rune-safe truncation, not a display-width guarantee: a body of
			// wide (CJK/emoji) runes still previews to noteBodyPreviewWidth
			// runes and can exceed that many cells, which the table then clips.
			if w := views.MaxColWidth("", []string{got}, 0); w > noteBodyPreviewWidth {
				t.Errorf("noteBodyPreview() width = %d, want <= %d", w, noteBodyPreviewWidth)
			}
		})
	}
	// The short case must survive untouched: 40 cells fit in a 60-cell column.
	if got := noteBodyPreview(strings.Repeat("é", 40)); got != strings.Repeat("é", 40) {
		t.Errorf("noteBodyPreview() truncated a body that fits the column: %q", got)
	}
}

func TestNotesShowRendersTheFullBody(t *testing.T) {
	stubEnv(t, nil)
	created := timestamppb.New(time.Unix(1_700_000_000, 0).UTC())
	fake := &fakeNotesClient{note: &pb.Note{
		Id: "note-1", RepoId: "repo-1", SessionId: "sess-1", ChatId: "chat-1",
		Body: longBody, Tags: []string{"ops", "infra"}, CreatedAt: created,
	}}
	cmd, out := notesSubCmd(t, "show")
	setFlag(t, cmd, "repo", "repo-1")

	if err := runNotesShow(cmd, fake, "note-1"); err != nil {
		t.Fatalf("runNotesShow() error = %v", err)
	}
	got := out.String()
	if !strings.Contains(got, longBody) {
		t.Errorf("show truncated the body; it must render in full:\n%s", got)
	}
	for _, want := range []string{"note-1", "repo-1", "sess-1", "chat-1", "ops, infra", "2023-11-14"} {
		if !strings.Contains(got, want) {
			t.Errorf("show output %q missing %q", got, want)
		}
	}
	if fake.gotRepo != "repo-1" || fake.gotID != "note-1" {
		t.Errorf("GetNote(%q, %q), want (repo-1, note-1)", fake.gotRepo, fake.gotID)
	}
}

// TestNotesShowWithoutRepoIsNotAnError pins that show/edit/rm resolve by note
// id: an unresolvable repo is only a missing remote routing key, never a
// failure.
func TestNotesShowWithoutRepoIsNotAnError(t *testing.T) {
	stubEnv(t, nil)
	fake := &fakeNotesClient{note: &pb.Note{Id: "note-1"}, resolved: &pb.ResolveContextResponse{}}
	cmd, _ := notesSubCmd(t, "show")

	if err := runNotesShow(cmd, fake, "note-1"); err != nil {
		t.Fatalf("runNotesShow() error = %v, want an unresolvable repo to be harmless", err)
	}
	if fake.gotRepo != "" {
		t.Errorf("GetNote repo = %q, want the empty routing key", fake.gotRepo)
	}
}

func TestNotesListAppliesOnlyTheFiltersGiven(t *testing.T) {
	stubEnv(t, nil)
	fake := &fakeNotesClient{resolved: &pb.ResolveContextResponse{}}
	cmd, out := notesSubCmd(t, "ls")

	if err := runNotesList(cmd, fake); err != nil {
		t.Fatalf("runNotesList() error = %v", err)
	}
	if fake.listed == nil {
		t.Fatal("runNotesList() sent no ListNotesRequest")
	}
	if fake.listed.RepoId != nil {
		t.Errorf("repo_id = %q, want unset (every repo) when nothing resolves", fake.listed.GetRepoId())
	}
	if fake.listed.SessionId != nil || fake.listed.ChatId != nil || fake.listed.Search != nil {
		t.Errorf("unrequested filters were set: %+v", fake.listed)
	}
	// The chat filter has no flag at all — the documented `ls` surface is
	// [--repo] [--tag] [--session] [--search] [--limit] [--json] — so it can
	// never be set from the command line.
	if flag := cmd.Flags().Lookup("chat"); flag != nil {
		t.Error("ls defines a --chat flag, which is not part of the command surface")
	}
	if len(fake.listed.GetTags()) != 0 || fake.listed.GetLimit() != 0 {
		t.Errorf("unrequested tags/limit were set: %+v", fake.listed)
	}
	if !strings.Contains(out.String(), "No notes.") {
		t.Errorf("empty result printed %q, want the empty-state line", out.String())
	}
}

func TestNotesListSendsEveryFilterGiven(t *testing.T) {
	stubEnv(t, nil)
	fake := &fakeNotesClient{}
	cmd, _ := notesSubCmd(t, "ls")
	setFlag(t, cmd, "repo", "repo-1")
	setFlag(t, cmd, "session", "sess-1")
	setFlag(t, cmd, "tag", "a")
	setFlag(t, cmd, "tag", "b")
	setFlag(t, cmd, "search", "milk")
	setFlag(t, cmd, "limit", "5")

	if err := runNotesList(cmd, fake); err != nil {
		t.Fatalf("runNotesList() error = %v", err)
	}
	if fake.listed.GetRepoId() != "repo-1" || fake.listed.GetSessionId() != "sess-1" ||
		fake.listed.GetSearch() != "milk" || fake.listed.GetLimit() != 5 {
		t.Fatalf("filters lost: %+v", fake.listed)
	}
	if strings.Join(fake.listed.GetTags(), ",") != "a,b" {
		t.Fatalf("tags = %v, want [a b]", fake.listed.GetTags())
	}
}

// TestNotesListDefaultsRepoFromContext is the ergonomic payoff: inside a repo,
// `boss notes ls` shows that repo's notes without naming it.
func TestNotesListDefaultsRepoFromContext(t *testing.T) {
	stubEnv(t, nil)
	fake := &fakeNotesClient{resolved: &pb.ResolveContextResponse{Repo: &pb.Repo{Id: "repo-ctx"}}}
	cmd, _ := notesSubCmd(t, "ls")

	if err := runNotesList(cmd, fake); err != nil {
		t.Fatalf("runNotesList() error = %v", err)
	}
	if fake.listed.GetRepoId() != "repo-ctx" {
		t.Fatalf("repo_id = %q, want repo-ctx from the working directory", fake.listed.GetRepoId())
	}
}

// TestNotesListDefaultsRepoFromAmbientEnv pins the middle rung of the `ls`
// precedence chain: a boss-managed agent pane exports BOSS_REPO_ID, so the
// listing an agent sees is scoped even when the daemon resolves nothing for the
// working directory.
func TestNotesListDefaultsRepoFromAmbientEnv(t *testing.T) {
	stubEnv(t, map[string]string{"BOSS_REPO_ID": "repo-env"})
	fake := &fakeNotesClient{resolved: &pb.ResolveContextResponse{}}
	cmd, _ := notesSubCmd(t, "ls")

	if err := runNotesList(cmd, fake); err != nil {
		t.Fatalf("runNotesList() error = %v", err)
	}
	if fake.listed.GetRepoId() != "repo-env" {
		t.Fatalf("repo_id = %q, want repo-env from $BOSS_REPO_ID", fake.listed.GetRepoId())
	}
}

// TestNotesListExplicitEmptyRepoListsEveryRepo pins the documented cross-repo
// escape hatch. An agent's pane always exports BOSS_REPO_ID, so `cd` out of the
// repo cannot widen the listing — only an explicit `--repo ""` can. Without
// this, the SKILL.md claim that you can list across every repo would be false
// in exactly the context that reads it.
func TestNotesListExplicitEmptyRepoListsEveryRepo(t *testing.T) {
	stubEnv(t, map[string]string{"BOSS_REPO_ID": "repo-env"})
	fake := &fakeNotesClient{resolved: &pb.ResolveContextResponse{Repo: &pb.Repo{Id: "repo-ctx"}}}
	cmd, _ := notesSubCmd(t, "ls")
	setFlag(t, cmd, "repo", "")

	if err := runNotesList(cmd, fake); err != nil {
		t.Fatalf("runNotesList() error = %v", err)
	}
	if fake.listed.RepoId != nil {
		t.Fatalf(`repo_id = %q, want unset — an explicit --repo "" means every repo`, fake.listed.GetRepoId())
	}
}

// TestNotesAddDropsBlankTags guards the write path's blank-dropping: sending a
// tag that normalises away would make the daemon reject the whole create, so a
// stray `--tag ""` must vanish rather than fail the note.
func TestNotesAddDropsBlankTags(t *testing.T) {
	stubEnv(t, nil)
	fake := &fakeNotesClient{note: &pb.Note{Id: "note-1"}}
	cmd, _ := notesSubCmd(t, "add")
	setFlag(t, cmd, "repo", "repo-1")
	setFlag(t, cmd, "tag", "  ")
	setFlag(t, cmd, "tag", "")

	if err := runNotesAdd(cmd, fake, "body"); err != nil {
		t.Fatalf("runNotesAdd() error = %v", err)
	}
	if len(fake.created.GetTags()) != 0 {
		t.Fatalf("tags = %v, want none — blank tags must not reach the daemon", fake.created.GetTags())
	}
}

func TestNotesListJSONEmitsAnArrayEvenWhenEmpty(t *testing.T) {
	stubEnv(t, nil)
	fake := &fakeNotesClient{resolved: &pb.ResolveContextResponse{}}
	cmd, out := notesSubCmd(t, "ls")
	setFlag(t, cmd, "json", "true")

	if err := runNotesList(cmd, fake); err != nil {
		t.Fatalf("runNotesList() error = %v", err)
	}
	if got := strings.TrimSpace(out.String()); got != "[]" {
		t.Fatalf("ls --json with no results = %q, want []", got)
	}
}

func TestNotesListJSONCarriesEveryNote(t *testing.T) {
	stubEnv(t, nil)
	fake := &fakeNotesClient{notes: []*pb.Note{
		{Id: "note-1", RepoId: "repo-1", Body: longBody, Tags: []string{"ops"}},
		{Id: "note-2", RepoId: "repo-1", Body: "second"},
	}}
	cmd, out := notesSubCmd(t, "ls")
	setFlag(t, cmd, "json", "true")

	if err := runNotesList(cmd, fake); err != nil {
		t.Fatalf("runNotesList() error = %v", err)
	}
	var got []noteJSON
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatalf("ls --json emitted invalid JSON %q: %v", out.String(), err)
	}
	if len(got) != 2 || got[0].ID != "note-1" || got[1].ID != "note-2" {
		t.Fatalf("ls --json = %+v, want both notes in order", got)
	}
	// JSON is the machine surface: the body is never truncated there.
	if got[0].Body != longBody {
		t.Fatalf("ls --json truncated the body: %q", got[0].Body)
	}
}

func TestNotesShowJSONEmitsTheStableSchema(t *testing.T) {
	stubEnv(t, nil)
	fake := &fakeNotesClient{note: &pb.Note{Id: "note-1", RepoId: "repo-1", Body: "body", Tags: []string{"a"}}}
	cmd, out := notesSubCmd(t, "show")
	setFlag(t, cmd, "json", "true")

	if err := runNotesShow(cmd, fake, "note-1"); err != nil {
		t.Fatalf("runNotesShow() error = %v", err)
	}
	var got noteJSON
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatalf("show --json emitted invalid JSON %q: %v", out.String(), err)
	}
	if got.ID != "note-1" || got.Body != "body" || strings.Join(got.Tags, ",") != "a" {
		t.Fatalf("show --json = %+v, want the fetched note", got)
	}
}

// TestNotesEditLeavesTagsUntouchedWithoutTheFlag and its sibling below are the
// surprising-semantics regression tests: --tag REPLACES the whole set, and its
// absence must leave the stored tags alone rather than clearing them.
func TestNotesEditLeavesTagsUntouchedWithoutTheFlag(t *testing.T) {
	stubEnv(t, nil)
	fake := &fakeNotesClient{note: &pb.Note{Id: "note-1"}}
	cmd, out := notesSubCmd(t, "edit")
	setFlag(t, cmd, "body", "new body")

	if err := runNotesEdit(cmd, fake, "note-1"); err != nil {
		t.Fatalf("runNotesEdit() error = %v", err)
	}
	if fake.updated.Tags != nil {
		t.Fatalf("tags = %+v, want nil (untouched) when --tag was not passed", fake.updated.Tags)
	}
	if fake.updated.GetBody() != "new body" {
		t.Errorf("body = %q, want the new body", fake.updated.GetBody())
	}
	if !strings.Contains(out.String(), "note-1") {
		t.Errorf("confirmation %q does not name the note id", out.String())
	}
}

func TestNotesEditReplacesTheWholeTagSet(t *testing.T) {
	stubEnv(t, nil)
	fake := &fakeNotesClient{note: &pb.Note{Id: "note-1"}}
	cmd, _ := notesSubCmd(t, "edit")
	setFlag(t, cmd, "tag", "x")
	setFlag(t, cmd, "tag", "y")

	if err := runNotesEdit(cmd, fake, "note-1"); err != nil {
		t.Fatalf("runNotesEdit() error = %v", err)
	}
	if fake.updated.Tags == nil {
		t.Fatal("tags = nil, want the replacement set when --tag was passed")
	}
	if got := strings.Join(fake.updated.GetTags().GetTags(), ","); got != "x,y" {
		t.Fatalf("tags = %v, want exactly [x y]", fake.updated.GetTags().GetTags())
	}
	if fake.updated.Body != nil {
		t.Errorf("body = %q, want nil (untouched) when --body was not passed", fake.updated.GetBody())
	}
}

// TestNotesEditEmptyTagFlagClearsTheSet pins the one way to clear tags:
// --tag "" is Changed, so it sends an empty replacement set rather than nil.
func TestNotesEditEmptyTagFlagClearsTheSet(t *testing.T) {
	stubEnv(t, nil)
	fake := &fakeNotesClient{note: &pb.Note{Id: "note-1"}}
	cmd, _ := notesSubCmd(t, "edit")
	setFlag(t, cmd, "tag", "")

	if err := runNotesEdit(cmd, fake, "note-1"); err != nil {
		t.Fatalf("runNotesEdit() error = %v", err)
	}
	if fake.updated.Tags == nil {
		t.Fatal("tags = nil, want a set-but-empty replacement that clears the tags")
	}
}

func TestNotesEditRequiresBodyOrTag(t *testing.T) {
	stubEnv(t, nil)
	fake := &fakeNotesClient{}
	cmd, _ := notesSubCmd(t, "edit")

	err := runNotesEdit(cmd, fake, "note-1")
	if err == nil {
		t.Fatal("runNotesEdit() with no changes succeeded, want an error instead of a no-op RPC")
	}
	if !strings.Contains(err.Error(), "--body") || !strings.Contains(err.Error(), "--tag") {
		t.Errorf("error %q does not name --body and --tag", err)
	}
	if fake.updated != nil {
		t.Error("runNotesEdit() issued a no-op RPC")
	}
}

func TestNotesEditJSONEmitsTheUpdatedNote(t *testing.T) {
	stubEnv(t, nil)
	fake := &fakeNotesClient{note: &pb.Note{Id: "note-1", Body: "new body"}}
	cmd, out := notesSubCmd(t, "edit")
	setFlag(t, cmd, "body", "new body")
	setFlag(t, cmd, "json", "true")

	if err := runNotesEdit(cmd, fake, "note-1"); err != nil {
		t.Fatalf("runNotesEdit() error = %v", err)
	}
	var got noteJSON
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatalf("edit --json emitted invalid JSON %q: %v", out.String(), err)
	}
	if got.ID != "note-1" || got.Body != "new body" {
		t.Fatalf("edit --json = %+v, want the updated note", got)
	}
}

func TestNotesRemoveNamesTheNote(t *testing.T) {
	stubEnv(t, map[string]string{"BOSS_REPO_ID": "repo-env"})
	fake := &fakeNotesClient{}
	cmd, out := notesSubCmd(t, "rm")

	if err := runNotesRemove(cmd, fake, "note-1"); err != nil {
		t.Fatalf("runNotesRemove() error = %v", err)
	}
	if fake.deletedID != "note-1" {
		t.Errorf("DeleteNote id = %q, want note-1", fake.deletedID)
	}
	if fake.deletedRepo != "repo-env" {
		t.Errorf("DeleteNote repo = %q, want the resolved routing key repo-env", fake.deletedRepo)
	}
	if !strings.Contains(out.String(), "note-1") {
		t.Errorf("confirmation %q does not name the note id", out.String())
	}
}

// TestNotesEditRoutesRepoID pins that the repo id reaches UpdateNote as the
// remote routing key.
func TestNotesEditRoutesRepoID(t *testing.T) {
	stubEnv(t, nil)
	fake := &fakeNotesClient{note: &pb.Note{Id: "note-1"}}
	cmd, _ := notesSubCmd(t, "edit")
	setFlag(t, cmd, "repo", "repo-1")
	setFlag(t, cmd, "body", "b")

	if err := runNotesEdit(cmd, fake, "note-1"); err != nil {
		t.Fatalf("runNotesEdit() error = %v", err)
	}
	if fake.updatedRepo != "repo-1" {
		t.Errorf("UpdateNote repo = %q, want repo-1", fake.updatedRepo)
	}
	if fake.updated.GetId() != "note-1" {
		t.Errorf("UpdateNote id = %q, want note-1", fake.updated.GetId())
	}
}
