// Package upstream manages the daemon's connection to the cloud orchestrator.
// It handles registration, heartbeat, and reconnection with exponential backoff.
package upstream

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"sync"
	"time"

	"encoding/json"
	"io"
	"net/url"
	"strings"

	"connectrpc.com/connect"
	"github.com/99designs/keyring"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/gen/bossanova/v1/bossanovav1connect"
	"github.com/recurser/bossalib/keyringutil"
	"github.com/recurser/bossalib/safego"
	"github.com/rs/zerolog"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// SessionLister provides access to the daemon's active sessions for syncing.
type SessionLister interface {
	ListSessions(ctx context.Context) ([]*pb.Session, error)
}

// Config holds the configuration for the upstream connection.
type Config struct {
	OrchestratorURL string // e.g. "https://api.bossanova.dev"
	DaemonID        string // unique daemon identifier
	Hostname        string // machine hostname
	UserJWT         string // user's OIDC JWT for initial registration
}

// defaultOrchestratorURL is the production bosso that bossd syncs with
// when BOSSD_ORCHESTRATOR_URL is unset. Set the env var to an empty string
// to force local-only mode (dev), or to a different URL (staging, self-host).
const defaultOrchestratorURL = "https://orchestrator.bossanova.dev"

// ConfigFromEnv reads upstream configuration from environment variables.
// Unset BOSSD_ORCHESTRATOR_URL uses defaultOrchestratorURL; an explicitly
// empty value opts out and returns nil (local-only mode). If BOSSD_USER_JWT
// is not set, falls back to reading the access token from the OS keychain
// (shared with "boss login").
func ConfigFromEnv() *Config {
	url, set := os.LookupEnv("BOSSD_ORCHESTRATOR_URL")
	if !set {
		url = defaultOrchestratorURL
	}
	if url == "" {
		return nil
	}

	hostname, _ := os.Hostname()

	jwt := os.Getenv("BOSSD_USER_JWT")
	if jwt == "" {
		jwt = loadTokenFromKeychain()
	}

	return &Config{
		OrchestratorURL: url,
		DaemonID:        os.Getenv("BOSSD_DAEMON_ID"),
		Hostname:        hostname,
		UserJWT:         jwt,
	}
}

// keychainTokens mirrors the boss CLI token structure for keychain reading.
type keychainTokens struct {
	AccessToken  string    `json:"access_token"`
	RefreshToken string    `json:"refresh_token,omitempty"`
	ExpiresAt    time.Time `json:"expires_at"`
}

// openKeyring opens the shared bossanova keyring. bossd runs as a daemon
// with no flag plumbing, so allowInsecure is hard-wired to false here — a
// broken environment should surface a real error rather than silently
// reverting to the hardcoded passphrase.
func openKeyring() (keyring.Keyring, error) {
	return keyring.Open(keyring.Config{
		ServiceName:              "bossanova",
		KeychainTrustApplication: true,
		FileDir:                  "~/.config/bossanova/keyring",
		FilePasswordFunc:         keyring.PromptFunc(keyringutil.New(false)),
	})
}

// keychainWarnOnce ensures the "run boss login to reset" hint is logged at
// most once per daemon process even when loadTokenFromKeychain is called
// multiple times.
var keychainWarnOnce sync.Once

func warnKeychainOnce(err error) {
	keychainWarnOnce.Do(func() {
		fmt.Fprintf(os.Stderr,
			"bossd: could not read stored credentials (%v) — run 'boss logout && boss login' on this host if auth was previously configured\n",
			err,
		)
	})
}

