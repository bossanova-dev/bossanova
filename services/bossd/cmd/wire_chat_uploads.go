package main

import (
	"github.com/rs/zerolog/log"

	"github.com/recurser/bossd/internal/chatupload"
)

func (w *daemonWiring) wireChatUploads() {
	var (
		chatUploadErr error
	)
	// --- BOS-661 chat file uploads ---
	//
	// The manager owns the daemon-local upload directory (0700, 0600 files
	// inside) and the streamed writes the TerminalStream feeds it. It is
	// built here, before the upstream block, for two reasons: the terminal
	// stream client below takes it as a dependency, and the stale-file
	// janitor must run even on a daemon with no upstream configured, so a
	// directory left behind by a previous run is still swept.
	//
	// Fail-soft: an unusable upload directory disables uploads (every
	// upload frame is answered with a permanent "not supported" result)
	// rather than refusing to start the daemon over an optional feature.
	w.chatUploads = &chatUploadSender{}
	// Reclaim the pre-move upload directory. Nothing sweeps it now that the
	// manager points at the system temp dir, so without this the files a
	// previous daemon left under the app data dir — inside the user's checkout,
	// on a dev-mode layout — would sit there forever.
	if legacyErr := removeLegacyChatUploadDir(w.appDataDir); legacyErr != nil {
		// Refusal-neutral: the reclaim can decline to touch anything at all
		// (a symlink or non-directory at that path), so this must not assert
		// a partial removal that never happened.
		log.Warn().Err(legacyErr).Msg("legacy chat upload directory not reclaimed")
	}
	w.chatUploadMgr, chatUploadErr = chatupload.NewManager(chatUploadDir(), w.chatUploads)
	if chatUploadErr != nil {
		log.Warn().Err(chatUploadErr).Msg("chat file uploads disabled: upload directory unavailable")
		w.chatUploadMgr = nil
	}
	// Converted explicitly rather than assigned: handing the terminal
	// stream client a typed-nil *chatupload.Manager would read as a
	// non-nil interface at its `uploads == nil` guard and panic on the
	// first upload frame.

	if w.chatUploadMgr != nil {
		w.chatUploadManager = w.chatUploadMgr
	}

}
