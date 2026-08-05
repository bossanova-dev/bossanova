package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/recurser/bossalib/models"
)

func newTestNoteParams(repoID string) CreateNoteParams {
	return CreateNoteParams{
		RepoID: repoID,
		Body:   "learned something",
		Tags:   []string{"tech-debt"},
	}
}

func strPtr(s string) *string { return &s }

// mustCreateNote creates a note or fails the test.
func mustCreateNote(t *testing.T, store *SQLiteNoteStore, params CreateNoteParams) *models.Note {
	t.Helper()
	note, err := store.Create(context.Background(), params)
	if err != nil {
		t.Fatalf("create note: %v", err)
	}
	return note
}

// noteIDs projects a note list to its ids, for order assertions.
func noteIDs(notes []*models.Note) []string {
	out := make([]string, 0, len(notes))
	for _, n := range notes {
		out = append(out, n.ID)
	}
	return out
}

func TestNoteStore_CreateGetRoundTrip(t *testing.T) {
	db := setupTestDB(t)
	store := NewNoteStore(db)
	ctx := context.Background()

	params := CreateNoteParams{
		RepoID:    "repo-1",
		SessionID: strPtr("session-1"),
		ChatID:    strPtr("chat-1"),
		Body:      "  a body with surrounding space preserved  ",
		Tags:      []string{"alpha", "beta"},
	}
	created := mustCreateNote(t, store, params)

	if created.ID == "" {
		t.Fatal("created note has empty id")
	}
	if created.RepoID != "repo-1" {
		t.Errorf("repo id = %q, want repo-1", created.RepoID)
	}
	if created.SessionID == nil || *created.SessionID != "session-1" {
		t.Errorf("session id = %v, want session-1", created.SessionID)
	}
	if created.ChatID == nil || *created.ChatID != "chat-1" {
		t.Errorf("chat id = %v, want chat-1", created.ChatID)
	}
	if created.Body != params.Body {
		t.Errorf("body = %q, want it stored verbatim (%q)", created.Body, params.Body)
	}
	if !reflect.DeepEqual(created.Tags, []string{"alpha", "beta"}) {
		t.Errorf("tags = %v, want [alpha beta]", created.Tags)
	}
	if created.CreatedAt.IsZero() || created.UpdatedAt.IsZero() {
		t.Errorf("timestamps not set: created=%v updated=%v", created.CreatedAt, created.UpdatedAt)
	}

	got, err := store.Get(ctx, created.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if !reflect.DeepEqual(got, created) {
		t.Errorf("get round-trip mismatch:\n got %+v\nwant %+v", got, created)
	}
}

func TestNoteStore_CreateIsIdempotentForRepoScopedKey(t *testing.T) {

	store := NewNoteStore(setupTestDB(t))
	key := "Release review: v2:YS5nb0Ax"
	first := mustCreateNote(t, store, CreateNoteParams{
		RepoID:         "repo-1",
		Body:           "first body",
		Tags:           []string{"improvement"},
		IdempotencyKey: &key,
	})
	second := mustCreateNote(t, store, CreateNoteParams{
		RepoID:         "repo-1",
		Body:           "later retry must not overwrite the first body",
		Tags:           []string{"different"},
		IdempotencyKey: &key,
	})
	if second.ID != first.ID {
		t.Fatalf("idempotent create id = %q, want existing note %q", second.ID, first.ID)
	}
	if second.Body != first.Body || !reflect.DeepEqual(second.Tags, first.Tags) {
		t.Fatalf("idempotent retry mutated existing note: %+v", second)
	}

	otherRepo := mustCreateNote(t, store, CreateNoteParams{
		RepoID:         "repo-2",
		Body:           "same key is independent per repo",
		IdempotencyKey: &key,
	})
	if otherRepo.ID == first.ID {
		t.Fatal("same idempotency key in another repo reused the first note")
	}
}

func TestNoteStore_CreateIdempotencyKeyArbitratesConcurrentWriters(t *testing.T) {
	store := NewNoteStore(setupFileDB(t))
	key := "Release review: v2:Y29uY3VycmVudC5nb0A0Mg"
	const writers = 8
	notes := make(chan *models.Note, writers)
	errs := make(chan error, writers)
	var wg sync.WaitGroup
	for i := 0; i < writers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			note, err := store.Create(context.Background(), CreateNoteParams{
				RepoID:         "repo-1",
				Body:           fmt.Sprintf("writer %d", i),
				IdempotencyKey: &key,
			})
			if err != nil {
				errs <- err
				return
			}
			notes <- note
		}(i)
	}
	wg.Wait()
	close(errs)
	close(notes)
	for err := range errs {
		t.Fatalf("concurrent idempotent create: %v", err)
	}
	var firstID string
	for note := range notes {
		if firstID == "" {
			firstID = note.ID
		} else if note.ID != firstID {
			t.Fatalf("concurrent idempotent creates returned %q and %q", firstID, note.ID)
		}
	}
	if firstID == "" {
		t.Fatal("concurrent idempotent creates returned no note")
	}
	repoID := "repo-1"
	stored, err := store.List(context.Background(), ListNotesFilter{RepoID: &repoID})
	if err != nil {
		t.Fatalf("list idempotent notes: %v", err)
	}
	if len(stored) != 1 || stored[0].ID != firstID {
		t.Fatalf("stored notes = %+v, want exactly %q", stored, firstID)
	}
}

