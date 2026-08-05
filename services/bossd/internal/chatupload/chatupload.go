// Package chatupload owns the daemon side of the BOS-661 chat file upload
// subprotocol: it streams bounded, acknowledged chunks straight to a
// randomized 0600 file under a daemon-local temporary directory and, on a
// clean finish, PREFILLS exactly one path into the chat's composer through
// the normal chat-delivery path.
//
// Prefill, not submit. The upload is one half of a sentence the user is
// still writing — they know where in their prompt the file belongs, and a
// submitted "I uploaded a file named …" both spends a turn on it and puts
// it in the wrong place. Injecting the bare path leaves the composer under
// the user's control: they position it, write around it, and send when the
// thought is finished. See ComposerText.
//
// Design invariants. Each is covered by a test in this package EXCEPT
// where the entry says otherwise — two are not, and saying so is cheaper
// than a reader trusting a blanket claim:
//
//   - No whole file is ever held in memory. Chunks are written through to
//     the open file handle as they arrive; the manager keeps only a byte
//     count and the sequence cursor. NOT directly tested: it is a property
//     of `upload` having nowhere to put a buffer, which a test can restate
//     but not falsify.
//   - The declared size is rejected above MaxUploadBytes and the finish is
//     rejected unless the received total matches the declaration exactly.
//   - Chunks must arrive strictly in order starting at seq 0. A gap, a
//     repeat, or an oversized chunk terminates the upload permanently.
//   - The client's filename is a DISPLAY name only. The on-disk basename
//     is always randomized; a name carrying a path separator, or one that
//     IS a "." or ".." segment, is rejected outright rather than sanitized.
//   - Any cancel, stream loss, write error, or timeout removes the partial
//     file. Only a successful finish — or a finish whose chat delivery
//     could not be confirmed (see ErrDeliveryUnconfirmed) — leaves bytes on
//     disk. The write-error branch is NOT tested: nothing here can fail a
//     write to an open 0600 file on demand without a seam for it.
//   - The upload directory is a real directory narrowed to mode 0700, or the
//     manager refuses to start; a symlink at that path is rejected rather
//     than followed, because it now sits in the system temp dir, whose name
//     space a hostile local user can reach (see NewManager). The refusal on
//     a directory owned by ANOTHER user is not tested — it falls out of the
//     Chmod returning EPERM, which a test cannot arrange without a second uid.
//   - Completed files stay available for the lifetime of the chat, bounded
//     by an explicit stale-file TTL janitor (see DefaultStaleTTL) so a
//     disconnected client cannot accumulate unbounded disk usage, and by a
//     directory quota (see MaxUploadDirBytes) so serial uploads cannot
//     retain unbounded bytes inside one TTL window.
package chatupload

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

