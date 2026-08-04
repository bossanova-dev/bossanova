package chatupload

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

type fakeSender struct {
	calls []struct {
		chatID  string
		message string
		submit  bool
	}
	err error
}

func (f *fakeSender) SendChatMessage(_ context.Context, chatID, message string, submit bool) error {
	f.calls = append(f.calls, struct {
		chatID  string
		message string
		submit  bool
	}{chatID, message, submit})
	return f.err
}

func newTestManager(t *testing.T, opts ...Option) (*Manager, *fakeSender) {
	t.Helper()
	sender := &fakeSender{}
	m, err := NewManager(filepath.Join(t.TempDir(), "uploads"), sender, opts...)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	return m, sender
}

func mustStart(t *testing.T, m *Manager, size uint64) {
	t.Helper()
	if err := m.Start("attach-1", "up-1", "chat-1", "notes.txt", size); err != nil {
		t.Fatalf("Start: %v", err)
	}
}

func uploadErr(t *testing.T, err error) *Error {
	t.Helper()
	if err == nil {
		t.Fatal("expected an error, got nil")
	}
	var ue *Error
	if !errors.As(err, &ue) {
		t.Fatalf("expected *chatupload.Error, got %T: %v", err, err)
	}
	return ue
}

func TestNewManagerCreatesOwnerOnlyDir(t *testing.T) {
	m, _ := newTestManager(t)
	info, err := os.Stat(m.Dir())
	if err != nil {
		t.Fatalf("stat upload dir: %v", err)
	}
	if got := info.Mode().Perm(); got != dirPerm {
		t.Fatalf("upload dir mode = %o, want %o", got, dirPerm)
	}
}

func TestHappyPathWritesRandomized0600FileAndSubmitsOneChatMessage(t *testing.T) {
	m, sender := newTestManager(t)
	payload := []byte("hello upload")
	mustStart(t, m, uint64(len(payload)))

	got, err := m.Chunk("attach-1", "up-1", 0, payload)
	if err != nil {
		t.Fatalf("Chunk: %v", err)
	}
	if got != uint64(len(payload)) {
		t.Fatalf("bytes received = %d, want %d", got, len(payload))
	}

	path, err := m.Finish(context.Background(), "attach-1", "up-1")
	if err != nil {
		t.Fatalf("Finish: %v", err)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat uploaded file: %v", err)
	}
	if got := info.Mode().Perm(); got != filePerm {
		t.Fatalf("file mode = %o, want %o", got, filePerm)
	}
	if base := filepath.Base(path); base == "notes.txt" || !strings.HasPrefix(base, "upload-") {
		t.Fatalf("basename %q is not randomized", base)
	}
	if !strings.HasSuffix(path, ".txt") {
		t.Fatalf("expected the display extension to be preserved, got %q", path)
	}
	if filepath.Dir(path) != m.Dir() {
		t.Fatalf("file %q escaped the upload dir %q", path, m.Dir())
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read uploaded file: %v", err)
	}
	if string(body) != string(payload) {
		t.Fatalf("contents = %q, want %q", body, payload)
	}

	if len(sender.calls) != 1 {
		t.Fatalf("chat messages sent = %d, want exactly 1", len(sender.calls))
	}
	call := sender.calls[0]
	if call.chatID != "chat-1" {
		t.Fatalf("chat id = %q, want chat-1", call.chatID)
	}
	if call.submit {
		t.Fatal("expected the path to be prefilled into the composer, not submitted")
	}
	if !strings.Contains(call.message, path) {
		t.Fatalf("message %q does not name the uploaded path", call.message)
	}
	if strings.Contains(call.message, string(payload)) {
		t.Fatalf("message %q leaked file contents", call.message)
	}
	if m.InFlight() != 0 {
		t.Fatalf("in-flight = %d after finish, want 0", m.InFlight())
	}
}

func TestStartRejectsMalformedMetadata(t *testing.T) {
	cases := []struct {
		name     string
		attach   string
		upload   string
		chat     string
		filename string
		size     uint64
	}{
		{"empty attach", "", "up-1", "chat-1", "a.txt", 10},
		{"empty upload", "attach-1", "", "chat-1", "a.txt", 10},
		{"empty chat", "attach-1", "up-1", "", "a.txt", 10},
		{"empty filename", "attach-1", "up-1", "chat-1", "", 10},
		{"absolute path", "attach-1", "up-1", "chat-1", "/etc/passwd", 10},
		{"relative traversal", "attach-1", "up-1", "chat-1", "../../etc/passwd", 10},
		{"dotdot fragment", "attach-1", "up-1", "chat-1", "a..b/c", 10},
		{"backslash", "attach-1", "up-1", "chat-1", `..\\windows`, 10},
		{"nul byte", "attach-1", "up-1", "chat-1", "a\x00b", 10},
		{"zero size", "attach-1", "up-1", "chat-1", "a.txt", 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m, _ := newTestManager(t)
			err := m.Start(tc.attach, tc.upload, tc.chat, tc.filename, tc.size)
			if ue := uploadErr(t, err); ue.Retryable {
				t.Fatalf("malformed metadata must be permanent, got retryable: %v", ue)
			}
			if m.InFlight() != 0 {
				t.Fatalf("in-flight = %d after rejection, want 0", m.InFlight())
			}
		})
	}
}

