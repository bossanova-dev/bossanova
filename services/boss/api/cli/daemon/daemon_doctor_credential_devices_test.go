package daemon

import (
	"bytes"
	"context"
	"errors"
	"os"
	"runtime"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/recurser/boss/internal/domain/rpcerr"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/spf13/cobra"
)

func TestDoctorCredentialDeviceReadiness_DaemonLifecycle_AC224801(t *testing.T) {
	for _, tc := range []struct {
		state       string
		probeErr    error
		absent      bool
		nilResponse bool
		unhealthy   bool
		want        string
	}{
		{state: "ready", want: "credential sharing: ok"},
		{state: "unavailable", want: "credential sharing: not configured (local-only daemon)"},
		{probeErr: rpcerr.New(rpcerr.FailedPrecondition, errors.New("device enrollment is not configured")), want: "credential sharing: not configured (local-only daemon)"},
		{probeErr: rpcerr.New(rpcerr.FailedPrecondition, errors.New("other precondition failed")), want: "credential sharing: unknown"},
		{state: "unknown", want: "credential sharing: unknown — readiness could not be determined"},
		{probeErr: connect.NewError(connect.CodeUnimplemented, errors.New("old daemon")), want: "credential sharing: unknown"},
		{probeErr: errors.New("secret local key failure"), want: "credential sharing: unknown — could not check readiness"},
		{nilResponse: true, want: "credential sharing: unknown"},
		{state: "future_state", want: "credential sharing: unknown"},
		{absent: true, want: "credential sharing: unknown"},
		{state: "revoked", unhealthy: true},
		{state: "unenrolled", unhealthy: true},
		{state: "enrollment_expired", unhealthy: true},
		{state: "enrollment_pending", unhealthy: true},
		{state: "bootstrap_unpinned", unhealthy: true},
	} {
		t.Run(tc.state+tc.want, func(t *testing.T) {
			deps := newTestGroup()
			output, cmd := deps.newLinuxDaemonDoctorRun(t)
			deps.stubRevisionDriftProbes(t, descendantRevisionDrift(), descendantRevisionDrift())
			calls := 0
			deps.credentialDeviceStatus = func(ctx context.Context) (*pb.GetAgentCredentialDeviceStatusResponse, error) {
				calls++
				if _, ok := ctx.Deadline(); !ok {
					t.Fatal("probe has no bounded deadline")
				}
				if tc.probeErr != nil {
					return nil, tc.probeErr
				}
				if tc.nilResponse {
					return nil, nil
				}
				if tc.absent {
					return &pb.GetAgentCredentialDeviceStatusResponse{}, nil
				}
				return &pb.GetAgentCredentialDeviceStatusResponse{Readiness: &pb.CredentialDeviceReadiness{
					State: tc.state,
					RemediationSteps: []*pb.CredentialDeviceRemediationStep{
						{MachineDaemonId: "approver", Command: "boss account devices approve delta --fingerprint ABCD"},
						{IsThisMachine: true, Command: "boss account devices approve bootstrap --fingerprint EFGH"},
					},
				}}, nil
			}
			err := deps.runDaemonDoctor(cmd)
			if tc.unhealthy {
				if !errors.Is(err, errDaemonDoctorUnhealthy) {
					t.Fatalf("error = %v, want unhealthy; output:\n%s", err, output)
				}
			} else if err != nil {
				t.Fatalf("error = %v; output:\n%s", err, output)
			}
			if strings.Contains(output.String(), "secret local key failure") {
				t.Fatal("probe error leaked into output")
			}
			if strings.Contains(output.String(), "Remediation:") {
				t.Fatalf("inline credential remedies produced an empty section:\n%s", output)
			}
			if calls != 1 {
				t.Fatalf("probe calls = %d, want 1", calls)
			}
			want := tc.want
			if tc.unhealthy {
				want = "credential sharing: " + tc.state + " — synced Codex accounts are unusable on this machine\n" +
					"  on approver: boss account devices approve delta --fingerprint ABCD\n" +
					"  on this machine: boss account devices approve bootstrap --fingerprint EFGH\n"
			}
			if !strings.Contains(output.String(), want) {
				t.Fatalf("missing %q in output:\n%s", want, output)
			}
		})
	}
}