const (
	// MaxUploadBytes is the hard ceiling on a single upload, enforced
	// independently here, in bosso, and in the browser.
	MaxUploadBytes = 500 * 1024 * 1024

	// MaxChunkBytes matches the existing 1 MiB web binary-frame cap, so a
	// chunk never needs to be split by a lower layer.
	MaxChunkBytes = 1024 * 1024

	// MaxInFlightChunks is the acknowledgement window: the number of chunks
	// that may be unacknowledged at once.
	//
	// It is the ONE limit in this file the daemon does not re-check, and
	// deliberately so — the daemon is the ACKNOWLEDGER. By the time Chunk
	// returns, every earlier chunk has already been acked, so there is no
	// outstanding-chunk count for it to police; the window only means
	// something to a sender. bosso is therefore its sole enforcement point
	// (wsUploadTracker.chunk in services/bosso/internal/server/
	// ws_attach_upload.go), and the value lives here only so the two
	// senders that DO honour it — bosso and the browser — cite one number.
	// Raising it here without raising wsUploadMaxInFlightChunks changes
	// nothing; raising it there is what moves the bound.
	MaxInFlightChunks = 4

	// MaxActiveUploadsPerAttach bounds concurrent uploads from one attach.
	// A human picking files needs very few at once; a client issuing more
	// is buggy or hostile.
	MaxActiveUploadsPerAttach = 4

	// MaxActiveUploads bounds concurrent uploads daemon-wide. Every
	// accepted start holds an open file descriptor until finish, cancel,
	// or the idle sweep, so this is the descriptor ceiling.
	MaxActiveUploads = 32

	// MaxActiveBytes bounds the aggregate *declared* size of in-flight
	// uploads. MaxUploadBytes is per file, so without this a client could
	// admit many maximum-size uploads and exhaust the disk between sweeps.
	MaxActiveBytes = 2 * 1024 * 1024 * 1024

	// MaxUploadDirBytes is a quota on the whole upload directory, which
	// the three limits above deliberately do not cover: a finished upload
	// leaves the active accounting entirely while its file stays on disk
	// for DefaultStaleTTL, so uploading *serially* — one at a time, each
	// finished before the next starts — would otherwise retain unbounded
	// bytes. 8 GiB is 16 maximum-size uploads, or many thousands of
	// ordinary ones, per 24h retention window: far beyond a day of human
	// file picking, and small next to a daemon host's disk.
	MaxUploadDirBytes = 8 * 1024 * 1024 * 1024

	// MaxUploadDirFiles bounds the same directory by entry count, because
	// a byte quota alone still permits millions of one-byte files —
	// exhausting inodes rather than blocks.
	MaxUploadDirFiles = 1024

	// DefaultStaleTTL is the retention decision for BOS-661: a completed
	// upload stays available to the live chat, but no longer than this.
	// It is deliberately conservative and explicit rather than "forever".
	DefaultStaleTTL = 24 * time.Hour

	// DefaultJanitorInterval is how often the stale sweep runs.
	DefaultJanitorInterval = time.Hour

	// DefaultIdleTimeout bounds a stalled in-flight upload (client went
	// away without a cancel and without closing the stream).
	DefaultIdleTimeout = 15 * time.Minute

	// dirPerm/filePerm: the upload directory is owner-only and every file
	// inside it is 0600.
	dirPerm  fs.FileMode = 0o700
	filePerm fs.FileMode = 0o600
)

// Error is a terminal upload failure. Retryable distinguishes transport
// loss (the client may resend the same file) from a permanent rejection
// (too large, malformed name, out-of-order chunks).
type Error struct {
	Msg       string
	Retryable bool
}

func (e *Error) Error() string { return e.Msg }

func permanent(format string, args ...any) *Error {
	return &Error{Msg: fmt.Sprintf(format, args...)}
}

func retryable(msg string) *Error {
	return &Error{Msg: msg, Retryable: true}
}

// ErrDeliveryUnconfirmed marks a chat delivery whose outcome could NOT be
// determined: the payload may already have reached the agent's pane, or it
// may never have been delivered at all. It is the one sender failure that
// must not delete the uploaded file (see Finish).
//
// This package now delivers with submit=false, so the daemon runs no submit
// verifier on its behalf and cannot produce this from a failed verification.
// It is kept because the interface still permits a sender to report it and
// because Finish treats a cancelled context identically — the epistemic state,
// and the cost of guessing wrong, are the same either way.
//
// It is a sentinel rather than a typed result because the distinction has to
// cross the ChatMessageSender boundary without this package importing the
// daemon's server package — that dependency is forbidden by the module
// boundary rules, and it is the whole reason ChatMessageSender is a narrow
// interface. The wiring layer owns the mapping: when
// Server.SendChatMessage answers with DELIVERY_STATE_UNCONFIRMED (see
// services/bossd/internal/server/send_chat_message.go, which returns that
// state rather than an error, and services/bossd/cmd/main.go, which turns a
// delivered=false response into an error), the adapter wraps this sentinel
// around the error it reports — e.g.
//
//	return fmt.Errorf("%w: %s", chatupload.ErrDeliveryUnconfirmed, notice)
//
// Finish matches it with errors.Is, so any wrapping shape works.
var ErrDeliveryUnconfirmed = errors.New("chatupload: chat delivery unconfirmed")

