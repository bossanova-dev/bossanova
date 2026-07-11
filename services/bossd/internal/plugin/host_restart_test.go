package plugin

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	grpcstatus "google.golang.org/grpc/status"

	"github.com/recurser/bossalib/config"
	bossanovav1 "github.com/recurser/bossalib/gen/bossanova/v1"
)

func TestNextRestartBackoff(t *testing.T) {
	cases := []struct {
		attempts int
		want     time.Duration
	}{
		{attempts: 0, want: 1 * time.Second},
		{attempts: 1, want: 2 * time.Second},
		{attempts: 2, want: 4 * time.Second},
		{attempts: 3, want: 8 * time.Second},
		{attempts: 8, want: 256 * time.Second},
		{attempts: 9, want: restartBackoffMax}, // 512s > 300s cap
		{attempts: 100, want: restartBackoffMax},
		{attempts: -5, want: 1 * time.Second}, // clamped to 0
	}
	for _, tc := range cases {
		if got := nextRestartBackoff(tc.attempts); got != tc.want {
			t.Errorf("nextRestartBackoff(%d) = %s, want %s", tc.attempts, got, tc.want)
		}
	}
}

// TestRestartPluginThrottledByBackoff verifies the backoff gate: when a
// plugin's nextRetryAt is still in the future, restartPlugin returns without
// attempting a relaunch (and without touching the dead client, so a nil client
// is safe) and leaves the attempt counter untouched.
func TestRestartPluginThrottledByBackoff(t *testing.T) {
	h := newHostForTest(nil)
	name := "throttled"
	h.restartState[name] = &restartInfo{attempts: 3, nextRetryAt: time.Now().Add(time.Hour)}

	// dead.client is nil on purpose: the gate must return before any client use.
	h.restartPlugin(context.Background(), managedPlugin{cfg: config.PluginConfig{Name: name}})

	if got := h.restartState[name].attempts; got != 3 {
		t.Errorf("throttled restart mutated attempts: got %d, want 3 (gate should return early)", got)
	}
}

// TestRestartPluginAbandonsLaunchOnCancel proves the shutdown-cancellation
// contract: when ctx is cancelled while a relaunch handshake is still blocked,
// restartPlugin stops waiting and returns promptly instead of hanging until the
// launch completes. That is what keeps Stop's h.wg.Wait() bounded when a broken
// replacement plugin wedges its handshake or GetInfo probe during shutdown.
func TestRestartPluginAbandonsLaunchOnCancel(t *testing.T) {
	h := newHostForTest(nil)
	name := "hung"

	release := make(chan struct{})
	launched := make(chan struct{})
	h.launchPluginFn = func(ctx context.Context, cfg config.PluginConfig) (managedPlugin, error) {
		close(launched)
		<-release // simulate a handshake/probe that hangs until we let it go
		return managedPlugin{}, errors.New("aborted after cancel")
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan struct{})
	go func() {
		// dead.client is nil: the reap nil-guard must be skipped so we reach the launch.
		h.restartPlugin(ctx, managedPlugin{cfg: config.PluginConfig{Name: name}})
		close(done)
	}()

	<-launched // the launch is now blocked in the seam
	cancel()   // Stop() cancels ctx during the launch window

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("restartPlugin did not return after ctx cancel while the launch was hung")
	}

	// Let the detached launcher goroutine finish so the background reaper drains.
	// Its managedPlugin has a nil client and a non-nil error, so it performs no kill.
	close(release)
}

// stubWorkflowService records StartWorkflow calls so a test can assert the host
// replayed the startup request after a respawn. It also models a reported
// workflow status (for EnsureWorkflowRunning / watchdog tests): GetWorkflowStatus
// returns `status`/`statusErr`, and StartWorkflow flips `status` to RUNNING so a
// re-probe by a losing single-flight caller observes the started workflow. All
// fields are mutex-guarded so the concurrency tests are race-clean.
type stubWorkflowService struct {
	mu         sync.Mutex
	startCalls int
	lastReq    *bossanovav1.StartWorkflowRequest

	status      bossanovav1.WorkflowStatus
	statusErr   error
	statusCalls int
	// statusGate, when non-nil, blocks GetWorkflowStatus until it is closed.
	// The single-flight test uses it to pin concurrent callers at the probe.
	statusGate chan struct{}
}

func (s *stubWorkflowService) GetInfo(context.Context) (*bossanovav1.PluginInfo, error) {
	return &bossanovav1.PluginInfo{Name: "repair"}, nil
}