func TestDoctorCredentialDeviceProbePreservesCancellation(t *testing.T) {
	deps := newTestGroup()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	deps.credentialDeviceStatus = func(ctx context.Context) (*pb.GetAgentCredentialDeviceStatusResponse, error) {
		if !errors.Is(ctx.Err(), context.Canceled) {
			t.Fatalf("probe context error = %v, want canceled", ctx.Err())
		}
		return nil, ctx.Err()
	}
	var output bytes.Buffer
	if deps.reportDaemonCredentialDevices(ctx, &output, &cobra.Command{}) {
		t.Fatal("canceled probe failed doctor")
	}
}

func TestDoctorCredentialDeviceStatusUsesCommandFactory(t *testing.T) {
	cmd := &cobra.Command{}
	deps := New(func(got *cobra.Command) (Client, error) {
		if got != cmd {
			t.Fatal("factory received a different command")
		}
		return authStateFake{}, nil
	})
	resp, err := deps.probeCredentialDeviceStatus(context.Background(), cmd)
	if err != nil || resp == nil {
		t.Fatalf("probe = %v, %v; want response", resp, err)
	}
}

func TestDoctorCredentialDeviceRemediationCommands(t *testing.T) {
	longCommand := "boss account devices approve " + strings.Repeat("d", 128) + " --fingerprint ABCD-EFGH-IJKL-MNOP"
	for _, tc := range []struct{ command, want string }{
		{longCommand, longCommand},
		{"boss\taccount\ndevices approve delta\u202e --fingerprint ABCD", "boss account devices approve delta --fingerprint ABCD"},
	} {
		t.Run(tc.command, func(t *testing.T) {
			deps := newTestGroup()
			deps.credentialDeviceStatus = func(context.Context) (*pb.GetAgentCredentialDeviceStatusResponse, error) {
				return &pb.GetAgentCredentialDeviceStatusResponse{Readiness: &pb.CredentialDeviceReadiness{
					State: "enrollment_pending",
					RemediationSteps: []*pb.CredentialDeviceRemediationStep{
						{IsThisMachine: true, Command: tc.command},
					},
				}}, nil
			}
			var output bytes.Buffer
			if !deps.reportDaemonCredentialDevices(context.Background(), &output, &cobra.Command{}) {
				t.Fatal("blocked readiness did not fail doctor")
			}
			if !strings.Contains(output.String(), "  on this machine: "+tc.want+"\n") {
				t.Fatalf("remediation was altered: %q", output.String())
			}
		})
	}
}

func TestDoctorCredentialDeviceInlineRemediationOnDarwin(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("LaunchAgent fixture requires macOS")
	}
	deps := newTestGroup()
	home, _, stagedPath := deps.prepareDaemonDoctorInstall(t)
	writeDaemonDoctorPlist(t, home, stagedPath)
	staged := time.Now().Add(-time.Hour)
	if err := os.Chtimes(stagedPath, staged, staged); err != nil {
		t.Fatal(err)
	}
	deps.writeDaemonDoctorStateStartedAt(t, stagedPath, time.Now())
	deps.credentialDeviceStatus = func(context.Context) (*pb.GetAgentCredentialDeviceStatusResponse, error) {
		return &pb.GetAgentCredentialDeviceStatusResponse{Readiness: &pb.CredentialDeviceReadiness{State: "enrollment_expired", RemediationSteps: []*pb.CredentialDeviceRemediationStep{{IsThisMachine: true, Command: "boss account devices request"}}}}, nil
	}
	var output bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetOut(&output)
	if err := deps.runDaemonDoctor(cmd); !errors.Is(err, errDaemonDoctorUnhealthy) {
		t.Fatalf("error = %v, want unhealthy; output:\n%s", err, &output)
	}
	if !strings.Contains(output.String(), "on this machine: boss account devices request") || strings.Contains(output.String(), "Remediation:") {
		t.Fatalf("inline credential remediation produced an empty section:\n%s", &output)
	}
}
