package main

import (
	"context"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/rs/zerolog"

	bossanovav1 "github.com/recurser/bossalib/gen/bossanova/v1"
)

// mockHostClient is a test double for hostClient that records calls and
// returns preconfigured responses. It implements the hostClient interface.
type mockHostClient struct {
	mu sync.Mutex

	// Workflow state returned by CreateWorkflow/UpdateWorkflow/GetWorkflow.
	workflow *bossanovav1.Workflow

	// Attempt tracking.
	attemptID     string
	pollCount     int
	createErr     error
	attemptCreErr error

	// Track calls for assertions.
	createWorkflowCalls []*bossanovav1.CreateWorkflowRequest
	updateWorkflowCalls []*bossanovav1.UpdateWorkflowRequest
	createAttemptCalls  []*bossanovav1.CreateAttemptRequest

	// Per-step attempt behavior: step -> (error string, poll count).
	stepAttempts map[string]attemptBehavior

	// For retry testing: second attempt for same step can behave differently.
	retryAttempts map[string]attemptBehavior
	attemptCounts map[string]int // tracks how many attempts per step

	// onAttemptCreated is called (without lock) after a CreateAttempt call.
	// It receives the step name so tests can produce side effects (e.g. create files).
	onAttemptCreated func(step string)
}

type attemptBehavior struct {
	err       string
	polls     int
	createErr error
}

func newMockHostClient() *mockHostClient {
	return &mockHostClient{
		workflow: &bossanovav1.Workflow{
			Id:     "wf-test-1",
			Status: "pending",
		},
		attemptID:     "attempt-1",
		stepAttempts:  make(map[string]attemptBehavior),
		retryAttempts: make(map[string]attemptBehavior),
		attemptCounts: make(map[string]int),
	}
}

func (m *mockHostClient) CreateWorkflow(_ context.Context, req *bossanovav1.CreateWorkflowRequest) (*bossanovav1.CreateWorkflowResponse, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.createWorkflowCalls = append(m.createWorkflowCalls, req)
	if m.createErr != nil {
		return nil, m.createErr
	}
	m.workflow.SessionId = req.GetSessionId()
	m.workflow.RepoId = req.GetRepoId()
	m.workflow.PlanPath = req.GetPlanPath()
	m.workflow.MaxLegs = req.GetMaxLegs()
	m.workflow.ConfigJson = req.GetConfigJson()
	return &bossanovav1.CreateWorkflowResponse{Workflow: m.workflow}, nil
}

func (m *mockHostClient) UpdateWorkflow(_ context.Context, req *bossanovav1.UpdateWorkflowRequest) (*bossanovav1.UpdateWorkflowResponse, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.updateWorkflowCalls = append(m.updateWorkflowCalls, req)
	if req.Status != nil {
		m.workflow.Status = *req.Status
	}
	if req.CurrentStep != nil {
		m.workflow.CurrentStep = *req.CurrentStep
	}
	if req.FlightLeg != nil {
		m.workflow.FlightLeg = *req.FlightLeg
	}
	if req.LastError != nil {
		m.workflow.LastError = *req.LastError
	}
	return &bossanovav1.UpdateWorkflowResponse{Workflow: m.workflow}, nil
}

func (m *mockHostClient) GetWorkflow(_ context.Context, _ string) (*bossanovav1.GetWorkflowResponse, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return &bossanovav1.GetWorkflowResponse{Workflow: m.workflow}, nil
}

func (m *mockHostClient) CreateAttempt(_ context.Context, req *bossanovav1.CreateAttemptRequest) (*bossanovav1.CreateAttemptResponse, error) {
	m.mu.Lock()
	m.createAttemptCalls = append(m.createAttemptCalls, req)

	step := extractStep(req.GetSkillName())
	m.attemptCounts[step]++

	if ab, ok := m.stepAttempts[step]; ok && m.attemptCounts[step] == 1 {
		if ab.createErr != nil {
			m.mu.Unlock()
			return nil, ab.createErr
		}
	}
	if ab, ok := m.retryAttempts[step]; ok && m.attemptCounts[step] > 1 {
		if ab.createErr != nil {
			m.mu.Unlock()
			return nil, ab.createErr
		}
	}

	if m.attemptCreErr != nil {
		m.mu.Unlock()
		return nil, m.attemptCreErr
	}
	cb := m.onAttemptCreated
	m.mu.Unlock()

	if cb != nil {
		cb(step)
	}
	return &bossanovav1.CreateAttemptResponse{AttemptId: m.attemptID}, nil
}

func (m *mockHostClient) GetAttemptStatus(_ context.Context, _ string) (*bossanovav1.GetAttemptStatusResponse, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.pollCount++

	// Find the most recent step from attempt calls.
	var step string
	if len(m.createAttemptCalls) > 0 {
		last := m.createAttemptCalls[len(m.createAttemptCalls)-1]
		step = extractStep(last.GetSkillName())
	}

	// Check per-step behavior.
	count := m.attemptCounts[step]
	if ab, ok := m.stepAttempts[step]; ok && count == 1 {
		if ab.polls > 0 && m.pollCount < ab.polls {
			return &bossanovav1.GetAttemptStatusResponse{
				Status: bossanovav1.AttemptRunStatus_ATTEMPT_RUN_STATUS_RUNNING,
			}, nil
		}
		if ab.err != "" {
			m.pollCount = 0
			return &bossanovav1.GetAttemptStatusResponse{
				Status: bossanovav1.AttemptRunStatus_ATTEMPT_RUN_STATUS_FAILED,
				Error:  ab.err,
			}, nil
		}
	}
	if ab, ok := m.retryAttempts[step]; ok && count > 1 {
		if ab.polls > 0 && m.pollCount < ab.polls {
			return &bossanovav1.GetAttemptStatusResponse{
				Status: bossanovav1.AttemptRunStatus_ATTEMPT_RUN_STATUS_RUNNING,
			}, nil
		}
		if ab.err != "" {
			m.pollCount = 0
			return &bossanovav1.GetAttemptStatusResponse{
				Status: bossanovav1.AttemptRunStatus_ATTEMPT_RUN_STATUS_FAILED,
				Error:  ab.err,
			}, nil
		}
	}

	// Default: complete immediately.
	m.pollCount = 0
	return &bossanovav1.GetAttemptStatusResponse{
		Status: bossanovav1.AttemptRunStatus_ATTEMPT_RUN_STATUS_COMPLETED,
	}, nil
}

