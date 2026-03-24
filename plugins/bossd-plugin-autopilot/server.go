package main

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/rs/zerolog"

	bossanovav1 "github.com/recurser/bossalib/gen/bossanova/v1"
)

// orchestrator implements the WorkflowService gRPC server for the autopilot
// plugin. It drives the plan→implement→handoff/resume→verify→land loop
// by calling back to the daemon via HostService RPCs.
type orchestrator struct {
	host   hostClient
	logger zerolog.Logger
}

func newOrchestrator(host hostClient, logger zerolog.Logger) *orchestrator {
	return &orchestrator{host: host, logger: logger}
}

// workflowConfig holds the parsed config for a workflow run. Fields mirror
// config.AutopilotConfig but are local to the plugin to avoid importing
// the config package.
type workflowConfig struct {
	Skills              skillOverrides `json:"skills,omitempty"`
	HandoffDir          string         `json:"handoff_dir,omitempty"`
	PollIntervalSeconds int            `json:"poll_interval_seconds,omitempty"`
	MaxFlightLegs       int            `json:"max_flight_legs,omitempty"`
	ConfirmLand         bool           `json:"confirm_land,omitempty"`
}

type skillOverrides struct {
	Plan      string `json:"plan,omitempty"`
	Implement string `json:"implement,omitempty"`
	Handoff   string `json:"handoff,omitempty"`
	Resume    string `json:"resume,omitempty"`
	Verify    string `json:"verify,omitempty"`
	Land      string `json:"land,omitempty"`
}

var defaultSkillNames = map[string]string{
	"plan":      "boss-create-tasks",
	"implement": "boss-implement",
	"handoff":   "boss-handoff",
	"resume":    "boss-resume",
	"verify":    "boss-verify",
	"land":      "boss-finalize",
}

func (c *workflowConfig) handoffDirectory() string {
	if c.HandoffDir != "" {
		return c.HandoffDir
	}
	return "docs/handoffs"
}

func (c *workflowConfig) pollInterval() time.Duration {
	if c.PollIntervalSeconds > 0 {
		return time.Duration(c.PollIntervalSeconds) * time.Second
	}
	return 5 * time.Second
}

func (c *workflowConfig) maxLegs() int {
	if c.MaxFlightLegs > 0 {
		return c.MaxFlightLegs
	}
	return 20
}

func (c *workflowConfig) skillName(step string) string {
	switch step {
	case "plan":
		if c.Skills.Plan != "" {
			return c.Skills.Plan
		}
	case "implement":
		if c.Skills.Implement != "" {
			return c.Skills.Implement
		}
	case "handoff":
		if c.Skills.Handoff != "" {
			return c.Skills.Handoff
		}
	case "resume":
		if c.Skills.Resume != "" {
			return c.Skills.Resume
		}
	case "verify":
		if c.Skills.Verify != "" {
			return c.Skills.Verify
		}
	case "land":
		if c.Skills.Land != "" {
			return c.Skills.Land
		}
	}
	if name, ok := defaultSkillNames[step]; ok {
		return name
	}
	return step
}

func parseWorkflowConfig(configJSON string) (*workflowConfig, error) {
	cfg := &workflowConfig{}
	if configJSON == "" {
		return cfg, nil
	}
	if err := json.Unmarshal([]byte(configJSON), cfg); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}
	return cfg, nil
}

// --- WorkflowService RPC implementations ---

func (o *orchestrator) GetInfo(_ context.Context, _ *bossanovav1.WorkflowServiceGetInfoRequest) (*bossanovav1.WorkflowServiceGetInfoResponse, error) { //nolint:unparam // interface implementation
	return &bossanovav1.WorkflowServiceGetInfoResponse{
		Info: &bossanovav1.PluginInfo{
			Name:         "autopilot",
			Version:      "0.1.0",
			Capabilities: []string{"workflow"},
		},
	}, nil
}

