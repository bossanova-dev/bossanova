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
	sharedplugin "github.com/recurser/bossalib/plugin"
	"github.com/recurser/bossalib/vcs"
	"github.com/recurser/bossd/internal/claude"
	"github.com/recurser/bossd/internal/db"
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
	cfg             config.PluginConfig
	client          *goplugin.Client
	taskSource      TaskSource      // cached dispensed interface, nil if not a task source
	workflowService WorkflowService // cached dispensed interface, nil if not a workflow service
	startedAt       time.Time
}

// Host manages the lifecycle of all configured plugins.
type Host struct {
	mu          sync.Mutex
	plugins     []managedPlugin
	eventBus    *eventbus.Bus
	hostService *HostServiceServer
	logger      zerolog.Logger
	cancel      context.CancelFunc
	wg          sync.WaitGroup
}

// New creates a plugin host. The VCS provider is used to create a
// HostServiceServer that plugins can call back to via the broker.
// If provider is nil, plugins will not have access to host services.
// Call Start to launch plugins.
func New(eventBus *eventbus.Bus, provider vcs.Provider, logger zerolog.Logger) *Host {
	var hostService *HostServiceServer
	if provider != nil {
		hostService = NewHostServiceServer(provider)
	}
	return &Host{
		eventBus:    eventBus,
		hostService: hostService,
		logger:      logger.With().Str("component", "plugin-host").Logger(),
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
			Plugins:         NewPluginMap(h.hostService),
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

		mp := managedPlugin{
			cfg:       cfg,
			client:    client,
			startedAt: time.Now(),
		}

		// Try to dispense the TaskSource interface. Not all plugins
		// implement it, so we silently skip on failure.
		rpcClient, err := client.Client()
		if err == nil {
			raw, err := rpcClient.Dispense(sharedplugin.PluginTypeTaskSource)
			if err == nil {
				if ts, ok := raw.(TaskSource); ok {
					mp.taskSource = ts
					h.logger.Info().Str("plugin", cfg.Name).Msg("dispensed TaskSource interface")
				}
			}

			// Try to dispense the WorkflowService interface.
			raw, err = rpcClient.Dispense(sharedplugin.PluginTypeWorkflow)
			if err == nil {
				if ws, ok := raw.(WorkflowService); ok {
					mp.workflowService = ws
					h.logger.Info().Str("plugin", cfg.Name).Msg("dispensed WorkflowService interface")
				}
			}
		}

		h.plugins = append(h.plugins, mp)
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

// GetTaskSources returns the cached TaskSource interfaces for all plugins
// that implement it. The interfaces are dispensed once at startup and
// reused for each poll cycle.
func (h *Host) GetTaskSources() []TaskSource {
	h.mu.Lock()
	defer h.mu.Unlock()

	var sources []TaskSource
	for _, p := range h.plugins {
		if p.taskSource != nil {
			sources = append(sources, p.taskSource)
		}
	}
	return sources
}

// SetWorkflowDeps injects workflow and attempt dependencies into the host
// service so that plugins can create/manage workflows and Claude attempts.
func (h *Host) SetWorkflowDeps(store db.WorkflowStore, runner claude.ClaudeRunner) {
	if h.hostService != nil {
		h.hostService.SetWorkflowDeps(store, runner)
	}
}

// GetWorkflowServices returns the cached WorkflowService interfaces for all
// plugins that implement it. The interfaces are dispensed once at startup
// and reused.
func (h *Host) GetWorkflowServices() []WorkflowService {
	h.mu.Lock()
	defer h.mu.Unlock()

	var services []WorkflowService
	for _, p := range h.plugins {
		if p.workflowService != nil {
			services = append(services, p.workflowService)
		}
	}
	return services
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