// ChatMessageSender is the narrow dependency the manager has on the
// daemon's chat-delivery path. It exists so this package never imports
// the server package (and so the delivery can be faked in tests). The
// live implementation wraps Server.SendChatMessage.
//
// An implementation MUST wrap ErrDeliveryUnconfirmed around any error it
// reports for a submission it could not verify. Returning a bare error for
// that case tells the manager the message definitely never arrived, and the
// upload is deleted underneath a prompt that may already name it.
type ChatMessageSender interface {
	SendChatMessage(ctx context.Context, chatID, message string, submit bool) error
}

// Manager tracks in-flight uploads for every attach on one daemon.
type Manager struct {
	dir         string
	sender      ChatMessageSender
	ttl         time.Duration
	idle        time.Duration
	maxDirBytes uint64
	maxDirFiles int
	now         func() time.Time
	// syncFile flushes a finished upload to disk. Production always uses
	// (*os.File).Sync; it is a field only so a test can substitute a hook
	// that blocks INSIDE the flush, which is the only way to observe that
	// Finish does its filesystem work outside m.mu (see
	// TestFinishOwnsTheFileOnceItStartsDelivering). Set once, before any
	// upload exists.
	syncFile func(*os.File) error
	mu       sync.Mutex
	uploads  map[string]*upload
}

// Option customizes a Manager. Kept tiny on purpose.
type Option func(*Manager)

// WithStaleTTL overrides DefaultStaleTTL for completed files.
func WithStaleTTL(d time.Duration) Option {
	return func(m *Manager) {
		if d > 0 {
			m.ttl = d
		}
	}
}

// WithIdleTimeout overrides DefaultIdleTimeout for in-flight uploads.
func WithIdleTimeout(d time.Duration) Option {
	return func(m *Manager) {
		if d > 0 {
			m.idle = d
		}
	}
}

// WithDirQuota overrides MaxUploadDirBytes/MaxUploadDirFiles for the
// upload directory. A zero value leaves the corresponding default in
// place; it exists so a test can fill the quota without writing
// gigabytes.
func WithDirQuota(maxBytes uint64, maxFiles int) Option {
	return func(m *Manager) {
		if maxBytes > 0 {
			m.maxDirBytes = maxBytes
		}
		if maxFiles > 0 {
			m.maxDirFiles = maxFiles
		}
	}
}

// WithClock injects a clock for deterministic janitor tests.
func WithClock(now func() time.Time) Option {
	return func(m *Manager) {
		if now != nil {
			m.now = now
		}
	}
}

type upload struct {
	attachID string
	uploadID string
	chatID   string
	declared uint64
	received uint64
	nextSeq  uint64
	path     string
	file     *os.File
	touched  time.Time
	// finishing is set by ClaimFinish and never cleared: an upload is
	// delivered at most once. It is what makes a repeated upload_finish
	// cheap to reject (see ClaimFinish) and what stops a late chunk from
	// appending to a file that is already being delivered.
	finishing bool
}

// NewManager creates the upload directory (0700) and returns a manager.
func NewManager(dir string, sender ChatMessageSender, opts ...Option) (*Manager, error) {
	return newManagerWithChmod(dir, sender, os.Chmod, opts...)
}

