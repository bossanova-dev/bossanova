// Package plugin manages the lifecycle of external plugins using
// hashicorp/go-plugin. Plugins are discovered from configuration,
// launched as subprocesses communicating over gRPC, and health-checked
// on a regular interval.
package plugin

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"sync"
	"time"

	goplugin "github.com/hashicorp/go-plugin"
	"github.com/rs/zerolog"

	"github.com/recurser/bossalib/config"
	bossanovav1 "github.com/recurser/bossalib/gen/bossanova/v1"
	sharedplugin "github.com/recurser/bossalib/plugin"
	"github.com/recurser/bossalib/safego"
	"github.com/recurser/bossalib/vcs"
	"github.com/recurser/bossd/internal/agent"
	"github.com/recurser/bossd/internal/db"
	"github.com/recurser/bossd/internal/plugin/eventbus"
	"github.com/recurser/bossd/internal/status"
)

// notifyStatusChangeTimeout bounds each plugin's NotifyStatusChange RPC so a
// slow plugin can't leak goroutines when status changes fire frequently.
const notifyStatusChangeTimeout = 5 * time.Second

// healthCheckInterval is how often the host pings each plugin and, since the
// plugin-supervision change, relaunches any that have exited. A var (not a
// const) so tests can shrink it to drive the restart path deterministically —
// mirrors watchdogPollInterval in host_service.go.
var healthCheckInterval = 30 * time.Second

// Plugin restart backoff. When a plugin subprocess exits (crash, OOM, or an
// external SIGTERM — see the incident that motivated this: all plugins were
// SIGTERM'd at once and the host never relaunched them, so every agent op
// failed with "connection refused" for ~12 minutes until a full daemon
// restart), the health loop relaunches it. Backoff throttles a genuinely
// broken binary so a crash-on-startup plugin can't hot-loop respawns.
const (
	restartBackoffBase = 1 * time.Second
	restartBackoffMax  = 5 * time.Minute
	// restartLoudAfter escalates the failed-restart log to error once a
	// plugin has failed this many consecutive relaunch attempts, so a
	// persistently-broken binary is visible instead of buried at warn.
	restartLoudAfter = 5
)

// restartInfo tracks per-plugin relaunch bookkeeping. Keyed by plugin name in
// Host.restartState (a parallel map, not a managedPlugin field, so it survives
// the managedPlugin replacement performed on a successful respawn).
type restartInfo struct {
	attempts    int
	nextRetryAt time.Time
}

// nextRestartBackoff returns the delay before the next relaunch attempt for a
// plugin that has already failed `attempts` times. Exponential
// (restartBackoffBase << attempts) capped at restartBackoffMax. attempts is
// clamped so the shift can't overflow.
func nextRestartBackoff(attempts int) time.Duration {
	if attempts < 0 {
		attempts = 0
	}
	// Cap the shift: 1s << 33 already exceeds restartBackoffMax by orders of
	// magnitude, so clamping here keeps the arithmetic well-defined without
	// changing the capped result.
	if attempts > 33 {
		attempts = 33
	}
	// attempts is provably in [0, 33] here (guarded above), so the shift amount
	// is a non-negative, bounded uint — no overflow is possible.
	var shift uint
	if attempts > 0 {
		shift = uint(attempts)
	}
	d := restartBackoffBase << shift
	if d <= 0 || d > restartBackoffMax {
		return restartBackoffMax
	}
	return d
}

// PluginStatus reports the runtime state of a single plugin.
type PluginStatus struct {
	Name    string
	Path    string
	Enabled bool
	Loaded  bool
	// Running is the legacy alias for Loaded. Kept so existing tests and
	// callers don't have to migrate at the same time as ListPlugins.
	Running      bool
	Capabilities []string
	Error        string
	ID           string
	StartedAt    time.Time
}

// missedPlugin tracks a configured plugin that did not produce a running
// managedPlugin entry. We hold onto these so `boss plugin list` can show
// disabled and failed plugins next to the loaded ones — otherwise an
// operator with a typo'd path has no way to tell whether the daemon
// even saw their config.
type missedPlugin struct {
	cfg     config.PluginConfig
	failed  bool   // true when start was attempted and errored
	loadErr string // populated when failed
}

// managedPlugin tracks a single running plugin process.
type managedPlugin struct {
	cfg             config.PluginConfig
	client          *goplugin.Client
	taskSource      TaskSource      // cached dispensed interface, nil if not a task source
	workflowService WorkflowService // cached dispensed interface, nil if not a workflow service
	agentRunner     AgentRunner     // cached dispensed interface, nil if not an agent runner
	startedAt       time.Time
}

// Host manages the lifecycle of all configured plugins.
type Host struct {
	mu          sync.Mutex
	plugins     []managedPlugin
	misses      []missedPlugin
	eventBus    *eventbus.Bus
	hostService *HostServiceServer
	logger      zerolog.Logger
	cancel      context.CancelFunc
	wg          sync.WaitGroup
	// cookieValue is the per-startup magic-cookie secret, generated by
	// Start and used for every plugin client launched in this daemon run.
	cookieValue string
	// settings is captured at Start and reused by launchPlugin when the
	// health loop relaunches a plugin (preparePluginConfigForStart needs it).
	// Written once under h.mu in Start, read-only thereafter — same treatment
	// as cookieValue.
	settings config.Settings
	// restartState tracks per-plugin relaunch backoff, keyed by plugin name.
	// All access is under h.mu. Niled by Stop alongside plugins/misses.
	restartState map[string]*restartInfo
	// launchPluginFn is the launcher restartPlugin invokes. nil in production
	// (restartPlugin falls back to h.launchPlugin); a test seam so the restart
	// path's shutdown-cancellation behavior can be driven without spawning a
	// real subprocess with a hung handshake.
	launchPluginFn func(context.Context, config.PluginConfig) (managedPlugin, error)
	// desiredWorkflows declares, per workflow-plugin name, the StartWorkflow
	// request that SHOULD be running — populated by the daemon at boot from
	// config (before/independent of plugin discovery) and by the
	// StartRepairWorkflow RPC. Boot, the restart replay, the health-tick
	// watchdog, and the RPC all converge on EnsureWorkflowRunning, which
	// reads this as the single source of truth. An entry here means
	// "CANCELLED is never a legitimate steady state for this plugin";
	// PAUSED is operator intent and is never overridden. Any future
	// cancel/disable surface MUST delete the entry (add a removal method here)
	// or the watchdog will re-arm within one health tick. Guarded by h.mu;
	// niled by Stop.
	desiredWorkflows map[string]*workflowDesire
}

