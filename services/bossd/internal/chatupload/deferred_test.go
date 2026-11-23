package chatupload

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// finishDeferred uploads payload under (attachID, uploadID) for chatID and
// holds it for a send.
func finishDeferred(t *testing.T, m *Manager, attachID, uploadID, chatID string, payload []byte) {
	t.Helper()
	if err := m.Start(attachID, uploadID, chatID, "shot.png", uint64(len(payload))); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if _, err := m.Chunk(attachID, uploadID, 0, payload); err != nil {
		t.Fatalf("Chunk: %v", err)
	}
	if err := m.ClaimFinish(attachID, uploadID); err != nil {
		t.Fatalf("ClaimFinish: %v", err)
	}
	if err := m.FinishDeferred(attachID, uploadID, "alice"); err != nil {
		t.Fatalf("FinishDeferred: %v", err)
	}
}

func TestFinishDeferredKeepsTheFileAndPrefillsNothing(t *testing.T) {
	m, sender := newTestManager(t)
	finishDeferred(t, m, "attach-1", "up-1", "chat-1", []byte("png bytes"))

	if len(sender.calls) != 0 {
		t.Fatalf("a deferred upload reached the chat: %+v", sender.calls)
	}
	paths, err := m.Attachments("chat-1", "alice", []string{"up-1"})
	if err != nil {
		t.Fatalf("Attachments: %v", err)
	}
	if len(paths) != 1 || filepath.Dir(paths[0]) != m.Dir() || !strings.HasSuffix(paths[0], ".png") {
		t.Fatalf("paths = %q, want one .png inside %q", paths, m.Dir())
	}
	body, err := os.ReadFile(paths[0])
	if err != nil || string(body) != "png bytes" {
		t.Fatalf("deferred file = %q, %v", body, err)
	}
	if m.InFlight() != 0 {
		t.Fatalf("a finished upload still counts as in flight: %d", m.InFlight())
	}
}

func TestAttachmentsResolveInTheOrderNamed(t *testing.T) {
	m, _ := newTestManager(t)
	finishDeferred(t, m, "attach-1", "up-1", "chat-1", []byte("one"))
	finishDeferred(t, m, "attach-2", "up-2", "chat-1", []byte("two"))

	paths, err := m.Attachments("chat-1", "alice", []string{"up-2", "up-1"})
	if err != nil {
		t.Fatalf("Attachments: %v", err)
	}
	var bodies []string
	for _, p := range paths {
		b, err := os.ReadFile(p)
		if err != nil {
			t.Fatalf("read %q: %v", p, err)
		}
		bodies = append(bodies, string(b))
	}
	if strings.Join(bodies, ",") != "two,one" {
		t.Fatalf("resolved bodies = %q, want two,one", bodies)
	}
}

func TestAttachmentsRefuseUnknownForeignExpiredOrRepeatedIDs(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	m, _ := newTestManager(t, WithClock(func() time.Time { return now }))
	finishDeferred(t, m, "attach-1", "up-1", "chat-1", []byte("mine"))
	finishDeferred(t, m, "attach-2", "up-other", "chat-2", []byte("theirs"))

	cases := map[string][]string{
		"unknown":        {"up-1", "up-missing"},
		"another chat":   {"up-other"},
		"named twice":    {"up-1", "up-1"},
		"too many named": make([]string, MaxAttachmentsPerSend+1),
	}
	for name, ids := range cases {
		paths, err := m.Attachments("chat-1", "alice", ids)
		if ue := uploadErr(t, err); ue.Retryable {
			t.Fatalf("%s: refusal is retryable: %q", name, ue.Msg)
		}
		if paths != nil {
			t.Fatalf("%s: a refused set still resolved paths %q", name, paths)
		}
	}

	now = now.Add(DefaultStaleTTL + time.Minute)
	if _, err := m.Attachments("chat-1", "alice", []string{"up-1"}); !strings.Contains(uploadErr(t, err).Msg, "unknown or expired") {
		t.Fatalf("expired attachment error = %v", err)
	}
}

func TestReleaseAttachmentsForgetsTheIDButLeavesTheFile(t *testing.T) {
	m, _ := newTestManager(t)
	finishDeferred(t, m, "attach-1", "up-1", "chat-1", []byte("keep me"))
	paths, err := m.Attachments("chat-1", "alice", []string{"up-1"})
	if err != nil {
		t.Fatalf("Attachments: %v", err)
	}

	m.ReleaseAttachments("chat-1", "alice", []string{"up-1"})

	if _, err := m.Attachments("chat-1", "alice", []string{"up-1"}); err == nil {
		t.Fatal("a released id resolved again")
	}
	if _, err := os.Stat(paths[0]); err != nil {
		t.Fatalf("release removed the file the agent still reads: %v", err)
	}
}