func TestNoteStore_CreateOmitsBlankProvenance(t *testing.T) {
	store := NewNoteStore(setupTestDB(t))
	note := mustCreateNote(t, store, CreateNoteParams{
		RepoID:    "repo-1",
		SessionID: strPtr("   "),
		ChatID:    strPtr(""),
		Body:      "body",
	})
	if note.SessionID != nil {
		t.Errorf("session id = %v, want nil for a blank value", *note.SessionID)
	}
	if note.ChatID != nil {
		t.Errorf("chat id = %v, want nil for a blank value", *note.ChatID)
	}
	if note.Tags != nil {
		t.Errorf("tags = %v, want nil when none supplied", note.Tags)
	}
}

func TestNoteStore_CreateNormalizesTags(t *testing.T) {
	store := NewNoteStore(setupTestDB(t))

	cases := []struct {
		name string
		tags []string
		want []string
	}{
		{name: "lowercases", tags: []string{"Tech-Debt"}, want: []string{"tech-debt"}},
		{name: "trims", tags: []string{"  spaced  "}, want: []string{"spaced"}},
		{
			name: "de-dups case variants",
			tags: []string{"Tech-Debt", "tech-debt", "TECH-DEBT"},
			want: []string{"tech-debt"},
		},
		{name: "sorts", tags: []string{"zulu", "alpha", "mike"}, want: []string{"alpha", "mike", "zulu"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			note := mustCreateNote(t, store, CreateNoteParams{
				RepoID: "repo-1", Body: "body", Tags: tc.tags,
			})
			if !reflect.DeepEqual(note.Tags, tc.want) {
				t.Errorf("tags = %v, want %v", note.Tags, tc.want)
			}
		})
	}
}

func TestNoteStore_CreateValidation(t *testing.T) {
	store := NewNoteStore(setupTestDB(t))
	ctx := context.Background()

	manyTags := make([]string, NoteMaxTags+1)
	for i := range manyTags {
		manyTags[i] = string(rune('a'+i/26)) + string(rune('a'+i%26))
	}

	cases := []struct {
		name   string
		params CreateNoteParams
	}{
		{name: "empty repo id", params: CreateNoteParams{RepoID: "  ", Body: "body"}},
		{name: "empty body", params: CreateNoteParams{RepoID: "repo-1", Body: ""}},
		{name: "whitespace body", params: CreateNoteParams{RepoID: "repo-1", Body: " \n\t "}},
		{
			name:   "oversize body",
			params: CreateNoteParams{RepoID: "repo-1", Body: strings.Repeat("x", NoteMaxBodyBytes+1)},
		},
		{
			name:   "empty tag",
			params: CreateNoteParams{RepoID: "repo-1", Body: "body", Tags: []string{"ok", "  "}},
		},
		{
			name: "oversize tag",
			params: CreateNoteParams{
				RepoID: "repo-1", Body: "body",
				Tags: []string{strings.Repeat("t", NoteMaxTagLength+1)},
			},
		},
		{
			name:   "too many tags",
			params: CreateNoteParams{RepoID: "repo-1", Body: "body", Tags: manyTags},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := store.Create(ctx, tc.params)
			if !errors.Is(err, ErrNoteInvalid) {
				t.Fatalf("err = %v, want ErrNoteInvalid", err)
			}
		})
	}
}

