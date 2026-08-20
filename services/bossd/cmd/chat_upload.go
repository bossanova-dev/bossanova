package main

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"connectrpc.com/connect"
	bossanovav1 "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossd/internal/chatupload"
)

// chatUploadDirName is the per-user directory BOS-661 uploads are streamed
// into, created inside the system temporary directory (os.TempDir, which
// honours TMPDIR).
//
// It lives there rather than beside the daemon's database so the HOST's own
// temp reaper sits underneath the manager's janitor as a backstop: on macOS
// os.TempDir is a per-user 0700 folder cleaned periodically and on boot, and
// on Linux systemd-tmpfiles cleans /tmp on a schedule. That backstop is only
// ever a backstop — chatupload.DefaultStaleTTL (24h) is far shorter than any
// of those windows, so the janitor is what actually reclaims a file — but it
// means an upload directory cannot outlive the daemon that stopped sweeping
// it. The previous location, <appDataDir>/chat-uploads, had no such floor:
// for a daemon whose app data dir sits inside a checkout (the dev-mode
// `.config` layout), the files accumulated in the user's working tree with
// nothing but a stopped daemon's janitor to remove them.
//
// The 0700 promise the manager makes (0600 files inside) survives the move,
// which is exactly what the comment this replaces worried about. The uploads
// never sit in a shared /tmp directly: they sit one level down, in a directory
// NewManager creates and then proves is a real directory — not a symlink — at
// mode 0700, so another local user can neither list it nor replace a file
// inside it. The uid suffix keeps two users on one host off the same name; the
// loser fails that check and disables uploads rather than sharing a directory.
const chatUploadDirName = "bossd-chat-uploads"

// legacyChatUploadDirName is the pre-move directory name, kept only so
// removeLegacyChatUploadDir can reclaim what a previous daemon left in the
// app data dir.
const legacyChatUploadDirName = "chat-uploads"

// chatUploadDir resolves the per-user upload directory.
//
// The name is FIXED rather than randomized (os.MkdirTemp) on purpose: the
// manager's janitor sweeps a directory inherited from a previous daemon run
// on its first pass, and a fresh random name every start would strand each
// run's leftovers in a directory nothing sweeps again.
func chatUploadDir() string {
	return filepath.Join(os.TempDir(), fmt.Sprintf("%s-%d", chatUploadDirName, os.Getuid()))
}

// removeLegacyChatUploadDir reclaims the pre-move upload directory, which no
// janitor sweeps once the manager points elsewhere. Best-effort: a failure
// here is reported by the caller and changes nothing else about startup.
//
// It removes only entries whose name carries the randomized "upload-" prefix
// the manager gives every file it creates, then rmdirs the directory itself —
// which succeeds only if it is now empty. That is deliberately narrower than
// os.RemoveAll: this path is derived from a resolved app data dir, and a
// mis-resolved one must not be able to take anything with it.
//
// The Lstat gate below is what makes that narrowing hold. os.ReadDir resolves
// a symlink AT THIS PATH, and the os.Remove calls below then resolve through
// it as a path component, so a symlink at the legacy path would redirect the
// whole reclaim into its target and unlink every "upload-" file there — files
// this daemon never wrote — silently, once per start. Scope that claim to the
// directory path deliberately: os.Remove on a symlink ENTRY inside a real
// legacy directory unlinks the link, never its target, so the loop below needs
// no per-entry guard and must not grow one. Refusing a symlink outright is the
// mitigation, mirroring the same Lstat/ModeSymlink/IsDir sequence
// newManagerWithChmod runs on the live upload directory. Its third check, the
// chmod-as-ownership-proof, is deliberately NOT copied: this is a delete-only
// path and must not mutate a directory it is about to refuse.
//
// Scope of the guarantee, precisely: Lstat inspects the FINAL path component,
// so this proves <appDataDir>/chat-uploads is itself a real directory — as of
// the Lstat call, not for the duration of the walk below. Two things it does
// not cover, both accepted rather than closed:
//   - Ancestors are still resolved by the kernel, so a symlinked appDataDir or
//     intermediate component still redirects the reclaim. They are trusted
//     because they derive from the operator-configured database path
//     (appDataDir := filepath.Dir(dbPath) in main.go).
//   - The window between this Lstat and the ReadDir below is not closed. The
//     persistent-symlink case is, and the residual race additionally requires
//     a concurrent local writer inside the app data directory.
func removeLegacyChatUploadDir(appDataDir string) error {
	dir := filepath.Join(appDataDir, legacyChatUploadDirName)
	info, statErr := os.Lstat(dir)
	switch {
	case statErr != nil:
		// Already gone is the overwhelmingly common case, and is success.
		if errors.Is(statErr, fs.ErrNotExist) {
			return nil
		}
		// Anything else is unexplained: fail closed, enumerating nothing.
		return fmt.Errorf("stat legacy upload dir: %w", statErr)
	case info.Mode()&fs.ModeSymlink != 0:
		// The remediation is named because the caller logs this on EVERY
		// start; without a way to end it the warning is permanent noise.
		return fmt.Errorf("legacy upload path %q is a symlink and was not reclaimed; remove the stale symlink at that path", dir)
	case !info.IsDir():
		// Remediation named for the same reason as the symlink branch: the
		// caller logs this on every start, so the operator needs a way to
		// end it.
		return fmt.Errorf("legacy upload path %q is not a directory and was not reclaimed; remove whatever is at that path", dir)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		// The gate above already answered the common already-gone case. This
		// branch survives as the race backstop: the directory can be removed
		// between that Lstat and this ReadDir, and a vanished directory is
		// still exactly the outcome this function wanted.
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("read legacy upload dir: %w", err)
	}
	kept := 0
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasPrefix(entry.Name(), "upload-") {
			kept++
			continue
		}
		if rmErr := os.Remove(filepath.Join(dir, entry.Name())); rmErr != nil && err == nil {
			err = fmt.Errorf("remove legacy upload file: %w", rmErr)
		}
	}
	if err != nil {
		return err
	}
	// Something not ours is in there. Leave it, and leave the directory
	// standing around it — the bytes this exists to reclaim are already gone,
	// and an rmdir here would only fail with ENOTEMPTY, which is not news.
	if kept > 0 {
		return nil
	}
	if rmErr := os.Remove(dir); rmErr != nil && !errors.Is(rmErr, fs.ErrNotExist) {
		return fmt.Errorf("remove legacy upload dir: %w", rmErr)
	}
	return nil
}

