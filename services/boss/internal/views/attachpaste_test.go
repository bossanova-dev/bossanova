package views

import (
	"context"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"reflect"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"

	bosspty "github.com/recurser/boss/internal/pty"
	"github.com/recurser/boss/internal/remotehost"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

// uploaderCalls records what a faked uploader was asked to do. Every field is a
// count or a captured argument, so a test can assert an ABSENCE — which is what
// the local-mode guarantee is made of.
type uploaderCalls struct {
	constructed int
	ensures     int
	ensureKeys  []string
	uploads     [][2]string // localPath, remoteDir
	removes     []string
	ctxs        []context.Context
}

// fakeUploader returns an uploader whose three remotehost calls are recorded
// rather than run. No test in this file may reach a real ssh: the failures they
// describe (a directory created for a user who never pasted, a temp dir left
// behind on detach) are all about calls that either happen or do not.
func fakeUploader(destination, sessionKey string, calls *uploaderCalls) *remotePasteUploader {
	u := newRemotePasteUploader(destination, sessionKey)
	u.ensureDir = func(ctx context.Context, opts remotehost.Options, key string) (string, error) {
		calls.ensures++
		calls.ensureKeys = append(calls.ensureKeys, key)
		calls.ctxs = append(calls.ctxs, ctx)
		if opts.Destination != destination {
			return "", errors.New("ensure got destination " + opts.Destination)
		}
		return "/home/deploy/.cache/boss/uploads/" + key, nil
	}
	u.uploadFile = func(ctx context.Context, opts remotehost.Options, localPath, remoteDir string) (string, error) {
		calls.uploads = append(calls.uploads, [2]string{localPath, remoteDir})
		calls.ctxs = append(calls.ctxs, ctx)
		if opts.Destination != destination {
			return "", errors.New("upload got destination " + opts.Destination)
		}
		return remoteDir + "/ab12cd/shot.png", nil
	}
	u.removeDir = func(ctx context.Context, opts remotehost.Options, remoteDir string) error {
		calls.removes = append(calls.removes, remoteDir)
		calls.ctxs = append(calls.ctxs, ctx)
		if opts.Destination != destination {
			return errors.New("remove got destination " + opts.Destination)
		}
		return nil
	}
	return u
}

// withFakeUploaderFactory swaps the constructor the attach path uses, so a test
// can see whether it was called at all and hand back a recording uploader when
// it was. Restored with t.Cleanup — it is a package-level var.
func withFakeUploaderFactory(t *testing.T, calls *uploaderCalls) {
	t.Helper()
	previous := newPasteUploader
	t.Cleanup(func() { newPasteUploader = previous })
	newPasteUploader = func(destination, sessionKey string) *remotePasteUploader {
		calls.constructed++
		return fakeUploader(destination, sessionKey, calls)
	}
}

// TestAttach_LocalAttachInstallsNoPasteUploader is the load-bearing half of the
// remote-only wiring, and it deliberately asserts absences rather than "nothing
// crashed": an unconditionally-installed uploader crashes nothing at all on a
// local attach — it just quietly puts an ssh copy in the path of every pasted
// image on a machine the agent is already running on.
//
// What the un-installed nil guarantees is the half only an uploader could
// break: with no uploader there is nothing a claimed paste could be handed to,
// so a local paste cannot be swallowed. It is NOT by itself what keeps the
// local stdin path byte-identical any more — since BOS-849 that attach installs
// the non-claiming notice and internal/pty does run its scanner, with
// conservation resting on imagePasteNoticeClaim's constant false (see
// SetPasteUploader and HasPasteUploader, both rewritten for this). Read this
// test together with its sibling below, which asserts the notice's presence.
func TestAttach_LocalAttachInstallsNoPasteUploader(t *testing.T) {
	withHostDestination(t, "")
	var calls uploaderCalls
	withFakeUploaderFactory(t, &calls)

	m := NewAttachModel(&attachTelemetryStub{}, context.Background(), bosspty.NewManager(), "session-1", "")
	// Two-step launch: attachReadyMsg only stages the off-loop RecordChat, and
	// the uploader wiring lives downstream of it in the chatRecordedMsg handler
	// (BOS-723). Driving only the first step would leave pendingExec nil and
	// make every absence asserted below vacuously true.
	got, _ := driveAttachReady(t, m, attachReadyMsg{
		session: &pb.Session{Id: "session-1", WorktreePath: "/Users/dev/worktrees/feature"},
	})

	if got.pendingExec == nil || got.pendingExec.ptycmd == nil {
		t.Fatal("pendingExec.ptycmd = nil, want the staged PTY command")
	}
	if got.pendingExec.ptycmd.HasPasteUploader() {
		t.Fatal("a local attach installed a paste uploader; with no uploader there is nothing a claimed paste could be handed to, so a local paste cannot be swallowed")
	}
	if got.pasteUploader != nil {
		t.Fatal("model kept a paste uploader on a local attach")
	}
	if calls.constructed != 0 {
		t.Fatalf("uploader constructed %d times locally, want 0", calls.constructed)
	}
	if calls.ensures != 0 || len(calls.uploads) != 0 {
		t.Fatalf("local attach consulted the remote machinery: %d ensures, %d uploads, want none",
			calls.ensures, len(calls.uploads))
	}
	// A local attach has nothing to clean up either — a cleanup command here
	// would mean an ssh on every local detach.
	if cmd := got.cleanupRemoteUploads(); cmd != nil {
		t.Fatal("local attach produced a remote-cleanup command")
	}
}

// TestAttach_LocalAttachInstallsTheImagePasteNotice is the twin of the test
// above, and the two are only meaningful together.
//
// A local attach installs no uploader — that is what the sibling asserts, and it
// is ALSO the no-claim property: swallowing a paste requires an uploader to hand
// it to, and internal/pty's claim path returns false for every body without one
// (TestImagePasteNoticeNeverClaimsAnything pins that constant directly, against
// the closure). So the pair "notice installed, uploader absent" is exactly
// "inspects pastes, cannot consume one".
//
// Asserting the notice's PRESENCE here is what stops the else branch from
// silently becoming a no-op. Without it, deleting the branch would leave every
// test in this file green while restoring the original bug: a boss on the far
// side of a plain ssh login inspecting nothing and explaining nothing while a
// pasted screenshot reaches the agent as a path on the wrong machine.
func TestAttach_LocalAttachInstallsTheImagePasteNotice(t *testing.T) {
	withHostDestination(t, "")
	var calls uploaderCalls
	withFakeUploaderFactory(t, &calls)

	m := NewAttachModel(&attachTelemetryStub{}, context.Background(), bosspty.NewManager(), "session-1", "")
	// Two-step launch, for the same reason the sibling documents: the wiring
	// lives in the chatRecordedMsg handler, so driving only attachReadyMsg
	// would make the assertion below vacuous.
	got, _ := driveAttachReady(t, m, attachReadyMsg{
		session: &pb.Session{Id: "session-1", WorktreePath: "/Users/dev/worktrees/feature"},
	})

	if got.pendingExec == nil || got.pendingExec.ptycmd == nil {
		t.Fatal("pendingExec.ptycmd = nil, want the staged PTY command")
	}
	if !got.pendingExec.ptycmd.HasImagePasteNotice() {
		t.Fatal("a local attach installed no image-paste notice: a screenshot pasted from another machine would reach the agent as an unopenable path with nothing to say so")
	}
	// The other half of "cannot claim". Duplicated from the sibling test on
	// purpose: the no-claim property is the CONJUNCTION of these two, and a
	// reader of this test should not have to take the second half on trust.
	if got.pendingExec.ptycmd.HasPasteUploader() {
		t.Fatal("the notice branch also installed an uploader; the notice must never be able to swallow a paste")
	}
	if calls.constructed != 0 {
		t.Fatalf("uploader constructed %d times locally, want 0", calls.constructed)
	}
}

// TestAttach_RemoteAttachInstallsThePasteUploader is the other half: under
// --host the same path must wire the uploader, keyed on the chat.
func TestAttach_RemoteAttachInstallsThePasteUploader(t *testing.T) {
	withHostDestination(t, "deploy@bastion.example.com")
	var calls uploaderCalls
	withFakeUploaderFactory(t, &calls)

	m := NewAttachModel(&attachTelemetryStub{}, context.Background(), bosspty.NewManager(), "session-1", "")
	// Two-step launch — see the local-attach test: the uploader is wired in the
	// chatRecordedMsg handler, so the staging step alone proves nothing.
	got, _ := driveAttachReady(t, m, attachReadyMsg{
		session: &pb.Session{Id: "session-1", WorktreePath: "/home/deploy/worktrees/feature"},
	})

	if got.pendingExec == nil || got.pendingExec.ptycmd == nil {
		t.Fatal("pendingExec.ptycmd = nil, want the staged PTY command")
	}
	if !got.pendingExec.ptycmd.HasPasteUploader() {
		t.Fatal("no paste uploader installed on a --host attach: a dragged-in image would name a local file the remote agent cannot open")
	}
	// And NOT the notice. Under --host the file is reachable and the transfer
	// runs, so "no such image on this machine · use boss --host" would be both
	// wrong and addressed to someone already following its advice; a genuine
	// failure there is reported by the uploader's own injected message.
	if got.pendingExec.ptycmd.HasImagePasteNotice() {
		t.Fatal("a --host attach installed the image-paste notice as well as the uploader")
	}
	if got.pasteUploader == nil {
		t.Fatal("model did not keep the uploader, so detach has nothing to clean up with")
	}
	if got.pasteUploader.destination != "deploy@bastion.example.com" {
		t.Fatalf("uploader destination = %q, want the attach's own destination", got.pasteUploader.destination)
	}
	// Keyed on the chat, not the session: two chats in one session are two
	// panes, and one detaching must not delete the other's pasted images.
	if got.pasteUploader.sessionKey != got.agentSessionID || got.agentSessionID == "" {
		t.Fatalf("uploader sessionKey = %q, want the chat id %q", got.pasteUploader.sessionKey, got.agentSessionID)
	}
	// Wiring only — still no round trip until something is actually pasted.
	if calls.ensures != 0 {
		t.Fatalf("attach made %d ensure calls, want 0 until the first paste", calls.ensures)
	}
}

// TestRemotePasteUploader_EnsuresTheRemoteDirLazilyAndOnce pins the whole cost
// model: nothing on construction, one ensure on the first paste, none on the
// next. Each ensure is an ssh round trip, so a per-paste ensure is a per-paste
// stall and a per-attach ensure creates a directory on the user's host for the
// vast majority of sessions that never paste anything.
func TestRemotePasteUploader_EnsuresTheRemoteDirLazilyAndOnce(t *testing.T) {
	var calls uploaderCalls
	u := fakeUploader("deploy@bastion.example.com", "chat-1", &calls)

	if calls.ensures != 0 || len(calls.uploads) != 0 {
		t.Fatalf("constructing the uploader called out: %d ensures, %d uploads, want none",
			calls.ensures, len(calls.uploads))
	}

	ctx := context.Background()
	first, err := u.upload(ctx, "/Users/dev/Desktop/shot.png")
	if err != nil {
		t.Fatalf("first upload: %v", err)
	}
	if calls.ensures != 1 {
		t.Fatalf("ensures after first upload = %d, want 1", calls.ensures)
	}
	if !reflect.DeepEqual(calls.ensureKeys, []string{"chat-1"}) {
		t.Fatalf("ensure keys = %v, want the chat key", calls.ensureKeys)
	}
	wantUpload := [2]string{"/Users/dev/Desktop/shot.png", "/home/deploy/.cache/boss/uploads/chat-1"}
	if len(calls.uploads) != 1 || calls.uploads[0] != wantUpload {
		t.Fatalf("uploads = %v, want exactly %v", calls.uploads, wantUpload)
	}
	// What comes back is what gets injected into the agent's input box, so it
	// must be the transport's own answer and nothing derived from it.
	if first != "/home/deploy/.cache/boss/uploads/chat-1/ab12cd/shot.png" {
		t.Fatalf("upload returned %q, want the remote path UploadFile reported", first)
	}

	if _, err := u.upload(ctx, "/Users/dev/Desktop/second.png"); err != nil {
		t.Fatalf("second upload: %v", err)
	}
	if calls.ensures != 1 {
		t.Fatalf("ensures after second upload = %d, want the directory to be remembered", calls.ensures)
	}
	if len(calls.uploads) != 2 {
		t.Fatalf("uploads = %d, want 2", len(calls.uploads))
	}

	// The pty layer cancels this context at teardown and then waits, without a
	// timeout, for the upload goroutines to exit — so an uploader that dropped
	// the context on the floor would hang the detach.
	for i, got := range calls.ctxs {
		if got != ctx {
			t.Fatalf("call %d ran under %v, want the caller's context", i, got)
		}
	}
}

// TestRemotePasteUploader_EnsureFailureIsRetryableAndRecordsNoDir covers the
// failure policy: the error reaches the caller (internal/pty turns it into the
// visible "upload failed" message, and injects no path), nothing is recorded for
// cleanup, and — deliberately — the NEXT paste tries again instead of the
// session being permanently unable to paste after one ssh hiccup.
func TestRemotePasteUploader_EnsureFailureIsRetryableAndRecordsNoDir(t *testing.T) {
	var calls uploaderCalls
	u := fakeUploader("deploy@bastion.example.com", "chat-1", &calls)
	boom := errors.New("ssh: connect to host bastion.example.com port 22: connection refused")
	working := u.ensureDir
	u.ensureDir = func(context.Context, remotehost.Options, string) (string, error) { return "", boom }

	if _, err := u.upload(context.Background(), "/Users/dev/Desktop/shot.png"); !errors.Is(err, boom) {
		t.Fatalf("upload error = %v, want the ensure failure so the pty layer can report it", err)
	}
	if len(calls.uploads) != 0 {
		t.Fatalf("uploads = %v, want none: there is no directory to write into", calls.uploads)
	}
	if u.remoteDir != "" {
		t.Fatalf("remoteDir = %q after a failed ensure, want nothing recorded for cleanup", u.remoteDir)
	}
	if err := u.cleanup(context.Background()); err != nil {
		t.Fatalf("cleanup after a failed ensure: %v", err)
	}
	if len(calls.removes) != 0 {
		t.Fatalf("cleanup removed %v though no directory was ever created", calls.removes)
	}

	// Retryable: one refused connection must not disable image pasting for the
	// rest of the attach.
	u.ensureDir = working
	if _, err := u.upload(context.Background(), "/Users/dev/Desktop/shot.png"); err != nil {
		t.Fatalf("retry after a transient ensure failure: %v", err)
	}
	if len(calls.uploads) != 1 {
		t.Fatalf("uploads after retry = %d, want 1", len(calls.uploads))
	}
}

// TestRemotePasteUploader_CleanupWithoutAnUploadNeverTouchesTheHost: the common
// case is a --host attach where nobody pasted anything. Removing a directory
// that was never created would be an ssh connection per detach for no reason.
func TestRemotePasteUploader_CleanupWithoutAnUploadNeverTouchesTheHost(t *testing.T) {
	var calls uploaderCalls
	u := fakeUploader("deploy@bastion.example.com", "chat-1", &calls)

	if err := u.cleanup(context.Background()); err != nil {
		t.Fatalf("cleanup with nothing created: %v", err)
	}
	if len(calls.removes) != 0 {
		t.Fatalf("removeDir called with %v though nothing was ever created", calls.removes)
	}
}

// TestAttach_DetachRemovesTheRemoteUploadDir: the user pressed Ctrl+X, so the
// pane is gone and the pasted images are dead weight on someone else's machine.
func TestAttach_DetachRemovesTheRemoteUploadDir(t *testing.T) {
	var calls uploaderCalls
	u := fakeUploader("deploy@bastion.example.com", "chat-1", &calls)
	u.remoteDir = "/home/deploy/.cache/boss/uploads/chat-1"

	m := NewAttachModel(&attachTelemetryStub{}, context.Background(), bosspty.NewManager(), "session-1", "")
	m.pasteUploader = u

	updated, cmd := m.Update(agentFinishedMsg{detached: true})
	got := updated.(AttachModel)

	if !got.Detached() {
		t.Fatal("Detached() = false after a detach; cleanup must not change the outcome it rides on")
	}
	if cmd == nil {
		t.Fatal("detach returned no command, so the remote upload dir leaks")
	}
	runAttachCmdGraph(t, cmd, func(msg tea.Msg) {
		t.Fatalf("cleanup produced a user-visible message %#v; it must be silent", msg)
	})
	if !reflect.DeepEqual(calls.removes, []string{"/home/deploy/.cache/boss/uploads/chat-1"}) {
		t.Fatalf("removeDir calls = %v, want the resolved upload dir", calls.removes)
	}
}

// TestAttach_ProcessExitRemovesTheRemoteUploadDir: the directory is just as dead
// when the pane exited on its own, on both the clean and the error path, and the
// existing commands on each of those paths must survive the addition.
func TestAttach_ProcessExitRemovesTheRemoteUploadDir(t *testing.T) {
	for _, tc := range []struct {
		name string
		msg  agentFinishedMsg
	}{
		{name: "clean_exit", msg: agentFinishedMsg{}},
		{name: "error_exit", msg: agentFinishedMsg{err: errors.New("exit status 1")}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var calls uploaderCalls
			u := fakeUploader("deploy@bastion.example.com", "chat-1", &calls)
			u.remoteDir = "/home/deploy/.cache/boss/uploads/chat-1"

			m := NewAttachModel(&attachTelemetryStub{}, context.Background(), bosspty.NewManager(), "session-1", "")
			m.pasteUploader = u

			updated, cmd := m.Update(tc.msg)
			got := updated.(AttachModel)

			if !got.returned {
				t.Fatal("returned = false after a process exit")
			}
			if cmd == nil {
				t.Fatal("process exit returned no command, so the remote upload dir leaks")
			}
			runAttachCmdGraph(t, cmd, func(tea.Msg) {})
			if !reflect.DeepEqual(calls.removes, []string{"/home/deploy/.cache/boss/uploads/chat-1"}) {
				t.Fatalf("removeDir calls = %v, want the resolved upload dir", calls.removes)
			}
		})
	}
}

