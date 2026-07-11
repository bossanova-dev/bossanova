package plugin

import (
	"context"
	"errors"
	"strconv"
	"sync"
	"testing"

	"github.com/recurser/bossalib/config"
	bossanovav1 "github.com/recurser/bossalib/gen/bossanova/v1"
)

// hostWithRepairStub builds a host with a single managed "repair" plugin whose
// workflowService is the given stub. The client is nil, which
// EnsureWorkflowRunning treats as live (c == nil branch).
func hostWithRepairStub(ws *stubWorkflowService) *Host {
	return newHostForTest([]managedPlugin{
		{cfg: config.PluginConfig{Name: "repair"}, workflowService: ws},
	})
}

// TestEnsureWorkflowRunning covers the status→action matrix: CANCELLED and
// UNSPECIFIED re-arm, RUNNING/PAUSED no-op, an undesired workflow or a probe
// error never starts and returns an error.
func TestEnsureWorkflowRunning(t *testing.T) {
	req := &bossanovav1.StartWorkflowRequest{ConfigJson: `{"cooldown_minutes":5}`}
	cases := []struct {
		name        string
		desired     *bossanovav1.StartWorkflowRequest
		status      bossanovav1.WorkflowStatus
		statusErr   error
		wantStart   int
		wantStarted bool
		wantErr     bool
	}{
		{name: "cancelled_rearms", desired: req, status: bossanovav1.WorkflowStatus_WORKFLOW_STATUS_CANCELLED, wantStart: 1, wantStarted: true},
		{name: "unspecified_rearms", desired: req, status: bossanovav1.WorkflowStatus_WORKFLOW_STATUS_UNSPECIFIED, wantStart: 1, wantStarted: true},
		{name: "running_noop", desired: req, status: bossanovav1.WorkflowStatus_WORKFLOW_STATUS_RUNNING, wantStart: 0},
		{name: "paused_untouched", desired: req, status: bossanovav1.WorkflowStatus_WORKFLOW_STATUS_PAUSED, wantStart: 0},
		{name: "not_desired_error", desired: nil, status: bossanovav1.WorkflowStatus_WORKFLOW_STATUS_CANCELLED, wantStart: 0, wantErr: true},
		{name: "status_error_no_start", desired: req, statusErr: errors.New("probe failed"), wantStart: 0, wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ws := &stubWorkflowService{status: tc.status, statusErr: tc.statusErr}
			h := hostWithRepairStub(ws)
			if tc.desired != nil {
				h.SetWorkflowDesired("repair", tc.desired)
			}

			_, started, err := h.EnsureWorkflowRunning(context.Background(), "repair")
			if tc.wantErr && err == nil {
				t.Fatalf("expected error, got nil")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got := ws.starts(); got != tc.wantStart {
				t.Errorf("StartWorkflow calls = %d, want %d", got, tc.wantStart)
			}
			if started != tc.wantStarted {
				t.Errorf("started = %v, want %v", started, tc.wantStarted)
			}
			if tc.wantStart == 1 && ws.lastReq != tc.desired {
				t.Errorf("StartWorkflow got req %p, want the registered desire %p", ws.lastReq, tc.desired)
			}
		})
	}
}

// TestEnsureWorkflowRunningSingleFlight fires many concurrent re-arm attempts at
// a CANCELLED workflow pinned at the probe, then releases them: exactly one
// StartWorkflow is issued (the single-flight winner) and the rest observe the
// now-RUNNING workflow.
func TestEnsureWorkflowRunningSingleFlight(t *testing.T) {
	gate := make(chan struct{})
	ws := &stubWorkflowService{
		status:     bossanovav1.WorkflowStatus_WORKFLOW_STATUS_CANCELLED,
		statusGate: gate,
	}
	h := hostWithRepairStub(ws)
	req := &bossanovav1.StartWorkflowRequest{ConfigJson: `{"cooldown_minutes":5}`}
	h.SetWorkflowDesired("repair", req)

	const n = 8
	var wg sync.WaitGroup
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func() {
			defer wg.Done()
			_, _, _ = h.EnsureWorkflowRunning(context.Background(), "repair")
		}()
	}
	close(gate) // release all probes
	wg.Wait()

	if got := ws.starts(); got != 1 {
		t.Fatalf("StartWorkflow calls = %d, want exactly 1 (single-flight)", got)
	}
}

// TestEnsureWorkflowRunningRearmsCancelledExactlyOnceWithRegisteredConfig proves
// the re-arm uses the registered config and does not restart an
// already-RUNNING workflow on a second call.
func TestEnsureWorkflowRunningRearmsCancelledExactlyOnceWithRegisteredConfig(t *testing.T) {
	ws := &stubWorkflowService{status: bossanovav1.WorkflowStatus_WORKFLOW_STATUS_CANCELLED}
	h := hostWithRepairStub(ws)
	req := &bossanovav1.StartWorkflowRequest{ConfigJson: `{"cooldown_minutes":7}`}
	h.SetWorkflowDesired("repair", req)

	if _, started, err := h.EnsureWorkflowRunning(context.Background(), "repair"); err != nil || !started {
		t.Fatalf("first ensure: started=%v err=%v, want started=true err=nil", started, err)
	}
	if got := ws.lastReq.GetConfigJson(); got != req.GetConfigJson() {
		t.Errorf("re-armed config = %q, want %q", got, req.GetConfigJson())
	}

	// StartWorkflow flipped the stub to RUNNING; a second ensure must no-op.
	ws.setStatus(bossanovav1.WorkflowStatus_WORKFLOW_STATUS_RUNNING)
	if _, started, err := h.EnsureWorkflowRunning(context.Background(), "repair"); err != nil || started {
		t.Fatalf("second ensure: started=%v err=%v, want started=false err=nil", started, err)
	}
	if got := ws.starts(); got != 1 {
		t.Errorf("StartWorkflow calls = %d, want 1 (running workflow never restarted)", got)
	}
}

