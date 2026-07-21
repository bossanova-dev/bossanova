// Package main: opencode on-disk introspection.
//
// Unlike codex (which persists each session as a rollout JSONL file that the
// twin's transcript.go tails), opencode keeps ALL session state in a single
// SQLite database (Drizzle ORM migrations, WAL mode). So the opencode
// introspection layer reads SQLite — not stdout, not a per-session file.
//
// Observed schema (pinned; opencode is undocumented and version-fragile, this
// was captured against a real ~/.local/share/opencode/opencode.db at
// opencode v1.17.x — treat table/column names as fragile and DEGRADE, never
// crash, on drift):
//
//	CREATE TABLE `session` (
//	  `id` text PRIMARY KEY,      -- ses_XXXXXXXXXXXXXXXXXXXX
//	  `project_id` text NOT NULL,
//	  `directory` text NOT NULL,  -- the session's working directory
//	  `title` text NOT NULL,      -- session title (auto-generated or renamed)
//	  `time_created` integer NOT NULL,
//	  `time_updated` integer NOT NULL,
//	  ... (many more columns; only id/directory/title/time_* are read here)
//	);
//	CREATE TABLE `message` (
//	  `id` text PRIMARY KEY,
//	  `session_id` text NOT NULL, -- FK -> session.id
//	  `time_created` integer NOT NULL,
//	  `time_updated` integer NOT NULL,
//	  `data` text NOT NULL        -- JSON blob; the message role lives at $.role
//	);
//
// The role ("user" | "assistant") is NOT a column — it is a field inside the
// message.data JSON blob, so LastTurnIsUser reads the newest message row and
// json-decodes its data to inspect role.
package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/rs/zerolog"

	"github.com/recurser/bossalib/loginshell"

	// modernc.org/sqlite is the pure-Go SQLite driver (registers the "sqlite"
	// database/sql driver). Pure-Go so the plugin cross-compiles without cgo,
	// matching the repo-wide pin (services/bossd, services/bosso).
	_ "modernc.org/sqlite"
)

// dbPathTimeout bounds the one-shot `opencode db path` subprocess. A status RPC
// must never block, so the resolver gives the binary a short window and
// degrades to env/XDG resolution if it does not answer.
const dbPathTimeout = 3 * time.Second

// queryTimeout bounds each read query so a pathological DB can never wedge a
// status RPC.
const queryTimeout = 2 * time.Second

// opencodeStore reads opencode's on-disk SQLite session store read-only and
// defensively. Every exported method degrades to a zero value (empty string /
// false / not-found) — never an error to the caller, never a panic — when the
// binary, DB file, table, or column is absent. A status RPC calling through it
// can therefore never crash or block on introspection.
type opencodeStore struct {
	logger     zerolog.Logger
	loginShell string

	// resolvePathFn, when non-nil, replaces the default DB-path resolver. It
	// exists so tests can point the store at a fixture DB without spawning the
	// real `opencode` binary. Production leaves it nil.
	resolvePathFn func() string

	mu              sync.Mutex
	cachedPath      string // memoized once a real DB file is found (stable per process)
	subprocessTried bool   // the `opencode db path` subprocess runs at most once
}

// GetChatTitle returns the session row's title for the given opencode session
// id, or "" when the id is empty / unknown / the DB is unavailable.
func (s *opencodeStore) GetChatTitle(sessionID string) string {
	if sessionID == "" {
		return ""
	}
	db, closeDB, ok := s.openDB()
	if !ok {
		return ""
	}
	defer closeDB()
	ctx, cancel := context.WithTimeout(context.Background(), queryTimeout)
	defer cancel()
	var title string
	// A missing `session` table or `title` column surfaces as a scan error
	// (no such table/column) and degrades to "" — exactly the drift tolerance
	// the schema-fragility risk demands.
	if err := db.QueryRowContext(ctx, "SELECT title FROM session WHERE id = ? LIMIT 1", sessionID).Scan(&title); err != nil {
		return ""
	}
	return title
}

