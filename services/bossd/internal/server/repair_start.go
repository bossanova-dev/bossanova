package server

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"connectrpc.com/connect"

	"github.com/recurser/bossalib/config"
	bossanovav1 "github.com/recurser/bossalib/gen/bossanova/v1"
)

// repairWorkflowProbe is the narrow slice of the repair plugin's WorkflowService
// that StartRepairWorkflow needs: read the current workflow status before
// deciding whether a re-arm is warranted.
type repairWorkflowProbe interface {
	GetWorkflowStatus(ctx context.Context, workflowID string) (*bossanovav1.WorkflowStatusInfo, error)
}

// startRepairWorkflowDeps are the func-typed seams StartRepairWorkflow depends
// on, so the branching logic unit-tests without a real plugin.Host (mirrors the
// RepairDoctor helper/test split). findRepair returns nil when no repair plugin
// is loaded.
type startRepairWorkflowDeps struct {
	findRepair     func(ctx context.Context) repairWorkflowProbe
	loadDesiredReq func() (*bossanovav1.StartWorkflowRequest, error)
	setDesired     func(name string, req *bossanovav1.StartWorkflowRequest)
	ensureRunning  func(ctx context.Context, name string) (bossanovav1.WorkflowStatus, bool, error)
}

// StartRepairWorkflow (re-)arms the auto-repair workflow so operators can
// recover from a stopped/zombie repair plugin without restarting bossd. It is
// the manual sibling of the boot arm path and the health-tick watchdog — all
// three converge on the host's EnsureWorkflowRunning single-flight primitive.
func (s *Server) StartRepairWorkflow(ctx context.Context, _ *connect.Request[bossanovav1.StartRepairWorkflowRequest]) (*connect.Response[bossanovav1.StartRepairWorkflowResponse], error) {
	if s.pluginHost == nil {
		return nil, connect.NewError(connect.CodeFailedPrecondition,
			fmt.Errorf("no repair plugin loaded — install bossd-plugin-repair and restart bossd"))
	}
	resp, err := startRepairWorkflow(ctx, startRepairWorkflowDeps{
		findRepair: func(ctx context.Context) repairWorkflowProbe {
			svc := s.findRepairWorkflow(ctx)
			if svc == nil {
				return nil
			}
			return svc
		},
		loadDesiredReq: loadRepairDesiredReq,
		setDesired:     s.pluginHost.SetWorkflowDesired,
		ensureRunning:  s.pluginHost.EnsureWorkflowRunning,
	})
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(resp), nil
}

// startRepairWorkflow contains the pure branching logic behind the RPC. RUNNING
// is reported back as already_running (never re-issuing StartWorkflow, whose
// re-entry cancels in-flight repairs); PAUSED is operator intent and left alone;
// any non-serving status (CANCELLED/UNSPECIFIED/…) is re-armed from freshly
// loaded settings via SetWorkflowDesired + EnsureWorkflowRunning.
func startRepairWorkflow(ctx context.Context, deps startRepairWorkflowDeps) (*bossanovav1.StartRepairWorkflowResponse, error) {
	svc := deps.findRepair(ctx)
	if svc == nil {
		return nil, connect.NewError(connect.CodeFailedPrecondition,
			fmt.Errorf("no repair plugin loaded — install bossd-plugin-repair and restart bossd"))
	}
	info, err := svc.GetWorkflowStatus(ctx, "")
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal,
			fmt.Errorf("probe repair workflow status: %w", err))
	}
	status := info.GetStatus()
	switch status {
	case bossanovav1.WorkflowStatus_WORKFLOW_STATUS_RUNNING:
		return &bossanovav1.StartRepairWorkflowResponse{
			AlreadyRunning: true,
			Detail:         "repair workflow already RUNNING; StartWorkflow not re-issued (re-entry cancels in-flight repairs)",
		}, nil
	case bossanovav1.WorkflowStatus_WORKFLOW_STATUS_PAUSED:
		return &bossanovav1.StartRepairWorkflowResponse{
			AlreadyRunning: false,
			Detail:         "repair workflow is PAUSED; a paused workflow is resumed by the operator, not re-armed by boss repair start",
		}, nil
	default:
		// UNSPECIFIED / PENDING / COMPLETED / FAILED / CANCELLED: the workflow
		// is not actively serving, so fall through to the re-arm path below.
	}

	req, err := deps.loadDesiredReq()
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal,
			fmt.Errorf("load repair settings: %w", err))
	}
	// Declare desired state FIRST so the watchdog owns re-arming even if the
	// synchronous ensure below fails transiently — same ordering as boot.
	deps.setDesired("repair", req)
	if _, _, err := deps.ensureRunning(ctx, "repair"); err != nil {
		return nil, connect.NewError(connect.CodeInternal,
			fmt.Errorf("ensure repair workflow running: %w", err))
	}
	return &bossanovav1.StartRepairWorkflowResponse{
		AlreadyRunning: false,
		Detail:         fmt.Sprintf("repair workflow re-armed (was %s)", shortWorkflowStatus(status)),
	}, nil
}

// loadRepairDesiredReq loads current daemon settings and marshals the repair
// section into a StartWorkflowRequest, so a re-arm applies any edits the
// operator made since boot (mirrors the boot auto-start marshal).
func loadRepairDesiredReq() (*bossanovav1.StartWorkflowRequest, error) {
	settings, err := config.Load()
	if err != nil {
		return nil, fmt.Errorf("load config: %w", err)
	}
	repairCfgJSON, err := json.Marshal(settings.Repair)
	if err != nil {
		return nil, fmt.Errorf("marshal repair settings: %w", err)
	}
	return &bossanovav1.StartWorkflowRequest{ConfigJson: string(repairCfgJSON)}, nil
}

// shortWorkflowStatus trims the WORKFLOW_STATUS_ prefix so detail strings read
// "was CANCELLED" rather than "was WORKFLOW_STATUS_CANCELLED".
func shortWorkflowStatus(s bossanovav1.WorkflowStatus) string {
	return strings.TrimPrefix(s.String(), "WORKFLOW_STATUS_")
}
