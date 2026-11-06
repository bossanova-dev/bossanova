package testharness

import "github.com/recurser/bossalib/kit/config"

// Realtime fixtures opt in to cloud control so their payloads drive session events.
type realtimeCloudControl struct{}

func (realtimeCloudControl) Mode() config.CloudControlMode { return config.CloudControlFull }
