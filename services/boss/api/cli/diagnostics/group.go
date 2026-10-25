package diagnostics

import (
	"context"

	"github.com/recurser/boss/api/cli/cliutil"
	daemonlife "github.com/recurser/boss/internal/daemonlife"

	"github.com/recurser/boss/internal/adapter/revisiondrift"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/kit/config"
	"github.com/recurser/bossalib/kit/telemetry"
	"github.com/spf13/cobra"
)

type Group struct {
	sessionCommand         func() *cobra.Command
	bossRevisionDriftProbe func(context.Context) revisiondrift.Drift
	captureRepairCompleted func(context.Context, telemetry.Client, string)
	captureRepairStarted   func(context.Context, telemetry.Client)
	cliCommandPaths        func() []string
	commandTelemetryClient func(*cobra.Command) telemetry.Client
	daemonSocketReachable  func(string) bool
	envRevisionDrift       func() revisiondrift.Drift
	newClient              func(*cobra.Command) (Client, error)
	osGetenv               func(key string) string
}

func defaultGroup() *Group {
	deps := &Group{}
	deps.osGetenv = func(key string) string { return config.EnvOr(key, "") }
	deps.envRevisionDrift = func() revisiondrift.Drift {
		return deps.bossRevisionDriftProbe(context.Background())
	}
	return deps
}

type Client interface {
	cliutil.SessionLister
	RepairDoctor(context.Context) (*pb.RepairDoctorResponse, error)
	StartRepairWorkflow(context.Context) (*pb.StartRepairWorkflowResponse, error)
}

// Options supplies runtime context without importing the process entry point.
type Options struct {
	CLICommandPaths        func() []string
	RevisionDrift          func(context.Context) revisiondrift.Drift
	TelemetryClient        func(*cobra.Command) telemetry.Client
	CaptureRepairStarted   func(context.Context, telemetry.Client)
	CaptureRepairCompleted func(context.Context, telemetry.Client, string)
	SessionCommand         func() *cobra.Command
}

func New(factory cliutil.ClientFactory[Client], opts Options) *Group {
	deps := defaultGroup()
	deps.newClient = factory
	deps.daemonSocketReachable = daemonlife.IsSocketReachable
	deps.cliCommandPaths = opts.CLICommandPaths
	if deps.cliCommandPaths == nil {
		deps.cliCommandPaths = func() []string { return nil }
	}
	deps.bossRevisionDriftProbe = opts.RevisionDrift
	if deps.bossRevisionDriftProbe == nil {
		deps.bossRevisionDriftProbe = func(context.Context) revisiondrift.Drift { return revisiondrift.Drift{} }
	}
	deps.commandTelemetryClient = opts.TelemetryClient
	if deps.commandTelemetryClient == nil {
		deps.commandTelemetryClient = func(*cobra.Command) telemetry.Client { return nil }
	}
	deps.captureRepairStarted = opts.CaptureRepairStarted
	if deps.captureRepairStarted == nil {
		deps.captureRepairStarted = func(context.Context, telemetry.Client) {}
	}
	deps.captureRepairCompleted = opts.CaptureRepairCompleted
	if deps.captureRepairCompleted == nil {
		deps.captureRepairCompleted = func(context.Context, telemetry.Client, string) {}
	}
	deps.sessionCommand = opts.SessionCommand
	return deps
}
func (deps *Group) Commands() []*cobra.Command {
	cmds := []*cobra.Command{deps.repairCmd()}
	if deps.sessionCommand != nil {
		cmds = append(cmds, deps.sessionCommand())
	}
	return append(cmds, deps.envCmd(), proofCmd(), fixTerminalCmd(), tailCmd(), deps.costCmd())
}