func newManagerWithChmod(dir string, sender ChatMessageSender, chmod func(string, fs.FileMode) error, opts ...Option) (*Manager, error) {
	if dir == "" {
		return nil, errors.New("chatupload: empty upload directory")
	}
	// Resolve before anything stores or uses the value. Finish publishes
	// this path into a chat, and the agent that reads it resolves relative
	// names against its own worktree rather than the daemon's working
	// directory — so a relative dir here would hand the agent a path it
	// cannot open. Make every downstream path absolute at the source.
	dir, err := filepath.Abs(dir)
	if err != nil {
		return nil, fmt.Errorf("chatupload: resolve upload dir: %w", err)
	}
	if err := os.MkdirAll(dir, dirPerm); err != nil {
		return nil, fmt.Errorf("chatupload: create upload dir: %w", err)
	}
	// MkdirAll leaves an existing directory's mode untouched, so a volume
	// pre-created as 0755/0777 would silently break the owner-only promise
	// above: other local users could enumerate upload names, and a writable
	// directory also lets them replace the 0600 files inside it. Narrow the
	// mode explicitly rather than trusting the create.
	//
	// Lstat, not Stat: this directory now lives in the system temp dir, whose
	// NAMESPACE is shared even where its entries are not (a Linux /tmp). A
	// hostile local user who wins the race to create the path can put a
	// SYMLINK there, and every check below would then describe the target
	// rather than the link — MkdirAll would have found it "already present",
	// Stat would report a perfectly good 0700 directory, and the Chmod would
	// succeed precisely when the target belongs to us. The manager would go
	// on to write uploads into whatever the attacker pointed at and, worse,
	// let Sweep delete anything there older than the stale TTL. Refusing a
	// symlink outright is the whole mitigation: the daemon owns this path or
	// it does not run uploads.
	info, err := os.Lstat(dir)
	if err != nil {
		return nil, fmt.Errorf("chatupload: stat upload dir: %w", err)
	}
	if info.Mode()&fs.ModeSymlink != 0 {
		return nil, fmt.Errorf("chatupload: upload path %q is a symlink", dir)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("chatupload: upload path %q is not a directory", dir)
	}
	// A matching mode mask is not ownership evidence: another user's 0700
	// directory looks identical here. Always ask the OS to set the mode so
	// ownership is proven by its authority check; a foreign-owned directory
	// fails with EPERM rather than becoming an upload location.
	if err := chmod(dir, dirPerm); err != nil {
		return nil, fmt.Errorf("chatupload: restrict upload dir permissions: %w", err)
	}
	m := &Manager{
		dir:         dir,
		sender:      sender,
		ttl:         DefaultStaleTTL,
		idle:        DefaultIdleTimeout,
		maxDirBytes: MaxUploadDirBytes,
		maxDirFiles: MaxUploadDirFiles,
		now:         time.Now,
		syncFile:    (*os.File).Sync,
		uploads:     make(map[string]*upload),
	}
	for _, opt := range opts {
		opt(m)
	}
	return m, nil
}

// Dir reports the daemon-local upload directory.
func (m *Manager) Dir() string { return m.dir }

func key(attachID, uploadID string) string { return attachID + "\x00" + uploadID }

// ValidateFilename enforces the display-name rules. It is exported so
// bosso can reject a bad name before any bytes cross the wire.
func ValidateFilename(name string) error {
	if name == "" {
		return permanent("upload: empty filename")
	}
	if len(name) > 255 {
		return permanent("upload: filename too long")
	}
	if !utf8.ValidString(name) {
		// The display name crosses bosso's proto3 TerminalUploadStart.Filename
		// on the way here, and a proto3 string field cannot hold invalid UTF-8
		// — marshalling one errors — so reject it at both ends rather than
		// sanitize it, which is also how every other rule in this validator
		// behaves. (It no longer reaches the chat: ComposerText injects the
		// path alone. The wire constraint is what this rule answers to.)
		return permanent("upload: filename must be valid UTF-8")
	}
	if strings.ContainsAny(name, "/\\\x00") {
		return permanent("upload: filename must not contain path separators")
	}
	// Traversal is ".." as a whole SEGMENT, and after the separator check
	// above the whole name IS the only segment — so comparing it is the
	// complete rule. A substring test would additionally reject ordinary
	// names that merely double a dot ("report..final.txt", "v1..2.csv"),
	// which traverse nothing.
	if name == "." || name == ".." {
		return permanent("upload: filename must not contain path traversal")
	}
	if filepath.Base(name) != name {
		return permanent("upload: filename must be a bare basename")
	}
	return nil
}