func (o *orchestrator) StartWorkflow(ctx context.Context, req *bossanovav1.StartWorkflowRequest) (*bossanovav1.StartWorkflowResponse, error) {
	planPath := req.GetPlanPath()
	if err := validatePlanPath(planPath); err != nil {
		return nil, fmt.Errorf("invalid plan path: %w", err)
	}

	cfg, err := parseWorkflowConfig(req.GetConfigJson())
	if err != nil {
		return nil, err
	}

	// Apply request-level overrides.
	if req.GetConfirmLand() {
		cfg.ConfirmLand = true
	}

	maxLegs := cfg.maxLegs()
	if req.GetMaxLegs() > 0 {
		maxLegs = int(req.GetMaxLegs())
	}

	// Create workflow in pending state.
	createResp, err := o.host.CreateWorkflow(ctx, &bossanovav1.CreateWorkflowRequest{
		SessionId:      req.GetSessionId(),
		RepoId:         req.GetRepoId(),
		PlanPath:       planPath,
		MaxLegs:        int32(maxLegs),
		StartCommitSha: "", // filled by daemon if needed
		ConfigJson:     req.GetConfigJson(),
	})
	if err != nil {
		return nil, fmt.Errorf("create workflow: %w", err)
	}

	workflowID := createResp.GetWorkflow().GetId()
	o.logger.Info().Str("workflow_id", workflowID).Str("plan", planPath).Msg("workflow created")

	// Transition to running.
	if _, err := o.host.UpdateWorkflow(ctx, &bossanovav1.UpdateWorkflowRequest{
		Id:     workflowID,
		Status: stringPtr("running"),
	}); err != nil {
		return nil, fmt.Errorf("update workflow to running: %w", err)
	}

	// Run the orchestration loop in the background. StartWorkflow returns
	// immediately so the caller can stream output or poll status.
	go o.runWorkflow(context.WithoutCancel(ctx), workflowID, planPath, cfg, maxLegs, "")

	return &bossanovav1.StartWorkflowResponse{
		WorkflowId: workflowID,
		Status: &bossanovav1.WorkflowStatusInfo{
			Id:          workflowID,
			Status:      bossanovav1.WorkflowStatus_WORKFLOW_STATUS_RUNNING,
			CurrentStep: bossanovav1.WorkflowStep_WORKFLOW_STEP_PLAN,
			FlightLeg:   0,
		},
	}, nil
}

func (o *orchestrator) PauseWorkflow(ctx context.Context, req *bossanovav1.PauseWorkflowRequest) (*bossanovav1.PauseWorkflowResponse, error) {
	resp, err := o.host.UpdateWorkflow(ctx, &bossanovav1.UpdateWorkflowRequest{
		Id:     req.GetWorkflowId(),
		Status: stringPtr("paused"),
	})
	if err != nil {
		return nil, fmt.Errorf("pause workflow: %w", err)
	}
	return &bossanovav1.PauseWorkflowResponse{
		Status: workflowToStatusInfo(resp.GetWorkflow()),
	}, nil
}

func (o *orchestrator) ResumeWorkflow(ctx context.Context, req *bossanovav1.ResumeWorkflowRequest) (*bossanovav1.ResumeWorkflowResponse, error) {
	wfResp, err := o.host.GetWorkflow(ctx, req.GetWorkflowId())
	if err != nil {
		return nil, fmt.Errorf("get workflow: %w", err)
	}
	wf := wfResp.GetWorkflow()

	cfg, err := parseWorkflowConfig(wf.GetConfigJson())
	if err != nil {
		return nil, err
	}

	// Transition back to running and clear any previous error.
	emptyErr := ""
	resp, err := o.host.UpdateWorkflow(ctx, &bossanovav1.UpdateWorkflowRequest{
		Id:        req.GetWorkflowId(),
		Status:    stringPtr("running"),
		LastError: &emptyErr,
	})
	if err != nil {
		return nil, fmt.Errorf("resume workflow: %w", err)
	}

	// Continue the orchestration loop from the persisted step.
	go o.runWorkflow(context.WithoutCancel(ctx), wf.GetId(), wf.GetPlanPath(), cfg, int(wf.GetMaxLegs()), wf.GetCurrentStep())

	return &bossanovav1.ResumeWorkflowResponse{
		Status: workflowToStatusInfo(resp.GetWorkflow()),
	}, nil
}