// The traversal rule is about a ".." SEGMENT, not about a doubled dot. A
// substring test also rejected ordinary names — "report..final.txt",
// "v1..2.csv" — that traverse nothing, so pin both halves: real traversal
// still refused, doubled dots admitted. Exercised against the exported
// validator, which is what bosso's validateUploadFilename mirrors
// rule-for-rule, and once through Start so nothing downstream re-rejects.
func TestValidateFilenameSeparatesTraversalFromDoubledDots(t *testing.T) {
	for _, name := range []string{"report..final.txt", "v1..2.csv", "..hidden", "trailing..", "a..b"} {
		if err := ValidateFilename(name); err != nil {
			t.Errorf("ValidateFilename(%q) = %v, want nil — a doubled dot is not traversal", name, err)
		}
	}
	for _, name := range []string{".", "..", "../etc/passwd", `..\windows`, "a/../b", "dir/..", "..\\"} {
		if err := ValidateFilename(name); err == nil {
			t.Errorf("ValidateFilename(%q) = nil, want a rejection", name)
		}
	}

	m, _ := newTestManager(t)
	if err := m.Start("attach-1", "up-1", "chat-1", "report..final.txt", 3); err != nil {
		t.Fatalf("Start with a doubled-dot filename: %v", err)
	}
}

func TestStartEnforces500MiBBoundary(t *testing.T) {
	m, _ := newTestManager(t)
	if err := m.Start("attach-1", "at-limit", "chat-1", "big.bin", MaxUploadBytes); err != nil {
		t.Fatalf("exactly 500 MiB must be accepted, got %v", err)
	}
	err := m.Start("attach-1", "over-limit", "chat-1", "big.bin", MaxUploadBytes+1)
	if ue := uploadErr(t, err); ue.Retryable {
		t.Fatalf("over-limit must be permanent, got retryable: %v", ue)
	}
	if m.InFlight() != 1 {
		t.Fatalf("in-flight = %d, want only the at-limit upload", m.InFlight())
	}
	// No bytes were written for either: the declared size alone decides.
	entries, err := os.ReadDir(m.Dir())
	if err != nil {
		t.Fatalf("read upload dir: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("files on disk = %d, want 1 (the accepted upload only)", len(entries))
	}
}

func TestStartRejectsDuplicateUploadID(t *testing.T) {
	m, _ := newTestManager(t)
	mustStart(t, m, 10)
	if ue := uploadErr(t, m.Start("attach-1", "up-1", "chat-1", "notes.txt", 10)); ue.Retryable {
		t.Fatalf("duplicate upload_id must be permanent, got %v", ue)
	}
}

func TestChunkRejectsSequenceViolationsAndDeletesPartial(t *testing.T) {
	cases := []struct {
		name string
		seqs []uint64
	}{
		{"does not start at zero", []uint64{1}},
		{"gap", []uint64{0, 2}},
		{"repeat", []uint64{0, 0}},
		{"reorder", []uint64{0, 1, 1}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m, _ := newTestManager(t)
			mustStart(t, m, 1024)

			var lastErr error
			for _, seq := range tc.seqs {
				if _, lastErr = m.Chunk("attach-1", "up-1", seq, []byte("abc")); lastErr != nil {
					break
				}
			}
			if ue := uploadErr(t, lastErr); ue.Retryable {
				t.Fatalf("order violation must be permanent, got retryable: %v", ue)
			}
			assertUploadDirEmpty(t, m)
			if m.InFlight() != 0 {
				t.Fatalf("in-flight = %d after order violation, want 0", m.InFlight())
			}
		})
	}
}

func TestChunkRejectsOversizedChunk(t *testing.T) {
	m, _ := newTestManager(t)
	mustStart(t, m, MaxChunkBytes*4)
	_, err := m.Chunk("attach-1", "up-1", 0, make([]byte, MaxChunkBytes+1))
	if ue := uploadErr(t, err); ue.Retryable {
		t.Fatalf("oversized chunk must be permanent, got %v", ue)
	}
	assertUploadDirEmpty(t, m)
}

func TestChunkRejectsPayloadBeyondDeclaredSize(t *testing.T) {
	m, _ := newTestManager(t)
	mustStart(t, m, 4)
	if _, err := m.Chunk("attach-1", "up-1", 0, []byte("abcd")); err != nil {
		t.Fatalf("first chunk: %v", err)
	}
	_, err := m.Chunk("attach-1", "up-1", 1, []byte("e"))
	if ue := uploadErr(t, err); ue.Retryable {
		t.Fatalf("overflow must be permanent, got %v", ue)
	}
	assertUploadDirEmpty(t, m)
}

func TestChunkOnUnknownUploadIsRejected(t *testing.T) {
	m, _ := newTestManager(t)
	if _, err := m.Chunk("attach-1", "nope", 0, []byte("x")); err == nil {
		t.Fatal("expected an error for an unknown upload_id")
	}
}

func TestFinishRejectsShortUploadAndDeletesPartial(t *testing.T) {
	m, sender := newTestManager(t)
	mustStart(t, m, 10)
	if _, err := m.Chunk("attach-1", "up-1", 0, []byte("abc")); err != nil {
		t.Fatalf("Chunk: %v", err)
	}
	_, err := m.Finish(context.Background(), "attach-1", "up-1")
	if ue := uploadErr(t, err); ue.Retryable {
		t.Fatalf("size mismatch must be permanent, got %v", ue)
	}
	assertUploadDirEmpty(t, m)
	if len(sender.calls) != 0 {
		t.Fatalf("chat messages sent = %d on a failed upload, want 0", len(sender.calls))
	}
}

func TestFinishRemovesFileWhenChatDeliveryFails(t *testing.T) {
	m, sender := newTestManager(t)
	sender.err = errors.New("tmux unavailable")
	mustStart(t, m, 3)
	if _, err := m.Chunk("attach-1", "up-1", 0, []byte("abc")); err != nil {
		t.Fatalf("Chunk: %v", err)
	}
	_, err := m.Finish(context.Background(), "attach-1", "up-1")
	if ue := uploadErr(t, err); !ue.Retryable {
		t.Fatalf("delivery failure should be retryable, got %v", ue)
	}
	assertUploadDirEmpty(t, m)
}