// loadTokenFromKeychain reads the access token stored by "boss login",
// refreshing it via the WorkOS API if expired. Returns "" when no tokens
// are stored or when the stored tokens can't be read; a decrypt failure
// (stale passphrase after the keyringutil rollout) is logged once.
func loadTokenFromKeychain() string {
	ring, err := openKeyring()
	if err != nil {
		return ""
	}

	item, err := ring.Get("workos-tokens")
	if err != nil {
		if !errors.Is(err, keyring.ErrKeyNotFound) {
			warnKeychainOnce(err)
		}
		return ""
	}

	var tokens keychainTokens
	if err := json.Unmarshal(item.Data, &tokens); err != nil {
		warnKeychainOnce(err)
		return ""
	}

	// Token still valid — use it directly.
	if tokens.AccessToken != "" && time.Now().Before(tokens.ExpiresAt) {
		return tokens.AccessToken
	}

	// Try to refresh using the refresh token.
	clientID := os.Getenv("BOSS_WORKOS_CLIENT_ID")
	if tokens.RefreshToken == "" || clientID == "" {
		return tokens.AccessToken // return possibly-expired token as last resort
	}

	refreshed, err := refreshWorkOSToken(clientID, tokens.RefreshToken)
	if err != nil {
		return tokens.AccessToken
	}

	// Preserve refresh token if the response didn't include a new one.
	if refreshed.RefreshToken == "" {
		refreshed.RefreshToken = tokens.RefreshToken
	}

	// Save refreshed tokens back to keychain.
	data, err := json.Marshal(refreshed)
	if err == nil {
		_ = ring.Set(keyring.Item{
			Key:         "workos-tokens",
			Data:        data,
			Label:       "Bossanova",
			Description: "WorkOS authentication tokens",
		})
	}

	return refreshed.AccessToken
}