func (o *orchestrator) CancelWorkflow(ctx context.Context, req *bossanovav1.CancelWorkflowRequest) (*bossanovav1.CancelWorkflowResponse, error) {
	resp, err := o.host.UpdateWorkflow(ctx, &bossanovav1.UpdateWorkflowRequest{
		Id:     req.GetWorkflowId(),
		Status: stringPtr("cancelled"),
	})
	if err != nil {
		return nil, fmt.Errorf("cancel workflow: %w", err)
	}
	return &bossanovav1.CancelWorkflowResponse{
		Status: workflowToStatusInfo(resp.GetWorkflow()),
	}, nil
}

func (o *orchestrator) GetWorkflowStatus(ctx context.Context, req *bossanovav1.GetWorkflowStatusRequest) (*bossanovav1.GetWorkflowStatusResponse, error) {
	resp, err := o.host.GetWorkflow(ctx, req.GetWorkflowId())
	if err != nil {
		return nil, fmt.Errorf("get workflow status: %w", err)
	}
	return &bossanovav1.GetWorkflowStatusResponse{
		Status: workflowToStatusInfo(resp.GetWorkflow()),
	}, nil
}

// --- Orchestration loop ---

func (o *orchestrator) runWorkflow(ctx context.Context, workflowID, planPath string, cfg *workflowConfig, maxLegs int, startStep string) {
	log := o.logger.With().Str("workflow_id", workflowID).Logger()

	// Step ordering for resume support. When resuming, skip already-completed steps.
	stepOrder := map[string]int{"plan": 1, "implement": 2, "resume": 3, "handoff": 3, "verify": 4, "land": 5}
	startIdx := stepOrder[startStep] // 0 if startStep is "" (start from beginning)

	// legStart tracks when the current flight leg began, so scanHandoffDir
	// can detect handoff files created since the leg started.
	var legStart time.Time

	// Step 1: Plan.
	if startIdx <= 1 {
		log.Info().Msg("starting plan step")
		if err := o.runFlightLeg(ctx, workflowID, "plan", planPath, cfg); err != nil {
			o.pauseWorkflowOnError(ctx, workflowID, "plan", err)
			return
		}
	}

	// Step 2: Implement (= Flight Leg 1).
	if startIdx <= 2 {
		// Capture time before implement so handoff files it creates are
		// detected by scanHandoffDir in the loop below.
		legStart = time.Now()

		// Implement is the first flight leg — update the counter so status
		// commands show progress immediately.
		legVal := int32(1)
		if _, err := o.host.UpdateWorkflow(ctx, &bossanovav1.UpdateWorkflowRequest{
			Id:        workflowID,
			FlightLeg: &legVal,
		}); err != nil {
			log.Warn().Err(err).Msg("failed to update flight leg counter")
		}

		log.Info().Msg("starting implement step")
		if err := o.runFlightLeg(ctx, workflowID, "implement", planPath, cfg); err != nil {
			o.pauseWorkflowOnError(ctx, workflowID, "implement", err)
			return
		}
	}

	// Step 3: Handoff/resume loop (Flight Legs 2..maxLegs).
	if startIdx <= 3 {
		if startIdx == 3 {
			// Resuming into the handoff loop — find existing handoff files.
			legStart = time.Time{}
		}
		for leg := 2; leg <= maxLegs; leg++ {
			// Check if workflow was paused/cancelled.
			if o.isStoppedOrDone(ctx, workflowID) {
				return
			}

			handoffFile, err := scanHandoffDir(cfg.handoffDirectory(), legStart)
			if err != nil {
				log.Warn().Err(err).Msg("failed to scan handoff directory")
				// If the handoff dir doesn't exist, proceed to verify.
				handoffFile = ""
			}

			if handoffFile == "" {
				// No new handoff file — proceed to verify.
				log.Info().Int("leg", leg).Msg("no new handoff file, proceeding to verify")
				break
			}

			// Update the flight leg counter in the DB so status commands reflect progress.
			legVal := int32(leg)
			if _, err := o.host.UpdateWorkflow(ctx, &bossanovav1.UpdateWorkflowRequest{
				Id:        workflowID,
				FlightLeg: &legVal,
			}); err != nil {
				log.Warn().Err(err).Int("leg", leg).Msg("failed to update flight leg counter")
			}

			// New handoff file found — resume with it.
			log.Info().Int("leg", leg).Str("handoff", handoffFile).Msg("found handoff file, resuming")
			legStart = time.Now()
			if err := o.runFlightLeg(ctx, workflowID, "resume", handoffFile, cfg); err != nil {
				o.pauseWorkflowOnError(ctx, workflowID, "resume", err)
				return
			}

			if leg == maxLegs {
				log.Warn().Int("max_legs", maxLegs).Msg("max flight legs reached, proceeding to verify")
			}
		}
	}

	// Step 4: Verify.
	if startIdx <= 4 {
		if o.isStoppedOrDone(ctx, workflowID) {
			return
		}
		log.Info().Msg("starting verify step")
		if err := o.runFlightLeg(ctx, workflowID, "verify", planPath, cfg); err != nil {
			o.pauseWorkflowOnError(ctx, workflowID, "verify", err)
			return
		}
	}

	// Step 5: Confirm-land pause (skip when resuming directly to land).
	if cfg.ConfirmLand && startStep != "land" {
		log.Info().Msg("pausing for landing confirmation")
		if _, err := o.host.UpdateWorkflow(ctx, &bossanovav1.UpdateWorkflowRequest{
			Id:          workflowID,
			Status:      stringPtr("paused"),
			CurrentStep: stringPtr("land"),
		}); err != nil {
			log.Error().Err(err).Msg("failed to pause for landing confirmation")
		}
		return
	}

	// Step 6: Land.
	if o.isStoppedOrDone(ctx, workflowID) {
		return
	}
	log.Info().Msg("starting land step")
	if err := o.runFlightLeg(ctx, workflowID, "land", planPath, cfg); err != nil {
		o.pauseWorkflowOnError(ctx, workflowID, "land", err)
		return
	}

	// Done — set flight leg to maxLegs so the status display shows completion
	// (e.g. "2/2" instead of "1/2" when the agent finished early).
	legVal := int32(maxLegs)
	if _, err := o.host.UpdateWorkflow(ctx, &bossanovav1.UpdateWorkflowRequest{
		Id:        workflowID,
		Status:    stringPtr("completed"),
		FlightLeg: &legVal,
	}); err != nil {
		log.Error().Err(err).Msg("failed to mark workflow completed")
	}
	log.Info().Msg("workflow completed")
}