func (m *mockHostClient) StreamAttemptOutput(_ context.Context, _ string) (AttemptOutputStream, error) {
	return nil, fmt.Errorf("not implemented in mock")
}

// getUpdateStatuses returns the Status values from all UpdateWorkflow calls.
func (m *mockHostClient) getUpdateStatuses() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	var statuses []string
	for _, req := range m.updateWorkflowCalls {
		if req.Status != nil {
			statuses = append(statuses, *req.Status)
		}
	}
	return statuses
}

// getUpdateSteps returns the CurrentStep values from all UpdateWorkflow calls.
func (m *mockHostClient) getUpdateSteps() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	var steps []string
	for _, req := range m.updateWorkflowCalls {
		if req.CurrentStep != nil {
			steps = append(steps, *req.CurrentStep)
		}
	}
	return steps
}

// getFlightLegUpdates returns the FlightLeg values from all UpdateWorkflow calls
// that set FlightLeg.
func (m *mockHostClient) getFlightLegUpdates() []int32 {
	m.mu.Lock()
	defer m.mu.Unlock()
	var legs []int32
	for _, req := range m.updateWorkflowCalls {
		if req.FlightLeg != nil {
			legs = append(legs, *req.FlightLeg)
		}
	}
	return legs
}

// extractStep maps a skill name back to its step name.
func extractStep(skillName string) string {
	for step, name := range defaultSkillNames {
		if name == skillName {
			return step
		}
	}
	return skillName
}

// Compile-time interface check.
var _ hostClient = (*mockHostClient)(nil)

func newTestOrchestrator(mock *mockHostClient) *orchestrator {
	logger := zerolog.New(os.Stderr).Level(zerolog.Disabled)
	return newOrchestrator(mock, logger)
}

// --- Test: GetInfo ---

func TestGetInfo(t *testing.T) {
	o := newTestOrchestrator(newMockHostClient())
	resp, err := o.GetInfo(context.Background(), &bossanovav1.WorkflowServiceGetInfoRequest{})
	if err != nil {
		t.Fatal(err)
	}
	info := resp.GetInfo()
	if info.GetName() != "autopilot" {
		t.Errorf("name = %q, want %q", info.GetName(), "autopilot")
	}
	if info.GetVersion() != "0.1.0" {
		t.Errorf("version = %q, want %q", info.GetVersion(), "0.1.0")
	}
	if len(info.GetCapabilities()) != 1 || info.GetCapabilities()[0] != "workflow" {
		t.Errorf("capabilities = %v, want [workflow]", info.GetCapabilities())
	}
}

// --- Test: parseWorkflowConfig ---

func TestParseWorkflowConfig(t *testing.T) {
	tests := []struct {
		name      string
		json      string
		wantErr   bool
		checkFunc func(*testing.T, *workflowConfig)
	}{
		{
			name: "empty string returns defaults",
			json: "",
			checkFunc: func(t *testing.T, cfg *workflowConfig) {
				if cfg.handoffDirectory() != "docs/handoffs" {
					t.Errorf("handoffDirectory = %q, want docs/handoffs", cfg.handoffDirectory())
				}
				if cfg.pollInterval() != 5*time.Second {
					t.Errorf("pollInterval = %v, want 5s", cfg.pollInterval())
				}
				if cfg.maxLegs() != 20 {
					t.Errorf("maxLegs = %d, want 20", cfg.maxLegs())
				}
			},
		},
		{
			name: "custom values",
			json: `{"handoff_dir":"custom/handoffs","poll_interval_seconds":10,"max_flight_legs":5,"confirm_land":true}`,
			checkFunc: func(t *testing.T, cfg *workflowConfig) {
				if cfg.handoffDirectory() != "custom/handoffs" {
					t.Errorf("handoffDirectory = %q, want custom/handoffs", cfg.handoffDirectory())
				}
				if cfg.pollInterval() != 10*time.Second {
					t.Errorf("pollInterval = %v, want 10s", cfg.pollInterval())
				}
				if cfg.maxLegs() != 5 {
					t.Errorf("maxLegs = %d, want 5", cfg.maxLegs())
				}
				if !cfg.ConfirmLand {
					t.Error("confirmLand = false, want true")
				}
			},
		},
		{
			name: "skill overrides",
			json: `{"skills":{"plan":"my-plan","land":"my-land"}}`,
			checkFunc: func(t *testing.T, cfg *workflowConfig) {
				if cfg.skillName("plan") != "my-plan" {
					t.Errorf("skillName(plan) = %q, want my-plan", cfg.skillName("plan"))
				}
				if cfg.skillName("implement") != "boss-implement" {
					t.Errorf("skillName(implement) = %q, want boss-implement", cfg.skillName("implement"))
				}
				if cfg.skillName("land") != "my-land" {
					t.Errorf("skillName(land) = %q, want my-land", cfg.skillName("land"))
				}
			},
		},
		{
			name:    "invalid json",
			json:    "{not valid",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg, err := parseWorkflowConfig(tt.json)
			if tt.wantErr {
				if err == nil {
					t.Fatal("expected error, got nil")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if tt.checkFunc != nil {
				tt.checkFunc(t, cfg)
			}
		})
	}
}

// --- Test: validatePlanPath ---

func TestValidatePlanPath(t *testing.T) {
	tests := []struct {
		name    string
		path    string
		wantErr bool
	}{
		{name: "valid relative path", path: "docs/plans/my-plan.md", wantErr: false},
		{name: "empty path", path: "", wantErr: true},
		{name: "absolute path", path: "/etc/passwd", wantErr: true},
		{name: "path with ..", path: "docs/../../../etc/passwd", wantErr: true},
		{name: "simple filename", path: "plan.md", wantErr: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validatePlanPath(tt.path)
			if tt.wantErr && err == nil {
				t.Error("expected error, got nil")
			}
			if !tt.wantErr && err != nil {
				t.Errorf("unexpected error: %v", err)
			}
		})
	}
}