func (s *stubWorkflowService) StartWorkflow(_ context.Context, req *bossanovav1.StartWorkflowRequest) (*bossanovav1.StartWorkflowResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.startCalls++
	s.lastReq = req
	// Starting a workflow makes it RUNNING: a subsequent probe (e.g. a losing
	// single-flight caller re-checking under the lock) must see RUNNING.
	s.status = bossanovav1.WorkflowStatus_WORKFLOW_STATUS_RUNNING
	return &bossanovav1.StartWorkflowResponse{}, nil
}

func (s *stubWorkflowService) PauseWorkflow(context.Context, string) (*bossanovav1.WorkflowStatusInfo, error) {
	return &bossanovav1.WorkflowStatusInfo{}, nil
}

func (s *stubWorkflowService) ResumeWorkflow(context.Context, string) (*bossanovav1.WorkflowStatusInfo, error) {
	return &bossanovav1.WorkflowStatusInfo{}, nil
}

func (s *stubWorkflowService) CancelWorkflow(context.Context, string) (*bossanovav1.WorkflowStatusInfo, error) {
	return &bossanovav1.WorkflowStatusInfo{}, nil
}

func (s *stubWorkflowService) GetWorkflowStatus(ctx context.Context, _ string) (*bossanovav1.WorkflowStatusInfo, error) {
	s.mu.Lock()
	gate := s.statusGate
	s.mu.Unlock()
	if gate != nil {
		select {
		case <-gate:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.statusCalls++
	if s.statusErr != nil {
		return nil, s.statusErr
	}
	return &bossanovav1.WorkflowStatusInfo{Status: s.status}, nil
}

// setStatus updates the reported status under the lock (test helper).
func (s *stubWorkflowService) setStatus(st bossanovav1.WorkflowStatus) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.status = st
}

// starts returns the recorded StartWorkflow call count under the lock.
func (s *stubWorkflowService) starts() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.startCalls
}

func (s *stubWorkflowService) NotifyStatusChange(context.Context, string, bossanovav1.DisplayStatus, bool) error {
	return nil
}

// TestRestartPluginReplaysWorkflowStart proves the respawn re-arm contract: when
// the health loop relaunches a workflow plugin, the host replays the
// StartWorkflow request the daemon registered at startup. Without it a reborn
// repair plugin boots with its workflow stopped and silently drops every
// NotifyStatusChange until a full daemon restart.
func TestRestartPluginReplaysWorkflowStart(t *testing.T) {
	name := "repair"
	// The dead entry currently in the host: nil client so restartPlugin's
	// swap-by-identity matches it (and the nil-client reap is skipped).
	h := newHostForTest([]managedPlugin{{cfg: config.PluginConfig{Name: name}}})

	req := &bossanovav1.StartWorkflowRequest{ConfigJson: `{"cooldown_minutes":5}`}
	h.SetWorkflowDesired(name, req)

	ws := &stubWorkflowService{}
	h.launchPluginFn = func(_ context.Context, cfg config.PluginConfig) (managedPlugin, error) {
		return managedPlugin{cfg: cfg, workflowService: ws}, nil
	}

	h.restartPlugin(context.Background(), h.plugins[0])

	if ws.startCalls != 1 {
		t.Fatalf("StartWorkflow replay calls = %d, want 1", ws.startCalls)
	}
	if got := ws.lastReq.GetConfigJson(); got != req.GetConfigJson() {
		t.Errorf("replayed config = %q, want %q", got, req.GetConfigJson())
	}
}

// TestRestartPluginNoWorkflowReplayWhenUnregistered verifies a plugin with no
// registered StartWorkflow request (e.g. a non-workflow plugin) is not replayed.
func TestRestartPluginNoWorkflowReplayWhenUnregistered(t *testing.T) {
	name := "codex"
	h := newHostForTest([]managedPlugin{{cfg: config.PluginConfig{Name: name}}})

	ws := &stubWorkflowService{}
	h.launchPluginFn = func(_ context.Context, cfg config.PluginConfig) (managedPlugin, error) {
		return managedPlugin{cfg: cfg, workflowService: ws}, nil
	}

	h.restartPlugin(context.Background(), h.plugins[0])

	if ws.startCalls != 0 {
		t.Errorf("StartWorkflow replayed for an unregistered plugin: calls = %d, want 0", ws.startCalls)
	}
}