// TestAttach_CleanupFailureDoesNotChangeTheDetach: a dropped link at detach is
// the LIKELIEST time for the removal to fail, and it is also the moment the user
// asked to leave. A leaked temp directory beats trapping them on an error
// screen, so the failure may not reach the model or the view at all.
func TestAttach_CleanupFailureDoesNotChangeTheDetach(t *testing.T) {
	var calls uploaderCalls
	u := fakeUploader("deploy@bastion.example.com", "chat-1", &calls)
	u.remoteDir = "/home/deploy/.cache/boss/uploads/chat-1"
	u.removeDir = func(_ context.Context, _ remotehost.Options, dir string) error {
		calls.removes = append(calls.removes, dir)
		return errors.New("ssh: connection closed by remote host")
	}

	m := NewAttachModel(&attachTelemetryStub{}, context.Background(), bosspty.NewManager(), "session-1", "")
	m.pasteUploader = u

	updated, cmd := m.Update(agentFinishedMsg{detached: true})
	got := updated.(AttachModel)

	if !got.Detached() {
		t.Fatal("Detached() = false when only the cleanup failed")
	}
	if got.err != nil || got.agentErr != nil {
		t.Fatalf("cleanup failure surfaced as err=%v agentErr=%v, want neither", got.err, got.agentErr)
	}
	runAttachCmdGraph(t, cmd, func(msg tea.Msg) {
		t.Fatalf("failed cleanup produced %#v; the user asked to detach and must see nothing", msg)
	})
	if len(calls.removes) != 1 {
		t.Fatalf("removeDir calls = %d, want the one attempt", len(calls.removes))
	}
	// And the view still renders the detach, not an error frame.
	if strings.Contains(got.View().Content, "Error") {
		t.Fatalf("view = %q, want no error after a failed cleanup", got.View().Content)
	}
}

