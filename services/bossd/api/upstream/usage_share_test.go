package upstream

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossd/internal/domain"
	"github.com/rs/zerolog"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

type sharedUsageCapture struct {
	accountID string
	poller    bool
	until     time.Time
	snapshot  domain.UsageSnapshot
	err       error
	calls     int
}

func (s *sharedUsageCapture) Set(id string, poller bool, until time.Time) {
	s.accountID, s.poller, s.until = id, poller, until
}
func (s *sharedUsageCapture) RecordUsageProbe(_ context.Context, id string, snapshot domain.UsageSnapshot) error {
	s.accountID, s.snapshot = id, snapshot
	s.calls++
	return s.err
}

func TestApplySharedUsagePreservesReading_Accounts_AC15(t *testing.T) {
	fetched := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	reset := fetched.Add(time.Hour)
	until := fetched.Add(6 * time.Minute)
	capture := &sharedUsageCapture{}
	client := NewStreamClient(StreamClientConfig{UsageShare: capture, UsageRecorder: capture, Logger: zerolog.Nop()})
	command := &pb.OrchestratorCommand{CommandId: "shared", Cmd: &pb.OrchestratorCommand_ApplyAgentAccountUsage{ApplyAgentAccountUsage: &pb.ApplyAgentAccountUsageCommand{
		LocalAgentAccountId: "local", IsUsagePoller: true, ThrottledUntilAt: timestamppb.New(until),
		Usage: &pb.UsageSnapshot{Util_5H: .4, Util_7D: .7, Reset_5H: timestamppb.New(reset), Status: "active", PlanTier: "max", FetchedAt: timestamppb.New(fetched), ModelWindows: []*pb.UsageModelWindow{{ModelName: "sonnet", Util: .8, ResetsAt: timestamppb.New(reset)}}},
	}}}
	result := client.dispatchCommand(context.Background(), command, nil).GetResult()
	if !result.GetOk() || result.GetCommandId() != "shared" {
		t.Fatalf("result = %v", result)
	}
	expected := domain.UsageSnapshot{Util5h: .4, Util7d: .7, Reset5h: &reset, Status: "active", PlanTier: "max", FetchedAt: &fetched, ModelWindows: []domain.UsageModelWindow{{ModelName: "sonnet", Util: .8, ResetsAt: &reset}}}
	if !reflect.DeepEqual(capture.snapshot, expected) {
		t.Fatalf("snapshot = %#v, want %#v", capture.snapshot, expected)
	}
	if capture.accountID != "local" || !capture.poller || !capture.until.Equal(until) {
		t.Fatalf("role/hint = %#v", capture)
	}
}

func TestApplySharedUsageRoleOnlyAndStoreFailure_Accounts_AC15(t *testing.T) {
	capture := &sharedUsageCapture{}
	client := NewStreamClient(StreamClientConfig{UsageShare: capture, UsageRecorder: capture, Logger: zerolog.Nop()})
	req := &pb.ApplyAgentAccountUsageCommand{LocalAgentAccountId: "local"}
	if !client.dispatchApplyAgentAccountUsage(context.Background(), "role", req).GetResult().GetOk() || capture.calls != 0 {
		t.Fatal("role-only update wrote a snapshot")
	}
	capture.err = errors.New("write failed")
	req.Usage = &pb.UsageSnapshot{}
	if client.dispatchApplyAgentAccountUsage(context.Background(), "failure", req).GetResult().GetOk() {
		t.Fatal("store failure acknowledged")
	}
}

func TestForwardSharedUsageReport_Accounts_AC15(t *testing.T) {
	report := &pb.AgentAccountUsageReport{LocalAgentAccountId: "local", Usage: &pb.UsageSnapshot{FetchedAt: timestamppb.New(time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC))}, ThrottledUntilAt: timestamppb.New(time.Date(2026, 1, 2, 1, 0, 0, 0, time.UTC))}
	client := NewStreamClient(StreamClientConfig{Logger: zerolog.Nop()})
	outbound := make(chan *pb.DaemonEvent, 1)
	client.forwardEvent(context.Background(), StreamEvent{AgentAccountUsageReport: report}, outbound, nil)
	select {
	case event := <-outbound:
		if !proto.Equal(event.GetAgentAccountUsageReport(), report) {
			t.Fatalf("report changed: %v", event)
		}
	default:
		t.Fatal("report was not forwarded")
	}
}
