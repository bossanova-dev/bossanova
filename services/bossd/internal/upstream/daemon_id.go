package upstream

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/recurser/bossalib/sqlutil"
)

// daemonIDFileName is the file under the daemon's data dir that holds the
// persisted, stable machine identifier.
const daemonIDFileName = "daemon-id"

// ResolveDaemonID returns the daemon_id, preferring (in order):
//
//  1. the BOSSD_DAEMON_ID env var, when set (explicit override);
//  2. a UUID persisted under dataDir, generated once on first run and
//     reused thereafter — stable across hostname changes and restarts;
//  3. the machine hostname, as a last-resort fallback when the persisted
//     file can't be read or written.
//
// Deriving the id from a persisted UUID rather than the hostname stops
// daemon_id rotation: a hostname change (kamikai.local -> kamikai-2.local)
// would otherwise orphan the old id's rows in the orchestrator read model,
// where they linger as phantom sessions. A non-nil error means the
// persisted path failed and the hostname fallback is returned; callers
// should log it but can still use the returned id for this run.
func ResolveDaemonID(getenv func(string) string, dataDir, hostname string) (string, error) {
	if getenv != nil {
		if v := strings.TrimSpace(getenv("BOSSD_DAEMON_ID")); v != "" {
			return v, nil
		}
	}
	if dataDir == "" {
		return hostname, fmt.Errorf("resolve daemon id: empty data dir; using hostname")
	}

	path := filepath.Join(dataDir, daemonIDFileName)
	if data, err := os.ReadFile(path); err == nil {
		if id := strings.TrimSpace(string(data)); id != "" {
			return id, nil
		}
	}

	// No usable persisted id — generate, persist, and return one.
	id, err := sqlutil.NewID()
	if err != nil {
		return hostname, fmt.Errorf("resolve daemon id: generate: %w", err)
	}
	if err := os.WriteFile(path, []byte(id+"\n"), 0o600); err != nil {
		// Returning the un-persisted id would re-rotate on the next start,
		// which is worse than a stable-within-run hostname; fall back.
		return hostname, fmt.Errorf("resolve daemon id: persist %s: %w", path, err)
	}
	return id, nil
}