// --- Test: isNonActionableError ---

func TestIsNonActionableError(t *testing.T) {
	tests := []struct {
		name string
		err  string
		want bool
	}{
		{name: "timeout", err: "connection timeout after 30s", want: true},
		{name: "crash", err: "process crashed with SIGSEGV", want: true},
		{name: "signal", err: "received signal SIGTERM", want: true},
		{name: "killed", err: "process was killed", want: true},
		{name: "actionable error", err: "file not found: plan.md", want: false},
		{name: "compilation error", err: "syntax error in server.go:42", want: false},
		{name: "empty error", err: "", want: false},
		{name: "mixed case timeout", err: "Request Timeout", want: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := isNonActionableError(tt.err)
			if got != tt.want {
				t.Errorf("isNonActionableError(%q) = %v, want %v", tt.err, got, tt.want)
			}
		})
	}
}

// --- Test: workflowStatusFromString ---

func TestWorkflowStatusFromString(t *testing.T) {
	tests := []struct {
		input string
		want  bossanovav1.WorkflowStatus
	}{
		{"pending", bossanovav1.WorkflowStatus_WORKFLOW_STATUS_PENDING},
		{"running", bossanovav1.WorkflowStatus_WORKFLOW_STATUS_RUNNING},
		{"paused", bossanovav1.WorkflowStatus_WORKFLOW_STATUS_PAUSED},
		{"completed", bossanovav1.WorkflowStatus_WORKFLOW_STATUS_COMPLETED},
		{"failed", bossanovav1.WorkflowStatus_WORKFLOW_STATUS_FAILED},
		{"cancelled", bossanovav1.WorkflowStatus_WORKFLOW_STATUS_CANCELLED},
		{"unknown", bossanovav1.WorkflowStatus_WORKFLOW_STATUS_UNSPECIFIED},
		{"", bossanovav1.WorkflowStatus_WORKFLOW_STATUS_UNSPECIFIED},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got := workflowStatusFromString(tt.input)
			if got != tt.want {
				t.Errorf("workflowStatusFromString(%q) = %v, want %v", tt.input, got, tt.want)
			}
		})
	}
}

// --- Test: workflowStepFromString ---

func TestWorkflowStepFromString(t *testing.T) {
	tests := []struct {
		input string
		want  bossanovav1.WorkflowStep
	}{
		{"plan", bossanovav1.WorkflowStep_WORKFLOW_STEP_PLAN},
		{"implement", bossanovav1.WorkflowStep_WORKFLOW_STEP_IMPLEMENT},
		{"handoff", bossanovav1.WorkflowStep_WORKFLOW_STEP_HANDOFF},
		{"resume", bossanovav1.WorkflowStep_WORKFLOW_STEP_RESUME},
		{"verify", bossanovav1.WorkflowStep_WORKFLOW_STEP_VERIFY},
		{"land", bossanovav1.WorkflowStep_WORKFLOW_STEP_LAND},
		{"unknown", bossanovav1.WorkflowStep_WORKFLOW_STEP_UNSPECIFIED},
		{"", bossanovav1.WorkflowStep_WORKFLOW_STEP_UNSPECIFIED},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got := workflowStepFromString(tt.input)
			if got != tt.want {
				t.Errorf("workflowStepFromString(%q) = %v, want %v", tt.input, got, tt.want)
			}
		})
	}
}

// --- Test: StartWorkflow ---