// TranscriptExists reports whether a session row exists for the given opencode
// session id. It is the SQLite analogue of the codex twin's on-disk rollout
// presence check: "does opencode already have state for this session id?".
func (s *opencodeStore) TranscriptExists(sessionID string) bool {
	if sessionID == "" {
		return false
	}
	db, closeDB, ok := s.openDB()
	if !ok {
		return false
	}
	defer closeDB()
	ctx, cancel := context.WithTimeout(context.Background(), queryTimeout)
	defer cancel()
	var exists int
	if err := db.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM session WHERE id = ?)", sessionID).Scan(&exists); err != nil {
		return false
	}
	return exists == 1
}

// LastTurnIsUser reports whether the newest message in the session is a user
// turn (message.data JSON role == "user"). Returns false when the session has
// no messages, the DB/table/column is absent, or the newest message is an
// assistant turn.
func (s *opencodeStore) LastTurnIsUser(sessionID string) bool {
	if sessionID == "" {
		return false
	}
	db, closeDB, ok := s.openDB()
	if !ok {
		return false
	}
	defer closeDB()
	ctx, cancel := context.WithTimeout(context.Background(), queryTimeout)
	defer cancel()
	var data string
	// Newest message: order by time_created desc, breaking ties on id desc to
	// match the (session_id, time_created, id) index ordering opencode itself
	// uses. A session with no messages yields sql.ErrNoRows and degrades false.
	if err := db.QueryRowContext(ctx,
		"SELECT data FROM message WHERE session_id = ? ORDER BY time_created DESC, id DESC LIMIT 1",
		sessionID).Scan(&data); err != nil {
		return false
	}
	var msg struct {
		Role string `json:"role"`
	}
	if err := json.Unmarshal([]byte(data), &msg); err != nil {
		return false
	}
	return msg.Role == "user"
}

// ResolveInteractiveSessionID returns the id of the newest session whose
// working directory matches workDir, and whether one was found. opencode has no
// per-process rollout-fd resolution (the codex path binds a rollout file the
// codex process holds open); the SQLite newest-by-directory query IS the
// resolution strategy for opencode. Returns ("", false) when no session
// matches or the DB is unavailable.
func (s *opencodeStore) ResolveInteractiveSessionID(workDir string) (string, bool) {
	if workDir == "" {
		return "", false
	}
	db, closeDB, ok := s.openDB()
	if !ok {
		return "", false
	}
	defer closeDB()
	ctx, cancel := context.WithTimeout(context.Background(), queryTimeout)
	defer cancel()
	// Directory matching needs symlink-eval + Clean (session.directory is stored
	// as opencode saw it, which may differ from the caller's path by a symlink),
	// which SQL can't do — so scan newest-first and return the first path-equal
	// row. Session counts are modest; newest-first ordering makes the first
	// match the newest match.
	rows, err := db.QueryContext(ctx, "SELECT id, directory FROM session ORDER BY time_updated DESC, time_created DESC")
	if err != nil {
		return "", false
	}
	defer func() { _ = rows.Close() }()
	want := normalizeDir(workDir) // invariant across the scan; normalize once, not per row
	for rows.Next() {
		var id, dir string
		if err := rows.Scan(&id, &dir); err != nil {
			continue
		}
		if normalizeDir(dir) == want {
			return id, true
		}
	}
	// A driver/context error mid-scan surfaces here; the not-found degrade
	// contract makes ("", false) correct either way, but check it explicitly
	// rather than mistaking an aborted scan for a clean no-match.
	if rows.Err() != nil {
		return "", false
	}
	return "", false
}

