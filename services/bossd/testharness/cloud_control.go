package testharness

import (
	"context"

	"github.com/recurser/bossalib/kit/config"
)

// Realtime fixtures opt in to cloud control so their payloads drive session events.
type realtimeCloudControl struct{}

func (realtimeCloudControl) Mode() config.CloudControlMode { return config.CloudControlFull }

func (p realtimeCloudControl) RepoMode(context.Context, string) (config.CloudControlMode, config.CloudControlSource) {
	return p.Mode(), config.CloudControlSourceMachine
}