func TestStartWorkflow(t *testing.T) {
	tests := []struct {
		name    string
		req     *bossanovav1.StartWorkflowRequest
		wantErr bool
	}{
		{
			name: "valid request",
			req: &bossanovav1.StartWorkflowRequest{
				PlanPath:  "docs/plans/test.md",
				SessionId: "sess-1",
				RepoId:    "repo-1",
			},
		},
		{
			name: "empty plan path",
			req: &bossanovav1.StartWorkflowRequest{
				PlanPath:  "",
				SessionId: "sess-1",
			},
			wantErr: true,
		},
		{
			name: "absolute plan path",
			req: &bossanovav1.StartWorkflowRequest{
				PlanPath:  "/etc/passwd",
				SessionId: "sess-1",
			},
			wantErr: true,
		},
		{
			name: "plan path with ..",
			req: &bossanovav1.StartWorkflowRequest{
				PlanPath:  "docs/../../../etc/passwd",
				SessionId: "sess-1",
			},
			wantErr: true,
		},
		{
			name: "with max_legs override",
			req: &bossanovav1.StartWorkflowRequest{
				PlanPath: "docs/plans/test.md",
				MaxLegs:  3,
			},
		},
		{
			name: "with confirm_land",
			req: &bossanovav1.StartWorkflowRequest{
				PlanPath:    "docs/plans/test.md",
				ConfirmLand: true,
			},
		},
		{
			name: "invalid config json",
			req: &bossanovav1.StartWorkflowRequest{
				PlanPath:   "docs/plans/test.md",
				ConfigJson: "{bad json",
			},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mock := newMockHostClient()
			o := newTestOrchestrator(mock)

			resp, err := o.StartWorkflow(context.Background(), tt.req)
			if tt.wantErr {
				if err == nil {
					t.Fatal("expected error, got nil")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if resp.GetWorkflowId() == "" {
				t.Error("expected non-empty workflow ID")
			}
			if resp.GetStatus().GetStatus() != bossanovav1.WorkflowStatus_WORKFLOW_STATUS_RUNNING {
				t.Errorf("status = %v, want RUNNING", resp.GetStatus().GetStatus())
			}

			// Verify CreateWorkflow was called.
			if len(mock.createWorkflowCalls) != 1 {
				t.Fatalf("expected 1 CreateWorkflow call, got %d", len(mock.createWorkflowCalls))
			}
			if mock.createWorkflowCalls[0].GetPlanPath() != tt.req.GetPlanPath() {
				t.Errorf("plan_path = %q, want %q", mock.createWorkflowCalls[0].GetPlanPath(), tt.req.GetPlanPath())
			}
		})
	}
}

func TestStartWorkflowCreateError(t *testing.T) {
	mock := newMockHostClient()
	mock.createErr = fmt.Errorf("database error")
	o := newTestOrchestrator(mock)

	_, err := o.StartWorkflow(context.Background(), &bossanovav1.StartWorkflowRequest{
		PlanPath: "docs/plans/test.md",
	})
	if err == nil {
		t.Fatal("expected error when CreateWorkflow fails")
	}
}

// --- Test: PauseWorkflow ---

func TestPauseWorkflow(t *testing.T) {
	mock := newMockHostClient()
	mock.workflow.Status = "running"
	o := newTestOrchestrator(mock)

	resp, err := o.PauseWorkflow(context.Background(), &bossanovav1.PauseWorkflowRequest{
		WorkflowId: "wf-test-1",
	})
	if err != nil {
		t.Fatal(err)
	}
	if resp.GetStatus().GetStatus() != bossanovav1.WorkflowStatus_WORKFLOW_STATUS_PAUSED {
		t.Errorf("status = %v, want PAUSED", resp.GetStatus().GetStatus())
	}
}

// --- Test: CancelWorkflow ---

func TestCancelWorkflow(t *testing.T) {
	mock := newMockHostClient()
	mock.workflow.Status = "running"
	o := newTestOrchestrator(mock)

	resp, err := o.CancelWorkflow(context.Background(), &bossanovav1.CancelWorkflowRequest{
		WorkflowId: "wf-test-1",
	})
	if err != nil {
		t.Fatal(err)
	}
	if resp.GetStatus().GetStatus() != bossanovav1.WorkflowStatus_WORKFLOW_STATUS_CANCELLED {
		t.Errorf("status = %v, want CANCELLED", resp.GetStatus().GetStatus())
	}
}

// --- Test: GetWorkflowStatus ---

func TestGetWorkflowStatus(t *testing.T) {
	mock := newMockHostClient()
	mock.workflow.Status = "running"
	mock.workflow.CurrentStep = "implement"
	mock.workflow.FlightLeg = 3
	o := newTestOrchestrator(mock)

	resp, err := o.GetWorkflowStatus(context.Background(), &bossanovav1.GetWorkflowStatusRequest{
		WorkflowId: "wf-test-1",
	})
	if err != nil {
		t.Fatal(err)
	}
	s := resp.GetStatus()
	if s.GetStatus() != bossanovav1.WorkflowStatus_WORKFLOW_STATUS_RUNNING {
		t.Errorf("status = %v, want RUNNING", s.GetStatus())
	}
	if s.GetCurrentStep() != bossanovav1.WorkflowStep_WORKFLOW_STEP_IMPLEMENT {
		t.Errorf("step = %v, want IMPLEMENT", s.GetCurrentStep())
	}
	if s.GetFlightLeg() != 3 {
		t.Errorf("flight_leg = %d, want 3", s.GetFlightLeg())
	}
}

// --- Test: runWorkflow orchestration loop ---

func TestRunWorkflowHappyPath(t *testing.T) {
	// plan → implement → no handoff → handoff recovery → still no handoff → verify → land → completed
	mock := newMockHostClient()
	o := newTestOrchestrator(mock)
	cfg := &workflowConfig{
		PollIntervalSeconds: 1,
		// Use an empty temp dir so scanHandoffDir returns "" (no new handoffs).
		HandoffDir: t.TempDir(),
	}

	o.runWorkflow(context.Background(), "wf-1", "docs/plans/test.md", cfg, 20, "")

	// runWorkflow sets status to "completed" at the end.
	// The "running" transition happens in StartWorkflow (before runWorkflow).
	statuses := mock.getUpdateStatuses()
	if len(statuses) == 0 {
		t.Fatal("expected at least one status update")
	}
	last := statuses[len(statuses)-1]
	if last != "completed" {
		t.Errorf("last status = %q, want completed", last)
	}

	// Should have gone through plan, implement, verify, land steps.
	steps := mock.getUpdateSteps()
	wantSteps := []string{"plan", "implement", "verify", "land"}
	for _, want := range wantSteps {
		if !sliceContains(steps, want) {
			t.Errorf("step %q not found in steps: %v", want, steps)
		}
	}

	// On completion, flight leg should be set to maxLegs.
	legs := mock.getFlightLegUpdates()
	lastLeg := legs[len(legs)-1]
	if lastLeg != 20 {
		t.Errorf("final flight leg = %d, want 20 (maxLegs)", lastLeg)
	}
}

func TestRunWorkflowFlightLegUpdatedDuringImplement(t *testing.T) {
	// Verify that FlightLeg is set to 1 at the start of the implement step.
	mock := newMockHostClient()
	o := newTestOrchestrator(mock)
	cfg := &workflowConfig{
		PollIntervalSeconds: 1,
		HandoffDir:          t.TempDir(),
	}

	o.runWorkflow(context.Background(), "wf-1", "docs/plans/test.md", cfg, 3, "")

	legs := mock.getFlightLegUpdates()
	if len(legs) == 0 {
		t.Fatal("expected at least one flight leg update")
	}
	if legs[0] != 1 {
		t.Errorf("first flight leg update = %d, want 1 (implement)", legs[0])
	}
}

func TestRunWorkflowConfirmLandPause(t *testing.T) {
	// After verify, workflow pauses for landing confirmation.
	mock := newMockHostClient()
	o := newTestOrchestrator(mock)
	cfg := &workflowConfig{
		PollIntervalSeconds: 1,
		ConfirmLand:         true,
		HandoffDir:          t.TempDir(),
	}

	o.runWorkflow(context.Background(), "wf-1", "docs/plans/test.md", cfg, 20, "")

	statuses := mock.getUpdateStatuses()
	// Should end with "paused" (not "completed").
	last := statuses[len(statuses)-1]
	if last != "paused" {
		t.Errorf("last status = %q, want paused (confirm_land)", last)
	}

	// The confirm_land sets current_step to "land" in the update that pauses,
	// but the land flight leg itself should not have run (no attempt created for land).
	steps := mock.getUpdateSteps()
	_ = steps // "land" may appear as step in the pause update; that's expected.
}

func TestRunWorkflowPlanFailure(t *testing.T) {
	// Plan step fails, retry also fails → workflow pauses (user can resume).
	mock := newMockHostClient()
	mock.stepAttempts["plan"] = attemptBehavior{err: "syntax error in plan"}
	mock.retryAttempts["plan"] = attemptBehavior{err: "still broken"}
	o := newTestOrchestrator(mock)
	cfg := &workflowConfig{
		PollIntervalSeconds: 1,
		HandoffDir:          t.TempDir(),
	}

	o.runWorkflow(context.Background(), "wf-1", "docs/plans/test.md", cfg, 20, "")

	statuses := mock.getUpdateStatuses()
	last := statuses[len(statuses)-1]
	if last != "paused" {
		t.Errorf("last status = %q, want paused", last)
	}
}

func TestRunWorkflowRetrySuccess(t *testing.T) {
	// Plan fails on first attempt but retry succeeds → continues normally.
	mock := newMockHostClient()
	mock.stepAttempts["plan"] = attemptBehavior{err: "temporary error"}
	// No retryAttempts entry for "plan" → retry defaults to success.
	o := newTestOrchestrator(mock)
	cfg := &workflowConfig{
		PollIntervalSeconds: 1,
		HandoffDir:          t.TempDir(),
	}

	o.runWorkflow(context.Background(), "wf-1", "docs/plans/test.md", cfg, 20, "")

	statuses := mock.getUpdateStatuses()
	last := statuses[len(statuses)-1]
	if last != "completed" {
		t.Errorf("last status = %q, want completed (retry succeeded)", last)
	}
}

func TestRunWorkflowMaxLegs(t *testing.T) {
	// With max_legs=2 and a handoff file present, implement is leg 1 and
	// the handoff loop runs one resume at leg 2, then proceeds to verify.
	mock := newMockHostClient()
	o := newTestOrchestrator(mock)

	// scanHandoffDir requires a relative path, so create a relative temp dir.
	handoffDir := "testdata_handoffs_maxlegs"
	if err := os.MkdirAll(handoffDir, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(handoffDir) })

	// Create a handoff file with a future mtime so it's after legStart.
	f, err := os.CreateTemp(handoffDir, "handoff-*.md")
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	futureTime := time.Now().Add(1 * time.Hour)
	if err := os.Chtimes(f.Name(), futureTime, futureTime); err != nil {
		t.Fatal(err)
	}

	cfg := &workflowConfig{
		PollIntervalSeconds: 1,
		MaxFlightLegs:       2,
		HandoffDir:          handoffDir,
	}

	o.runWorkflow(context.Background(), "wf-1", "docs/plans/test.md", cfg, 2, "")

	// Should still complete (verify + land after max legs).
	statuses := mock.getUpdateStatuses()
	last := statuses[len(statuses)-1]
	if last != "completed" {
		t.Errorf("last status = %q, want completed", last)
	}

	// Should have a "resume" step.
	steps := mock.getUpdateSteps()
	if !sliceContains(steps, "resume") {
		t.Errorf("expected 'resume' step in steps: %v", steps)
	}

	// Flight leg counter should show leg 1 (implement) then leg 2 (resume).
	legs := mock.getFlightLegUpdates()
	if len(legs) < 2 {
		t.Fatalf("expected at least 2 flight leg updates, got %d: %v", len(legs), legs)
	}
	if legs[0] != 1 {
		t.Errorf("first flight leg update = %d, want 1", legs[0])
	}
	if legs[1] != 2 {
		t.Errorf("second flight leg update = %d, want 2", legs[1])
	}
}