// The other arm of the same branch: a submit the daemon could not verify may
// already be running in the pane naming this exact path, so the file has to
// survive even though Finish still reports the failure.
func TestFinishRetainsFileWhenChatDeliveryIsUnconfirmed(t *testing.T) {
	now := time.Now()
	clock := func() time.Time { return now }
	m, sender := newTestManager(t, WithClock(clock), WithStaleTTL(time.Hour), WithIdleTimeout(time.Hour))
	// Wrapped, not returned bare: the wiring layer reports the daemon's
	// notice text and marks it unconfirmed with the sentinel.
	sender.err = fmt.Errorf("no live composer was drawn: %w", ErrDeliveryUnconfirmed)
	mustStart(t, m, 3)
	if _, err := m.Chunk("attach-1", "up-1", 0, []byte("abc")); err != nil {
		t.Fatalf("Chunk: %v", err)
	}

	_, err := m.Finish(context.Background(), "attach-1", "up-1")
	ue := uploadErr(t, err)
	if ue.Retryable {
		t.Fatalf("an unconfirmed submit must not invite a resend, got retryable error %v", ue)
	}
	if !strings.Contains(ue.Msg, "unconfirmed") {
		t.Fatalf("error %q does not name the unconfirmed delivery", ue.Msg)
	}

	retained := uploadDirFiles(t, m)
	if len(retained) != 1 {
		t.Fatalf("upload files after an unconfirmed submit = %v, want exactly the retained upload", retained)
	}
	if body, readErr := os.ReadFile(retained[0]); readErr != nil || string(body) != "abc" {
		t.Fatalf("retained file contents = %q (err %v), want %q", body, readErr, "abc")
	}
	if m.InFlight() != 0 {
		t.Fatalf("in-flight = %d after an unconfirmed finish, want 0", m.InFlight())
	}

	// Retention is bounded: the retained file is no longer tracked as live, so
	// the stale janitor reaps it exactly like a delivered upload.
	if removed := m.Sweep(); removed != 0 {
		t.Fatalf("fresh sweep removed %d retained files, want 0", removed)
	}
	now = now.Add(2 * time.Hour)
	if removed := m.Sweep(); removed != 1 {
		t.Fatalf("stale sweep removed %d files, want 1 — the retained upload must not leak", removed)
	}
	assertUploadDirEmpty(t, m)
}

func TestCancelDeletesPartialFile(t *testing.T) {
	m, _ := newTestManager(t)
	mustStart(t, m, 100)
	if _, err := m.Chunk("attach-1", "up-1", 0, []byte("partial")); err != nil {
		t.Fatalf("Chunk: %v", err)
	}
	m.Cancel("attach-1", "up-1")
	assertUploadDirEmpty(t, m)
	if m.InFlight() != 0 {
		t.Fatalf("in-flight = %d after cancel, want 0", m.InFlight())
	}
	// Cancelling an unknown id must be a no-op, not a panic.
	m.Cancel("attach-1", "up-1")
}

func TestCancelAttachDropsOnlyThatAttach(t *testing.T) {
	m, _ := newTestManager(t)
	if err := m.Start("attach-1", "a", "chat-1", "a.txt", 10); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := m.Start("attach-1", "b", "chat-1", "b.txt", 10); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := m.Start("attach-2", "c", "chat-2", "c.txt", 10); err != nil {
		t.Fatalf("Start: %v", err)
	}
	m.CancelAttach("attach-1")
	if m.InFlight() != 1 {
		t.Fatalf("in-flight = %d after CancelAttach, want 1", m.InFlight())
	}
	m.CancelAll()
	if m.InFlight() != 0 {
		t.Fatalf("in-flight = %d after CancelAll, want 0", m.InFlight())
	}
	assertUploadDirEmpty(t, m)
}

func TestSweepPrunesStaleCompletedFilesButKeepsFreshAndLiveOnes(t *testing.T) {
	now := time.Now()
	clock := func() time.Time { return now }
	m, _ := newTestManager(t, WithClock(clock), WithStaleTTL(time.Hour), WithIdleTimeout(time.Hour))

	// One completed upload.
	mustStart(t, m, 3)
	if _, err := m.Chunk("attach-1", "up-1", 0, []byte("abc")); err != nil {
		t.Fatalf("Chunk: %v", err)
	}
	done, err := m.Finish(context.Background(), "attach-1", "up-1")
	if err != nil {
		t.Fatalf("Finish: %v", err)
	}
	// One live in-flight upload, kept fresh.
	if err := m.Start("attach-2", "up-2", "chat-2", "live.txt", 10); err != nil {
		t.Fatalf("Start: %v", err)
	}

	if removed := m.Sweep(); removed != 0 {
		t.Fatalf("fresh sweep removed %d files, want 0", removed)
	}
	if _, err := os.Stat(done); err != nil {
		t.Fatalf("completed file was pruned too early: %v", err)
	}

	// Advance past the TTL but keep the in-flight upload touched so only
	// the completed file ages out.
	now = now.Add(2 * time.Hour)
	if _, err := m.Chunk("attach-2", "up-2", 0, []byte("x")); err != nil {
		t.Fatalf("Chunk on live upload: %v", err)
	}
	if removed := m.Sweep(); removed != 1 {
		t.Fatalf("stale sweep removed %d files, want 1", removed)
	}
	if _, err := os.Stat(done); !os.IsNotExist(err) {
		t.Fatalf("stale completed file still present: %v", err)
	}
	if m.InFlight() != 1 {
		t.Fatalf("live upload was swept; in-flight = %d, want 1", m.InFlight())
	}
}