// runFlightLeg executes a single flight leg: creates a Claude attempt for the
// given step and polls until completion. On failure, performs one smart retry.
func (o *orchestrator) runFlightLeg(ctx context.Context, workflowID, step, input string, cfg *workflowConfig) error {
	log := o.logger.With().Str("workflow_id", workflowID).Str("step", step).Logger()

	// Update workflow state.
	if _, err := o.host.UpdateWorkflow(ctx, &bossanovav1.UpdateWorkflowRequest{
		Id:          workflowID,
		CurrentStep: stringPtr(step),
	}); err != nil {
		return fmt.Errorf("update step to %s: %w", step, err)
	}

	// Build prompt.
	skillName := cfg.skillName(step)
	prompt := fmt.Sprintf("/%s %s", skillName, input)

	// Create attempt.
	attemptResp, err := o.host.CreateAttempt(ctx, &bossanovav1.CreateAttemptRequest{
		WorkflowId: workflowID,
		SkillName:  skillName,
		Input:      prompt,
		WorkDir:    "", // daemon uses session's work dir
	})
	if err != nil {
		return fmt.Errorf("create attempt for %s: %w", step, err)
	}

	attemptID := attemptResp.GetAttemptId()
	log.Info().Str("attempt_id", attemptID).Msg("attempt started")

	// Poll for completion.
	lastError, err := o.pollAttempt(ctx, attemptID, cfg.pollInterval())
	if err != nil {
		return fmt.Errorf("poll attempt for %s: %w", step, err)
	}

	// Check for soft failures: Claude may exit 0 but report an error in its
	// output (e.g. "Unknown skill: boss-plan"). Inspect the final output lines.
	if lastError == "" {
		if softErr := o.checkOutputForSoftFailure(ctx, attemptID); softErr != "" {
			lastError = softErr
		}
	}

	if lastError == "" {
		log.Info().Msg("flight leg completed successfully")
		return nil
	}

	// Attempt failed — try smart retry once.
	log.Warn().Str("error", lastError).Msg("attempt failed, trying smart retry")
	return o.smartRetry(ctx, workflowID, step, input, lastError, cfg)
}

