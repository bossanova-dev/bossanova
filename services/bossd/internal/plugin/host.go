// Package plugin manages the lifecycle of external plugins using
// hashicorp/go-plugin. Plugins are discovered from configuration,
// launched as subprocesses communicating over gRPC, and health-checked
// on a regular interval.
package plugin

import (
	"context"
	"fmt"
	"os/exec"
	"sync"
	"time"

	goplugin "github.com/hashicorp/go-plugin"
	"github.com/rs/zerolog"

	"github.com/recurser/bossalib/config"
	"github.com/recurser/bossd/internal/plugin/eventbus"
)

// healthCheckInterval is how often the host pings each plugin.
const healthCheckInterval = 30 * time.Second

// PluginStatus reports the runtime state of a single plugin.
type PluginStatus struct {
	Name      string
	Running   bool
	ID        string
	StartedAt time.Time
}

// managedPlugin tracks a single running plugin process.
type managedPlugin struct {
	cfg       config.PluginConfig
	client    *goplugin.Client
	startedAt time.Time
}

// Host manages the lifecycle of all configured plugins.
type Host struct {
	mu       sync.Mutex
	plugins  []managedPlugin
	eventBus *eventbus.Bus
	logger   zerolog.Logger
	cancel   context.CancelFunc
	wg       sync.WaitGroup
}

// New creates a plugin host. Call Start to launch plugins.
func New(eventBus *eventbus.Bus, logger zerolog.Logger) *Host {
	return &Host{
		eventBus: eventBus,
		logger:   logger.With().Str("component", "plugin-host").Logger(),
	}
}

// Start discovers and launches all enabled plugins from the given
// configuration. It also starts a background health-check goroutine.
func (h *Host) Start(ctx context.Context, cfgs []config.PluginConfig) error {
	h.mu.Lock()
	defer h.mu.Unlock()

	for _, cfg := range cfgs {
		if !cfg.Enabled {
			h.logger.Info().Str("plugin", cfg.Name).Msg("plugin disabled, skipping")
			continue
		}

		client := goplugin.NewClient(&goplugin.ClientConfig{
			HandshakeConfig: Handshake,
			Plugins:         PluginMap,
			Cmd:             exec.Command(cfg.Path),
			AllowedProtocols: []goplugin.Protocol{
				goplugin.ProtocolGRPC,
			},
			Logger: newHCLogAdapter(h.logger.With().Str("plugin", cfg.Name).Logger()),
		})

		// Start launches the subprocess and performs the handshake.
		_, err := client.Client()
		if err != nil {
			client.Kill()
			return fmt.Errorf("start plugin %q: %w", cfg.Name, err)
		}

		h.logger.Info().
			Str("plugin", cfg.Name).
			Str("path", cfg.Path).
			Str("id", client.ID()).
			Msg("plugin started")

		h.plugins = append(h.plugins, managedPlugin{
			cfg:       cfg,
			client:    client,
			startedAt: time.Now(),
		})
	}

	// Start health-check loop.
	hctx, cancel := context.WithCancel(ctx)
	h.cancel = cancel
	h.wg.Go(func() {
		h.healthCheckLoop(hctx)
	})

	h.logger.Info().Int("count", len(h.plugins)).Msg("plugin host started")
	return nil
}

// Stop gracefully kills all plugin processes and stops health checking.
// Stop is idempotent.
func (h *Host) Stop() error {
	h.mu.Lock()
	defer h.mu.Unlock()

	if h.cancel != nil {
		h.cancel()
		h.cancel = nil
	}

	// Wait for health-check goroutine to exit before killing plugins,
	// so we don't race on the plugins slice.
	h.mu.Unlock()
	h.wg.Wait()
	h.mu.Lock()

	for i := range h.plugins {
		h.plugins[i].client.Kill()
		h.logger.Info().Str("plugin", h.plugins[i].cfg.Name).Msg("plugin stopped")
	}
	h.plugins = nil

	h.logger.Info().Msg("plugin host stopped")
	return nil
}

// Plugins returns the current status of all managed plugins.
func (h *Host) Plugins() []PluginStatus {
	h.mu.Lock()
	defer h.mu.Unlock()

	statuses := make([]PluginStatus, len(h.plugins))
	for i, p := range h.plugins {
		statuses[i] = PluginStatus{
			Name:      p.cfg.Name,
			Running:   !p.client.Exited(),
			ID:        p.client.ID(),
			StartedAt: p.startedAt,
		}
	}
	return statuses
}

// healthCheckLoop periodically pings each plugin and logs failures.
func (h *Host) healthCheckLoop(ctx context.Context) {
	ticker := time.NewTicker(healthCheckInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			h.pingAll()
		}
	}
}

// pingAll checks each plugin's health.
func (h *Host) pingAll() {
	h.mu.Lock()
	defer h.mu.Unlock()

	for _, p := range h.plugins {
		if p.client.Exited() {
			h.logger.Warn().Str("plugin", p.cfg.Name).Msg("plugin process exited")
			continue
		}

		rpcClient, err := p.client.Client()
		if err != nil {
			h.logger.Warn().Err(err).Str("plugin", p.cfg.Name).Msg("health check: failed to get client")
			continue
		}

		if err := rpcClient.Ping(); err != nil {
			h.logger.Warn().Err(err).Str("plugin", p.cfg.Name).Msg("health check: ping failed")
		}
	}
}