// TestNoteStore_CreateAcceptsLimits pins that the caps are inclusive: a body of
// exactly NoteMaxBodyBytes, a tag of exactly NoteMaxTagLength, and exactly
// NoteMaxTags tags are all accepted, so the rejection tests above are testing
// an off-by-one boundary rather than a blanket refusal.
func TestNoteStore_CreateAcceptsLimits(t *testing.T) {
	store := NewNoteStore(setupTestDB(t))

	exactTags := make([]string, NoteMaxTags)
	for i := range exactTags {
		exactTags[i] = string(rune('a'+i/26)) + string(rune('a'+i%26))
	}
	note := mustCreateNote(t, store, CreateNoteParams{
		RepoID: "repo-1",
		Body:   strings.Repeat("x", NoteMaxBodyBytes),
		Tags:   append(exactTags[1:], strings.Repeat("t", NoteMaxTagLength)),
	})
	if len(note.Body) != NoteMaxBodyBytes {
		t.Errorf("body length = %d, want %d", len(note.Body), NoteMaxBodyBytes)
	}
	if len(note.Tags) != NoteMaxTags {
		t.Errorf("tags = %d, want %d", len(note.Tags), NoteMaxTags)
	}
}

// TestNoteStore_CreateDeDupBeforeCap pins that the tag cap is applied AFTER
// de-duplication: a caller repeating one tag past the cap is not rejected.
func TestNoteStore_CreateDeDupBeforeCap(t *testing.T) {
	store := NewNoteStore(setupTestDB(t))
	repeated := make([]string, NoteMaxTags+10)
	for i := range repeated {
		repeated[i] = "same-tag"
	}
	note := mustCreateNote(t, store, CreateNoteParams{
		RepoID: "repo-1", Body: "body", Tags: repeated,
	})
	if !reflect.DeepEqual(note.Tags, []string{"same-tag"}) {
		t.Errorf("tags = %v, want [same-tag]", note.Tags)
	}
}

func TestNoteStore_GetMissing(t *testing.T) {
	store := NewNoteStore(setupTestDB(t))
	if _, err := store.Get(context.Background(), "nope"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("err = %v, want sql.ErrNoRows", err)
	}
}

func TestNoteStore_ListFilters(t *testing.T) {
	store := NewNoteStore(setupTestDB(t))
	ctx := context.Background()

	a := mustCreateNote(t, store, CreateNoteParams{
		RepoID: "repo-1", SessionID: strPtr("session-1"), ChatID: strPtr("chat-1"),
		Body: "Refactored the cron scheduler", Tags: []string{"tech-debt", "cron"},
	})
	b := mustCreateNote(t, store, CreateNoteParams{
		RepoID: "repo-1", SessionID: strPtr("session-2"),
		Body: "Flaky webhook test", Tags: []string{"testing"},
	})
	c := mustCreateNote(t, store, CreateNoteParams{
		RepoID: "repo-2",
		Body:   "Unrelated repo note", Tags: []string{"cron"},
	})

	cases := []struct {
		name   string
		filter ListNotesFilter
		want   []string
	}{
		{name: "no filter returns all", filter: ListNotesFilter{}, want: []string{a.ID, b.ID, c.ID}},
		{name: "by repo", filter: ListNotesFilter{RepoID: strPtr("repo-1")}, want: []string{a.ID, b.ID}},
		{name: "by session", filter: ListNotesFilter{SessionID: strPtr("session-2")}, want: []string{b.ID}},
		{name: "by chat", filter: ListNotesFilter{ChatID: strPtr("chat-1")}, want: []string{a.ID}},
		{name: "by single tag", filter: ListNotesFilter{Tags: []string{"testing"}}, want: []string{b.ID}},
		{
			name:   "tags are any-of not all-of",
			filter: ListNotesFilter{Tags: []string{"testing", "cron"}},
			want:   []string{a.ID, b.ID, c.ID},
		},
		{
			name:   "tag filter is case-insensitive",
			filter: ListNotesFilter{Tags: []string{"Tech-Debt"}},
			want:   []string{a.ID},
		},
		{
			name:   "tag filter matches nothing",
			filter: ListNotesFilter{Tags: []string{"absent"}},
			want:   nil,
		},
		{
			name:   "search is a case-insensitive substring",
			filter: ListNotesFilter{Search: strPtr("FLAKY")},
			want:   []string{b.ID},
		},
		{
			name:   "search combines with repo filter",
			filter: ListNotesFilter{RepoID: strPtr("repo-1"), Search: strPtr("note")},
			want:   nil,
		},
		{
			name:   "tag and repo filters are ANDed",
			filter: ListNotesFilter{RepoID: strPtr("repo-1"), Tags: []string{"cron"}},
			want:   []string{a.ID},
		},
	}
	all := []*models.Note{a, b, c}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := store.List(ctx, tc.filter)
			if err != nil {
				t.Fatalf("list: %v", err)
			}
			// created_at is millisecond-granular, so notes written in the same
			// millisecond fall back to the id tiebreak; sort the expectation
			// the same way the store's ORDER BY does. Ordering itself is
			// asserted separately by TestNoteStore_ListOrderingAndLimit.
			want := append([]string(nil), tc.want...)
			sortNotesLikeStore(want, all)
			if len(got) == 0 && len(want) == 0 {
				return
			}
			if !reflect.DeepEqual(noteIDs(got), want) {
				t.Errorf("ids = %v, want %v", noteIDs(got), want)
			}
		})
	}
}

