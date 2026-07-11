package server

import (
	"context"
	"errors"
	"strings"
	"testing"

	"connectrpc.com/connect"

	bossanovav1 "github.com/recurser/bossalib/gen/bossanova/v1"
)

// stubRepairProbe is a minimal repairWorkflowProbe recording GetWorkflowStatus
// calls and reporting a canned status/error.
type stubRepairProbe struct {
	status bossanovav1.WorkflowStatus
	err    error
	calls  int
}

func (s *stubRepairProbe) GetWorkflowStatus(_ context.Context, _ string) (*bossanovav1.WorkflowStatusInfo, error) {
	s.calls++
	if s.err != nil {
		return nil, s.err
	}
	return &bossanovav1.WorkflowStatusInfo{Status: s.status}, nil
}

func TestStartRepairWorkflow(t *testing.T) {
	desiredReq := &bossanovav1.StartWorkflowRequest{ConfigJson: `{"cooldown_minutes":5}`}

	cases := []struct {
		name string
		// probe is nil to simulate "no repair plugin loaded".
		probe *stubRepairProbe
		// loadErr, if set, makes loadDesiredReq fail.
		loadErr error
		// ensureErr, if set, makes ensureRunning fail.
		ensureErr error

		wantCode         connect.Code // 0 = expect success
		wantAlreadyRun   bool
		wantEnsureCalls  int
		wantDetailSubstr string
	}{
		{
			name:            "no_repair_plugin_failed_precondition",
			probe:           nil,
			wantCode:        connect.CodeFailedPrecondition,
			wantEnsureCalls: 0,
		},
		{
			name:             "running_already_running_no_ensure",
			probe:            &stubRepairProbe{status: bossanovav1.WorkflowStatus_WORKFLOW_STATUS_RUNNING},
			wantAlreadyRun:   true,
			wantEnsureCalls:  0,
			wantDetailSubstr: "already RUNNING",
		},
		{
			name:             "paused_untouched_no_ensure",
			probe:            &stubRepairProbe{status: bossanovav1.WorkflowStatus_WORKFLOW_STATUS_PAUSED},
			wantAlreadyRun:   false,
			wantEnsureCalls:  0,
			wantDetailSubstr: "operator",
		},
		{
			name:             "cancelled_rearms_once",
			probe:            &stubRepairProbe{status: bossanovav1.WorkflowStatus_WORKFLOW_STATUS_CANCELLED},
			wantAlreadyRun:   false,
			wantEnsureCalls:  1,
			wantDetailSubstr: "re-armed (was CANCELLED)",
		},
		{
			name:            "probe_error_internal",
			probe:           &stubRepairProbe{err: errors.New("probe boom")},
			wantCode:        connect.CodeInternal,
			wantEnsureCalls: 0,
		},
		{
			name:            "load_config_error_internal",
			probe:           &stubRepairProbe{status: bossanovav1.WorkflowStatus_WORKFLOW_STATUS_CANCELLED},
			loadErr:         errors.New("load boom"),
			wantCode:        connect.CodeInternal,
			wantEnsureCalls: 0,
		},
		{
			name:            "ensure_error_internal",
			probe:           &stubRepairProbe{status: bossanovav1.WorkflowStatus_WORKFLOW_STATUS_CANCELLED},
			ensureErr:       errors.New("ensure boom"),
			wantCode:        connect.CodeInternal,
			wantEnsureCalls: 1,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var ensureCalls int
			var setName string
			var setReq *bossanovav1.StartWorkflowRequest
			var ensureName string

			deps := startRepairWorkflowDeps{
				findRepair: func(_ context.Context) repairWorkflowProbe {
					if tc.probe == nil {
						return nil
					}
					return tc.probe
				},
				loadDesiredReq: func() (*bossanovav1.StartWorkflowRequest, error) {
					if tc.loadErr != nil {
						return nil, tc.loadErr
					}
					return desiredReq, nil
				},
				setDesired: func(name string, req *bossanovav1.StartWorkflowRequest) {
					setName = name
					setReq = req
				},
				ensureRunning: func(_ context.Context, name string) (bossanovav1.WorkflowStatus, bool, error) {
					ensureCalls++
					ensureName = name
					if tc.ensureErr != nil {
						return bossanovav1.WorkflowStatus_WORKFLOW_STATUS_UNSPECIFIED, false, tc.ensureErr
					}
					return bossanovav1.WorkflowStatus_WORKFLOW_STATUS_RUNNING, true, nil
				},
			}

			resp, err := startRepairWorkflow(context.Background(), deps)

			if tc.wantCode != 0 {
				if err == nil {
					t.Fatalf("expected error code %v, got nil (resp=%+v)", tc.wantCode, resp)
				}
				if got := connect.CodeOf(err); got != tc.wantCode {
					t.Fatalf("error code = %v, want %v (err=%v)", got, tc.wantCode, err)
				}
			} else {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				if resp.GetAlreadyRunning() != tc.wantAlreadyRun {
					t.Fatalf("already_running = %v, want %v", resp.GetAlreadyRunning(), tc.wantAlreadyRun)
				}
				if tc.wantDetailSubstr != "" && !strings.Contains(resp.GetDetail(), tc.wantDetailSubstr) {
					t.Fatalf("detail = %q, want substring %q", resp.GetDetail(), tc.wantDetailSubstr)
				}
			}

			if ensureCalls != tc.wantEnsureCalls {
				t.Fatalf("ensureRunning calls = %d, want %d", ensureCalls, tc.wantEnsureCalls)
			}
			// The rearm path sets desire before ensuring; verify the exact name +
			// request threaded through on the happy CANCELLED case.
			if tc.name == "cancelled_rearms_once" {
				if setName != "repair" {
					t.Fatalf("setDesired name = %q, want %q", setName, "repair")
				}
				if setReq != desiredReq {
					t.Fatalf("setDesired req pointer mismatch")
				}
				if ensureName != "repair" {
					t.Fatalf("ensureRunning name = %q, want %q", ensureName, "repair")
				}
			}
			if tc.probe != nil && tc.wantCode != connect.CodeFailedPrecondition && tc.probe.calls != 1 {
				t.Fatalf("GetWorkflowStatus calls = %d, want 1", tc.probe.calls)
			}
		})
	}
}
