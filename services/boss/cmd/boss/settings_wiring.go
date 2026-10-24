package main

import settingscli "github.com/recurser/boss/api/cli/settings"

func newSettingsGroup() *settingscli.Group {
	return settingscli.New(settingscli.Options{
		TelemetryClient:             commandTelemetryClient,
		CaptureAuthChangedWithEmail: captureAuthChangedWithEmail,
		TelemetryEnabled:            commandTelemetryEnabled,
		DistinctID:                  commandDistinctID,
	})
}
