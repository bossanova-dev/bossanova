package e2e_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/recurser/boss/internal/clitest"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
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

// The distinctive tail makes the full-body read assertion observable.
const noteBodyTail = "TAIL-MARKER-VISIBLE-ONLY-IN-SHOW"

// testLongBody is comfortably longer than the 60-rune BODY column cap, so the
// preview/full-text difference is observable rather than incidental.
const testLongBody = "the retry loop drops the last error on the floor when the context " +
	"deadline fires first, which is why the sweep saw an empty reason " + noteBodyTail

// TestJourney_Notes_AC1_AC2_Notes_RoundTrip drives the whole command group in the order an agent
// would: record a note, find it by tag, read it in full, change it, and remove
// it. Each step is asserted against the previous step's output, so a break
// anywhere in the client/daemon chain surfaces here.
func TestJourney_Notes_AC1_AC2_Notes_RoundTrip(t *testing.T) {
	h := clitest.New(t, clitest.WithRepos(&pb.Repo{Id: "rt-repo", DisplayName: "roundtrip"}), clitest.WithEnv("BOSS_REPO_ID=rt-repo"))

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
	if created.RepoID != h.RepoIDs["rt-repo"] {
		t.Errorf("repo id = %q, want rt-repo", created.RepoID)
	}
	// Tags come back normalised: trimmed, lowercased, de-duplicated, sorted.
	if got := strings.Join(created.Tags, ","); got != "perf,sweep" {
		t.Errorf("tags = %q, want perf,sweep", got)
	}

	// Each filter has a persisted decoy it must exclude.
	for _, decoy := range []struct{ body, tag string }{{"unrelated body", "sweep"}, {testLongBody, "other"}} {
		result := h.Run("notes", "add", decoy.body, "--tag", decoy.tag, "--json")
		if !result.Success() {
			t.Fatalf("decoy creation: %+v", result)
		}
	}
	found := h.Run("notes", "ls", "--tag", "sweep", "--search", "retry loop drops", "--json")
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
	var remaining []noteJSON
	if err := json.Unmarshal([]byte(after.Stdout), &remaining); err != nil {
		t.Fatal(err)
	}
	if len(remaining) != 2 {
		t.Fatalf("removing the target changed decoys: %s", after.Stdout)
	}
	for _, note := range remaining {
		if note.ID == created.ID {
			t.Fatalf("removed note remains listed: %s", after.Stdout)
		}
	}
}