// TestRestartPluginRejectsCapabilityLoss proves that a relaunch whose GetInfo
// probe transiently dropped a capability the dead entry advertised is treated as
// a failed restart, not a successful swap: the degraded instance is NOT swapped
// in, and the backoff is left armed (attempts not reset, nextRetryAt still set)
// so the next health tick retries the probes instead of leaving agent dispatch
// silently disabled until a full daemon restart.
func TestRestartPluginRejectsCapabilityLoss(t *testing.T) {
	name := "claude"
	deadRunner := &stubAgentRunner{name: "dead"}
	// nil client so the swap-by-identity would otherwise match (nil == nil) and
	// the nil-client reap is skipped — isolating the capability-loss gate.
	h := newHostForTest([]managedPlugin{
		{cfg: config.PluginConfig{Name: name}, agentRunner: deadRunner},
	})

	// Relaunch handshakes but the AgentRunner GetInfo probe transiently failed,
	// so launchPlugin returned a managedPlugin with a nil agentRunner and no error.
	h.launchPluginFn = func(_ context.Context, cfg config.PluginConfig) (managedPlugin, error) {
		return managedPlugin{cfg: cfg}, nil
	}

	h.restartPlugin(context.Background(), h.plugins[0])

	// The degraded instance must not have replaced the dead entry: the plugin
	// still resolves to a runner via AgentRunnerByName.
	if got := h.AgentRunnerByName(name); got != AgentRunner(deadRunner) {
		t.Fatalf("capability-losing relaunch was swapped in: AgentRunnerByName(%q) = %v, want the previous runner", name, got)
	}

	// The backoff must stay armed so the next tick retries, rather than being
	// reset as a successful swap would do.
	info := h.restartState[name]
	if info == nil {
		t.Fatalf("restartState[%q] missing", name)
	}
	if info.attempts != 1 {
		t.Errorf("attempts = %d, want 1 (backoff must stay armed after a capability-losing restart)", info.attempts)
	}
	if info.nextRetryAt.IsZero() {
		t.Error("nextRetryAt was reset; a failed restart must keep the backoff window armed")
	}
}

// TestAgentRunnerProxyFollowsSwap is the deterministic proof of the incident
// fix: a proxy captured once at startup (as main.go does) keeps resolving to
// the CURRENT dispensed runner after the plugin is swapped (respawned), and
// reports Unavailable while the plugin is momentarily absent — never a stale
// dead client.
func TestAgentRunnerProxyFollowsSwap(t *testing.T) {
	ctx := context.Background()
	// stubAgentRunner.GetInfo echoes its name field, so we use it as a version
	// marker distinct from the plugin's cfg.Name ("claude").
	v1 := &stubAgentRunner{name: "v1"}
	h := newHostForTest([]managedPlugin{
		{cfg: config.PluginConfig{Name: "claude"}, agentRunner: v1},
	})

	// Capture the proxy exactly as main.go does at startup.
	proxy := h.AgentRunners()["claude"]
	if proxy == nil {
		t.Fatal("AgentRunners did not return a proxy for claude")
	}

	info, err := proxy.GetInfo(ctx)
	if err != nil {
		t.Fatalf("proxy.GetInfo (v1): %v", err)
	}
	if info.GetName() != "v1" {
		t.Fatalf("proxy resolved to %q, want v1", info.GetName())
	}

	// Simulate a respawn: the health loop replaces the managedPlugin's
	// dispensed runner with a freshly-dispensed one (new subprocess/socket).
	v2 := &stubAgentRunner{name: "v2"}
	h.mu.Lock()
	h.plugins[0].agentRunner = v2
	h.mu.Unlock()

	info, err = proxy.GetInfo(ctx)
	if err != nil {
		t.Fatalf("proxy.GetInfo (v2): %v", err)
	}
	if info.GetName() != "v2" {
		t.Errorf("captured proxy did not follow the swap: resolved to %q, want v2", info.GetName())
	}

	// While the plugin is dead and not yet relaunched, the proxy reports
	// Unavailable rather than dialing a dead socket.
	h.mu.Lock()
	h.plugins = nil
	h.mu.Unlock()

	if _, err := proxy.GetInfo(ctx); err == nil {
		t.Error("proxy.GetInfo should error while plugin is absent")
	} else if grpcstatus.Code(err) != codes.Unavailable {
		t.Errorf("proxy.GetInfo absent-plugin error = %v, want codes.Unavailable", err)
	}
}