// TestNoteStore_ListAttachesTagsAcrossBatchBoundary pins that tag attachment
// still works when the result set spans more than one attachTags chunk. The
// batching exists so an unlimited List over a large repo cannot exceed SQLite's
// bind-parameter limit; this test crosses the boundary so a broken chunk loop
// (a dropped remainder, or an off-by-one) shows up as notes missing their tags
// rather than only failing on a 32k-row database no test would ever build.
func TestNoteStore_ListAttachesTagsAcrossBatchBoundary(t *testing.T) {
	store := NewNoteStore(setupTestDB(t))
	ctx := context.Background()

	const total = noteTagBatchSize + 7 // deliberately not a multiple of the chunk
	for i := 0; i < total; i++ {
		mustCreateNote(t, store, CreateNoteParams{
			RepoID: "repo-1",
			Body:   fmt.Sprintf("note %d", i),
			Tags:   []string{"batched"},
		})
	}

	got, err := store.List(ctx, ListNotesFilter{RepoID: strPtr("repo-1")})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(got) != total {
		t.Fatalf("notes = %d, want %d", len(got), total)
	}
	for i, n := range got {
		if !reflect.DeepEqual(n.Tags, []string{"batched"}) {
			t.Fatalf("note %d (%s) tags = %v, want [batched] — a chunk was dropped", i, n.ID, n.Tags)
		}
	}
}