// chatUploadSender adapts the daemon's verified chat-delivery RPC to the
// narrow chatupload.ChatMessageSender interface, so the upload manager can
// submit its one path instruction without importing the server package.
//
// The Server is wired post-hoc (after server.New) — the same pattern as
// creatorAdapter and cmdHandlerStream.Waker — because the manager is
// constructed earlier, alongside the terminal stream client that drives it.
type chatUploadSender struct {
	mu     sync.RWMutex
	server chatMessageSender
}

// setServer binds the live server. Called once during startup, before any
// stream is running; the lock is there so the race detector sees a happens
// -before edge rather than to support rebinding.
func (s *chatUploadSender) setServer(server chatMessageSender) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.server = server
}

// SendChatMessage implements chatupload.ChatMessageSender.
//
// The UNCONFIRMED branch is load-bearing: SendChatMessage answers a submit
// whose verifier could not read the pane with delivered=false plus
// DELIVERY_STATE_UNCONFIRMED, meaning the prompt MAY already be running in
// the agent. Wrapping chatupload.ErrDeliveryUnconfirmed tells the manager
// to KEEP the uploaded file — deleting it would strand an already-delivered
// path on a file that no longer exists. Every other undelivered state is
// a definite non-delivery, and the manager removes the file.
//
// The adapter stays generic over submit even though its only caller now passes
// false (the upload prefills its path rather than submitting it, so no
// verifier runs and UNCONFIRMED cannot originate here): the branch costs one
// comparison, and the alternative is an adapter that silently mistranslates
// the day something submits through it.
func (s *chatUploadSender) SendChatMessage(ctx context.Context, chatID, message string, submit bool) error {
	s.mu.RLock()
	server := s.server
	s.mu.RUnlock()
	if server == nil {
		return errors.New("chat upload: chat delivery not wired")
	}

	resp, err := server.SendChatMessage(ctx, connect.NewRequest(&bossanovav1.SendChatMessageRequest{
		AgentSessionId: chatID,
		Message:        message,
		Submit:         submit,
		WakeIfAsleep:   true,
	}))
	if err != nil {
		return fmt.Errorf("chat upload: send chat message: %w", err)
	}
	if resp == nil || resp.Msg == nil {
		return errors.New("chat upload: empty send chat message response")
	}
	if resp.Msg.GetDelivered() {
		return nil
	}
	if resp.Msg.GetDeliveryState() == bossanovav1.SendChatMessageResponse_DELIVERY_STATE_UNCONFIRMED {
		return fmt.Errorf("%w: %s", chatupload.ErrDeliveryUnconfirmed, undeliveredReason(resp.Msg))
	}
	return fmt.Errorf("chat upload: %s", undeliveredReason(resp.Msg))
}
