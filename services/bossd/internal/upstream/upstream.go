// Package upstream manages the daemon's connection to the cloud orchestrator.
// It handles registration, heartbeat, and reconnection with exponential backoff.
package upstream

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"sync"
	"time"

	"connectrpc.com/connect"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/gen/bossanova/v1/bossanovav1connect"
	"github.com/recurser/bossalib/safego"
	"github.com/rs/zerolog"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// Config holds the configuration for the upstream connection.
type Config struct {
	OrchestratorURL string // e.g. "https://api.bossanova.dev"
	DaemonID        string // unique daemon identifier
	Hostname        string // machine hostname
	UserJWT         string // user's OIDC JWT for initial registration
}

// ConfigFromEnv reads upstream configuration from environment variables.
// Returns nil if BOSSD_ORCHESTRATOR_URL is not set (local-only mode).
func ConfigFromEnv() *Config {
	url := os.Getenv("BOSSD_ORCHESTRATOR_URL")
	if url == "" {
		return nil
	}

	hostname, _ := os.Hostname()

	return &Config{
		OrchestratorURL: url,
		DaemonID:        os.Getenv("BOSSD_DAEMON_ID"),
		Hostname:        hostname,
		UserJWT:         os.Getenv("BOSSD_USER_JWT"),
	}
}

const (
	heartbeatInterval = 30 * time.Second
	initialBackoff    = 1 * time.Second
	maxBackoff        = 60 * time.Second
	backoffMultiplier = 2.0
)

// Manager coordinates the daemon's upstream connection to the orchestrator.
type Manager struct {
	client bossanovav1connect.OrchestratorServiceClient
	config Config
	logger zerolog.Logger

	mu           sync.RWMutex
	connected    bool
	sessionToken string // returned by RegisterDaemon, used for heartbeat auth
	repoIDs      []string

	stopCh chan struct{}
	done   chan struct{}
}

// NewManager creates a Manager. Call Connect to start the registration and heartbeat loop.
func NewManager(cfg Config, logger zerolog.Logger) *Manager {
	httpClient := &http.Client{Timeout: 30 * time.Second}
	client := bossanovav1connect.NewOrchestratorServiceClient(httpClient, cfg.OrchestratorURL)

	return &Manager{
		client: client,
		config: cfg,
		logger: logger.With().Str("component", "upstream").Logger(),
		stopCh: make(chan struct{}),
		done:   make(chan struct{}),
	}
}

// newManagerWithClient creates a Manager with a custom client (for testing).
func newManagerWithClient(cfg Config, client bossanovav1connect.OrchestratorServiceClient, logger zerolog.Logger) *Manager {
	return &Manager{
		client: client,
		config: cfg,
		logger: logger.With().Str("component", "upstream").Logger(),
		stopCh: make(chan struct{}),
		done:   make(chan struct{}),
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

	safego.Go(m.logger, m.heartbeatLoop)
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

// Stop terminates the heartbeat loop and waits for it to finish.
func (m *Manager) Stop() {
	close(m.stopCh)
	<-m.done
	m.mu.Lock()
	m.connected = false
	m.mu.Unlock()
	m.logger.Info().Msg("upstream connection stopped")
}

// register calls RegisterDaemon with the user's JWT.
func (m *Manager) register(ctx context.Context) error {
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

	req := connect.NewRequest(&pb.HeartbeatRequest{
		DaemonId:       m.config.DaemonID,
		Timestamp:      timestamppb.Now(),
		ActiveSessions: 0, // TODO: wire to lifecycle session count
	})
	req.Header().Set("Authorization", "Bearer "+token)

	_, err := m.client.Heartbeat(context.Background(), req)
	return err
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