// TestNoteStore_ListTagFilterFailsClosed pins that a tag filter whose entries
// all normalise away matches NOTHING rather than everything.
//
// A tag list is a narrowing predicate, so dropping it when it normalises empty
// would turn a malformed filter into an unbounded dump — `boss notes list
// --tag ""` or an MCP `{"tags": [""]}` would hand the caller the entire table
// instead of the topic they asked for. The assertions are deliberately paired:
// each case first proves the unfiltered store DOES hold notes (so an empty
// result cannot pass vacuously on an empty database), then requires the
// filtered result to be empty.
func TestNoteStore_ListTagFilterFailsClosed(t *testing.T) {
	store := NewNoteStore(setupTestDB(t))
	ctx := context.Background()

	mustCreateNote(t, store, CreateNoteParams{
		RepoID: "repo-1", Body: "Refactored the cron scheduler", Tags: []string{"tech-debt"},
	})
	mustCreateNote(t, store, CreateNoteParams{
		RepoID: "repo-2", Body: "Flaky webhook test", Tags: []string{"testing"},
	})

	// Guard the guard: without a tag filter the store returns rows, so the
	// "want nothing" assertions below are non-vacuous.
	all, err := store.List(ctx, ListNotesFilter{})
	if err != nil {
		t.Fatalf("list all: %v", err)
	}
	if len(all) != 2 {
		t.Fatalf("precondition: got %d notes, want 2 — the fail-closed assertions would be vacuous", len(all))
	}

	cases := []struct {
		name string
		tags []string
	}{
		{name: "single empty tag", tags: []string{""}},
		{name: "single whitespace tag", tags: []string{"   "}},
		{name: "every entry blank", tags: []string{"", "  ", "\t"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := store.List(ctx, ListNotesFilter{Tags: tc.tags})
			if err != nil {
				t.Fatalf("list: %v", err)
			}
			if len(got) != 0 {
				t.Errorf("ids = %v, want none — a tag filter that normalises empty must match nothing, not every note", noteIDs(got))
			}
		})
	}

	// A partially-blank list still filters on the entries that survive, rather
	// than failing closed on the whole request.
	got, err := store.List(ctx, ListNotesFilter{Tags: []string{"", "testing"}})
	if err != nil {
		t.Fatalf("list mixed: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("ids = %v, want the single 'testing' note — blank entries are dropped, not fatal", noteIDs(got))
	}
	if !reflect.DeepEqual(got[0].Tags, []string{"testing"}) {
		t.Errorf("tags = %v, want [testing]", got[0].Tags)
	}

	// A BLANK SEARCH deliberately does the opposite of a blank tag: it means
	// "no search". Pinned so the asymmetry is a recorded decision rather than
	// an accident — an empty substring matches every body, so applying and
	// dropping the clause return the same rows, which is exactly why the
	// fail-closed rule that governs tags does not apply here.
	t.Run("blank search means no search", func(t *testing.T) {
		blank := "   "
		got, err := store.List(ctx, ListNotesFilter{Search: &blank})
		if err != nil {
			t.Fatalf("list: %v", err)
		}
		if len(got) != 2 {
			t.Errorf("notes = %d, want all 2 — a blank search term is not a narrowing predicate", len(got))
		}
	})
}

// TestNoteStore_ListSearchTreatsWildcardsLiterally pins that a % or _ in the
// search term matches that character, not "anything" — otherwise a caller
// searching for "50%" would silently match every note.
func TestNoteStore_ListSearchTreatsWildcardsLiterally(t *testing.T) {
	store := NewNoteStore(setupTestDB(t))
	ctx := context.Background()

	percentHit := mustCreateNote(t, store, CreateNoteParams{RepoID: "repo-1", Body: "cut runtime by 50% today"})
	// A decoy the unescaped pattern "%50%%" WOULD match ("50" then anything) but
	// the escaped pattern "%50\%%" must not: it has no literal "50%".
	mustCreateNote(t, store, CreateNoteParams{RepoID: "repo-1", Body: "deleted 500 lines"})

	underscoreHit := mustCreateNote(t, store, CreateNoteParams{RepoID: "repo-2", Body: "renamed repo_id column"})
	// Decoy for "_": unescaped, "repo_id" matches "repoXid" too.
	mustCreateNote(t, store, CreateNoteParams{RepoID: "repo-2", Body: "renamed repoXid column"})

	cases := []struct {
		name   string
		repo   string
		search string
		want   string
	}{
		{name: "percent is literal", repo: "repo-1", search: "50%", want: percentHit.ID},
		{name: "underscore is literal", repo: "repo-2", search: "repo_id", want: underscoreHit.ID},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := store.List(ctx, ListNotesFilter{RepoID: &tc.repo, Search: &tc.search})
			if err != nil {
				t.Fatalf("list: %v", err)
			}
			if !reflect.DeepEqual(noteIDs(got), []string{tc.want}) {
				t.Errorf("ids = %v, want only [%s] — the wildcard must be matched literally", noteIDs(got), tc.want)
			}
		})
	}
}