func TestFinishDeferredRefusesASecondFileUnderOneID(t *testing.T) {
	m, _ := newTestManager(t)
	finishDeferred(t, m, "attach-1", "up-1", "chat-1", []byte("first"))
	if err := m.Start("attach-2", "up-1", "chat-1", "again.png", 6); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if _, err := m.Chunk("attach-2", "up-1", 0, []byte("second")); err != nil {
		t.Fatalf("Chunk: %v", err)
	}
	if err := m.ClaimFinish("attach-2", "up-1"); err != nil {
		t.Fatalf("ClaimFinish: %v", err)
	}
	uploadErr(t, m.FinishDeferred("attach-2", "up-1", "alice"))

	paths, err := m.Attachments("chat-1", "alice", []string{"up-1"})
	if err != nil {
		t.Fatalf("Attachments: %v", err)
	}
	if body, _ := os.ReadFile(paths[0]); string(body) != "first" {
		t.Fatalf("id now names %q, want the first file", body)
	}
	entries, _ := os.ReadDir(m.Dir())
	if len(entries) != 1 {
		t.Fatalf("refused duplicate left %d files, want 1", len(entries))
	}
}

func TestSweepRemovesAnUnsentDeferredFileAtTheStaleTTL(t *testing.T) {
	now := time.Now()
	m, _ := newTestManager(t, WithClock(func() time.Time { return now }))
	finishDeferred(t, m, "attach-1", "up-1", "chat-1", []byte("never sent"))
	paths, err := m.Attachments("chat-1", "alice", []string{"up-1"})
	if err != nil {
		t.Fatalf("Attachments: %v", err)
	}

	now = now.Add(DefaultStaleTTL + time.Hour)
	if removed := m.Sweep(); removed != 1 {
		t.Fatalf("Sweep removed %d files, want 1", removed)
	}
	if _, err := os.Stat(paths[0]); !os.IsNotExist(err) {
		t.Fatalf("unsent deferred file survived the janitor: %v", err)
	}
	if _, err := m.Attachments("chat-1", "alice", []string{"up-1"}); err == nil {
		t.Fatal("a swept id still resolves")
	}
}

// finishAs uploads payload as uploader and holds it for their send.
func finishAs(t *testing.T, m *Manager, attachID, uploaderID string, payload []byte) {
	t.Helper()
	if err := m.Start(attachID, "up-1", "chat-1", "shot.png", uint64(len(payload))); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if _, err := m.Chunk(attachID, "up-1", 0, payload); err != nil {
		t.Fatalf("Chunk: %v", err)
	}
	if err := m.ClaimFinish(attachID, "up-1"); err != nil {
		t.Fatalf("ClaimFinish: %v", err)
	}
	if err := m.FinishDeferred(attachID, "up-1", uploaderID); err != nil {
		t.Fatalf("FinishDeferred as %s: %v", uploaderID, err)
	}
}

func TestDeferredUploadsBelongToTheirUploader(t *testing.T) {
	m, _ := newTestManager(t)
	finishAs(t, m, "attach-alice", "alice", []byte("alice's"))
	// The same client-chosen id from another user in the same chat neither
	// collides with alice's file nor replaces it.
	finishAs(t, m, "attach-bob", "bob", []byte("bob's"))

	for uploader, want := range map[string]string{"alice": "alice's", "bob": "bob's"} {
		paths, err := m.Attachments("chat-1", uploader, []string{"up-1"})
		if err != nil {
			t.Fatalf("%s: Attachments: %v", uploader, err)
		}
		if body, _ := os.ReadFile(paths[0]); string(body) != want {
			t.Fatalf("%s's id names %q, want %q", uploader, body, want)
		}
	}
	if _, err := m.Attachments("chat-1", "mallory", []string{"up-1"}); err == nil {
		t.Fatal("a user who uploaded nothing resolved another user's file")
	}
	m.ReleaseAttachments("chat-1", "bob", []string{"up-1"})
	if _, err := m.Attachments("chat-1", "alice", []string{"up-1"}); err != nil {
		t.Fatalf("bob's send spent alice's file: %v", err)
	}
}

func TestFinishDeferredRefusesAFileWithNoUploader(t *testing.T) {
	m, _ := newTestManager(t)
	if err := m.Start("attach-1", "up-1", "chat-1", "shot.png", 3); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if _, err := m.Chunk("attach-1", "up-1", 0, []byte("abc")); err != nil {
		t.Fatalf("Chunk: %v", err)
	}
	if err := m.ClaimFinish("attach-1", "up-1"); err != nil {
		t.Fatalf("ClaimFinish: %v", err)
	}
	uploadErr(t, m.FinishDeferred("attach-1", "up-1", ""))
	if entries, _ := os.ReadDir(m.Dir()); len(entries) != 0 {
		t.Fatalf("an unowned upload left %d files", len(entries))
	}
}