func TestSweepDropsIdleInFlightUploads(t *testing.T) {
	now := time.Now()
	m, _ := newTestManager(t, WithClock(func() time.Time { return now }), WithIdleTimeout(time.Minute))
	mustStart(t, m, 100)
	if _, err := m.Chunk("attach-1", "up-1", 0, []byte("partial")); err != nil {
		t.Fatalf("Chunk: %v", err)
	}
	now = now.Add(10 * time.Minute)
	m.Sweep()
	if m.InFlight() != 0 {
		t.Fatalf("idle upload survived the sweep; in-flight = %d", m.InFlight())
	}
	assertUploadDirEmpty(t, m)
	if _, err := m.Chunk("attach-1", "up-1", 1, []byte("more")); err == nil {
		t.Fatal("expected a further chunk on a swept upload to be rejected")
	}
}

func TestRunJanitorStopsOnContextCancel(t *testing.T) {
	m, _ := newTestManager(t)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		m.RunJanitor(ctx, 10*time.Millisecond)
		close(done)
	}()
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("RunJanitor did not return after context cancel")
	}
}

// The upload directory now lives in the system temp dir, whose namespace is
// shared on a Linux host. A symlink planted at the path must be refused, not
// followed: following it would write uploads into the attacker's chosen
// directory and let Sweep delete aged files there.
func TestNewManagerRefusesASymlinkedUploadDir(t *testing.T) {
	base := t.TempDir()
	target := filepath.Join(base, "target")
	if err := os.MkdirAll(target, 0o700); err != nil {
		t.Fatalf("create symlink target: %v", err)
	}
	link := filepath.Join(base, "uploads")
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlinks unavailable on this platform: %v", err)
	}

	m, err := NewManager(link, &fakeSender{})
	if err == nil {
		t.Fatalf("NewManager accepted a symlinked upload dir (%q)", m.Dir())
	}
	if !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("error %v does not name the symlink as the reason", err)
	}
}

func TestComposerTextIsThePathAndNothingElse(t *testing.T) {
	const path = "/tmp/uploads/upload-123.pdf"
	text := ComposerText(path)
	// Anything beyond the path lands in a composer the user is about to type
	// in, so assert the WHOLE string rather than that it merely contains the
	// path — a prose wrapper would satisfy a Contains check. Padding is
	// included in "anything": the prefill delivery types TrimSpace of this
	// payload, so a trailing space added here would be silently dropped and
	// this function would no longer describe what reaches the pane.
	if text != path {
		t.Fatalf("composer text = %q, want the bare path %q", text, path)
	}
	// A newline in a prefill is an Enter into the composer: it would submit
	// the very message this path exists not to submit.
	if strings.ContainsAny(text, "\n\r") {
		t.Fatalf("composer text must be a single line, got %q", text)
	}
}

func TestSafeExtIgnoresHostileExtensions(t *testing.T) {
	cases := map[string]string{
		"notes.txt":             ".txt",
		"archive.tar.gz":        ".gz",
		"no-extension":          "",
		"weird.":                "",
		"payload.thisiswaylong": "",
		"trick.tx/t":            "",
	}
	for name, want := range cases {
		if got := safeExt(name); got != want {
			t.Fatalf("safeExt(%q) = %q, want %q", name, got, want)
		}
	}
}

// uploadDirFiles lists the absolute paths currently held in the upload dir.
func uploadDirFiles(t *testing.T, m *Manager) []string {
	t.Helper()
	entries, err := os.ReadDir(m.Dir())
	if err != nil {
		t.Fatalf("read upload dir: %v", err)
	}
	paths := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		paths = append(paths, filepath.Join(m.Dir(), e.Name()))
	}
	return paths
}

func assertUploadDirEmpty(t *testing.T, m *Manager) {
	t.Helper()
	entries, err := os.ReadDir(m.Dir())
	if err != nil {
		t.Fatalf("read upload dir: %v", err)
	}
	if len(entries) != 0 {
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Fatalf("upload dir should be empty, found: %v", names)
	}
}

// A pre-created volume keeps its own mode through os.MkdirAll, so the
// owner-only guarantee has to be re-asserted rather than assumed.
func TestNewManagerNarrowsPreExistingWideDir(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "uploads")
	if err := os.MkdirAll(dir, 0o777); err != nil {
		t.Fatalf("pre-create upload dir: %v", err)
	}
	// Defeat umask, so the directory really is world-writable going in.
	if err := os.Chmod(dir, 0o777); err != nil {
		t.Fatalf("widen upload dir: %v", err)
	}
	if info, err := os.Stat(dir); err != nil {
		t.Fatalf("stat pre-existing dir: %v", err)
	} else if info.Mode().Perm() != 0o777 {
		t.Fatalf("precondition: dir mode = %o, want 777", info.Mode().Perm())
	}

	if _, err := NewManager(dir, &fakeSender{}); err != nil {
		t.Fatalf("NewManager on existing dir: %v", err)
	}

	info, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("stat upload dir: %v", err)
	}
	if got := info.Mode().Perm(); got != dirPerm {
		t.Fatalf("upload dir mode = %o, want %o", got, dirPerm)
	}
}

func TestNewManagerRejectsNonDirectoryPath(t *testing.T) {
	path := filepath.Join(t.TempDir(), "uploads")
	if err := os.WriteFile(path, []byte("not a dir"), 0o600); err != nil {
		t.Fatalf("write blocking file: %v", err)
	}
	if _, err := NewManager(path, &fakeSender{}); err == nil {
		t.Fatal("NewManager should reject a path that is not a directory")
	}
}