func TestNoteStore_ListOrderingAndLimit(t *testing.T) {
	store := NewNoteStore(setupTestDB(t))
	ctx := context.Background()

	var created []*models.Note
	for i := 0; i < 5; i++ {
		created = append(created, mustCreateNote(t, store, newTestNoteParams("repo-1")))
	}
	want := noteIDs(created)
	// created_at is millisecond-granular, so same-millisecond rows fall back to
	// the id tiebreak; sort the expectation the same way the store does.
	sortNotesLikeStore(want, created)

	got, err := store.List(ctx, ListNotesFilter{RepoID: strPtr("repo-1")})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if !reflect.DeepEqual(noteIDs(got), want) {
		t.Errorf("order = %v, want %v", noteIDs(got), want)
	}

	// The same ordering must hold under a limit — the limit takes a prefix, it
	// does not reshuffle.
	limit := 2
	got, err = store.List(ctx, ListNotesFilter{RepoID: strPtr("repo-1"), Limit: &limit})
	if err != nil {
		t.Fatalf("list limited: %v", err)
	}
	if !reflect.DeepEqual(noteIDs(got), want[:limit]) {
		t.Errorf("limited order = %v, want %v", noteIDs(got), want[:limit])
	}

	// A repeated read returns the identical order: the listing is stable, not
	// dependent on insertion or page-cache order.
	again, err := store.List(ctx, ListNotesFilter{RepoID: strPtr("repo-1")})
	if err != nil {
		t.Fatalf("list again: %v", err)
	}
	if !reflect.DeepEqual(noteIDs(again), want) {
		t.Errorf("repeat order = %v, want %v", noteIDs(again), want)
	}
}

// sortNotesLikeStore sorts ids in place by (created_at, id), mirroring the
// store's ORDER BY so the ordering assertion is independent of how many notes
// happened to land in the same millisecond.
func sortNotesLikeStore(ids []string, notes []*models.Note) {
	byID := map[string]*models.Note{}
	for _, n := range notes {
		byID[n.ID] = n
	}
	for i := 1; i < len(ids); i++ {
		for j := i; j > 0; j-- {
			a, b := byID[ids[j-1]], byID[ids[j]]
			less := b.CreatedAt.Before(a.CreatedAt) ||
				(b.CreatedAt.Equal(a.CreatedAt) && b.ID < a.ID)
			if !less {
				break
			}
			ids[j-1], ids[j] = ids[j], ids[j-1]
		}
	}
}

