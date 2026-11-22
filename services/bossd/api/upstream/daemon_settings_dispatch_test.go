package upstream

import (
	"context"
	"errors"
	"testing"

	"connectrpc.com/connect"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/kit/config"
	"github.com/rs/zerolog"
	"google.golang.org/protobuf/proto"
)

type fakeDaemonSettings struct {
	result *pb.DaemonSettingsResult
	err    error
	update *pb.DaemonSettingsUpdate
}

func (f *fakeDaemonSettings) GetDaemonSettings(context.Context) (*pb.DaemonSettingsResult, error) {
	return f.result, f.err
}

func (f *fakeDaemonSettings) UpdateDaemonSettings(_ context.Context, update *pb.DaemonSettingsUpdate) (*pb.DaemonSettingsResult, error) {
	f.update = update
	return f.result, f.err
}

func newDaemonSettingsClient(settings DaemonSettingsHandler, mode config.CloudControlMode) *StreamClient {
	return NewStreamClient(StreamClientConfig{
		CloudControl:   fixedCloudControl(mode),
		TargetObjects:  dispatcherTargetFixture{},
		DaemonSettings: settings,
		Logger:         zerolog.Nop(),
	})
}

func dispatchDaemonSettings(t *testing.T, client *StreamClient, cmd *pb.OrchestratorCommand) *pb.CommandResult {
	t.Helper()
	out := make(chan *pb.DaemonEvent, 1)
	if ev := client.dispatchCommand(context.Background(), cmd, out); ev != nil {
		return ev.GetResult()
	}
	return recvEvent(t, out).GetResult()
}

func getDaemonSettingsCommand() *pb.OrchestratorCommand {
	return &pb.OrchestratorCommand{CommandId: "get", Cmd: &pb.OrchestratorCommand_GetDaemonSettings{GetDaemonSettings: &pb.GetDaemonSettingsCommand{}}}
}

func updateDaemonSettingsCommand(update *pb.DaemonSettingsUpdate) *pb.OrchestratorCommand {
	return &pb.OrchestratorCommand{CommandId: "update", Cmd: &pb.OrchestratorCommand_UpdateDaemonSettings{
		UpdateDaemonSettings: &pb.UpdateDaemonSettingsCommand{Update: update},
	}}
}

func TestDispatch_DaemonSettings(t *testing.T) {
	reply := &pb.DaemonSettingsResult{Settings: &pb.DaemonSettings{DaemonName: "studio"}, IsRestartNeeded: true}
	fake := &fakeDaemonSettings{result: reply}
	client := newDaemonSettingsClient(fake, config.CloudControlFull)

	got := dispatchDaemonSettings(t, client, getDaemonSettingsCommand())
	if !got.GetOk() || got.GetCommandId() != "get" || !proto.Equal(got.GetDaemonSettings(), reply) {
		t.Fatalf("get result = %v, want the handler's settings", got)
	}

	update := &pb.DaemonSettingsUpdate{DaemonName: proto.String("studio")}
	got = dispatchDaemonSettings(t, client, updateDaemonSettingsCommand(update))
	if !got.GetOk() || got.GetCommandId() != "update" || !proto.Equal(got.GetDaemonSettings(), reply) {
		t.Fatalf("update result = %v, want the handler's settings and restart flag", got)
	}
	if !proto.Equal(fake.update, update) {
		t.Fatalf("handler got %v, want %v", fake.update, update)
	}
}

func TestDispatch_DaemonSettingsErrorCodes(t *testing.T) {
	for _, tc := range []struct {
		err  error
		want pb.CommandResult_ErrorCode
	}{
		{connect.NewError(connect.CodeInvalidArgument, errors.New("bad poll interval")), pb.CommandResult_ERROR_CODE_INVALID_ARGUMENT},
		{connect.NewError(connect.CodePermissionDenied, errors.New("bypass needs full")), pb.CommandResult_ERROR_CODE_CLOUD_CONTROL_DISABLED},
		{connect.NewError(connect.CodeFailedPrecondition, errors.New("x")), pb.CommandResult_ERROR_CODE_FAILED_PRECONDITION},
		{connect.NewError(connect.CodeInternal, errors.New("disk")), pb.CommandResult_ERROR_CODE_UNSPECIFIED},
	} {
		t.Run(tc.want.String(), func(t *testing.T) {
			client := newDaemonSettingsClient(&fakeDaemonSettings{err: tc.err}, config.CloudControlFull)
			for _, cmd := range []*pb.OrchestratorCommand{getDaemonSettingsCommand(), updateDaemonSettingsCommand(&pb.DaemonSettingsUpdate{})} {
				got := dispatchDaemonSettings(t, client, cmd)
				if got.GetOk() || got.GetErrorCode() != tc.want {
					t.Fatalf("%s result = %v, want error code %v", cmd.GetCommandId(), got, tc.want)
				}
			}
		})
	}
}

// TestDispatch_DaemonSettingsUnderRestrictedCloudControl: reading settings is
// metadata observation, so read-only and private machines answer it; changing
// them is control, refused before the handler runs.
func TestDispatch_DaemonSettingsUnderRestrictedCloudControl(t *testing.T) {
	for _, mode := range []config.CloudControlMode{config.CloudControlReadOnly, config.CloudControlPrivate} {
		t.Run(string(mode), func(t *testing.T) {
			fake := &fakeDaemonSettings{result: &pb.DaemonSettingsResult{Settings: &pb.DaemonSettings{}}}
			client := newDaemonSettingsClient(fake, mode)

			if got := dispatchDaemonSettings(t, client, getDaemonSettingsCommand()); !got.GetOk() {
				t.Fatalf("get = %v, want ok", got)
			}
			got := dispatchDaemonSettings(t, client, updateDaemonSettingsCommand(&pb.DaemonSettingsUpdate{DaemonName: proto.String("x")}))
			if got.GetOk() || got.GetErrorCode() != pb.CommandResult_ERROR_CODE_CLOUD_CONTROL_DISABLED {
				t.Fatalf("update = %v, want a cloud control refusal", got)
			}
			if fake.update != nil {
				t.Fatal("a refused update reached the handler")
			}
		})
	}
}

func TestCommandHandlerAdapter_DaemonSettingsForwardsToWiredServer(t *testing.T) {
	adapter := &CommandHandlerAdapter{}
	if _, err := adapter.GetDaemonSettings(context.Background()); err == nil {
		t.Fatal("unwired adapter answered get_daemon_settings")
	}
	reply := &pb.DaemonSettingsResult{Settings: &pb.DaemonSettings{DaemonName: "studio"}}
	fake := &fakeDaemonSettings{result: reply}
	adapter.Settings = fake
	update := &pb.DaemonSettingsUpdate{PollIntervalSeconds: proto.Int32(60)}
	got, err := adapter.UpdateDaemonSettings(context.Background(), update)
	if err != nil || got != reply || fake.update != update {
		t.Fatalf("update = %v, %v; forwarded %v; want the server's reply for the same update", got, err, fake.update)
	}
}
