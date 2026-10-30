package realstackrpc

import (
	"context"
	"errors"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/recurser/boss/internal/domain/rpcerr"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

type scenarioOperationsWire struct {
	Client
	mergeID, archiveID string
	head, mergePin     string
	readFailure        error
	report             *pb.ChatStatusReport
	failure            error
}

func (w *scenarioOperationsWire) GetSession(_ context.Context, _ *connect.Request[pb.GetSessionRequest]) (*connect.Response[pb.GetSessionResponse], error) {
	return connect.NewResponse(&pb.GetSessionResponse{Session: &pb.Session{PrDisplayHeadSha: w.head}}), w.readFailure
}
func (w *scenarioOperationsWire) MergeSession(_ context.Context, r *connect.Request[pb.MergeSessionRequest]) (*connect.Response[pb.MergeSessionResponse], error) {
	w.mergeID = r.Msg.Id
	w.mergePin = r.Msg.ExpectedHeadSha
	return connect.NewResponse(&pb.MergeSessionResponse{}), w.failure
}
func (w *scenarioOperationsWire) ArchiveSession(_ context.Context, r *connect.Request[pb.ArchiveSessionRequest]) (*connect.Response[pb.ArchiveSessionResponse], error) {
	w.archiveID = r.Msg.Id
	return connect.NewResponse(&pb.ArchiveSessionResponse{IsDeferred: true}), w.failure
}
func (w *scenarioOperationsWire) ReportChatStatus(_ context.Context, r *connect.Request[pb.ReportChatStatusRequest]) (*connect.Response[pb.ReportChatStatusResponse], error) {
	w.report = r.Msg.Reports[0]
	return connect.NewResponse(&pb.ReportChatStatusResponse{}), w.failure
}
func TestOperationsAdaptDaemonInputsAndRefusals(t *testing.T) {
	wire := &scenarioOperationsWire{head: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}
	client := Operations{Client: wire}
	stamp := time.Date(2026, 10, 26, 0, 0, 0, 0, time.UTC)
	if err := client.MergeSession(t.Context(), "merge"); err != nil || wire.mergeID != "merge" || wire.mergePin != wire.head {
		t.Fatal("merge", err, wire.mergeID)
	}
	if pending, err := client.ArchiveSession(t.Context(), "archive"); err != nil || !pending || wire.archiveID != "archive" {
		t.Fatal("archive", pending, err)
	}
	if err := client.ReportChatStatus(t.Context(), "chat", pb.ChatStatus_CHAT_STATUS_WORKING, stamp); err != nil {
		t.Fatal(err)
	}
	if wire.report.AgentSessionId != "chat" || wire.report.Status != pb.ChatStatus_CHAT_STATUS_WORKING || !wire.report.LastOutputAt.AsTime().Equal(stamp) {
		t.Fatal(wire.report)
	}
	wire.failure = connect.NewError(connect.CodeFailedPrecondition, errors.New("merge blocked"))
	if err := client.MergeSession(t.Context(), "merge"); rpcerr.KindOf(err) != rpcerr.FailedPrecondition || rpcerr.Message(err) != "merge blocked" {
		t.Fatal("refusal not classified", err)
	}
	if pending, err := client.ArchiveSession(t.Context(), "archive"); pending || rpcerr.KindOf(err) != rpcerr.FailedPrecondition {
		t.Fatal("archive refusal", pending, err)
	}
	if err := client.ReportChatStatus(t.Context(), "chat", pb.ChatStatus_CHAT_STATUS_IDLE, stamp); rpcerr.KindOf(err) != rpcerr.FailedPrecondition {
		t.Fatal("heartbeat refusal", err)
	}
}

func TestMergeSessionRequiresObservedHead(t *testing.T) {
	wire := &scenarioOperationsWire{}
	client := Operations{Client: wire}
	if err := client.MergeSession(t.Context(), "merge"); rpcerr.KindOf(err) != rpcerr.FailedPrecondition || wire.mergeID != "" {
		t.Fatal("missing head did not refuse before merge", err, wire.mergeID)
	}
	wire.readFailure = connect.NewError(connect.CodeUnavailable, errors.New("daemon unavailable"))
	if err := client.MergeSession(t.Context(), "merge"); rpcerr.KindOf(err) != rpcerr.Unavailable || wire.mergeID != "" {
		t.Fatal("read failure did not prevent merge", err, wire.mergeID)
	}
}