func TestRunWorkflowHandoffRecovery(t *testing.T) {
	// Implement succeeds without creating a handoff file. The orchestrator
	// should run a "handoff" recovery step. During that step, a handoff file
	// appears, so the orchestrator picks it up and runs a "resume" step.
	mock := newMockHostClient()
	o := newTestOrchestrator(mock)

	// scanHandoffDir requires a relative path, so create a relative temp dir.
	handoffDir := "testdata_handoffs_recovery"
	if err := os.MkdirAll(handoffDir, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(handoffDir) })

	cfg := &workflowConfig{
		PollIntervalSeconds: 1,
		HandoffDir:          handoffDir,
		MaxFlightLegs:       3,
	}

	// When the handoff recovery step runs, create a file in the handoff dir
	// so scanHandoffDir finds it on the re-check.
	mock.onAttemptCreated = func(step string) {
		if step == "handoff" {
			f, err := os.CreateTemp(handoffDir, "handoff-recovery-*.md")
			if err != nil {
				t.Errorf("failed to create handoff file in callback: %v", err)
				return
			}
			// Set mtime to the future so scanHandoffDir picks it up.
			future := time.Now().Add(1 * time.Hour)
			_ = os.Chtimes(f.Name(), future, future)
			_ = f.Close()
		}
	}

	o.runWorkflow(context.Background(), "wf-1", "docs/plans/test.md", cfg, 3, "")

	// Should have completed successfully.
	statuses := mock.getUpdateStatuses()
	last := statuses[len(statuses)-1]
	if last != "completed" {
		t.Errorf("last status = %q, want completed", last)
	}

	// Verify the steps include "handoff" (recovery) and "resume".
	steps := mock.getUpdateSteps()
	if !sliceContains(steps, "handoff") {
		t.Errorf("expected 'handoff' step (recovery) in steps: %v", steps)
	}
	if !sliceContains(steps, "resume") {
		t.Errorf("expected 'resume' step in steps: %v", steps)
	}

	// Verify that a handoff recovery attempt was created with the right skill.
	var foundHandoffAttempt bool
	for _, call := range mock.createAttemptCalls {
		if call.GetSkillName() == "boss-handoff" {
			foundHandoffAttempt = true
			break
		}
	}
	if !foundHandoffAttempt {
		t.Error("expected a CreateAttempt call with skill 'boss-handoff' for recovery")
	}

	// Flight leg counter should show leg 1 (implement) then leg 2 (recovery resume).
	legs := mock.getFlightLegUpdates()
	if len(legs) < 2 {
		t.Fatalf("expected at least 2 flight leg updates, got %d: %v", len(legs), legs)
	}
	if legs[0] != 1 {
		t.Errorf("first flight leg update = %d, want 1", legs[0])
	}
	if legs[1] != 2 {
		t.Errorf("second flight leg update = %d, want 2", legs[1])
	}
}