// refreshWorkOSToken exchanges a refresh token for a new access token.
func refreshWorkOSToken(clientID, refreshToken string) (*keychainTokens, error) {
	data := url.Values{
		"grant_type":    {"refresh_token"},
		"client_id":     {clientID},
		"refresh_token": {refreshToken},
	}

	resp, err := http.Post(
		"https://api.workos.com/user_management/authenticate",
		"application/x-www-form-urlencoded",
		strings.NewReader(data.Encode()),
	)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("refresh failed (HTTP %d): %s", resp.StatusCode, string(body))
	}

	var result struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		ExpiresIn    int    `json:"expires_in"`
		User         struct {
			Email string `json:"email"`
		} `json:"user"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return nil, err
	}

	return &keychainTokens{
		AccessToken:  result.AccessToken,
		RefreshToken: result.RefreshToken,
		ExpiresAt:    time.Now().Add(time.Duration(result.ExpiresIn) * time.Second),
	}, nil
}

const (
	heartbeatInterval = 30 * time.Second
	syncInterval      = 30 * time.Second
	initialBackoff    = 1 * time.Second
	maxBackoff        = 60 * time.Second
	backoffMultiplier = 2.0
)

// Manager coordinates the daemon's upstream connection to the orchestrator.
type Manager struct {
	client   bossanovav1connect.OrchestratorServiceClient
	config   Config
	logger   zerolog.Logger
	sessions SessionLister

	mu           sync.RWMutex
	connected    bool
	running      bool   // true while heartbeat/sync goroutines are active
	sessionToken string // returned by RegisterDaemon, used for heartbeat auth
	repoIDs      []string

	stopCh   chan struct{}
	done     chan struct{}
	syncDone chan struct{}
}

// NewManager creates a Manager. Call Connect to start the registration and heartbeat loop.
func NewManager(cfg Config, logger zerolog.Logger, sessions SessionLister) *Manager {
	httpClient := &http.Client{Timeout: 30 * time.Second}
	client := bossanovav1connect.NewOrchestratorServiceClient(httpClient, cfg.OrchestratorURL)

	return &Manager{
		client:   client,
		config:   cfg,
		logger:   logger.With().Str("component", "upstream").Logger(),
		sessions: sessions,
		stopCh:   make(chan struct{}),
		done:     make(chan struct{}),
		syncDone: make(chan struct{}),
	}
}

// newManagerWithClient creates a Manager with a custom client (for testing).
func newManagerWithClient(cfg Config, client bossanovav1connect.OrchestratorServiceClient, logger zerolog.Logger, sessions SessionLister) *Manager {
	return &Manager{
		client:   client,
		config:   cfg,
		logger:   logger.With().Str("component", "upstream").Logger(),
		sessions: sessions,
		stopCh:   make(chan struct{}),
		done:     make(chan struct{}),
		syncDone: make(chan struct{}),
	}
}

// Connect registers with the orchestrator and starts the heartbeat loop.
// It blocks until registration succeeds or the context is cancelled.
// After successful registration, heartbeat runs in a background goroutine.
func (m *Manager) Connect(ctx context.Context, repoIDs []string) error {
	m.mu.Lock()
	m.repoIDs = repoIDs
	m.mu.Unlock()

	if err := m.register(ctx); err != nil {
		return fmt.Errorf("register: %w", err)
	}

	m.mu.Lock()
	m.running = true
	m.mu.Unlock()

	safego.Go(m.logger, m.heartbeatLoop)
	safego.Go(m.logger, m.syncLoop)
	return nil
}

// IsConnected returns true if the daemon is registered and heartbeating.
func (m *Manager) IsConnected() bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.connected
}

// SessionToken returns the daemon's session token for authenticated requests.
func (m *Manager) SessionToken() string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.sessionToken
}

// stopLoops terminates the heartbeat and sync goroutines if they are running.
// It is safe to call when loops are not running (no-op).
func (m *Manager) stopLoops() {
	m.mu.RLock()
	wasRunning := m.running
	m.mu.RUnlock()

	if !wasRunning {
		return
	}

	close(m.stopCh)
	<-m.done
	<-m.syncDone

	m.mu.Lock()
	m.running = false
	m.mu.Unlock()
}

// Stop terminates the heartbeat and sync loops and waits for them to finish.
func (m *Manager) Stop() {
	m.stopLoops()
	m.mu.Lock()
	m.connected = false
	m.mu.Unlock()
	m.logger.Info().Msg("upstream connection stopped")
}

// NotifyLogin re-reads credentials from the keychain, stops any existing
// loops, and reconnects to the orchestrator. This is called by the daemon
// server when the CLI notifies it of a successful login.
func (m *Manager) NotifyLogin(ctx context.Context, repoIDs []string) error {
	m.stopLoops()

	// Reinitialize channels for the new loop lifetime.
	m.stopCh = make(chan struct{})
	m.done = make(chan struct{})
	m.syncDone = make(chan struct{})

	return m.Connect(ctx, repoIDs)
}

// NotifyLogout stops the upstream connection loops and marks the manager as
// disconnected. Called by the daemon server when the CLI notifies it of a logout.
func (m *Manager) NotifyLogout() {
	m.Stop()
}

// register calls RegisterDaemon with the user's JWT.
func (m *Manager) register(ctx context.Context) error {
	// Refresh JWT from keychain if possible (token may have expired since startup).
	if jwt := loadTokenFromKeychain(); jwt != "" {
		m.config.UserJWT = jwt
	}

	req := connect.NewRequest(&pb.RegisterDaemonRequest{
		DaemonId: m.config.DaemonID,
		Hostname: m.config.Hostname,
		RepoIds:  m.repoIDs,
	})
	req.Header().Set("Authorization", "Bearer "+m.config.UserJWT)

	resp, err := m.client.RegisterDaemon(ctx, req)
	if err != nil {
		return err
	}

	m.mu.Lock()
	m.sessionToken = resp.Msg.SessionToken
	m.connected = true
	m.mu.Unlock()

	m.logger.Info().
		Str("daemon_id", resp.Msg.DaemonId).
		Msg("registered with orchestrator")

	return nil
}

// heartbeatLoop sends periodic heartbeats to the orchestrator.
// On failure, it retries with exponential backoff. If it fails too many
// times, it re-registers.
func (m *Manager) heartbeatLoop() {
	defer close(m.done)

	ticker := time.NewTicker(heartbeatInterval)
	defer ticker.Stop()

	consecutiveFailures := 0

	for {
		select {
		case <-m.stopCh:
			return
		case <-ticker.C:
			if err := m.sendHeartbeat(); err != nil {
				consecutiveFailures++
				m.logger.Warn().Err(err).
					Int("failures", consecutiveFailures).
					Msg("heartbeat failed")

				if consecutiveFailures >= 3 {
					m.mu.Lock()
					m.connected = false
					m.mu.Unlock()

					m.logger.Warn().Msg("lost connection, attempting re-registration")
					if err := m.reconnect(); err != nil {
						m.logger.Error().Err(err).Msg("re-registration failed")
					} else {
						consecutiveFailures = 0
					}
				}
			} else {
				if consecutiveFailures > 0 {
					m.logger.Info().Msg("heartbeat recovered")
				}
				consecutiveFailures = 0
			}
		}
	}
}

// sendHeartbeat sends a single heartbeat RPC.
func (m *Manager) sendHeartbeat() error {
	m.mu.RLock()
	token := m.sessionToken
	m.mu.RUnlock()

	// Get actual active session count
	activeCount := int32(0)
	if m.sessions != nil {
		sessions, err := m.sessions.ListSessions(context.Background())
		if err == nil {
			activeCount = int32(len(sessions))
		}
		// If list fails, fall back to 0 (don't break heartbeat)
	}

	req := connect.NewRequest(&pb.HeartbeatRequest{
		DaemonId:       m.config.DaemonID,
		Timestamp:      timestamppb.Now(),
		ActiveSessions: activeCount,
	})
	req.Header().Set("Authorization", "Bearer "+token)

	_, err := m.client.Heartbeat(context.Background(), req)
	return err
}

// syncLoop periodically syncs active sessions to the orchestrator.
func (m *Manager) syncLoop() {
	defer close(m.syncDone)

	ticker := time.NewTicker(syncInterval)
	defer ticker.Stop()

	for {
		select {
		case <-m.stopCh:
			return
		case <-ticker.C:
			// Only sync if we're connected
			if !m.IsConnected() {
				continue
			}

			if err := m.syncSessions(); err != nil {
				m.logger.Warn().Err(err).Msg("session sync failed")
			}
		}
	}
}

// syncSessions syncs the daemon's active sessions to the orchestrator.
func (m *Manager) syncSessions() error {
	if m.sessions == nil {
		return nil // No session lister configured
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	sessions, err := m.sessions.ListSessions(ctx)
	if err != nil {
		return fmt.Errorf("list sessions: %w", err)
	}

	m.mu.RLock()
	token := m.sessionToken
	m.mu.RUnlock()

	req := connect.NewRequest(&pb.SyncSessionsRequest{
		DaemonId: m.config.DaemonID,
		Sessions: sessions,
	})
	req.Header().Set("Authorization", "Bearer "+token)

	_, err = m.client.SyncSessions(ctx, req)
	if err != nil {
		return fmt.Errorf("sync RPC: %w", err)
	}

	m.logger.Debug().Int("count", len(sessions)).Msg("synced sessions to orchestrator")
	return nil
}

// reconnect attempts to re-register with exponential backoff.
func (m *Manager) reconnect() error {
	backoff := initialBackoff

	for {
		select {
		case <-m.stopCh:
			return fmt.Errorf("stopped during reconnect")
		default:
		}

		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		err := m.register(ctx)
		cancel()

		if err == nil {
			m.logger.Info().Msg("re-registered with orchestrator")
			return nil
		}

		m.logger.Warn().Err(err).
			Dur("backoff", backoff).
			Msg("re-registration failed, retrying")

		select {
		case <-m.stopCh:
			return fmt.Errorf("stopped during reconnect backoff")
		case <-time.After(backoff):
		}

		backoff = time.Duration(float64(backoff) * backoffMultiplier)
		if backoff > maxBackoff {
			backoff = maxBackoff
		}
	}
}
