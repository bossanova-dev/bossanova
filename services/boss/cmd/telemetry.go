package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"strings"

	"github.com/recurser/bossalib/config"
	"github.com/recurser/bossalib/telemetry"
	"github.com/spf13/cobra"
)

type executedCommandContextKey struct{}
type commandTelemetryContextKey struct{}

type executedCommandState struct {
	command *cobra.Command
}

func commandTelemetryConfig(s config.Settings) telemetry.Config {
	return telemetry.FromSettings(s, "boss")
}

func commandTelemetryProperties(commandPath string, _ []string) map[string]any {
	return map[string]any{
		"command": strings.TrimSpace(commandPath),
	}
}

func captureCommand(ctx context.Context, client telemetry.Client, cmd *cobra.Command, err error) {
	if client == nil || cmd == nil {
		return
	}
	if !commandTelemetryEnabled() {
		return
	}

	status := "success"
	if err != nil {
		status = "error"
	}
	props := commandTelemetryProperties(cmd.CommandPath(), nil)
	props["status"] = status
	client.Capture(ctx, telemetry.EventCLICommandInvoked, localDistinctID(), props)
}

func commandTelemetryClient(cmd *cobra.Command) telemetry.Client {
	if cmd == nil || cmd.Context() == nil {
		return nil
	}
	client, _ := cmd.Context().Value(commandTelemetryContextKey{}).(telemetry.Client)
	return client
}

func captureAuthChanged(ctx context.Context, client telemetry.Client, action string) {
	if client == nil {
		return
	}
	if !commandTelemetryEnabled() {
		return
	}
	client.Capture(ctx, telemetry.EventAuthChanged, localDistinctID(), map[string]any{
		"source": "cli",
		"action": action,
	})
}

func captureRepairStarted(ctx context.Context, client telemetry.Client) {
	if client == nil {
		return
	}
	if !commandTelemetryEnabled() {
		return
	}
	client.Capture(ctx, telemetry.EventRepairStarted, localDistinctID(), map[string]any{
		"source": "cli",
	})
}

func captureRepairCompleted(ctx context.Context, client telemetry.Client, status string) {
	if client == nil {
		return
	}
	if !commandTelemetryEnabled() {
		return
	}
	client.Capture(ctx, telemetry.EventRepairCompleted, localDistinctID(), map[string]any{
		"source": "cli",
		"status": status,
	})
}

func commandTelemetryEnabled() bool {
	settings, err := config.Load()
	return err == nil && settings.EventTracingEnabled
}

func localDistinctID() string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return "local:unknown"
	}
	hash := stableHash(home)
	if len(hash) < 16 {
		return "local:unknown"
	}
	return "local:" + hash[:16]
}

func stableHash(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}

func recordExecutedCommand(cmd *cobra.Command) {
	if cmd == nil {
		return
	}
	state, _ := cmd.Context().Value(executedCommandContextKey{}).(*executedCommandState)
	if state == nil {
		return
	}
	state.command = cmd
}

func executedCommand(ctx context.Context) *cobra.Command {
	if ctx == nil {
		return nil
	}
	state, _ := ctx.Value(executedCommandContextKey{}).(*executedCommandState)
	if state == nil {
		return nil
	}
	return state.command
}

func installExecutedCommandRecorder(root *cobra.Command) {
	if root == nil {
		return
	}
	existing := root.PersistentPreRunE
	root.PersistentPreRunE = func(cmd *cobra.Command, args []string) error {
		recordExecutedCommand(cmd)
		if existing != nil {
			return existing(cmd, args)
		}
		return nil
	}
}