// Start opens a new upload. chatID is resolved by the caller from the
// attach registry — the wire protocol never lets a client name a chat it
// is not already attached to.
func (m *Manager) Start(attachID, uploadID, chatID, filename string, sizeBytes uint64) error {
	if attachID == "" {
		return permanent("upload: empty attach_id")
	}
	if uploadID == "" {
		return permanent("upload: empty upload_id")
	}
	if chatID == "" {
		return permanent("upload: empty chat_id")
	}
	if err := ValidateFilename(filename); err != nil {
		return err
	}
	if sizeBytes == 0 {
		return permanent("upload: empty file")
	}
	if sizeBytes > MaxUploadBytes {
		return permanent("upload: file exceeds %d byte limit", uint64(MaxUploadBytes))
	}

	// Measure the directory BEFORE taking m.mu. dirUsage is an os.ReadDir
	// plus a stat per entry — up to MaxUploadDirFiles (1024) syscalls — and
	// m.mu is the registry lock every Chunk, Cancel and Sweep also needs,
	// on a daemon whose single terminal-stream reader goroutine drives the
	// upload path alongside terminal input, resize, close and the BOS-376
	// pongs. The measurement is advisory ground truth rather than a
	// transactional invariant (the unreceived-bytes reservation below is
	// what makes the gate sound against concurrent admissions), so taking
	// it a moment early costs nothing and keeps ~1000 syscalls out of the
	// critical section.
	dirBytes, dirFiles := m.dirUsage()

	m.mu.Lock()
	defer m.mu.Unlock()
	if _, exists := m.uploads[key(attachID, uploadID)]; exists {
		return permanent("upload: duplicate upload_id")
	}

	// Admission control. The duplicate-id check above bounds nothing: a
	// client issuing unique ids can open arbitrarily many uploads, each
	// holding a descriptor until finish/cancel/sweep, and MaxUploadBytes
	// is enforced per file only. Bound the count per attach, the count
	// daemon-wide, and the aggregate declared bytes *before* creating the
	// file, so a burst cannot exhaust descriptors or disk.
	var active, activeForAttach int
	var activeBytes, unreceivedBytes uint64
	for _, up := range m.uploads {
		active++
		activeBytes += up.declared
		// The bytes this upload has promised but not yet written. Guard the
		// subtraction rather than assuming declared >= received: received is
		// only ever bounded by declared inside Chunk, and an unsigned
		// underflow here would wrap to a colossal reservation that wedges
		// the directory shut.
		if up.declared > up.received {
			unreceivedBytes += up.declared - up.received
		}
		if up.attachID == attachID {
			activeForAttach++
		}
	}
	if activeForAttach >= MaxActiveUploadsPerAttach {
		return retryable("upload: too many concurrent uploads for this attach")
	}
	if active >= MaxActiveUploads {
		return retryable("upload: too many concurrent uploads")
	}
	if activeBytes+sizeBytes > MaxActiveBytes {
		return retryable("upload: concurrent upload byte limit reached")
	}

	// The three gates above only see uploads that are still in flight. A
	// finish drops the entry from all of them while the completed file
	// stays on disk for the whole stale TTL, so serial uploads — never
	// more than one active — bypass them entirely and could retain an
	// unbounded number of maximum-size files. Quota the directory itself.
	// Measuring it rather than tracking a counter is deliberate: the
	// measurement is ground truth, so it cannot drift out of step with the
	// janitor sweep, an operator deleting a file by hand, or a daemon
	// restart that inherits a directory of retained files.
	//
	// The measurement alone is not an admission bound, though: an in-flight
	// upload contributes only the bytes it has written so far, so several
	// starts issued before any of them is filled each see the same near-empty
	// directory and are collectively admitted well past the quota. Reserve
	// every active upload's unreceived declared bytes alongside the new
	// upload's own, so the gate compares the quota against the directory's
	// eventual size rather than its current one. (dirBytes/dirFiles were
	// measured just above, outside the lock — see the comment there.)
	if dirFiles >= m.maxDirFiles {
		return retryable("upload: retained upload file limit reached")
	}
	if dirBytes+unreceivedBytes+sizeBytes > m.maxDirBytes {
		return retryable("upload: retained upload byte limit reached")
	}

	f, err := os.CreateTemp(m.dir, "upload-*"+safeExt(filename))
	if err != nil {
		return retryable("upload: create temporary file failed")
	}
	// CreateTemp already uses 0600, but state it explicitly so a future
	// umask or platform difference cannot silently widen the mode.
	if err := f.Chmod(filePerm); err != nil {
		_ = f.Close()
		_ = os.Remove(f.Name())
		return retryable("upload: secure temporary file failed")
	}

	m.uploads[key(attachID, uploadID)] = &upload{
		attachID: attachID,
		uploadID: uploadID,
		chatID:   chatID,
		declared: sizeBytes,
		path:     f.Name(),
		file:     f,
		touched:  m.now(),
	}
	return nil
}

