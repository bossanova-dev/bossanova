package views

// This file owns the one thing a --host attach has to do with a pasted image:
// put the file where the agent can actually open it. Split from attach.go, which
// is the Bubble Tea model, for the same reason attachtransport.go is — the
// ssh-shaped concern reads as one thing and the view stays a view.
//
// The whole file is dead weight in local mode by design: nothing here is
// constructed, and internal/pty's uploader hook is never installed, so the stdin
// path a local attach runs is the one it ran before this existed.

import (
	"context"
	"sync"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/rs/zerolog/log"

	"github.com/recurser/boss/internal/remotehost"
)

// remoteUploadCleanupTimeout bounds the detach-time removal of the remote upload
// directory.
//
// A few seconds is the right order: the work is one `ssh … rm -rf` against a host
// the user was just interactively attached to, so a healthy link answers in
// well under a second, and the only thing a longer bound would buy is a longer
// wedge. It has to be bounded at all because the alternative is a goroutine
// pinned forever on a half-open connection — the exact failure the attach's own
// keepalives exist to eject the user from.
const remoteUploadCleanupTimeout = 5 * time.Second

// remotePasteUploader turns a local image path into one the remote agent can
// open, lazily creating the session's remote upload directory on first use and
// remembering it so detach can remove it.
//
// It is always held by pointer. bubbletea copies the model value on every
// Update, so a by-value field would hand each copy its own idea of whether the
// directory had been created — the closure the upload goroutine runs would
// resolve a directory that the model that later runs cleanup had never heard of,
// and the temp dir would leak on every session.
type remotePasteUploader struct {
	destination string
	sessionKey  string

	// The three remotehost calls are fields rather than direct calls so tests can
	// substitute fakes. Every one of them shells out to ssh: without this seam
	// the only way to exercise the laziness, the retry-after-failure policy, or
	// the cleanup would be a real host and a real remote account.
	ensureDir  func(ctx context.Context, opts remotehost.Options, sessionKey string) (string, error)
	uploadFile func(ctx context.Context, opts remotehost.Options, localPath, remoteDir string) (string, error)
	removeDir  func(ctx context.Context, opts remotehost.Options, remoteDir string) error

	// controlPath resolves the tunnel's multiplexing socket at call time. It is
	// a seam for the same reason the three above are: the value comes from
	// package state that --host startup sets, so a test asserting that the
	// socket reaches ssh cannot otherwise arrange one without a real tunnel.
	controlPath func() string

	// mu guards remoteDir and serialises the ensure round trip. The pty layer
	// runs every upload on its own goroutine, so two quick pastes race here, and
	// two concurrent ensures would be two directories with only one remembered
	// for cleanup.
	mu sync.Mutex
	// remoteDir is the resolved upload directory, "" until a first ensure has
	// succeeded.
	remoteDir string
}

// newPasteUploader is indirected through a package var for exactly the reason
// agentChatTitle and agentTranscriptAbsentOrEmpty in attach.go are: it is the
// only way a test can prove the LOCAL path never reaches this machinery at all.
// Everything an uploader would go on to do is an ssh call that a local test
// never makes, so "nothing crashed" is indistinguishable from "one was built and
// simply never used" — and the second of those is the bug.
var newPasteUploader = newRemotePasteUploader

// newRemotePasteUploader returns an uploader for one attach, keyed on the chat
// so two chats against the same host cannot see each other's pastes.
//
// Constructing it performs no I/O — see upload for why that matters.
func newRemotePasteUploader(destination, sessionKey string) *remotePasteUploader {
	return &remotePasteUploader{
		destination: destination,
		sessionKey:  sessionKey,
		ensureDir:   remotehost.EnsureUploadDir,
		uploadFile:  remotehost.UploadFile,
		removeDir:   remotehost.RemoveUploadDir,
		controlPath: remoteHostControlPath,
	}
}