// openDB resolves the DB path and opens it read-only, degrading cleanly (ok=false,
// never a surfaced error) when the DB can't be resolved or opened.
//
// It tries two read-only DSNs in order and uses the first that opens:
//
//  1. mode=ro — a plain read-only connection. In WAL mode (opencode's journal
//     mode) this takes a shared read mark and sees the writer's latest COMMITTED
//     state, INCLUDING commits still only in the -wal that have not been
//     checkpointed into the main file. That freshness is what a read racing a
//     LIVE opencode needs: an immutable open reads only the main file and would
//     miss a just-created session/message (empirically, it can even see "no such
//     table" while all data still sits in the -wal), so mode=ro is the correct
//     default. bossd runs opencode as the same user with a writable data dir, so
//     the shared -shm lock file this needs can always be created.
//  2. mode=ro&immutable=1 — fallback for the rare case where (1) cannot open
//     (e.g. a WAL DB on genuinely read-only media whose -shm cannot be created,
//     which SQLite reports as SQLITE_READONLY_CANTLOCK). immutable reads the main
//     file only and never needs a lock file, so it always opens; the cost is
//     possibly-stale reads (and, per the SQLite docs, an incorrect read if the
//     main file is concurrently checkpointed) — acceptable as a last resort since
//     every read is a single wrapped statement that worst-case degrades.
//
// NOTE: SQLite URI filenames are not percent-encoded here; an opencode data dir
// containing a `?` or `#` in its path would fail to open and (correctly) degrade
// to the zero value rather than crash.
func (s *opencodeStore) openDB() (db *sql.DB, closeDB func(), ok bool) {
	path := s.resolvePath()
	if path == "" {
		return nil, nil, false
	}
	for _, dsn := range []string{
		"file:" + path + "?mode=ro",
		"file:" + path + "?mode=ro&immutable=1",
	} {
		handle, err := sql.Open("sqlite", dsn)
		if err != nil {
			s.logger.Debug().Err(err).Str("dsn", dsn).Msg("opencode: sqlite open failed; trying next DSN")
			continue
		}
		// sql.Open is lazy; force one connection so a bad path/DSN degrades here.
		ctx, cancel := context.WithTimeout(context.Background(), queryTimeout)
		err = handle.PingContext(ctx)
		cancel()
		if err != nil {
			_ = handle.Close()
			s.logger.Debug().Err(err).Str("dsn", dsn).Msg("opencode: sqlite ping failed; trying next DSN")
			continue
		}
		return handle, func() { _ = handle.Close() }, true
	}
	s.logger.Debug().Str("path", path).Msg("opencode: sqlite open failed for all DSNs; degrading")
	return nil, nil, false
}

// resolvePath resolves the opencode DB path, memoizing a successful hit.
//
// Order (per the ticket, subprocess-first): `opencode db path` → OPENCODE_DB →
// XDG default. The undocumented, channel-dependent filename
// (`opencode-<channel>.db`) means the XDG fallback GLOBS `opencode*.db` rather
// than assuming a fixed name.
//
// Caching (the Open-Questions resolution): a resolved, existing path is
// memoized for the process lifetime so status RPCs don't re-resolve; the
// expensive `opencode db path` subprocess runs at most once (subprocessTried);
// the cheap env/XDG fallbacks are retried each call until a DB appears, so a
// plugin that starts before opencode's first run still resolves later.
//
// The mutex is held ONLY to read/update the memoized state — never across the
// up-to-3s `opencode db path` exec. The lone caller that wins the one-shot
// subprocess claim runs it lock-free; every concurrent status RPC that arrives
// during that first resolution falls straight through to the cheap env/XDG
// probes instead of blocking on the lock, preserving the store's "never block on
// introspection" guarantee.
func (s *opencodeStore) resolvePath() string {
	if s.resolvePathFn != nil {
		return s.resolvePathFn()
	}
	s.mu.Lock()
	if s.cachedPath != "" {
		p := s.cachedPath
		s.mu.Unlock()
		return p
	}
	// Claim the single subprocess attempt under the lock, but run it below with
	// the lock released.
	runSubprocess := !s.subprocessTried
	if runSubprocess {
		s.subprocessTried = true
	}
	s.mu.Unlock()

	if runSubprocess {
		if p := s.resolveViaSubprocess(); p != "" {
			return s.memoizePath(p)
		}
	}
	if p := os.Getenv("OPENCODE_DB"); p != "" && fileExists(p) {
		return s.memoizePath(p)
	}
	if p := resolveViaXDG(); p != "" {
		return s.memoizePath(p)
	}
	return ""
}