func TestNoteStore_Update(t *testing.T) {
	store := NewNoteStore(setupTestDB(t))
	ctx := context.Background()

	t.Run("body only leaves tags alone", func(t *testing.T) {
		note := mustCreateNote(t, store, CreateNoteParams{
			RepoID: "repo-1", Body: "old body", Tags: []string{"keep"},
		})
		updated, err := store.Update(ctx, UpdateNoteParams{ID: note.ID, Body: strPtr("new body")})
		if err != nil {
			t.Fatalf("update: %v", err)
		}
		if updated.Body != "new body" {
			t.Errorf("body = %q, want new body", updated.Body)
		}
		if !reflect.DeepEqual(updated.Tags, []string{"keep"}) {
			t.Errorf("tags = %v, want [keep] untouched", updated.Tags)
		}
	})

	t.Run("tags only leave body alone and replace not merge", func(t *testing.T) {
		note := mustCreateNote(t, store, CreateNoteParams{
			RepoID: "repo-1", Body: "body", Tags: []string{"old-a", "old-b"},
		})
		updated, err := store.Update(ctx, UpdateNoteParams{ID: note.ID, Tags: []string{"New"}})
		if err != nil {
			t.Fatalf("update: %v", err)
		}
		if updated.Body != "body" {
			t.Errorf("body = %q, want unchanged", updated.Body)
		}
		if !reflect.DeepEqual(updated.Tags, []string{"new"}) {
			t.Errorf("tags = %v, want [new] — replace, not merge", updated.Tags)
		}
		// The replaced rows are gone from the join table, not merely hidden.
		var remaining int
		if err := storeDB(t, store).QueryRow(
			"SELECT COUNT(*) FROM note_tags WHERE note_id = ?", note.ID,
		).Scan(&remaining); err != nil {
			t.Fatalf("count tags: %v", err)
		}
		if remaining != 1 {
			t.Errorf("note_tags rows = %d, want 1", remaining)
		}
	})

	t.Run("empty non-nil tags clears the set", func(t *testing.T) {
		note := mustCreateNote(t, store, CreateNoteParams{
			RepoID: "repo-1", Body: "body", Tags: []string{"gone"},
		})
		updated, err := store.Update(ctx, UpdateNoteParams{ID: note.ID, Tags: []string{}})
		if err != nil {
			t.Fatalf("update: %v", err)
		}
		if len(updated.Tags) != 0 {
			t.Errorf("tags = %v, want cleared", updated.Tags)
		}
	})

	t.Run("both fields", func(t *testing.T) {
		note := mustCreateNote(t, store, CreateNoteParams{
			RepoID: "repo-1", Body: "body", Tags: []string{"a"},
		})
		updated, err := store.Update(ctx, UpdateNoteParams{
			ID: note.ID, Body: strPtr("both"), Tags: []string{"b", "c"},
		})
		if err != nil {
			t.Fatalf("update: %v", err)
		}
		if updated.Body != "both" {
			t.Errorf("body = %q, want both", updated.Body)
		}
		if !reflect.DeepEqual(updated.Tags, []string{"b", "c"}) {
			t.Errorf("tags = %v, want [b c]", updated.Tags)
		}
	})

	t.Run("no fields is a no-op", func(t *testing.T) {
		note := mustCreateNote(t, store, CreateNoteParams{
			RepoID: "repo-1", Body: "body", Tags: []string{"a"},
		})
		updated, err := store.Update(ctx, UpdateNoteParams{ID: note.ID})
		if err != nil {
			t.Fatalf("update: %v", err)
		}
		if !reflect.DeepEqual(updated, note) {
			t.Errorf("no-op update changed the note:\n got %+v\nwant %+v", updated, note)
		}
	})

	t.Run("missing note", func(t *testing.T) {
		if _, err := store.Update(ctx, UpdateNoteParams{ID: "nope", Body: strPtr("x")}); !errors.Is(err, sql.ErrNoRows) {
			t.Fatalf("err = %v, want sql.ErrNoRows", err)
		}
	})

	t.Run("validation", func(t *testing.T) {
		note := mustCreateNote(t, store, newTestNoteParams("repo-1"))
		cases := []struct {
			name   string
			params UpdateNoteParams
		}{
			{name: "empty id", params: UpdateNoteParams{ID: "  ", Body: strPtr("x")}},
			{name: "empty body", params: UpdateNoteParams{ID: note.ID, Body: strPtr("   ")}},
			{
				name:   "oversize body",
				params: UpdateNoteParams{ID: note.ID, Body: strPtr(strings.Repeat("x", NoteMaxBodyBytes+1))},
			},
			{
				name:   "oversize tag",
				params: UpdateNoteParams{ID: note.ID, Tags: []string{strings.Repeat("t", NoteMaxTagLength+1)}},
			},
			{name: "empty tag", params: UpdateNoteParams{ID: note.ID, Tags: []string{" "}}},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				if _, err := store.Update(ctx, tc.params); !errors.Is(err, ErrNoteInvalid) {
					t.Fatalf("err = %v, want ErrNoteInvalid", err)
				}
			})
		}
		// A rejected update leaves the note untouched.
		after, err := store.Get(ctx, note.ID)
		if err != nil {
			t.Fatalf("get: %v", err)
		}
		if after.Body != "learned something" || !reflect.DeepEqual(after.Tags, []string{"tech-debt"}) {
			t.Errorf("rejected update mutated the note: %+v", after)
		}
	})
}

func TestNoteStore_DeleteIdempotentAndCascades(t *testing.T) {
	db := setupTestDB(t)
	store := NewNoteStore(db)
	ctx := context.Background()

	note := mustCreateNote(t, store, CreateNoteParams{
		RepoID: "repo-1", Body: "body", Tags: []string{"a", "b"},
	})
	survivor := mustCreateNote(t, store, CreateNoteParams{
		RepoID: "repo-1", Body: "other", Tags: []string{"c"},
	})

	if err := store.Delete(ctx, note.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := store.Get(ctx, note.ID); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("get after delete = %v, want sql.ErrNoRows", err)
	}
	// Deleting again is a nil no-op.
	if err := store.Delete(ctx, note.ID); err != nil {
		t.Fatalf("second delete: %v", err)
	}
	// Deleting an id that never existed is also a nil no-op.
	if err := store.Delete(ctx, "never-existed"); err != nil {
		t.Fatalf("delete absent: %v", err)
	}

	var orphans int
	if err := db.QueryRow("SELECT COUNT(*) FROM note_tags WHERE note_id = ?", note.ID).Scan(&orphans); err != nil {
		t.Fatalf("count orphan tags: %v", err)
	}
	if orphans != 0 {
		t.Errorf("orphan note_tags rows = %d, want 0 (ON DELETE CASCADE)", orphans)
	}

	// The cascade is scoped to the deleted note.
	got, err := store.Get(ctx, survivor.ID)
	if err != nil {
		t.Fatalf("get survivor: %v", err)
	}
	if !reflect.DeepEqual(got.Tags, []string{"c"}) {
		t.Errorf("survivor tags = %v, want [c]", got.Tags)
	}
}