// upload copies localPath to the remote machine and returns the path the agent
// can open it at. It satisfies bosspty.PasteUpload.
//
// ctx is passed straight through to remotehost, which runs its ssh under it.
// That is load-bearing rather than tidy: the pty layer cancels this context at
// teardown and then waits, without a timeout, for every upload goroutine to
// exit — so an upload that ignored ctx would hang the detach.
func (u *remotePasteUploader) upload(ctx context.Context, localPath string) (string, error) {
	dir, err := u.ensureRemoteDir(ctx)
	if err != nil {
		return "", err
	}
	return u.uploadFile(ctx, u.options(), localPath, dir)
}

// ensureRemoteDir resolves the upload directory, creating it on the first call.
//
// Lazy because EnsureUploadDir is an ssh round trip: doing it at attach time
// would put a network call on the path of every --host attach, and would create
// a directory on the user's host for the overwhelming majority of sessions that
// never paste an image at all.
//
// A failure records nothing, so the NEXT paste retries. The alternative —
// remembering that the ensure failed — would let one transient ssh hiccup
// disable image pasting for the rest of a long-lived attach, with no way to
// recover short of detaching. The cost of retrying is one extra `mkdir -p` on a
// path that is idempotent by construction.
func (u *remotePasteUploader) ensureRemoteDir(ctx context.Context) (string, error) {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.remoteDir != "" {
		return u.remoteDir, nil
	}
	dir, err := u.ensureDir(ctx, u.options(), u.sessionKey)
	if err != nil {
		return "", err
	}
	u.remoteDir = dir
	return dir, nil
}

// cleanup removes the remote upload directory, and is a no-op when no upload
// ever created one — which is the common case, so it must not cost an ssh
// connection to discover.
//
// The remembered path is cleared as it is taken, making a second cleanup a
// no-op rather than a second `rm -rf` against a path that is already gone.
func (u *remotePasteUploader) cleanup(ctx context.Context) error {
	u.mu.Lock()
	dir := u.remoteDir
	u.remoteDir = ""
	u.mu.Unlock()

	if dir == "" {
		return nil
	}
	return u.removeDir(ctx, u.options(), dir)
}

// options is the remotehost configuration for this uploader: the destination
// and the tunnel's multiplexing socket. RemoteSocket is about daemon discovery
// and has nothing to say about a file copy, and the command factory is left nil
// so production uses the real ssh.
//
// ControlPath is read here rather than captured at construction because the
// tunnel and the uploader are built at different times by different layers, and
// reading it late means an uploader can never hold a path from a tunnel that has
// since been replaced. It is empty for every non---host attach, which leaves
// this exactly as it was.
func (u *remotePasteUploader) options() remotehost.Options {
	opts := remotehost.Options{Destination: u.destination}
	// nil rather than a stub for an uploader built by a test that has nothing to
	// say about multiplexing: an unset seam means "no control path", which is
	// the same thing every local attach reports.
	if u.controlPath != nil {
		opts.ControlPath = u.controlPath()
	}
	return opts
}

// cleanupRemoteUploads returns the command that removes this attach's remote
// upload directory, or nil when there is nothing to remove (local mode, or a
// remote attach that never uploaded anything). A nil tea.Cmd is compacted away
// by tea.Batch.
//
// Strictly best-effort: the command swallows the error into a debug log and
// returns a nil message, so a failed cleanup cannot change the detach outcome,
// cannot surface an error to the user, and cannot keep the view from returning.
// A leaked temp directory on the remote host is a far better outcome than a user
// trapped on an error screen when they asked to detach.
//
// The context is fresh rather than m.ctx on purpose. m.ctx is the TUI's context
// and can already be cancelled by the time a detach runs, which would make the
// cleanup a guaranteed no-op — the directory would leak every time, and no test
// short of a real host would notice.
func (m AttachModel) cleanupRemoteUploads() tea.Cmd {
	uploader := m.pasteUploader
	if uploader == nil {
		return nil
	}
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), remoteUploadCleanupTimeout)
		defer cancel()
		if err := uploader.cleanup(ctx); err != nil {
			log.Debug().Err(err).Msg("could not remove the remote paste upload dir")
		}
		return nil
	}
}