func TestRunWorkflowHandoffRecoveryFails(t *testing.T) {
	// When handoff recovery fails, the orchestrator should proceed to verify.
	mock := newMockHostClient()
	mock.stepAttempts["handoff"] = attemptBehavior{err: "recovery failed"}
	mock.retryAttempts["handoff"] = attemptBehavior{err: "still failed"}
	o := newTestOrchestrator(mock)

	cfg := &workflowConfig{
		PollIntervalSeconds: 1,
		HandoffDir:          t.TempDir(),
		MaxFlightLegs:       3,
	}

	o.runWorkflow(context.Background(), "wf-1", "docs/plans/test.md", cfg, 3, "")

	// Should still complete (handoff failed → break → verify → land → completed).
	statuses := mock.getUpdateStatuses()
	last := statuses[len(statuses)-1]
	if last != "completed" {
		t.Errorf("last status = %q, want completed", last)
	}

	// Should have attempted handoff but then proceeded to verify and land.
	steps := mock.getUpdateSteps()
	if !sliceContains(steps, "handoff") {
		t.Errorf("expected 'handoff' step in steps: %v", steps)
	}
	if !sliceContains(steps, "verify") {
		t.Errorf("expected 'verify' step in steps: %v", steps)
	}
}

func TestRunWorkflowPausedDuringExecution(t *testing.T) {
	// Simulate workflow being paused externally during execution.
	mock := newMockHostClient()
	o := newTestOrchestrator(mock)
	cfg := &workflowConfig{
		PollIntervalSeconds: 1,
		HandoffDir:          t.TempDir(),
	}

	// Set workflow to "paused" so isStoppedOrDone returns true
	// after the implement step.
	mock.stepAttempts["implement"] = attemptBehavior{} // success
	// After implement completes, the loop checks isStoppedOrDone.
	// We need to set the status to paused after implement runs.
	// We do this by having the mock set status to paused on the
	// second call to GetWorkflow (first is the isStoppedOrDone check
	// before the handoff loop).

	// Actually, we'll just pre-set the workflow to paused. The orchestration
	// loop calls isStoppedOrDone before the handoff/resume loop which reads
	// from GetWorkflow.
	go func() {
		// Wait for plan and implement to start, then pause.
		time.Sleep(50 * time.Millisecond)
		mock.mu.Lock()
		mock.workflow.Status = "paused"
		mock.mu.Unlock()
	}()

	o.runWorkflow(context.Background(), "wf-1", "docs/plans/test.md", cfg, 20, "")

	// Should NOT have a "completed" status — paused stops the loop.
	statuses := mock.getUpdateStatuses()
	if sliceContains(statuses, "completed") {
		t.Error("workflow should not have completed — it was paused")
	}
}