// TestPrefillPastesThroughTheSharedHelper guards the change that made
// bosspty.BracketedPaste the ONE construction site for the sequence. Two things
// have to hold: the bytes are the same ones the hand-rolled payload produced
// (including no trailing newline, which is what would submit the turn before the
// user has written their prompt), and attach.go no longer spells the escapes out
// — a second spelling is how the two paths drift.
func TestPrefillPastesThroughTheSharedHelper(t *testing.T) {
	const plan = "implement the thing"
	// The literal payload the prefill built by hand before it shared the helper.
	want := append([]byte("\x1b[200~"+plan), "\x1b[201~"...)
	if got := bosspty.BracketedPaste(plan); !reflect.DeepEqual(got, want) {
		t.Fatalf("BracketedPaste(%q) = %q, want the byte-identical legacy payload %q", plan, got, want)
	}
	if strings.HasSuffix(string(want), "\n") {
		t.Fatal("the prefill payload ends in a newline, which submits the turn before the user has typed anything")
	}

	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "attach.go", nil, parser.SkipObjectResolution)
	if err != nil {
		t.Fatalf("parse attach.go: %v", err)
	}

	var prefill *ast.FuncDecl
	for _, decl := range file.Decls {
		if fn, ok := decl.(*ast.FuncDecl); ok && fn.Name.Name == "prefillClaudeInput" {
			prefill = fn
		}
	}
	if prefill == nil {
		t.Fatal("prefillClaudeInput not found in attach.go")
	}

	// No hand-rolled escapes anywhere in the file: the point of the change is
	// that there is exactly one place the sequence is built.
	ast.Inspect(file, func(n ast.Node) bool {
		lit, ok := n.(*ast.BasicLit)
		if !ok || lit.Kind != token.STRING {
			return true
		}
		if strings.Contains(lit.Value, `[200~`) || strings.Contains(lit.Value, `[201~`) {
			t.Errorf("attach.go builds a bracketed paste by hand at %s: %s — use bosspty.BracketedPaste",
				fset.Position(lit.Pos()), lit.Value)
		}
		return true
	})

	var callsHelper bool
	ast.Inspect(prefill.Body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != "BracketedPaste" {
			return true
		}
		pkg, ok := sel.X.(*ast.Ident)
		if !ok || pkg.Name != "bosspty" {
			return true
		}
		if len(call.Args) != 1 {
			return true
		}
		arg, ok := call.Args[0].(*ast.Ident)
		callsHelper = ok && arg.Name == "plan"
		return true
	})
	if !callsHelper {
		t.Fatal("prefillClaudeInput does not paste bosspty.BracketedPaste(plan); the shared construction site is the only one that may build the sequence")
	}
}
