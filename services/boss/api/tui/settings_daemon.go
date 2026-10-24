package tui

import (
	"github.com/recurser/boss/api/tui/settings"
	daemonrpc "github.com/recurser/boss/internal/adapter/daemonrpc"
	daemon "github.com/recurser/boss/internal/daemonlife"
	"github.com/recurser/bossalib/contract/daemon/socketauth"
)

type settingsMCPTokenManager struct{}

func (settingsMCPTokenManager) LoadMCPToken() (string, error) {
	socketPath, err := daemonrpc.DefaultSocketPath()
	if err != nil {
		return "", err
	}
	return socketauth.LoadOrCreateHTTPToken(socketPath)
}
func (settingsMCPTokenManager) RotateMCPToken() (settings.MCPTokenRotation, error) {
	socketPath, err := daemonrpc.DefaultSocketPath()
	if err != nil {
		return settings.MCPTokenRotation{}, err
	}
	result, err := daemon.RotateMCPHTTPToken(socketPath, daemon.McpGetStatus, daemon.McpStop, daemon.McpStart)
	return settings.MCPTokenRotation{Token: result.Token, Restarted: result.Restarted, ManualRestartRequired: result.ManualRestartRequired}, err
}