// Chunk writes one in-order slice and reports the running byte total. The
// caller must only acknowledge after this returns nil — the write has
// already reached the file handle at that point.
func (m *Manager) Chunk(attachID, uploadID string, seq uint64, data []byte) (uint64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	up, ok := m.uploads[key(attachID, uploadID)]
	if !ok {
		return 0, permanent("upload: unknown upload_id")
	}
	if len(data) == 0 {
		m.abortLocked(up)
		return 0, permanent("upload: empty chunk")
	}
	if len(data) > MaxChunkBytes {
		m.abortLocked(up)
		return 0, permanent("upload: chunk exceeds %d byte limit", MaxChunkBytes)
	}
	if up.finishing {
		// Delivery has been claimed, so the byte total is already fixed and
		// the file may be mid-sync. Appending here would race the delivery
		// goroutine and publish a path whose contents no longer match the
		// declaration that was validated.
		m.abortLocked(up)
		return 0, permanent("upload: chunk after finish")
	}
	if seq != up.nextSeq {
		m.abortLocked(up)
		return 0, permanent("upload: out-of-order chunk")
	}
	if up.received+uint64(len(data)) > up.declared {
		m.abortLocked(up)
		return 0, permanent("upload: payload exceeds declared size")
	}

	if _, err := up.file.Write(data); err != nil {
		m.abortLocked(up)
		return 0, retryable("upload: write failed")
	}
	up.received += uint64(len(data))
	up.nextSeq++
	up.touched = m.now()
	return up.received, nil
}

// ClaimFinish reserves an upload for delivery. It is the cheap SYNCHRONOUS
// half of Finish, and the caller must run it — on whatever goroutine reads
// the wire — before spawning the goroutine that calls Finish.
//
// The split is what bounds concurrency. Finish's own existence check happens
// after the caller has already committed to a goroutine, so a client that
// repeats upload_finish for one id would spawn one delivery goroutine per
// frame: unbounded, at line rate, for a ~30-byte message, each one able to
// block on a full outbound queue. Claiming here rejects the second and every
// later finish for an id before anything is spawned, so live delivery
// goroutines are bounded by the admission limits that already bound live
// uploads (MaxActiveUploads / MaxActiveUploadsPerAttach).
//
// A claim is permanent: it is never released, because an upload is delivered
// at most once. Cancel and the idle sweep still remove a claimed upload, in
// which case the delivery goroutine's Finish reports an unknown upload_id.
func (m *Manager) ClaimFinish(attachID, uploadID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	up, ok := m.uploads[key(attachID, uploadID)]
	if !ok {
		return permanent("upload: unknown upload_id")
	}
	if up.finishing {
		return permanent("upload: duplicate finish")
	}
	up.finishing = true
	return nil
}