// TestEnsureWorkflowRunningConcurrentRedeclareRaceClean hammers SetWorkflowDesired
// (which writes desire.req under h.mu) concurrently with EnsureWorkflowRunning
// (which reads the desired request before StartWorkflow). It exists to catch the
// two-mutex data race that arises if the request field is read under desire.mu
// while written under h.mu — `go test -race` flags that; a clean run proves the
// field is guarded by a single lock.
func TestEnsureWorkflowRunningConcurrentRedeclareRaceClean(t *testing.T) {
	ws := &stubWorkflowService{status: bossanovav1.WorkflowStatus_WORKFLOW_STATUS_CANCELLED}
	h := hostWithRepairStub(ws)
	h.SetWorkflowDesired("repair", &bossanovav1.StartWorkflowRequest{ConfigJson: `{"cooldown_minutes":1}`})

	var wg sync.WaitGroup
	const n = 16
	wg.Add(2 * n)
	for i := 0; i < n; i++ {
		go func(i int) {
			defer wg.Done()
			// Re-declare desired with a fresh request (update path: writes desire.req).
			h.SetWorkflowDesired("repair", &bossanovav1.StartWorkflowRequest{
				ConfigJson: `{"cooldown_minutes":` + strconv.Itoa(i+2) + `}`,
			})
		}(i)
		go func() {
			defer wg.Done()
			// Re-arm (reads the desired request). Keep the workflow re-armable by
			// resetting the stub to CANCELLED so the read path (StartWorkflow) runs.
			ws.setStatus(bossanovav1.WorkflowStatus_WORKFLOW_STATUS_CANCELLED)
			_, _, _ = h.EnsureWorkflowRunning(context.Background(), "repair")
		}()
	}
	wg.Wait()
}

// TestEnsureWorkflowRunningNoLivePlugin errors when the named workflow is
// desired but no live plugin serves it.
func TestEnsureWorkflowRunningNoLivePlugin(t *testing.T) {
	h := newHostForTest(nil)
	h.SetWorkflowDesired("repair", &bossanovav1.StartWorkflowRequest{})
	if _, started, err := h.EnsureWorkflowRunning(context.Background(), "repair"); err == nil || started {
		t.Fatalf("want error and started=false for absent plugin, got started=%v err=%v", started, err)
	}
}

// TestHealthTickReArmsCancelledDesiredWorkflow drives the watchdog directly:
// a CANCELLED desired workflow is re-armed exactly once, and a subsequent tick
// against the now-RUNNING workflow issues no further start.
func TestHealthTickReArmsCancelledDesiredWorkflow(t *testing.T) {
	ws := &stubWorkflowService{status: bossanovav1.WorkflowStatus_WORKFLOW_STATUS_CANCELLED}
	h := hostWithRepairStub(ws)
	req := &bossanovav1.StartWorkflowRequest{ConfigJson: `{"cooldown_minutes":5}`}
	h.SetWorkflowDesired("repair", req)

	h.ensureDesiredWorkflowsRunning(context.Background())
	if got := ws.starts(); got != 1 {
		t.Fatalf("after first tick StartWorkflow calls = %d, want 1", got)
	}
	if got := ws.lastReq.GetConfigJson(); got != req.GetConfigJson() {
		t.Errorf("re-armed config = %q, want %q", got, req.GetConfigJson())
	}

	ws.setStatus(bossanovav1.WorkflowStatus_WORKFLOW_STATUS_RUNNING)
	h.ensureDesiredWorkflowsRunning(context.Background())
	if got := ws.starts(); got != 1 {
		t.Errorf("after second tick StartWorkflow calls = %d, want still 1", got)
	}
}

// TestHealthTickSkipsUndesiredAndPaused proves the watchdog never starts a
// workflow that is either not desired (regardless of its CANCELLED status) or
// PAUSED (operator intent).
func TestHealthTickSkipsUndesiredAndPaused(t *testing.T) {
	undesired := &stubWorkflowService{status: bossanovav1.WorkflowStatus_WORKFLOW_STATUS_CANCELLED}
	paused := &stubWorkflowService{status: bossanovav1.WorkflowStatus_WORKFLOW_STATUS_PAUSED}
	h := newHostForTest([]managedPlugin{
		{cfg: config.PluginConfig{Name: "other"}, workflowService: undesired},
		{cfg: config.PluginConfig{Name: "repair"}, workflowService: paused},
	})
	// Only the paused plugin is desired.
	h.SetWorkflowDesired("repair", &bossanovav1.StartWorkflowRequest{ConfigJson: `{}`})

	h.ensureDesiredWorkflowsRunning(context.Background())

	if got := undesired.starts(); got != 0 {
		t.Errorf("undesired workflow started %d times, want 0", got)
	}
	if got := paused.starts(); got != 0 {
		t.Errorf("paused workflow started %d times, want 0 (operator intent preserved)", got)
	}
}