// memoizePath caches p as the resolved DB path if none is cached yet and returns
// the winning value. First writer wins, so concurrent resolvers converge on one
// stable path for the process lifetime.
func (s *opencodeStore) memoizePath(p string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cachedPath == "" {
		s.cachedPath = p
	}
	return s.cachedPath
}

// resolveViaSubprocess runs `opencode db path` (through the same login-shell
// wrap the run path uses, so nodenv/asdf shims resolve the binary) with a short
// timeout and returns its reported path when the binary exists, exits 0, and
// the path is a real file. Any failure returns "" so resolution falls through.
func (s *opencodeStore) resolveViaSubprocess() string {
	argv := []string{"opencode", "db", "path"}
	if s.loginShell != "" {
		argv = loginshell.Wrap(s.loginShell, loginshell.Flags(s.loginShell), argv)
	}
	ctx, cancel := context.WithTimeout(context.Background(), dbPathTimeout)
	defer cancel()
	// #nosec G204 G702 -- const `opencode db path` argv wrapped through the login shell; loginShell is trusted daemon config (Settings.Plugins[opencode]), not attacker input; no shell string interpolation; mirrors the run path and the version probe
	// owner=@recurser review-by=2027-01-18 issue=BOS-28
	out, err := exec.CommandContext(ctx, argv[0], argv[1:]...).Output()
	if err != nil {
		return ""
	}
	// Take the last non-empty line: a login-shell wrap may emit banner lines
	// before the command's own single-line path output.
	p := strings.TrimSpace(string(out))
	if idx := strings.LastIndexByte(p, '\n'); idx >= 0 {
		p = strings.TrimSpace(p[idx+1:])
	}
	if p != "" && fileExists(p) {
		return p
	}
	return ""
}

// resolveViaXDG returns the opencode DB path from the XDG data dir, globbing
// for the channel-dependent `opencode*.db` filename (preferring the canonical
// `opencode.db`). Returns "" when the data dir or DB is absent.
func resolveViaXDG() string {
	base := os.Getenv("XDG_DATA_HOME")
	if base == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return ""
		}
		base = filepath.Join(home, ".local", "share")
	}
	dir := filepath.Join(base, "opencode")
	if p := filepath.Join(dir, "opencode.db"); fileExists(p) {
		return p
	}
	matches, _ := filepath.Glob(filepath.Join(dir, "opencode*.db"))
	for _, m := range matches {
		// The glob excludes -wal/-shm sidecars (they don't end in ".db"), but
		// guard on the suffix and existence anyway.
		if strings.HasSuffix(m, ".db") && fileExists(m) {
			return m
		}
	}
	return ""
}

// fileExists reports whether path is an existing, non-directory file.
func fileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}

// sameWorkDir reports whether two working-directory paths refer to the same
// location after symlink-eval + Clean, mirroring the codex twin's sameWorkDir
// (plugins/bossd-plugin-codex/transcript.go). EvalSymlinks failure (e.g. a
// stored directory that no longer exists) falls back to Abs so comparison still
// works on cleaned absolute paths.
func sameWorkDir(a, b string) bool {
	return normalizeDir(a) == normalizeDir(b)
}

func normalizeDir(p string) string {
	if resolved, err := filepath.EvalSymlinks(p); err == nil {
		p = resolved
	} else if abs, err := filepath.Abs(p); err == nil {
		p = abs
	}
	return filepath.Clean(p)
}