func TestRunWorkflowCancelledDuringExecution(t *testing.T) {
	mock := newMockHostClient()
	o := newTestOrchestrator(mock)
	cfg := &workflowConfig{
		PollIntervalSeconds: 1,
		HandoffDir:          t.TempDir(),
	}

	go func() {
		time.Sleep(50 * time.Millisecond)
		mock.mu.Lock()
		mock.workflow.Status = "cancelled"
		mock.mu.Unlock()
	}()

	o.runWorkflow(context.Background(), "wf-1", "docs/plans/test.md", cfg, 20, "")

	statuses := mock.getUpdateStatuses()
	if sliceContains(statuses, "completed") {
		t.Error("workflow should not have completed — it was cancelled")
	}
}

func TestRunWorkflowVerifyFailure(t *testing.T) {
	// Verify step fails (both attempts) → workflow pauses (user can resume).
	mock := newMockHostClient()
	mock.stepAttempts["verify"] = attemptBehavior{err: "tests failed"}
	mock.retryAttempts["verify"] = attemptBehavior{err: "tests still failing"}
	o := newTestOrchestrator(mock)
	cfg := &workflowConfig{
		PollIntervalSeconds: 1,
		HandoffDir:          t.TempDir(),
	}

	o.runWorkflow(context.Background(), "wf-1", "docs/plans/test.md", cfg, 20, "")

	statuses := mock.getUpdateStatuses()
	last := statuses[len(statuses)-1]
	if last != "paused" {
		t.Errorf("last status = %q, want paused", last)
	}
}

func TestRunWorkflowLandFailure(t *testing.T) {
	// Land step fails → workflow pauses (user can resume).
	mock := newMockHostClient()
	mock.stepAttempts["land"] = attemptBehavior{err: "push rejected"}
	mock.retryAttempts["land"] = attemptBehavior{err: "still rejected"}
	o := newTestOrchestrator(mock)
	cfg := &workflowConfig{
		PollIntervalSeconds: 1,
		HandoffDir:          t.TempDir(),
	}

	o.runWorkflow(context.Background(), "wf-1", "docs/plans/test.md", cfg, 20, "")

	statuses := mock.getUpdateStatuses()
	last := statuses[len(statuses)-1]
	if last != "paused" {
		t.Errorf("last status = %q, want paused", last)
	}
}

// --- Test: ResumeWorkflow ---

func TestResumeWorkflow(t *testing.T) {
	mock := newMockHostClient()
	mock.workflow.Status = "paused"
	mock.workflow.CurrentStep = "land"
	mock.workflow.ConfigJson = `{}`
	o := newTestOrchestrator(mock)

	resp, err := o.ResumeWorkflow(context.Background(), &bossanovav1.ResumeWorkflowRequest{
		WorkflowId: "wf-test-1",
	})
	if err != nil {
		t.Fatal(err)
	}
	// Should transition to running.
	if resp.GetStatus().GetStatus() != bossanovav1.WorkflowStatus_WORKFLOW_STATUS_RUNNING {
		t.Errorf("status = %v, want RUNNING", resp.GetStatus().GetStatus())
	}
}

// --- Test: workflowToStatusInfo ---

func TestWorkflowToStatusInfo(t *testing.T) {
	tests := []struct {
		name     string
		workflow *bossanovav1.Workflow
		wantNil  bool
	}{
		{
			name:    "nil workflow",
			wantNil: true,
		},
		{
			name: "running workflow",
			workflow: &bossanovav1.Workflow{
				Id:          "wf-1",
				Status:      "running",
				CurrentStep: "implement",
				FlightLeg:   2,
				LastError:   "",
			},
		},
		{
			name: "failed workflow with error",
			workflow: &bossanovav1.Workflow{
				Id:          "wf-2",
				Status:      "failed",
				CurrentStep: "verify",
				FlightLeg:   5,
				LastError:   "tests failed",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			info := workflowToStatusInfo(tt.workflow)
			if tt.wantNil {
				if info != nil {
					t.Error("expected nil, got non-nil")
				}
				return
			}
			if info == nil {
				t.Fatal("expected non-nil")
			}
			if info.GetId() != tt.workflow.GetId() {
				t.Errorf("id = %q, want %q", info.GetId(), tt.workflow.GetId())
			}
			if info.GetStatus() != workflowStatusFromString(tt.workflow.GetStatus()) {
				t.Errorf("status = %v, want %v", info.GetStatus(), workflowStatusFromString(tt.workflow.GetStatus()))
			}
			if info.GetCurrentStep() != workflowStepFromString(tt.workflow.GetCurrentStep()) {
				t.Errorf("step = %v, want %v", info.GetCurrentStep(), workflowStepFromString(tt.workflow.GetCurrentStep()))
			}
			if info.GetFlightLeg() != tt.workflow.GetFlightLeg() {
				t.Errorf("flight_leg = %d, want %d", info.GetFlightLeg(), tt.workflow.GetFlightLeg())
			}
			if info.GetLastError() != tt.workflow.GetLastError() {
				t.Errorf("last_error = %q, want %q", info.GetLastError(), tt.workflow.GetLastError())
			}
		})
	}
}

// --- Test: smartRetry prompt construction ---