// Finish syncs and closes the file, then prefills exactly one chat message —
// the resulting path — into the chat's composer without submitting it. It
// returns the absolute path so the caller can audit the outcome (never the
// contents).
//
// Callers that reach Finish from a wire frame MUST have claimed the upload
// with ClaimFinish first; see that method for why.
func (m *Manager) Finish(ctx context.Context, attachID, uploadID string) (string, error) {
	// Take OWNERSHIP of the handle under the lock, then do the file I/O
	// outside it.
	//
	// The Sync below is an fsync of up to MaxUploadBytes (500 MiB) — seconds
	// on a busy disk — and it runs on a delivery goroutine, while m.mu is
	// also taken by Start, Chunk and Cancel on the daemon's SINGLE
	// terminal-stream reader goroutine, which additionally serves terminal
	// input, resize, close and the BOS-376 ping/pong heartbeat. Holding the
	// registry lock across the flush parked all of that behind one upload
	// (reachable with two concurrent uploads, which MaxActiveUploadsPerAttach
	// = 4 permits by design), re-introducing exactly the stall the delivery
	// goroutine exists to avoid.
	//
	// Detaching is safe: ClaimFinish already set `finishing`, so Chunk
	// refuses to append, and removing the map entry here means Cancel, the
	// idle sweep and CancelAll can no longer reach the handle either.
	m.mu.Lock()
	up, ok := m.uploads[key(attachID, uploadID)]
	if !ok {
		m.mu.Unlock()
		return "", permanent("upload: unknown upload_id")
	}
	if up.received != up.declared {
		m.abortLocked(up)
		m.mu.Unlock()
		return "", permanent("upload: size mismatch")
	}
	delete(m.uploads, key(attachID, uploadID))
	file, path, chatID := up.file, up.path, up.chatID
	up.file = nil
	m.mu.Unlock()

	if err := m.syncFile(file); err != nil {
		_ = file.Close()
		_ = os.Remove(path)
		return "", retryable("upload: sync failed")
	}
	if err := file.Close(); err != nil {
		_ = os.Remove(path)
		return "", retryable("upload: close failed")
	}

	if m.sender == nil {
		_ = os.Remove(path)
		return "", retryable("upload: chat delivery unavailable")
	}
	// submit=false: the path is PREFILLED into the composer, never sent. See
	// the package doc for why. It also removes the whole submit-verification
	// surface from this path — no Enter, no pane poll, no modal gate — so the
	// unconfirmed branch below is now reachable only through a cancelled
	// context rather than through a verifier that could not read the pane.
	if err := m.sender.SendChatMessage(ctx, chatID, ComposerText(path), false); err != nil {
		// A cancelled or expired context is the SAME epistemic state as an
		// explicit ErrDeliveryUnconfirmed: the path may already be sitting in
		// the user's composer, or it may never have been typed at all, and the
		// error alone cannot tell the two apart.
		//
		// This is not a corner case. The daemon's terminal-stream teardown
		// cancels the context it handed us BEFORE joining the delivery
		// goroutine (see openStream's teardown ordering in
		// services/bossd/internal/upstream/terminal_stream.go), so every
		// ordinary stream blip — reconnect, heartbeat miss, re-auth — lands
		// an in-flight finish here. Treating that as a definite non-delivery
		// would delete the file underneath a composer that may already name
		// it, which is exactly the unrecoverable outcome the unconfirmed
		// branch below exists to prevent.
		//
		// Retention stays bounded: the upload has already left m.uploads, so
		// the stale-file janitor and the directory quota govern the file
		// from here exactly as they do for a delivered one.
		if errors.Is(err, ErrDeliveryUnconfirmed) || ctx.Err() != nil {
			// KEEP the file. The path may already have reached the composer.
			// Deleting here on every sender error would strand it on a path that
			// no longer exists — the one outcome the user cannot recover from,
			// because nothing tells them: they send the prompt they were writing
			// and the agent reads a dangling filename.
			//
			// Prefill narrows how this is reached but not that it is: without a
			// submit there is no verifier to return DELIVERY_STATE_UNCONFIRMED,
			// so in practice this is now the cancelled-context arm above. The
			// sentinel is still honoured because the sender interface still
			// permits it and the cost of being wrong is unchanged.
			//
			// Retention cannot leak. The upload has already left m.uploads, so
			// the stale-file janitor treats this file exactly like a delivered
			// one: Sweep does not see it in the live set and removes it once it
			// ages past the stale TTL (DefaultStaleTTL), and it counts against
			// the directory quota in the meantime.
			//
			// Reported as permanent, not retryable: re-uploading the same file
			// would put a SECOND path into a composer that may already hold the
			// first, which the user then has to notice and delete.
			return "", permanent("upload: chat delivery unconfirmed; file retained")
		}
		// Delivery definitely did not happen. The bytes are fine, but nothing
		// names them, so remove the file rather than leave an orphan the user
		// never learned about.
		_ = os.Remove(path)
		return "", retryable("upload: chat delivery failed")
	}
	return path, nil
}

// ComposerText renders the single string injected into the chat's composer:
// the finished upload's absolute path, with no formatting at all.
//
// No trailing space, and not for want of wanting one. A prefill lands at the
// cursor, so a separator would stop the path welding onto whatever the user
// had already typed ("look at" + path reads as "look at/var/folders/…") — but
// it cannot be delivered from here. The single-line prefill path types
// strings.TrimSpace of the payload (Client.sendPlan in
// services/bossd/internal/tmux/tmux.go, which trims so the submit verifier
// can match what it typed), so any padding added here is stripped one layer
// down. Returning the bare path is what this function can actually promise;
// adding a space would only make the string it returns disagree with the
// string that reaches the pane.
//
// The display filename is deliberately absent. It was in the old submitted
// sentence, which is gone; keeping it would put a second thing in a composer
// the user is about to write in, and safeExt already carries the one part of
// it an agent needs to recognise the file.
func ComposerText(path string) string {
	return path
}