// pollAttempt polls GetAttemptStatus until the attempt is no longer running.
// Returns the error string from the attempt (empty if successful).
func (o *orchestrator) pollAttempt(ctx context.Context, attemptID string, interval time.Duration) (string, error) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-ticker.C:
			resp, err := o.host.GetAttemptStatus(ctx, attemptID)
			if err != nil {
				return "", fmt.Errorf("get attempt status: %w", err)
			}

			switch resp.GetStatus() {
			case bossanovav1.AttemptRunStatus_ATTEMPT_RUN_STATUS_RUNNING:
				continue
			case bossanovav1.AttemptRunStatus_ATTEMPT_RUN_STATUS_COMPLETED:
				return "", nil
			case bossanovav1.AttemptRunStatus_ATTEMPT_RUN_STATUS_FAILED:
				return resp.GetError(), nil
			default:
				// Completed or unknown — treat as completed.
				return "", nil
			}
		}
	}
}

// smartRetry performs a single retry with context about the previous failure.
func (o *orchestrator) smartRetry(ctx context.Context, workflowID, step, input, lastError string, cfg *workflowConfig) error {
	log := o.logger.With().Str("workflow_id", workflowID).Str("step", step).Logger()

	skillName := cfg.skillName(step)
	var prompt string
	if lastError == "" || isNonActionableError(lastError) {
		prompt = fmt.Sprintf("/%s %s\n\nPrevious attempt failed unexpectedly. Please try again.", skillName, input)
	} else {
		prompt = fmt.Sprintf("/%s %s\n\nPrevious attempt failed with: %s\nPlease address this and continue.", skillName, input, lastError)
	}

	attemptResp, err := o.host.CreateAttempt(ctx, &bossanovav1.CreateAttemptRequest{
		WorkflowId: workflowID,
		SkillName:  skillName,
		Input:      prompt,
		WorkDir:    "",
	})
	if err != nil {
		return fmt.Errorf("create retry attempt for %s: %w", step, err)
	}

	attemptID := attemptResp.GetAttemptId()
	log.Info().Str("attempt_id", attemptID).Msg("retry attempt started")

	retryError, err := o.pollAttempt(ctx, attemptID, cfg.pollInterval())
	if err != nil {
		return fmt.Errorf("poll retry attempt for %s: %w", step, err)
	}

	if retryError != "" {
		return fmt.Errorf("retry failed for %s: %s", step, retryError)
	}

	log.Info().Msg("retry succeeded")
	return nil
}

// checkOutputForSoftFailure inspects the final output lines of a completed
// attempt for known failure patterns that Claude reports with exit code 0
// (e.g. "Unknown skill: boss-plan"). Returns an error string if detected.
func (o *orchestrator) checkOutputForSoftFailure(ctx context.Context, attemptID string) string {
	resp, err := o.host.GetAttemptStatus(ctx, attemptID)
	if err != nil {
		return ""
	}
	lines := resp.GetOutputLines()
	// Check the last few lines for known failure patterns in stream-json output.
	start := len(lines) - 5
	if start < 0 {
		start = 0
	}
	for _, line := range lines[start:] {
		if strings.Contains(line, "Unknown skill:") {
			return "unknown skill (check that boss-* skills are installed in the target worktree)"
		}
	}
	return ""
}

