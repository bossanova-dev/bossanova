package settings

import (
	"context"

	daemonrpc "github.com/recurser/boss/internal/adapter/daemonrpc"
	"github.com/recurser/boss/internal/auth"
	"github.com/recurser/bossalib/telemetry"
	"github.com/spf13/cobra"
)

// Options supplies command telemetry and the daemon and browser boundaries.
// Each group owns its dependencies, so constructing another root cannot change them.
type Options struct {
	TelemetryClient             func(*cobra.Command) telemetry.Client
	CaptureAuthChangedWithEmail func(context.Context, telemetry.Client, string, string)
	TelemetryEnabled            func() bool
	DistinctID                  func() string
	DaemonAuthSocketPath        func() (string, error)
	NewDaemonAuthNotifier       func(string) DaemonAuthNotifier
	OpenCloudCheckoutURL        func(string) error
}

type Group struct{ options Options }

func New(options Options) *Group {
	if options.TelemetryClient == nil {
		options.TelemetryClient = func(*cobra.Command) telemetry.Client { return nil }
	}
	if options.CaptureAuthChangedWithEmail == nil {
		options.CaptureAuthChangedWithEmail = func(context.Context, telemetry.Client, string, string) {}
	}
	if options.TelemetryEnabled == nil {
		options.TelemetryEnabled = func() bool { return false }
	}
	if options.DistinctID == nil {
		options.DistinctID = func() string { return "" }
	}
	if options.DaemonAuthSocketPath == nil {
		options.DaemonAuthSocketPath = daemonrpc.DefaultSocketPath
	}
	if options.NewDaemonAuthNotifier == nil {
		options.NewDaemonAuthNotifier = func(socket string) DaemonAuthNotifier { return daemonrpc.NewLocal(socket) }
	}
	if options.OpenCloudCheckoutURL == nil {
		options.OpenCloudCheckoutURL = auth.OpenBrowser
	}
	return &Group{options: options}
}