// workflowDesire pairs the canonical start request with a per-plugin
// single-flight lock. The lock serializes probe+start across boot, restart
// replay, watchdog ticks, and the StartRepairWorkflow RPC — StartWorkflow
// re-entry on a RUNNING workflow cancels in-flight repairs, so exactly one
// starter may act at a time and each starter re-probes under the lock.
type workflowDesire struct {
	mu sync.Mutex
	// req is guarded by the host's h.mu (NOT mu): SetWorkflowDesired writes it
	// under h.mu and EnsureWorkflowRunning captures it under h.mu. Reading it
	// under mu instead would split the field across two locks and reintroduce
	// the BOS-346 data race — keep every req access under h.mu.
	req *bossanovav1.StartWorkflowRequest
}

// hasEnabledPlugin reports whether any of the configs is marked Enabled.
func hasEnabledPlugin(cfgs []config.PluginConfig) bool {
	for _, c := range cfgs {
		if c.Enabled {
			return true
		}
	}
	return false
}

// pluginEnvVarPrefix is prepended to every key in cfg.Config when projecting
// the plugin's settings into its subprocess environment. Plugins read their
// own config via os.Getenv(pluginEnvVarPrefix+key).
const pluginEnvVarPrefix = "BOSS_PLUGIN_"

// pluginEnvFromConfig returns "BOSS_PLUGIN_<key>=<value>" entries for each
// pair in cfg.Config. The daemon merges these into the plugin subprocess
// environment so plugins (which run as separate binaries and therefore can't
// import the daemon's config package) can still see their own settings —
// notably the claude plugin's dangerously_skip_permissions toggle, which
// otherwise stays at its zero value and silently breaks /print mode in the
// repair workflow.
func pluginEnvFromConfig(cfg config.PluginConfig) []string {
	if len(cfg.Config) == 0 {
		return nil
	}
	env := make([]string, 0, len(cfg.Config))
	for k, v := range cfg.Config {
		env = append(env, pluginEnvVarPrefix+k+"="+v)
	}
	return env
}

// injectLoginShell adds the user's login shell to an agent plugin's config so
// the plugin can launch its CLI through it. No-op for non-agent plugins and
// when no login shell is known.
func injectLoginShell(pluginName string, cfg map[string]string, settings config.Settings) map[string]string {
	if settings.LoginShell == "" {
		return cfg
	}
	if !isAgentProviderPlugin(pluginName, settings) {
		return cfg
	}

	next := make(map[string]string, len(cfg)+1)
	for k, v := range cfg {
		next[k] = v
	}
	next["login_shell"] = settings.LoginShell
	return next
}

func injectSessionStartReadyDeadline(cfg map[string]string, settings config.Settings) map[string]string {
	next := make(map[string]string, len(cfg)+1)
	for k, v := range cfg {
		next[k] = v
	}
	next[config.SessionStartReadyDeadlinePluginKey] = fmt.Sprintf("%d", int(settings.TmuxDelivery.SessionStartReadyDeadline()/time.Second))
	return next
}

func isAgentProviderPlugin(pluginName string, settings config.Settings) bool {
	if pluginName == "codex" || pluginName == "claude" {
		return true
	}
	for _, known := range settings.KnownAgentProviders {
		if pluginName == known {
			return true
		}
	}
	return false
}

func preparePluginConfigForStart(cfg config.PluginConfig, settings config.Settings) config.PluginConfig {
	cfg.Config = injectLoginShell(cfg.Name, cfg.Config, settings)
	// Repair is not an agent provider, but its StartChatRun ceiling must track
	// the daemon's resolved readiness deadline too.
	cfg.Config = injectSessionStartReadyDeadline(cfg.Config, settings)
	return cfg
}

// generatePluginCookie returns a fresh 32-byte hex string for the per-startup
// magic cookie. It is called once per daemon startup (Host.Start).
func generatePluginCookie() (string, error) {
	var buf [32]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", fmt.Errorf("generate plugin cookie: %w", err)
	}
	return hex.EncodeToString(buf[:]), nil
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
		eventBus:         eventBus,
		hostService:      hostService,
		logger:           logger.With().Str("component", "plugin-host").Logger(),
		restartState:     make(map[string]*restartInfo),
		desiredWorkflows: make(map[string]*workflowDesire),
	}
}