func TestStartBoundsConcurrentUploadsPerAttach(t *testing.T) {
	m, _ := newTestManager(t)
	for i := range MaxActiveUploadsPerAttach {
		id := fmt.Sprintf("up-%d", i)
		if err := m.Start("attach-1", id, "chat-1", "notes.txt", 16); err != nil {
			t.Fatalf("Start %s: %v", id, err)
		}
	}

	err := m.Start("attach-1", "one-too-many", "chat-1", "notes.txt", 16)
	ue := uploadErr(t, err)
	if !ue.Retryable {
		t.Fatalf("per-attach limit should be retryable, got %+v", ue)
	}
	if got := m.InFlight(); got != MaxActiveUploadsPerAttach {
		t.Fatalf("in-flight uploads = %d, want %d", got, MaxActiveUploadsPerAttach)
	}
	// The rejected start must not have leaked a file descriptor or a file.
	entries, err := os.ReadDir(m.Dir())
	if err != nil {
		t.Fatalf("read upload dir: %v", err)
	}
	if len(entries) != MaxActiveUploadsPerAttach {
		t.Fatalf("upload dir has %d files, want %d", len(entries), MaxActiveUploadsPerAttach)
	}

	// A different attach still has its own budget.
	if err := m.Start("attach-2", "up-0", "chat-1", "notes.txt", 16); err != nil {
		t.Fatalf("second attach should have its own budget: %v", err)
	}
}

func TestStartBoundsGlobalConcurrentUploads(t *testing.T) {
	m, _ := newTestManager(t)
	// Spread across attaches so the per-attach limit is not what trips.
	for i := range MaxActiveUploads {
		attach := fmt.Sprintf("attach-%d", i/MaxActiveUploadsPerAttach)
		id := fmt.Sprintf("up-%d", i)
		if err := m.Start(attach, id, "chat-1", "notes.txt", 16); err != nil {
			t.Fatalf("Start %s/%s: %v", attach, id, err)
		}
	}

	ue := uploadErr(t, m.Start("attach-fresh", "up-x", "chat-1", "notes.txt", 16))
	if !ue.Retryable {
		t.Fatalf("global limit should be retryable, got %+v", ue)
	}
	if got := m.InFlight(); got != MaxActiveUploads {
		t.Fatalf("in-flight uploads = %d, want %d", got, MaxActiveUploads)
	}
}

// MaxUploadBytes is per file, so the aggregate bound is a separate gate:
// four maximum-size uploads fit, and the next one must not.
func TestStartBoundsAggregateDeclaredBytes(t *testing.T) {
	m, _ := newTestManager(t)
	var admitted uint64
	for i := 0; ; i++ {
		if admitted+MaxUploadBytes > MaxActiveBytes {
			break
		}
		attach := fmt.Sprintf("attach-%d", i)
		if err := m.Start(attach, "up-0", "chat-1", "notes.txt", MaxUploadBytes); err != nil {
			t.Fatalf("Start %s: %v", attach, err)
		}
		admitted += MaxUploadBytes
	}
	if m.InFlight() >= MaxActiveUploads {
		t.Fatalf("precondition: byte gate must trip before the count gate, in-flight = %d", m.InFlight())
	}

	ue := uploadErr(t, m.Start("attach-last", "up-0", "chat-1", "notes.txt", MaxUploadBytes))
	if !ue.Retryable {
		t.Fatalf("aggregate byte limit should be retryable, got %+v", ue)
	}
	// Nothing was written; the declared sizes alone drove the rejection.
	assertNoPartialBytesOnDisk(t, m)
}

// completeUpload runs one whole upload to a successful finish, which is
// the shape that defeats the in-flight gates: nothing is ever concurrent,
// yet each finish leaves a retained file behind for the stale TTL.
func completeUpload(t *testing.T, m *Manager, uploadID string, payload []byte) string {
	t.Helper()
	if err := m.Start("attach-1", uploadID, "chat-1", "notes.txt", uint64(len(payload))); err != nil {
		t.Fatalf("Start %s: %v", uploadID, err)
	}
	if _, err := m.Chunk("attach-1", uploadID, 0, payload); err != nil {
		t.Fatalf("Chunk %s: %v", uploadID, err)
	}
	path, err := m.Finish(context.Background(), "attach-1", uploadID)
	if err != nil {
		t.Fatalf("Finish %s: %v", uploadID, err)
	}
	return path
}