// pauseWorkflowOnError pauses a workflow after a flight leg failure, preserving
// the error so the user can inspect it and resume.
func (o *orchestrator) pauseWorkflowOnError(ctx context.Context, workflowID, step string, err error) {
	o.logger.Error().Err(err).Str("workflow_id", workflowID).Str("step", step).Msg("workflow paused on error")
	errStr := err.Error()
	if _, updateErr := o.host.UpdateWorkflow(ctx, &bossanovav1.UpdateWorkflowRequest{
		Id:        workflowID,
		Status:    stringPtr("paused"),
		LastError: &errStr,
	}); updateErr != nil {
		o.logger.Error().Err(updateErr).Msg("failed to pause workflow on error")
	}
}

// isStoppedOrDone checks if the workflow has been paused or cancelled.
func (o *orchestrator) isStoppedOrDone(ctx context.Context, workflowID string) bool {
	resp, err := o.host.GetWorkflow(ctx, workflowID)
	if err != nil {
		o.logger.Error().Err(err).Str("workflow_id", workflowID).Msg("failed to check workflow status")
		return false
	}
	status := resp.GetWorkflow().GetStatus()
	return status == "paused" || status == "cancelled" || status == "completed" || status == "failed"
}

// --- Validation helpers ---

func validatePlanPath(path string) error {
	if path == "" {
		return fmt.Errorf("plan path is required")
	}
	if filepath.IsAbs(path) {
		return fmt.Errorf("plan path must be relative, got: %s", path)
	}
	if strings.Contains(path, "..") {
		return fmt.Errorf("plan path must not contain '..': %s", path)
	}
	return nil
}

func isNonActionableError(errStr string) bool {
	lower := strings.ToLower(errStr)
	return strings.Contains(lower, "timeout") ||
		strings.Contains(lower, "crash") ||
		strings.Contains(lower, "signal") ||
		strings.Contains(lower, "killed")
}

// --- Proto converters ---

func workflowToStatusInfo(w *bossanovav1.Workflow) *bossanovav1.WorkflowStatusInfo {
	if w == nil {
		return nil
	}
	return &bossanovav1.WorkflowStatusInfo{
		Id:          w.GetId(),
		Status:      workflowStatusFromString(w.GetStatus()),
		CurrentStep: workflowStepFromString(w.GetCurrentStep()),
		FlightLeg:   w.GetFlightLeg(),
		LastError:   w.GetLastError(),
	}
}

func workflowStatusFromString(s string) bossanovav1.WorkflowStatus {
	switch s {
	case "pending":
		return bossanovav1.WorkflowStatus_WORKFLOW_STATUS_PENDING
	case "running":
		return bossanovav1.WorkflowStatus_WORKFLOW_STATUS_RUNNING
	case "paused":
		return bossanovav1.WorkflowStatus_WORKFLOW_STATUS_PAUSED
	case "completed":
		return bossanovav1.WorkflowStatus_WORKFLOW_STATUS_COMPLETED
	case "failed":
		return bossanovav1.WorkflowStatus_WORKFLOW_STATUS_FAILED
	case "cancelled":
		return bossanovav1.WorkflowStatus_WORKFLOW_STATUS_CANCELLED
	default:
		return bossanovav1.WorkflowStatus_WORKFLOW_STATUS_UNSPECIFIED
	}
}

func workflowStepFromString(s string) bossanovav1.WorkflowStep {
	switch s {
	case "plan":
		return bossanovav1.WorkflowStep_WORKFLOW_STEP_PLAN
	case "implement":
		return bossanovav1.WorkflowStep_WORKFLOW_STEP_IMPLEMENT
	case "handoff":
		return bossanovav1.WorkflowStep_WORKFLOW_STEP_HANDOFF
	case "resume":
		return bossanovav1.WorkflowStep_WORKFLOW_STEP_RESUME
	case "verify":
		return bossanovav1.WorkflowStep_WORKFLOW_STEP_VERIFY
	case "land":
		return bossanovav1.WorkflowStep_WORKFLOW_STEP_LAND
	default:
		return bossanovav1.WorkflowStep_WORKFLOW_STEP_UNSPECIFIED
	}
}

func stringPtr(s string) *string {
	return &s
}