// Start discovers and launches all enabled plugins from the given
// configuration. It also starts a background health-check goroutine.
func (h *Host) Start(ctx context.Context, cfgs []config.PluginConfig, settings config.Settings) error {
	h.mu.Lock()
	defer h.mu.Unlock()

	// Only validate host-service deps when there's at least one enabled
	// plugin that could actually call back. With no plugins running,
	// partially-wired hosts (e.g. unit tests) stay usable.
	if h.hostService != nil && hasEnabledPlugin(cfgs) {
		if err := h.hostService.Validate(); err != nil {
			return fmt.Errorf("plugin host: %w", err)
		}
	}

	cookie, err := generatePluginCookie()
	if err != nil {
		return err
	}
	h.cookieValue = cookie
	h.settings = settings
	if h.restartState == nil {
		h.restartState = make(map[string]*restartInfo)
	}

	// Registering the same plugin twice gives each instance its own
	// in-memory state, so per-session dedup maps (e.g. repair's
	// `repairing` map) can't coordinate across instances and both will
	// spawn a chat for the same trigger. Drop duplicates loudly so the
	// misconfiguration is visible — main.go also dedupes settings.Plugins
	// on load, this is the last-line defense in case a different caller
	// hands us a duplicated slice.
	if deduped, dropped := config.DedupPluginConfigs(cfgs); dropped {
		h.logger.Warn().Int("before", len(cfgs)).Int("after", len(deduped)).Msg("dropping duplicate plugin entries in Start")
		cfgs = deduped
	}

	for _, cfg := range cfgs {
		if !cfg.Enabled {
			h.logger.Info().Str("plugin", cfg.Name).Msg("plugin disabled, skipping")
			h.misses = append(h.misses, missedPlugin{cfg: cfg})
			continue
		}

		mp, err := h.launchPlugin(ctx, cfg)
		if err != nil {
			// A single failing plugin is logged and skipped rather than
			// aborting the host: one broken binary on disk (e.g. a stale build
			// after a schema change, or a plugin that panics on startup)
			// shouldn't DOS the daemon and block every other plugin from
			// loading. Validate() already enforces the stricter "fail loud on
			// unwired host deps" path before this loop.
			h.logger.Error().
				Err(err).
				Str("plugin", cfg.Name).
				Str("path", cfg.Path).
				Msg("failed to start plugin; skipping and continuing with remaining plugins")
			h.misses = append(h.misses, missedPlugin{
				cfg:     cfg,
				failed:  true,
				loadErr: err.Error(),
			})
			continue
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

// workflowStatusProbeTimeout bounds the GetWorkflowStatus probe and the
// StartWorkflow re-arm inside EnsureWorkflowRunning so a hung plugin can't
// wedge boot, the restart replay, the watchdog tick, or the RPC.
const workflowStatusProbeTimeout = 5 * time.Second

// SetWorkflowDesired declares that the workflow plugin `name` SHOULD be running
// with `req`. It is the single source of truth read by EnsureWorkflowRunning
// (boot, restart replay, health-tick watchdog, StartRepairWorkflow RPC). A nil
// req is a no-op — only real requests are declared desired. Create-or-update
// under h.mu; the per-name single-flight lock is preserved across updates.
func (h *Host) SetWorkflowDesired(name string, req *bossanovav1.StartWorkflowRequest) {
	if req == nil {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.desiredWorkflows == nil {
		// Stop niled it; the daemon is shutting down.
		return
	}
	if d := h.desiredWorkflows[name]; d != nil {
		d.req = req
		return
	}
	h.desiredWorkflows[name] = &workflowDesire{req: req}
}

// WorkflowDesire returns the declared StartWorkflow request for `name`, or nil
// if the workflow is not desired-started.
func (h *Host) WorkflowDesire(name string) *bossanovav1.StartWorkflowRequest {
	h.mu.Lock()
	defer h.mu.Unlock()
	if d := h.desiredWorkflows[name]; d != nil {
		return d.req
	}
	return nil
}

// EnsureWorkflowRunning is the single-flight primitive every re-arm path shares
// (boot, restart replay, health-tick watchdog, StartRepairWorkflow RPC). It
// probes the plugin's current WorkflowStatus and, unless it is already RUNNING
// or PAUSED, issues StartWorkflow with the declared desired request. The
// per-plugin lock serializes probe+start so concurrent callers issue at most one
// StartWorkflow (re-entry on a RUNNING workflow cancels in-flight repairs).
//
// Returns the observed status (the pre-start status when it did start one, so
// callers/logs can report what was healed FROM), whether it started a workflow,
// and an error when the workflow is not desired, no live plugin serves that
// name, or the probe/start RPC fails.
func (h *Host) EnsureWorkflowRunning(ctx context.Context, name string) (bossanovav1.WorkflowStatus, bool, error) {
	h.mu.Lock()
	desire := h.desiredWorkflows[name]
	// Capture the desired request under h.mu — the same lock SetWorkflowDesired
	// writes desire.req under. Reading it later only under desire.mu would leave
	// the field guarded by two different mutexes (a data race a concurrent
	// SetWorkflowDesired update would trip). A one-tick-stale req under a
	// concurrent re-declare is harmless and idempotent; single-flight still
	// re-probes status under desire.mu below.
	var desiredReq *bossanovav1.StartWorkflowRequest
	if desire != nil {
		desiredReq = desire.req
	}
	var svc WorkflowService
	for i := range h.plugins {
		if h.plugins[i].cfg.Name == name && h.plugins[i].workflowService != nil {
			if c := h.plugins[i].client; c == nil || !c.Exited() {
				svc = h.plugins[i].workflowService
			}
			break
		}
	}
	h.mu.Unlock()
	if desire == nil {
		return bossanovav1.WorkflowStatus_WORKFLOW_STATUS_UNSPECIFIED, false, fmt.Errorf("workflow %q is not desired-started", name)
	}
	if svc == nil {
		return bossanovav1.WorkflowStatus_WORKFLOW_STATUS_UNSPECIFIED, false, fmt.Errorf("no live workflow plugin %q", name)
	}

	desire.mu.Lock() // single-flight: serialize probe+start per plugin
	defer desire.mu.Unlock()

	probeCtx, cancel := context.WithTimeout(ctx, workflowStatusProbeTimeout)
	defer cancel()
	info, err := svc.GetWorkflowStatus(probeCtx, "")
	if err != nil {
		return bossanovav1.WorkflowStatus_WORKFLOW_STATUS_UNSPECIFIED, false, fmt.Errorf("probe workflow status for %q: %w", name, err)
	}
	switch info.GetStatus() {
	case bossanovav1.WorkflowStatus_WORKFLOW_STATUS_RUNNING,
		bossanovav1.WorkflowStatus_WORKFLOW_STATUS_PAUSED:
		// RUNNING is the steady state; PAUSED is operator intent. Neither is
		// re-armed (StartWorkflow re-entry on RUNNING cancels in-flight repairs).
		return info.GetStatus(), false, nil
	default:
		// UNSPECIFIED / PENDING / COMPLETED / FAILED / CANCELLED: the workflow
		// is not actively serving, so re-arm it from the desired request.
	}
	startCtx, cancelStart := context.WithTimeout(ctx, workflowStatusProbeTimeout)
	defer cancelStart()
	if _, err := svc.StartWorkflow(startCtx, desiredReq); err != nil {
		return info.GetStatus(), false, fmt.Errorf("start workflow %q: %w", name, err)
	}
	// Report the observed pre-start status (not the resulting RUNNING) so the
	// watchdog log names what it healed FROM — the signal this feature exists
	// to produce during an incident.
	return info.GetStatus(), true, nil
}

// launchPlugin spawns a single plugin subprocess, performs the go-plugin
// handshake, and dispenses+probes each service interface it implements.
//
// It MUST NOT be called while holding h.mu: client.Client() blocks on the
// subprocess handshake, and the health loop's restart path relies on launching
// without the lock so GetTaskSources/AgentRunnerByName/etc. stay responsive
// during a relaunch. It reads only immutable-after-Start host state
// (h.cookieValue, h.settings, h.hostService, h.logger). On handshake failure it
// kills the client and returns the error; the caller decides whether that's a
// startup miss (Start) or a failed restart attempt (pingAll backoff).
//
// ctx bounds the GetInfo probes so a plugin that handshakes but then hangs a
// probe can't block launchPlugin indefinitely: on cancellation the probe aborts
// and the interface is simply treated as un-dispensed. The go-plugin handshake
// (client.Client()) is bounded by go-plugin's own StartTimeout; the restart
// path additionally abandons the whole launch on cancellation (see
// restartPlugin) so daemon shutdown never waits on it.
func (h *Host) launchPlugin(ctx context.Context, cfg config.PluginConfig) (managedPlugin, error) {
	cfg = preparePluginConfigForStart(cfg, h.settings)

	// #nosec G204 -- runs an operator-configured plugin binary path (cfg.Path); local-trust, not attacker-controlled
	// owner=@recurser review-by=2027-01-18 issue=BOS-28
	cmd := exec.Command(cfg.Path)
	// Project per-plugin Config entries into the subprocess environment
	// (BOSS_PLUGIN_<key>=<value>). Plugins are separate binaries that
	// can't import bossalib/config, so this is how settings reach them.
	if extra := pluginEnvFromConfig(cfg); len(extra) > 0 {
		cmd.Env = append(os.Environ(), extra...)
	}

	client := goplugin.NewClient(&goplugin.ClientConfig{
		HandshakeConfig:  NewHandshake(h.cookieValue),
		VersionedPlugins: NewVersionedPluginMap(h.hostService),
		Cmd:              cmd,
		AllowedProtocols: []goplugin.Protocol{
			goplugin.ProtocolGRPC,
		},
		Logger: newHCLogAdapter(h.logger.With().Str("plugin", cfg.Name).Logger()),
	})

	// Client launches the subprocess and performs the handshake.
	rpcClient, err := client.Client()
	if err != nil {
		client.Kill()
		return managedPlugin{}, err
	}

	h.logger.Info().
		Str("plugin", cfg.Name).
		Str("path", cfg.Path).
		Str("id", client.ID()).
		Int("pid", pluginPID(client)).
		Msg("plugin started")

	mp := managedPlugin{
		cfg:       cfg,
		client:    client,
		startedAt: time.Now(),
	}

	// Try to dispense each interface. Because all plugin types share the same
	// gRPC connection, Dispense succeeds even if the plugin subprocess doesn't
	// implement the service. We probe with GetInfo to verify the service is
	// actually registered before caching.
	raw, derr := rpcClient.Dispense(sharedplugin.PluginTypeTaskSource)
	if derr == nil {
		if ts, ok := raw.(TaskSource); ok {
			if _, probeErr := ts.GetInfo(ctx); probeErr == nil {
				mp.taskSource = ts
				h.logger.Info().Str("plugin", cfg.Name).Msg("dispensed TaskSource interface")
			}
		}
	}

	raw, derr = rpcClient.Dispense(sharedplugin.PluginTypeWorkflow)
	if derr == nil {
		if ws, ok := raw.(WorkflowService); ok {
			if _, probeErr := ws.GetInfo(ctx); probeErr == nil {
				mp.workflowService = ws
				h.logger.Info().Str("plugin", cfg.Name).Msg("dispensed WorkflowService interface")
			}
		}
	}

	raw, derr = rpcClient.Dispense(sharedplugin.PluginTypeAgentRunner)
	if derr == nil {
		if ar, ok := raw.(AgentRunner); ok {
			if _, probeErr := ar.GetInfo(ctx); probeErr == nil {
				mp.agentRunner = ar
				h.logger.Info().Str("plugin", cfg.Name).Msg("dispensed AgentRunner interface")
			}
		}
	}

	return mp, nil
}

// pluginPID returns the subprocess PID for a go-plugin client, or 0 if it is
// not yet known. Logged at launch so an external signal (e.g. the SIGTERM that
// took down every plugin in the incident this supervision addresses) can be
// correlated to a PID after the fact.
func pluginPID(client *goplugin.Client) int {
	if client == nil {
		return 0
	}
	if rc := client.ReattachConfig(); rc != nil {
		return rc.Pid
	}
	return 0
}

// pluginClientID returns the go-plugin client's ID, or "" for a nil client.
// Nil-safe like pluginPID so the launchPluginFn test seam can drive the restart
// path (which logs the ID) without a real subprocess.
func pluginClientID(client *goplugin.Client) string {
	if client == nil {
		return ""
	}
	return client.ID()
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
		client := h.plugins[i].client
		pid := 0
		if rc := client.ReattachConfig(); rc != nil {
			pid = rc.Pid
		}
		killWithTimeout(h.logger, h.plugins[i].cfg.Name, client.Kill, pid, killTimeout)
		h.logger.Info().Str("plugin", h.plugins[i].cfg.Name).Msg("plugin stopped")
	}
	h.plugins = nil
	h.misses = nil
	h.restartState = nil
	h.desiredWorkflows = nil

	h.logger.Info().Msg("plugin host stopped")
	return nil
}

// killTimeout bounds how long Stop waits for a single plugin to shut down.
// go-plugin's own Kill already does graceful-close → 2s wait → SIGKILL, but
// it then blocks on an internal waitgroup that can stall indefinitely if a
// log-pump goroutine leaks. 3s gives go-plugin's graceful path a full chance
// to run before we fall through to the direct OS-level SIGKILL fallback.
const killTimeout = 3 * time.Second

// killWithTimeout runs kill in a panic-recovered goroutine and bounds the
// wait. If kill does not return within timeout, we SIGKILL pid directly so
// daemon shutdown cannot hang on a stuck subprocess. Any goroutine still
// running inside go-plugin's Kill is left to drain on its own — closing the
// subprocess's pipes causes it to finish shortly after. Factored into a
// func(kill, pid) signature so the timeout path is testable without a real
// go-plugin subprocess.
func killWithTimeout(logger zerolog.Logger, name string, kill func(), pid int, timeout time.Duration) {
	done := safego.Go(logger, kill)
	select {
	case <-done:
		return
	case <-time.After(timeout):
		logger.Warn().
			Str("plugin", name).
			Dur("timeout", timeout).
			Int("pid", pid).
			Msg("plugin Kill timed out; force-killing via SIGKILL")
		if pid > 0 {
			if proc, err := os.FindProcess(pid); err == nil {
				_ = proc.Kill()
			}
		}
	}
}

// Plugins returns the current status of all running managed plugins.
// Disabled and failed entries are excluded — use AllPlugins for the full
// configured set.
func (h *Host) Plugins() []PluginStatus {
	h.mu.Lock()
	defer h.mu.Unlock()

	statuses := make([]PluginStatus, len(h.plugins))
	for i, p := range h.plugins {
		running := !p.client.Exited()
		statuses[i] = PluginStatus{
			Name:         p.cfg.Name,
			Path:         p.cfg.Path,
			Enabled:      p.cfg.Enabled,
			Loaded:       running,
			Running:      running,
			Capabilities: managedPluginCapabilities(p),
			ID:           p.client.ID(),
			StartedAt:    p.startedAt,
		}
	}
	return statuses
}

// AllPlugins returns the status of every plugin the host attempted to
// manage this run, including disabled config entries and start failures.
// Loaded plugins come first in registration order, followed by misses in
// the order they were encountered. Powers `boss plugin list` so an
// operator with a typo'd path can see their plugin in the output instead
// of grepping daemon logs.
func (h *Host) AllPlugins() []PluginStatus {
	h.mu.Lock()
	defer h.mu.Unlock()

	statuses := make([]PluginStatus, 0, len(h.plugins)+len(h.misses))
	for _, p := range h.plugins {
		running := !p.client.Exited()
		statuses = append(statuses, PluginStatus{
			Name:         p.cfg.Name,
			Path:         p.cfg.Path,
			Enabled:      p.cfg.Enabled,
			Loaded:       running,
			Running:      running,
			Capabilities: managedPluginCapabilities(p),
			ID:           p.client.ID(),
			StartedAt:    p.startedAt,
		})
	}
	for _, m := range h.misses {
		statuses = append(statuses, PluginStatus{
			Name:    m.cfg.Name,
			Path:    m.cfg.Path,
			Enabled: m.cfg.Enabled,
			Error:   m.loadErr,
		})
	}
	return statuses
}

// managedPluginCapabilities returns the dispensed-interface labels for a
// loaded plugin. Order is stable so consumers can render it without
// re-sorting.
func managedPluginCapabilities(p managedPlugin) []string {
	caps := make([]string, 0, 3)
	if p.agentRunner != nil {
		caps = append(caps, "agent")
	}
	if p.workflowService != nil {
		caps = append(caps, "workflow")
	}
	if p.taskSource != nil {
		caps = append(caps, "task_source")
	}
	return caps
}

// lostCapabilities returns the capability labels that prev advertised but
// replacement does not. Used by the restart path to reject a relaunch whose
// GetInfo probes transiently dropped a service the dead entry previously had:
// each probe in launchPlugin leaves the service field nil (with no error) when
// it fails or times out, so swapping such a degraded instance in would drop the
// plugin from AgentRunnerByName / GetWorkflowServices / GetTaskSources AND reset
// the restart backoff — leaving agent dispatch or auto-repair silently disabled
// until a full daemon restart. Order matches managedPluginCapabilities.
func lostCapabilities(prev, replacement managedPlugin) []string {
	var lost []string
	if prev.agentRunner != nil && replacement.agentRunner == nil {
		lost = append(lost, "agent")
	}
	if prev.workflowService != nil && replacement.workflowService == nil {
		lost = append(lost, "workflow")
	}
	if prev.taskSource != nil && replacement.taskSource == nil {
		lost = append(lost, "task_source")
	}
	return lost
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

// SetSessionDeps injects the dependencies needed for session-related RPCs.
func (h *Host) SetSessionDeps(repos db.RepoStore, sessions db.SessionStore, chats db.AgentChatStore, tracker *status.DisplayTracker, chatTracker *status.Tracker) {
	if h.hostService != nil {
		h.hostService.SetSessionDeps(repos, sessions, chats, tracker, chatTracker)
	}
}

// SetRepairLease injects the per-session single-repairer lease manager, shared
// with the API server so repair_active reads and lease enforcement agree.
func (h *Host) SetRepairLease(m *status.RepairLeaseManager) {
	if h.hostService != nil {
		h.hostService.SetRepairLease(m)
	}
}

// SetAgentClients injects the per-name registry of AgentRunnerClient gRPC
// clients (keyed by plugin Name, matching session.AgentName) so the
// host's StartAgentRun / WaitAgentRun RPCs can route to the right plugin
// based on the session record. Called from main.go after pluginHost.Start
// has dispensed every AgentRunner plugin.
func (h *Host) SetAgentClients(m map[string]agent.AgentRunnerClient) {
	if h.hostService != nil {
		h.hostService.SetAgentClients(m)
	}
}

// SetAgentLogsDir sets the bossd-owned directory where the agent plugin
// writes per-session NDJSON log files. Required alongside SetAgentClients.
func (h *Host) SetAgentLogsDir(dir string) {
	if h.hostService != nil {
		h.hostService.SetAgentLogsDir(dir)
	}
}

// SetProofEnvResolver injects the proof env overlay resolver used on the
// plugin-side agent spawn path. The HostServiceServer constructor defaults to
// a hermetic no-op resolver so tests never open the real OS keyring (its Linux
// dbus backend leaks a connection goroutine per open); the daemon calls this
// from main.go with the real keyring-backed resolver.
func (h *Host) SetProofEnvResolver(r proofEnvResolver) {
	if h.hostService != nil {
		h.hostService.SetProofEnvResolver(r)
	}
}

// SetAccountEnvResolver injects the per-account spawn env resolver used on the
// plugin-side agent spawn path (parity with the lifecycle spawn seams). No-op
// when no HostService exists (no plugins loaded).
func (h *Host) SetAccountEnvResolver(r accountEnvResolver) {
	if h.hostService != nil {
		h.hostService.SetAccountEnvResolver(r)
	}
}

// NotifyStatusChange broadcasts a PR status change to all workflow plugins.
// Each plugin call runs in its own panic-recovered goroutine with a bounded
// timeout so a slow plugin cannot hold a goroutine open indefinitely.
func (h *Host) NotifyStatusChange(ctx context.Context, sessionID string, displayStatus vcs.DisplayStatus, hasFailures bool) {
	services := h.GetWorkflowServices()
	pbStatus := bossanovav1.DisplayStatus(safeInt32(int(displayStatus)))
	for _, svc := range services {
		s := svc
		safego.Go(h.logger, func() {
			cctx, cancel := context.WithTimeout(ctx, notifyStatusChangeTimeout)
			defer cancel()
			if err := s.NotifyStatusChange(cctx, sessionID, pbStatus, hasFailures); err != nil {
				h.logger.Warn().Err(err).Str("session_id", sessionID).Msg("failed to notify plugin of status change")
			}
		})
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

// AgentClientNames returns the names of agent plugins currently wired into
// the host service. Used by RepairDoctor; nil-safe (returns nil if the host
// service hasn't been configured yet).
func (h *Host) AgentClientNames() []string {
	if h.hostService == nil {
		return nil
	}
	return h.hostService.AgentClientNames()
}

// AgentLogsDir returns the directory where per-session NDJSON log files
// land. Empty string if the host service hasn't been configured.
func (h *Host) AgentLogsDir() string {
	if h.hostService == nil {
		return ""
	}
	return h.hostService.AgentLogsDir()
}

// HostService returns the underlying *HostServiceServer (or nil if no
// plugins are configured). The daemon entrypoint passes this to the
// HookServer so /hooks/agent-run-complete/{id} can route into the
// per-run completion state.
func (h *Host) HostService() *HostServiceServer {
	return h.hostService
}

// AgentRunner returns the first plugin's AgentRunner interface, or nil
// if no plugin implements it. This is the legacy single-agent helper used
// as a default fallback. For multi-agent dispatch, prefer AgentRunnerByName
// (resolve a specific plugin by name) or AgentRunners (full registry).
func (h *Host) AgentRunner() AgentRunner {
	h.mu.Lock()
	defer h.mu.Unlock()
	for i := range h.plugins {
		if h.plugins[i].agentRunner != nil {
			return h.plugins[i].agentRunner
		}
	}
	return nil
}

// AgentRunnerByName returns the cached AgentRunner for the plugin whose
// configured Name matches name. Returns nil if no such plugin is loaded
// or if the matching plugin did not dispense an AgentRunner interface.
func (h *Host) AgentRunnerByName(name string) AgentRunner {
	h.mu.Lock()
	defer h.mu.Unlock()
	for i := range h.plugins {
		if h.plugins[i].cfg.Name == name && h.plugins[i].agentRunner != nil {
			return h.plugins[i].agentRunner
		}
	}
	return nil
}

// AgentRunners returns a fresh map of plugin name -> AgentRunner for every
// loaded plugin that dispensed an AgentRunner. The returned map is a copy:
// mutating it does not affect the host. Returns an empty (non-nil) map when
// no AgentRunner plugins are loaded.
//
// Each value is a *agentRunnerProxy, NOT the raw dispensed client. This is
// load-bearing: main.go calls AgentRunners() once at startup and distributes
// the values into long-lived registries (HostServiceServer.agentClients,
// agent.Dispatcher.runners, the account materializer/smoke runner). The raw
// dispensed client is bound to one plugin subprocess's unix socket, so after
// the health loop respawns a plugin (fresh subprocess, fresh socket) any raw
// client captured at startup is permanently dead ("connection refused"). The
// proxy resolves AgentRunnerByName live on every call, so those captured
// references transparently pick up the freshly-dispensed client after a swap.
func (h *Host) AgentRunners() map[string]AgentRunner {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make(map[string]AgentRunner, len(h.plugins))
	for i := range h.plugins {
		if h.plugins[i].agentRunner != nil {
			name := h.plugins[i].cfg.Name
			out[name] = &agentRunnerProxy{host: h, name: name}
		}
	}
	return out
}

// healthCheckLoop periodically pings each plugin, logs failures, and relaunches
// any plugin whose subprocess has exited.
func (h *Host) healthCheckLoop(ctx context.Context) {
	ticker := time.NewTicker(healthCheckInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			h.pingAll(ctx)
			h.ensureDesiredWorkflowsRunning(ctx)
		}
	}
}

// ensureDesiredWorkflowsRunning probes every desired workflow on a live
// plugin and re-arms any that report CANCELLED. This heals: zombie plugins
// (alive process, stopped workflow — the BOS-346 incident), restart replays
// that failed, and boot-time probe hiccups. It never touches PAUSED
// (operator intent) or RUNNING (StartWorkflow re-entry cancels in-flight
// repairs). Failures log at Error and retry naturally on the next tick —
// deliberate loudness, no extra backoff. Runs after pingAll each tick, on the
// single healthCheckLoop goroutine, so it never races itself.
func (h *Host) ensureDesiredWorkflowsRunning(ctx context.Context) {
	h.mu.Lock()
	names := make([]string, 0, len(h.desiredWorkflows))
	for name := range h.desiredWorkflows {
		names = append(names, name)
	}
	h.mu.Unlock()
	for _, name := range names {
		status, started, err := h.EnsureWorkflowRunning(ctx, name)
		switch {
		case err != nil:
			h.logger.Error().Err(err).Str("plugin", name).Msg("workflow watchdog: ensure failed; retrying next health tick")
		case started:
			// status is the observed pre-start status (CANCELLED in the incident,
			// but UNSPECIFIED/FAILED/etc. are also re-armable) — log it verbatim
			// rather than asserting CANCELLED.
			h.logger.Error().Str("plugin", name).Str("was", status.String()).Msg("workflow watchdog: workflow was not running while desired-started; re-armed")
		}
	}
}

// pingTimeout bounds each individual plugin ping. go-plugin's Ping() has no
// context, so we race it against a timer in a goroutine — a hung ping can't
// block the health-check loop or shutdown (see Stop below, which waits on h.wg).
const pingTimeout = 5 * time.Second

// pingAll checks each plugin's health and relaunches any that have exited.
//
// Snapshots the plugin list under h.mu, then releases the lock before issuing
// any RPC or relaunch. Holding h.mu across a blocking Ping (or the relaunch
// handshake) would deadlock shutdown because Stop also takes h.mu, so a hung
// plugin would wedge the whole daemon.
//
// There is exactly one healthCheckLoop goroutine, so pingAll never races
// itself; the only competing actor is Stop, guarded by the ctx checks and the
// swap-by-identity inside restartPlugin.
func (h *Host) pingAll(ctx context.Context) {
	h.mu.Lock()
	snapshot := make([]managedPlugin, len(h.plugins))
	copy(snapshot, h.plugins)
	h.mu.Unlock()

	for _, p := range snapshot {
		if ctx.Err() != nil {
			return
		}
		if p.client.Exited() {
			// The exit reason (e.g. "signal: terminated") is already logged by
			// go-plugin itself through the hclog adapter; we add the pid so an
			// external signal can be traced, then relaunch.
			h.logger.Warn().
				Str("plugin", p.cfg.Name).
				Int("pid", pluginPID(p.client)).
				Msg("plugin process exited; attempting restart")
			h.restartPlugin(ctx, p)
			continue
		}

		rpcClient, err := p.client.Client()
		if err != nil {
			h.logger.Warn().Err(err).Str("plugin", p.cfg.Name).Msg("health check: failed to get client")
			continue
		}

		// Race Ping against a timer. A hung Ping leaks one goroutine per
		// cycle, but the health-check loop keeps running and Stop doesn't block.
		done := make(chan error, 1)
		safego.Go(h.logger, func() {
			done <- rpcClient.Ping()
		})
		select {
		case err := <-done:
			if err != nil {
				h.logger.Warn().Err(err).Str("plugin", p.cfg.Name).Msg("health check: ping failed")
			}
		case <-time.After(pingTimeout):
			h.logger.Warn().Str("plugin", p.cfg.Name).Dur("timeout", pingTimeout).Msg("health check: ping timed out")
		}
	}
}

// restartPlugin relaunches a plugin whose subprocess has exited, subject to
// per-plugin exponential backoff. `dead` is the snapshot entry pingAll observed
// as Exited().
//
// The blocking relaunch runs WITHOUT holding h.mu (matching pingAll's "never
// hold the lock across an RPC" invariant); only the backoff bookkeeping and the
// final swap take the lock. Called solely from the single healthCheckLoop
// goroutine, so it never races itself. Races with Stop are handled two ways:
// the ctx re-check after launch discards a fresh child if the daemon is tearing
// down, and the swap-by-identity only replaces the entry if the dead client is
// still the live one for that name (Stop nils the slice; a prior tick may have
// already relaunched it).
func (h *Host) restartPlugin(ctx context.Context, dead managedPlugin) {
	name := dead.cfg.Name

	// Backoff gate.
	h.mu.Lock()
	if h.restartState == nil {
		// Stop niled it; the daemon is shutting down.
		h.mu.Unlock()
		return
	}
	info := h.restartState[name]
	if info == nil {
		info = &restartInfo{}
		h.restartState[name] = info
	}
	if time.Now().Before(info.nextRetryAt) {
		next := info.nextRetryAt
		h.mu.Unlock()
		h.logger.Debug().Str("plugin", name).Time("next_retry", next).Msg("plugin restart throttled by backoff")
		return
	}
	// Arm the next backoff window BEFORE launching so a plugin that keeps
	// failing to relaunch (e.g. its binary was replaced with a broken build)
	// can't hot-loop respawns across ticks.
	attempts := info.attempts
	info.attempts++
	info.nextRetryAt = time.Now().Add(nextRestartBackoff(attempts))
	h.mu.Unlock()

	// Reap the dead client so go-plugin releases its broker/log-pump goroutines
	// before we relaunch (also SIGKILLs the pid if Kill hangs). Guarded for a
	// nil client so the launchPluginFn test seam can exercise this path.
	if dead.client != nil {
		killWithTimeout(h.logger, name, dead.client.Kill, pluginPID(dead.client), killTimeout)
	}

	// Relaunch OUTSIDE the lock (blocking handshake). Run it in a goroutine and
	// race it against ctx so a hung handshake or probe can't wedge daemon
	// shutdown: Stop cancels ctx and then waits on h.wg, and this call is the
	// only thing keeping the health-loop goroutine alive during a relaunch. If
	// ctx fires first we stop waiting and let a detached reaper kill whatever
	// child the launch eventually produces, so h.wg.Wait() stays bounded.
	launch := h.launchPlugin
	if h.launchPluginFn != nil {
		launch = h.launchPluginFn
	}
	type launchResult struct {
		mp  managedPlugin
		err error
	}
	resultCh := make(chan launchResult, 1)
	safego.Go(h.logger, func() {
		mp, err := launch(ctx, dead.cfg)
		resultCh <- launchResult{mp: mp, err: err}
	})

	var res launchResult
	select {
	case <-ctx.Done():
		// Daemon is tearing down. Don't block shutdown on the launch; reap the
		// replacement in the background once it finishes so we don't leak a
		// subprocess. This goroutine is intentionally not tracked by h.wg —
		// Stop must not wait on it.
		safego.Go(h.logger, func() {
			r := <-resultCh
			if r.err == nil {
				killWithTimeout(h.logger, name, r.mp.client.Kill, pluginPID(r.mp.client), killTimeout)
			}
		})
		return
	case res = <-resultCh:
	}

	if res.err != nil {
		event := h.logger.Warn()
		if attempts+1 >= restartLoudAfter {
			event = h.logger.Error()
		}
		event.Err(res.err).
			Str("plugin", name).
			Int("attempts", attempts+1).
			Msg("plugin restart failed; will retry after backoff")
		return
	}
	newMP := res.mp

	// Stop may have won the race while we launched: if ctx is cancelled the
	// daemon is tearing down, so kill the fresh child and discard it.
	if ctx.Err() != nil {
		killWithTimeout(h.logger, name, newMP.client.Kill, pluginPID(newMP.client), killTimeout)
		return
	}

	// Reject a relaunch that lost a capability the dead entry advertised. A
	// GetInfo probe in launchPlugin that transiently fails or times out leaves
	// the service field nil with no error, so swapping the degraded instance in
	// would drop the plugin from AgentRunnerByName / GetWorkflowServices AND
	// reset the backoff below — leaving agent dispatch or auto-repair silently
	// disabled until a full daemon restart. Treat it as a failed restart
	// instead: reap the degraded child, leave the already-armed backoff in place,
	// and let the next health tick retry the probes. Only relaunches compare
	// capabilities (there is always a prior `dead` entry here); the initial
	// Start path has no predecessor to lose capabilities against.
	if lost := lostCapabilities(dead, newMP); len(lost) > 0 {
		if newMP.client != nil {
			killWithTimeout(h.logger, name, newMP.client.Kill, pluginPID(newMP.client), killTimeout)
		}
		event := h.logger.Warn()
		if attempts+1 >= restartLoudAfter {
			event = h.logger.Error()
		}
		event.
			Str("plugin", name).
			Int("attempts", attempts+1).
			Strs("lost_capabilities", lost).
			Msg("plugin restart lost a capability the previous instance had; treating as failed restart and will retry after backoff")
		return
	}

	// Swap by identity under the lock. Only replace if the dead client is still
	// the live entry for this name.
	h.mu.Lock()
	swapped := false
	for i := range h.plugins {
		if h.plugins[i].cfg.Name == name && h.plugins[i].client == dead.client {
			h.plugins[i] = newMP
			swapped = true
			break
		}
	}
	if swapped {
		if h.restartState != nil {
			if info := h.restartState[name]; info != nil {
				info.attempts = 0
				info.nextRetryAt = time.Time{}
			}
		}
	}
	h.mu.Unlock()

	if !swapped {
		// Stop cleared the slice, or another tick already replaced the entry:
		// discard the fresh child so we don't leak a subprocess.
		killWithTimeout(h.logger, name, newMP.client.Kill, pluginPID(newMP.client), killTimeout)
		h.logger.Debug().Str("plugin", name).Msg("discarded plugin restart; dead client no longer the live entry")
		return
	}

	// A freshly spawned workflow plugin boots with its workflow stopped (the
	// repair plugin rejects every NotifyStatusChange until StartWorkflow
	// supplies its config), so re-arm it from desired state via the shared
	// single-flight primitive — otherwise the plugin comes back alive but
	// auto-repair stays silently disabled until a full daemon restart. Only
	// plugins with a registered desire are re-armed (non-workflow plugins have
	// none, so this is a no-op for them). Run outside h.mu (StartWorkflow is an
	// RPC). Best-effort: the process is up regardless, and the health-tick
	// watchdog retries on the next tick if this fails.
	if h.WorkflowDesire(name) != nil {
		if _, started, err := h.EnsureWorkflowRunning(ctx, name); err != nil {
			h.logger.Error().Err(err).Str("plugin", name).Msg("failed to re-arm workflow after plugin restart; the workflow watchdog retries on the next health tick")
		} else if started {
			h.logger.Info().Str("plugin", name).Msg("re-armed workflow after plugin restart")
		}
	}

	h.logger.Info().
		Str("plugin", name).
		Str("id", pluginClientID(newMP.client)).
		Int("pid", pluginPID(newMP.client)).
		Msg("plugin restarted")
}