func TestStartBoundsRetainedCompletedBytes(t *testing.T) {
	m, _ := newTestManager(t, WithDirQuota(16, 0))
	completeUpload(t, m, "up-1", []byte("12345678"))
	completeUpload(t, m, "up-2", []byte("12345678"))
	if m.InFlight() != 0 {
		t.Fatalf("in-flight = %d, want 0 (uploads ran strictly serially)", m.InFlight())
	}

	// 16 retained bytes now fill the quota, so even a one-byte upload has
	// to be refused: the in-flight gates see an idle manager.
	ue := uploadErr(t, m.Start("attach-1", "up-3", "chat-1", "notes.txt", 1))
	if !ue.Retryable {
		t.Fatalf("retained byte limit should be retryable, got %+v", ue)
	}
	entries, err := os.ReadDir(m.Dir())
	if err != nil {
		t.Fatalf("read upload dir: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("upload dir has %d files, want the 2 retained ones only", len(entries))
	}
}

func TestStartBoundsRetainedFileCount(t *testing.T) {
	m, _ := newTestManager(t, WithDirQuota(0, 2))
	completeUpload(t, m, "up-1", []byte("a"))
	completeUpload(t, m, "up-2", []byte("b"))

	// Two one-byte files are nowhere near any byte quota; only the entry
	// count can reject this.
	ue := uploadErr(t, m.Start("attach-1", "up-3", "chat-1", "notes.txt", 1))
	if !ue.Retryable {
		t.Fatalf("retained file limit should be retryable, got %+v", ue)
	}
	if !strings.Contains(ue.Msg, "file limit") {
		t.Fatalf("expected the file-count limit to reject this, got %q", ue.Msg)
	}
}

// The quota is measured, not counted, so the janitor deleting a retained
// file must hand its budget straight back — otherwise the manager wedges
// shut one TTL window after the first burst.
func TestSweepReleasesRetainedQuota(t *testing.T) {
	now := time.Now()
	m, _ := newTestManager(t,
		WithClock(func() time.Time { return now }),
		WithStaleTTL(time.Hour),
		WithDirQuota(8, 0),
	)
	completeUpload(t, m, "up-1", []byte("12345678"))
	if ue := uploadErr(t, m.Start("attach-1", "up-2", "chat-1", "notes.txt", 1)); !ue.Retryable {
		t.Fatalf("precondition: quota should be full and retryable, got %+v", ue)
	}

	now = now.Add(2 * time.Hour)
	if removed := m.Sweep(); removed != 1 {
		t.Fatalf("stale sweep removed %d files, want 1", removed)
	}
	if err := m.Start("attach-1", "up-2", "chat-1", "notes.txt", 8); err != nil {
		t.Fatalf("quota did not recover after the sweep: %v", err)
	}
}

// Measuring the directory only sees bytes already written, so two uploads
// started before either is filled both look affordable on their own: with
// a 10-byte quota and 6 retained bytes, each 4-byte start sees 6+4. Filling
// both would then leave 14 bytes on disk. Admission has to reserve the
// unwritten remainder of every upload already in flight.
func TestStartReservesUnfinishedUploadBytesAgainstDirQuota(t *testing.T) {
	const quota = 10
	m, _ := newTestManager(t, WithDirQuota(quota, 0))
	completeUpload(t, m, "up-0", []byte("123456")) // 6 retained bytes.

	if err := m.Start("attach-1", "up-1", "chat-1", "a.txt", 4); err != nil {
		t.Fatalf("the first 4-byte upload fits the remaining quota, got %v", err)
	}
	// Deliberately write none of up-1's bytes: its file is empty on disk, so
	// a usage-only check sees 6+4 all over again and admits the second one.
	err := m.Start("attach-1", "up-2", "chat-1", "b.txt", 4)
	if err == nil {
		t.Fatal("second concurrent upload was admitted: the first upload's 4 unwritten bytes were not reserved against the directory quota")
	}
	ue := uploadErr(t, err)
	if !ue.Retryable {
		t.Fatalf("quota rejection should be retryable, got %+v", ue)
	}
	if !strings.Contains(ue.Msg, "byte limit") {
		t.Fatalf("expected the retained byte limit to reject this, got %q", ue.Msg)
	}

	// Filling every upload that *was* admitted must stay inside the quota.
	if _, err := m.Chunk("attach-1", "up-1", 0, []byte("abcd")); err != nil {
		t.Fatalf("Chunk: %v", err)
	}
	if _, err := m.Finish(context.Background(), "attach-1", "up-1"); err != nil {
		t.Fatalf("Finish: %v", err)
	}
	if got, _ := m.dirUsage(); got > quota {
		t.Fatalf("upload dir holds %d bytes, over the %d-byte quota", got, quota)
	}
}

// Finish publishes its path into a chat, and the agent reading it resolves
// relative names against its own worktree — so a relative upload dir would
// name a file the agent cannot open.
func TestNewManagerResolvesRelativeDirToAbsolutePublishedPath(t *testing.T) {
	t.Chdir(t.TempDir())
	sender := &fakeSender{}
	m, err := NewManager(filepath.Join("relative", "uploads"), sender)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	if !filepath.IsAbs(m.Dir()) {
		t.Fatalf("Dir() = %q, want an absolute path", m.Dir())
	}

	path := completeUpload(t, m, "up-1", []byte("abc"))
	if !filepath.IsAbs(path) {
		t.Fatalf("Finish returned %q, want an absolute path", path)
	}
	if len(sender.calls) != 1 {
		t.Fatalf("chat messages sent = %d, want exactly 1", len(sender.calls))
	}
	if !strings.Contains(sender.calls[0].message, path) {
		t.Fatalf("published message %q does not name the absolute path %q", sender.calls[0].message, path)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("published path is not openable as given: %v", err)
	}
}

func assertNoPartialBytesOnDisk(t *testing.T, m *Manager) {
	t.Helper()
	entries, err := os.ReadDir(m.Dir())
	if err != nil {
		t.Fatalf("read upload dir: %v", err)
	}
	for _, e := range entries {
		info, err := e.Info()
		if err != nil {
			t.Fatalf("stat %s: %v", e.Name(), err)
		}
		if info.Size() != 0 {
			t.Fatalf("%s has %d bytes, want 0", e.Name(), info.Size())
		}
	}
}

// --- BOS-661 review round 1: finish claiming and cancellation semantics ---

// ClaimFinish is what bounds delivery-goroutine spawn: the caller runs it on
// its reader goroutine and only spawns on nil. If a repeat were admitted, a
// client could turn a ~30-byte frame into an unbounded goroutine pile.
func TestClaimFinishAdmitsOnceAndRefusesRepeats(t *testing.T) {
	m, _ := newTestManager(t)
	mustStart(t, m, 3)
	if _, err := m.Chunk("attach-1", "up-1", 0, []byte("abc")); err != nil {
		t.Fatalf("Chunk: %v", err)
	}
	if err := m.ClaimFinish("attach-1", "up-1"); err != nil {
		t.Fatalf("first ClaimFinish: %v", err)
	}
	for i := range 3 {
		ue := uploadErr(t, m.ClaimFinish("attach-1", "up-1"))
		if ue.Retryable {
			t.Fatalf("repeat %d: a duplicate finish must be permanent, got %v", i, ue)
		}
		if !strings.Contains(ue.Msg, "duplicate finish") {
			t.Fatalf("repeat %d: error %q does not name the duplicate finish", i, ue.Msg)
		}
	}
	// The claim must not have disturbed the upload: the real finish still
	// delivers exactly once.
	if _, err := m.Finish(context.Background(), "attach-1", "up-1"); err != nil {
		t.Fatalf("Finish after claim: %v", err)
	}
}

func TestClaimFinishRejectsUnknownUpload(t *testing.T) {
	m, _ := newTestManager(t)
	if ue := uploadErr(t, m.ClaimFinish("attach-1", "nope")); ue.Retryable {
		t.Fatalf("unknown upload_id must be permanent, got %v", ue)
	}
}

// A claimed upload's byte total is already fixed and its file may be
// mid-sync, so a late chunk must not append to it.
func TestChunkAfterClaimFinishIsRejectedAndDeletesPartial(t *testing.T) {
	m, _ := newTestManager(t)
	mustStart(t, m, 10)
	if _, err := m.Chunk("attach-1", "up-1", 0, []byte("abc")); err != nil {
		t.Fatalf("Chunk: %v", err)
	}
	if err := m.ClaimFinish("attach-1", "up-1"); err != nil {
		t.Fatalf("ClaimFinish: %v", err)
	}
	ue := uploadErr(t, func() error {
		_, err := m.Chunk("attach-1", "up-1", 1, []byte("def"))
		return err
	}())
	if ue.Retryable {
		t.Fatalf("a chunk after finish must be permanent, got %v", ue)
	}
	assertUploadDirEmpty(t, m)
}

// The daemon's stream teardown cancels the delivery context BEFORE joining
// the delivery goroutine, so an ordinary stream blip lands every in-flight
// finish on a cancelled context. That is epistemically identical to an
// explicit ErrDeliveryUnconfirmed — the submit may already be running in the
// pane — so the bytes must SURVIVE rather than be deleted underneath a
// prompt that already names them.
func TestFinishRetainsFileWhenDeliveryContextIsCancelled(t *testing.T) {
	now := time.Now()
	clock := func() time.Time { return now }
	m, sender := newTestManager(t, WithClock(clock), WithStaleTTL(time.Hour), WithIdleTimeout(time.Hour))
	// A bare, non-sentinel error — exactly what a context-cancelled RPC
	// surfaces. Without the ctx.Err() check this takes the delete branch.
	sender.err = errors.New("context canceled")
	mustStart(t, m, 3)
	if _, err := m.Chunk("attach-1", "up-1", 0, []byte("abc")); err != nil {
		t.Fatalf("Chunk: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := m.Finish(ctx, "attach-1", "up-1")
	ue := uploadErr(t, err)
	if ue.Retryable {
		t.Fatalf("an unconfirmed submit must not invite a resend, got retryable %v", ue)
	}
	if !strings.Contains(ue.Msg, "unconfirmed") {
		t.Fatalf("error %q does not name the unconfirmed delivery", ue.Msg)
	}

	retained := uploadDirFiles(t, m)
	if len(retained) != 1 {
		t.Fatalf("upload files after a cancelled submit = %v, want the retained upload", retained)
	}
	if body, readErr := os.ReadFile(retained[0]); readErr != nil || string(body) != "abc" {
		t.Fatalf("retained file contents = %q (err %v), want %q", body, readErr, "abc")
	}

	// Retention stays bounded by the janitor, exactly as for the sentinel arm.
	now = now.Add(2 * time.Hour)
	if removed := m.Sweep(); removed != 1 {
		t.Fatalf("stale sweep removed %d files, want 1 — the retained upload must not leak", removed)
	}
	assertUploadDirEmpty(t, m)
}

// The guard must be the CONTEXT, not the error text: a live context with a
// genuine delivery failure still deletes, or every real failure would leak.
func TestFinishStillRemovesFileWhenDeliveryFailsOnLiveContext(t *testing.T) {
	m, sender := newTestManager(t)
	sender.err = errors.New("context canceled")
	mustStart(t, m, 3)
	if _, err := m.Chunk("attach-1", "up-1", 0, []byte("abc")); err != nil {
		t.Fatalf("Chunk: %v", err)
	}
	if _, err := m.Finish(context.Background(), "attach-1", "up-1"); err == nil {
		t.Fatal("expected a delivery failure")
	}
	assertUploadDirEmpty(t, m)
}

// RunJanitor must sweep BEFORE arming its ticker. Without the immediate
// pass, files inherited from a previous daemon run sit untouched for a whole
// DefaultJanitorInterval (an hour), and an upload abandoned just before a
// restart holds its quota reservation for up to interval+idle.
func TestRunJanitorSweepsImmediatelyBeforeTheFirstTick(t *testing.T) {
	now := time.Now()
	m, _ := newTestManager(t, WithClock(func() time.Time { return now }), WithStaleTTL(time.Hour))

	// A completed file left behind by an earlier run, already past the TTL.
	stale := filepath.Join(m.Dir(), "upload-inherited.bin")
	if err := os.WriteFile(stale, []byte("old"), 0o600); err != nil {
		t.Fatalf("seed stale file: %v", err)
	}
	old := now.Add(-2 * time.Hour)
	if err := os.Chtimes(stale, old, old); err != nil {
		t.Fatalf("age stale file: %v", err)
	}

	// An interval far longer than the test will ever wait, so ONLY the
	// startup sweep can be what removes the file.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		m.RunJanitor(ctx, time.Hour)
	}()

	deadline := time.After(3 * time.Second)
	for {
		if _, err := os.Stat(stale); os.IsNotExist(err) {
			break
		}
		select {
		case <-deadline:
			t.Fatal("the inherited stale file survived janitor startup; the first sweep is a whole interval away")
		default:
			time.Sleep(5 * time.Millisecond)
		}
	}
	cancel()
	<-done
}

// --- BOS-661 review round 2 --------------------------------------------------

// The display name is carried to the daemon inside TerminalUploadStart.Filename,
// a proto3 `string`, and google.golang.org/protobuf refuses to marshal invalid
// UTF-8 in one — on bosso's side that error tears down the TerminalStream
// shared by every attach on this daemon. ValidateFilename is documented as the
// exact mirror of bosso's validator, so the rule has to exist at both ends.
func TestValidateFilenameRejectsInvalidUTF8(t *testing.T) {
	bad := string([]byte{'r', 'e', 'p', 0xff, 0xfe, '.', 'p', 'd', 'f'})

	err := ValidateFilename(bad)
	ue := uploadErr(t, err)
	if ue.Retryable {
		t.Errorf("a malformed name is permanent, got retryable %v", ue)
	}
	if !strings.Contains(ue.Msg, "UTF-8") {
		t.Errorf("rejection = %q, want it to name UTF-8", ue.Msg)
	}

	// And the admission path refuses it before a descriptor is opened.
	m, _ := newTestManager(t)
	if err := m.Start("attach-1", "up-1", "chat-1", bad, 4); err == nil {
		t.Fatal("Start admitted a non-UTF-8 filename")
	}
	if m.InFlight() != 0 {
		t.Errorf("in-flight = %d after a rejected start, want 0", m.InFlight())
	}
	assertUploadDirEmpty(t, m)
}

// blockingSender holds SendChatMessage open so a test can act on the manager
// while a delivery is genuinely in flight.
type blockingSender struct {
	entered chan struct{}
	release chan struct{}
	mu      sync.Mutex
	calls   int
}

func (b *blockingSender) SendChatMessage(_ context.Context, _, _ string, _ bool) error {
	b.mu.Lock()
	b.calls++
	b.mu.Unlock()
	close(b.entered)
	<-b.release
	return nil
}

// Finish detaches the file handle and drops the registry entry BEFORE it
// syncs, closes and delivers, so that a 500 MiB fsync cannot be held across
// the mutex that the daemon's single terminal-stream reader needs for every
// other upload frame (and, behind it, terminal input, resize and the BOS-376
// pongs). This test pins BOTH halves of that ownership transfer.
//
// Liveness — the half the transfer exists for: while Finish is inside the
// flush, m.mu must be free, so the reader goroutine's own registry calls
// still run. The flush is the only place the difference is observable, which
// is why the sync is a seam (Manager.syncFile): the delivery-time assertions
// below cannot see it, because the pre-fix code ALSO released the lock and
// dropped the entry before calling the sender. Blocking the sender alone
// therefore proves nothing about where the lock was held.
//
// Safety — once Finish has claimed the upload, no cancel path may reach its
// bytes: a late Cancel/CancelAttach/CancelAll/Sweep must not delete a file
// whose prompt is already being submitted.
func TestFinishOwnsTheFileOnceItStartsDelivering(t *testing.T) {
	sender := &blockingSender{entered: make(chan struct{}), release: make(chan struct{})}
	m, err := NewManager(filepath.Join(t.TempDir(), "uploads"), sender)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	// Park Finish inside the flush. This stands in for the multi-second
	// fsync of a large upload, which is the whole reason the handle is
	// detached under the lock rather than flushed under it.
	inFlush := make(chan struct{})
	releaseFlush := make(chan struct{})
	m.syncFile = func(f *os.File) error {
		close(inFlush)
		<-releaseFlush
		return f.Sync()
	}
	mustStart(t, m, 3)
	if _, err := m.Chunk("attach-1", "up-1", 0, []byte("abc")); err != nil {
		t.Fatalf("Chunk: %v", err)
	}
	if err := m.ClaimFinish("attach-1", "up-1"); err != nil {
		t.Fatalf("ClaimFinish: %v", err)
	}

	type outcome struct {
		path string
		err  error
	}
	done := make(chan outcome, 1)
	go func() {
		path, err := m.Finish(context.Background(), "attach-1", "up-1")
		done <- outcome{path, err}
	}()

	<-inFlush
	// The registry lock must be free while the flush runs. Probe it with the
	// calls the terminal-stream reader goroutine actually makes — a Cancel
	// for an unrelated id (a no-op that still takes m.mu) and Start, the
	// admission path a second upload would arrive on.
	probed := make(chan error, 1)
	go func() {
		m.Cancel("attach-2", "up-2")
		probed <- m.Start("attach-2", "up-2", "chat-1", "other.txt", 4)
	}()
	select {
	case err := <-probed:
		if err != nil {
			t.Fatalf("Start during the upload flush: %v", err)
		}
	case <-time.After(5 * time.Second):
		close(releaseFlush)
		t.Fatal("the registry lock was held across the upload flush: a concurrent Cancel/Start never returned, so a 500 MiB fsync would park the daemon's terminal-stream reader")
	}
	// Drop the probe upload again so the delivery-time assertions below see
	// only the upload under test.
	m.Cancel("attach-2", "up-2")
	close(releaseFlush)

	<-sender.entered
	// Every teardown path, while the submission is in flight. None may touch
	// the file, and none may block: the registry lock is not held here.
	m.Cancel("attach-1", "up-1")
	m.CancelAttach("attach-1")
	m.CancelAll()
	if got := m.InFlight(); got != 0 {
		t.Errorf("in-flight = %d during delivery, want 0 — Finish already claimed the upload", got)
	}
	m.Sweep()
	close(sender.release)

	got := <-done
	if got.err != nil {
		t.Fatalf("Finish: %v", got.err)
	}
	body, readErr := os.ReadFile(got.path)
	if readErr != nil {
		t.Fatalf("the delivered file was removed underneath its own prompt: %v", readErr)
	}
	if string(body) != "abc" {
		t.Fatalf("delivered file contents = %q, want %q", body, "abc")
	}
}