// Cancel abandons one upload and removes its partial file. It is safe to
// call for an unknown id (a client cancelling after a terminal result).
func (m *Manager) Cancel(attachID, uploadID string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if up, ok := m.uploads[key(attachID, uploadID)]; ok {
		m.abortLocked(up)
	}
}

// CancelAttach abandons every upload belonging to one attach. Called on
// attach close and on terminal-stream loss so a disconnect can never
// leave partial data behind.
func (m *Manager) CancelAttach(attachID string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, up := range m.uploads {
		if up.attachID == attachID {
			m.abortLocked(up)
		}
	}
}

// CancelAll abandons every in-flight upload (daemon shutdown).
func (m *Manager) CancelAll() {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, up := range m.uploads {
		m.abortLocked(up)
	}
}

// InFlight reports how many uploads are currently open. Test/observability
// helper; it never exposes filenames or paths.
func (m *Manager) InFlight() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.uploads)
}

// abortLocked closes and removes a partial upload. Caller holds m.mu.
func (m *Manager) abortLocked(up *upload) {
	if up.file != nil {
		_ = up.file.Close()
		up.file = nil
	}
	_ = os.Remove(up.path)
	delete(m.uploads, key(up.attachID, up.uploadID))
}

// dirUsage totals the bytes and the number of files the upload directory
// currently holds, in-flight partials included. An unreadable directory
// or entry contributes nothing rather than failing the caller — the
// admission gate errs open on a stat problem, and the sweep still runs.
func (m *Manager) dirUsage() (totalBytes uint64, files int) {
	entries, err := os.ReadDir(m.dir)
	if err != nil {
		return 0, 0
	}
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			continue
		}
		files++
		if size := info.Size(); size > 0 {
			totalBytes += uint64(size)
		}
	}
	return totalBytes, files
}

// Sweep drops in-flight uploads idle beyond the idle timeout and deletes
// completed files older than the stale TTL. It returns the number of
// files removed. Files belonging to a live in-flight upload are never
// swept regardless of age.
func (m *Manager) Sweep() int {
	now := m.now()

	m.mu.Lock()
	active := make(map[string]struct{}, len(m.uploads))
	for _, up := range m.uploads {
		if now.Sub(up.touched) > m.idle {
			m.abortLocked(up)
			continue
		}
		active[up.path] = struct{}{}
	}
	m.mu.Unlock()

	entries, err := os.ReadDir(m.dir)
	if err != nil {
		return 0
	}
	removed := 0
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		path := filepath.Join(m.dir, entry.Name())
		if _, live := active[path]; live {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			continue
		}
		if now.Sub(info.ModTime()) <= m.ttl {
			continue
		}
		if os.Remove(path) == nil {
			removed++
		}
	}
	return removed
}

// RunJanitor sweeps immediately, then on a fixed interval until ctx is
// cancelled.
//
// The immediate sweep is not cosmetic. Without it the first pass is one
// whole interval away (DefaultJanitorInterval, an hour), so a directory of
// files inherited from a previous daemon run stays untouched for that hour
// — and an in-flight upload abandoned just before a restart is not reclaimed
// at its 15-minute idle timeout but at the first tick after it, up to ~75
// minutes later, holding its quota reservation the entire time.
func (m *Manager) RunJanitor(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		interval = DefaultJanitorInterval
	}
	m.Sweep()
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			m.Sweep()
		}
	}
}

// safeExt returns the display name's extension when it is short and
// alphanumeric, so an agent sees a usable suffix without the client ever
// influencing the rest of the on-disk name.
func safeExt(filename string) string {
	ext := filepath.Ext(filename)
	if len(ext) < 2 || len(ext) > 10 {
		return ""
	}
	for _, r := range ext[1:] {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		default:
			return ""
		}
	}
	return ext
}
