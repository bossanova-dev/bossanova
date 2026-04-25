package upstream

import (
	"context"
	"os"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

// SessionLister provides access to the daemon's active sessions. The
// StreamClient derives its session snapshot from this interface via
// NewSessionSnapshotReader; bossd's cmd/main.go constructs the adapter
// directly over the db.SessionStore.
//
// Pre-Phase-7 this interface was also consumed by the legacy
// upstream.Manager (SyncSessions loop). The Manager was deleted with
// the reverse-stream rollout; this interface survived because the
// snapshot reader still needs it.
type SessionLister interface {
	ListSessions(ctx context.Context) ([]*pb.Session, error)
}

// Config holds the configuration for the upstream connection. Kept as a
// free-standing struct (rather than rolled into StreamClientConfig) so
// cmd/main.go can build the Connect client and registration call
// independently of the StreamClient wiring.
type Config struct {
	OrchestratorURL string // e.g. "https://orchestrator.bossanova.dev"
	DaemonID        string // unique daemon identifier
	Hostname        string // machine hostname
	UserJWT         string // user's OIDC JWT for initial registration
}

// defaultOrchestratorURL is the production bosso that bossd syncs with
// when BOSSD_ORCHESTRATOR_URL is unset. Set the env var to an empty string
// to force local-only mode (dev), or to a different URL (staging,
// self-host).
const defaultOrchestratorURL = "https://orchestrator.bossanova.dev"

// ConfigFromEnv reads upstream configuration from environment variables.
// Unset BOSSD_ORCHESTRATOR_URL uses defaultOrchestratorURL; an explicitly
// empty value opts out and returns nil (local-only mode). If
// BOSSD_DAEMON_ID is not set, falls back to the machine hostname so
// registration succeeds out of the box.
//
// Keychain fallback for BOSSD_USER_JWT is NOT performed here — the
// StreamClient's TokenProvider owns keychain reads / refreshes so the
// startup path and the long-lived refresh loop share a single source of
// truth. cmd/main.go pulls the initial token via
// NewKeychainTokenProvider().Token() when BOSSD_USER_JWT is empty.
func ConfigFromEnv() *Config {
	url, set := os.LookupEnv("BOSSD_ORCHESTRATOR_URL")
	if !set {
		url = defaultOrchestratorURL
	}
	if url == "" {
		return nil
	}

	hostname, _ := os.Hostname()
	daemonID := os.Getenv("BOSSD_DAEMON_ID")
	if daemonID == "" {
		daemonID = hostname
	}

	return &Config{
		OrchestratorURL: url,
		DaemonID:        daemonID,
		Hostname:        hostname,
		UserJWT:         os.Getenv("BOSSD_USER_JWT"),
	}
}