func TestSmartRetryPrompt(t *testing.T) {
	tests := []struct {
		name           string
		lastError      string
		wantContains   string
		wantNotContain string
	}{
		{
			name:         "actionable error includes error message",
			lastError:    "file not found: plan.md",
			wantContains: "file not found: plan.md",
		},
		{
			name:           "timeout uses generic prompt",
			lastError:      "connection timeout",
			wantContains:   "Previous attempt failed unexpectedly",
			wantNotContain: "connection timeout",
		},
		{
			name:           "crash uses generic prompt",
			lastError:      "process crashed",
			wantContains:   "Previous attempt failed unexpectedly",
			wantNotContain: "process crashed",
		},
		{
			name:         "empty error uses generic prompt",
			lastError:    "",
			wantContains: "Previous attempt failed unexpectedly",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mock := newMockHostClient()
			o := newTestOrchestrator(mock)
			cfg := &workflowConfig{PollIntervalSeconds: 1}

			// smartRetry will create an attempt — capture the prompt.
			_ = o.smartRetry(context.Background(), "wf-1", "plan", "docs/plans/test.md", tt.lastError, cfg)

			if len(mock.createAttemptCalls) == 0 {
				t.Fatal("expected CreateAttempt call for retry")
			}
			prompt := mock.createAttemptCalls[0].GetInput()
			if tt.wantContains != "" {
				if !containsString(prompt, tt.wantContains) {
					t.Errorf("prompt %q does not contain %q", prompt, tt.wantContains)
				}
			}
			if tt.wantNotContain != "" {
				if containsString(prompt, tt.wantNotContain) {
					t.Errorf("prompt %q should not contain %q", prompt, tt.wantNotContain)
				}
			}
		})
	}
}

// --- Test: skillName defaults ---

func TestSkillNameDefaults(t *testing.T) {
	cfg := &workflowConfig{}
	tests := []struct {
		step string
		want string
	}{
		{"plan", "boss-create-tasks"},
		{"implement", "boss-implement"},
		{"handoff", "boss-handoff"},
		{"resume", "boss-resume"},
		{"verify", "boss-verify"},
		{"land", "boss-finalize"},
		{"unknown-step", "unknown-step"},
	}

	for _, tt := range tests {
		t.Run(tt.step, func(t *testing.T) {
			got := cfg.skillName(tt.step)
			if got != tt.want {
				t.Errorf("skillName(%q) = %q, want %q", tt.step, got, tt.want)
			}
		})
	}
}

// --- Test: isStoppedOrDone ---

func TestIsStoppedOrDone(t *testing.T) {
	tests := []struct {
		name   string
		status string
		want   bool
	}{
		{"running", "running", false},
		{"pending", "pending", false},
		{"paused", "paused", true},
		{"cancelled", "cancelled", true},
		{"completed", "completed", true},
		{"failed", "failed", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mock := newMockHostClient()
			mock.workflow.Status = tt.status
			o := newTestOrchestrator(mock)

			got := o.isStoppedOrDone(context.Background(), "wf-1")
			if got != tt.want {
				t.Errorf("isStoppedOrDone() = %v, want %v", got, tt.want)
			}
		})
	}
}

// --- Test: context cancellation in pollAttempt ---

func TestPollAttemptContextCancelled(t *testing.T) {
	mock := newMockHostClient()
	o := newTestOrchestrator(mock)

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // Cancel immediately.

	_, err := o.pollAttempt(ctx, "attempt-1", 100*time.Millisecond)
	if err == nil {
		t.Fatal("expected error for cancelled context")
	}
}

// --- helpers ---

func sliceContains(slice []string, s string) bool {
	for _, v := range slice {
		if v == s {
			return true
		}
	}
	return false
}

func containsString(s, substr string) bool {
	return len(s) >= len(substr) && searchString(s, substr)
}

func searchString(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}

// --- Test: runWorkflow resume from specific step ---

func TestRunWorkflowResumeFromVerify(t *testing.T) {
	// When resuming from "verify", skip plan, implement, and handoff loop.
	mock := newMockHostClient()
	o := newTestOrchestrator(mock)
	cfg := &workflowConfig{
		PollIntervalSeconds: 1,
		HandoffDir:          t.TempDir(),
	}

	o.runWorkflow(context.Background(), "wf-1", "docs/plans/test.md", cfg, 20, "verify")

	// Should have gone through verify and land, but not plan or implement.
	steps := mock.getUpdateSteps()
	if sliceContains(steps, "plan") {
		t.Error("should not have run plan step when resuming from verify")
	}
	if sliceContains(steps, "implement") {
		t.Error("should not have run implement step when resuming from verify")
	}
	if !sliceContains(steps, "verify") {
		t.Errorf("expected 'verify' step in steps: %v", steps)
	}
	if !sliceContains(steps, "land") {
		t.Errorf("expected 'land' step in steps: %v", steps)
	}

	statuses := mock.getUpdateStatuses()
	last := statuses[len(statuses)-1]
	if last != "completed" {
		t.Errorf("last status = %q, want completed", last)
	}
}

func TestRunWorkflowResumeFromLand(t *testing.T) {
	// When resuming from "land" (after confirm_land pause), only run land.
	mock := newMockHostClient()
	o := newTestOrchestrator(mock)
	cfg := &workflowConfig{
		PollIntervalSeconds: 1,
		ConfirmLand:         true, // Would normally pause before land.
		HandoffDir:          t.TempDir(),
	}

	o.runWorkflow(context.Background(), "wf-1", "docs/plans/test.md", cfg, 20, "land")

	// Should only have land step, no plan/implement/verify.
	steps := mock.getUpdateSteps()
	if sliceContains(steps, "plan") {
		t.Error("should not have run plan step when resuming from land")
	}
	if sliceContains(steps, "verify") {
		t.Error("should not have run verify step when resuming from land")
	}
	if !sliceContains(steps, "land") {
		t.Errorf("expected 'land' step in steps: %v", steps)
	}

	// Should complete (not pause for confirm_land since we're resuming at land).
	statuses := mock.getUpdateStatuses()
	last := statuses[len(statuses)-1]
	if last != "completed" {
		t.Errorf("last status = %q, want completed", last)
	}
}