// TestNoteStore_SessionDeleteLeavesNoteIntact is the regression test protecting
// the whole use case: notes are harvested from PAST runs, and sessions are
// archived and removed routinely, so a cascade from sessions would delete
// exactly the rows the sweep exists to read. Deleting the referenced session
// must leave the note — and its session_id provenance — untouched.
func TestNoteStore_SessionDeleteLeavesNoteIntact(t *testing.T) {
	db := setupTestDB(t)
	repoStore := NewRepoStore(db)
	sessionStore := NewSessionStore(db)
	noteStore := NewNoteStore(db)
	ctx := context.Background()

	repo := createTestRepo(t, repoStore)
	session, err := sessionStore.Create(ctx, CreateSessionParams{
		RepoID:       repo.ID,
		Title:        "a session",
		Plan:         "a plan",
		WorktreePath: "/tmp/wt",
		BranchName:   "branch",
		BaseBranch:   "main",
	})
	if err != nil {
		t.Fatalf("create session: %v", err)
	}

	note := mustCreateNote(t, noteStore, CreateNoteParams{
		RepoID:    repo.ID,
		SessionID: &session.ID,
		Body:      "what this run learned",
		Tags:      []string{"learning"},
	})

	if err := sessionStore.Delete(ctx, session.ID); err != nil {
		t.Fatalf("delete session: %v", err)
	}
	// Guard against a vacuous pass: the session must really be gone, otherwise
	// "the note survived" proves nothing about cascade behaviour.
	var sessionRows int
	if err := db.QueryRow("SELECT COUNT(*) FROM sessions WHERE id = ?", session.ID).Scan(&sessionRows); err != nil {
		t.Fatalf("count sessions: %v", err)
	}
	if sessionRows != 0 {
		t.Fatalf("session rows after delete = %d, want 0", sessionRows)
	}

	got, err := noteStore.Get(ctx, note.ID)
	if err != nil {
		t.Fatalf("get note after session delete: %v", err)
	}
	if got.SessionID == nil || *got.SessionID != session.ID {
		t.Errorf("session id = %v, want the deleted session's id preserved as provenance", got.SessionID)
	}
	if got.Body != "what this run learned" {
		t.Errorf("body = %q, want unchanged", got.Body)
	}
	if !reflect.DeepEqual(got.Tags, []string{"learning"}) {
		t.Errorf("tags = %v, want [learning]", got.Tags)
	}

	// It is still reachable through the session filter, which is what the
	// harvesting sweep uses.
	listed, err := noteStore.List(ctx, ListNotesFilter{SessionID: &session.ID})
	if err != nil {
		t.Fatalf("list by session: %v", err)
	}
	if len(listed) != 1 || listed[0].ID != note.ID {
		t.Errorf("list by deleted session = %v, want the note", noteIDs(listed))
	}
}

// TestNoteStore_TagFilterUsesIndex asserts SQLite's planner picks
// idx_note_tags_tag for the any-of tag lookup instead of scanning note_tags.
func TestNoteStore_TagFilterUsesIndex(t *testing.T) {
	db := setupTestDB(t)
	rows, err := db.Query(
		"EXPLAIN QUERY PLAN SELECT id FROM notes WHERE id IN (SELECT note_id FROM note_tags WHERE tag IN (?, ?))",
		"a", "b")
	if err != nil {
		t.Fatalf("explain: %v", err)
	}
	defer func() { _ = rows.Close() }()
	var plan string
	for rows.Next() {
		var id, parent, notused int
		var detail string
		if err := rows.Scan(&id, &parent, &notused, &detail); err != nil {
			t.Fatalf("scan: %v", err)
		}
		plan += detail + "\n"
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
	if !strings.Contains(plan, "idx_note_tags_tag") {
		t.Errorf("tag lookup does not use idx_note_tags_tag:\n%s", plan)
	}
}

// storeDB exposes a store's handle for assertions that need raw SQL.
func storeDB(t *testing.T, s *SQLiteNoteStore) *sql.DB {
	t.Helper()
	return s.db
}
