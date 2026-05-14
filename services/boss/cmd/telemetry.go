package main

import (
	"context"
	"os"
	"strings"

	"github.com/recurser/boss/internal/auth"
	"github.com/recurser/bossalib/config"
	"github.com/recurser/bossalib/telemetry"
	"github.com/spf13/cobra"
)

type executedCommandContextKey struct{}
type commandTelemetryContextKey struct{}

type executedCommandState struct {
	command *cobra.Command
}

type commandTelemetryUser struct {
	email      string
	distinctID string
}

var commandTelemetryEmailLookup = commandTelemetryEmail

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
	client.Capture(ctx, telemetry.EventCLICommandInvoked, commandDistinctID(), props)
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
	user := commandTelemetryUserFromEmail(commandTelemetryEmailLookup())
	captureAuthChangedForUser(ctx, client, action, user)
}

func captureAuthChangedWithEmail(ctx context.Context, client telemetry.Client, action, email string) {
	if client == nil {
		return
	}
	if !commandTelemetryEnabled() {
		return
	}
	captureAuthChangedForUser(ctx, client, action, commandTelemetryUserFromEmail(email))
}

func captureAuthChangedForUser(ctx context.Context, client telemetry.Client, action string, user commandTelemetryUser) {
	if action == "login" {
		identifyCommandUserWithIdentity(ctx, client, user)
		aliasLocalToCommandUserWithIdentity(ctx, client, user)
	}
	client.Capture(ctx, telemetry.EventAuthChanged, user.distinctIDOrLocal(), map[string]any{
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
	client.Capture(ctx, telemetry.EventRepairStarted, commandDistinctID(), map[string]any{
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
	client.Capture(ctx, telemetry.EventRepairCompleted, commandDistinctID(), map[string]any{
		"source": "cli",
		"status": status,
	})
}

func commandTelemetryEnabled() bool {
	settings, err := config.Load()
	return err == nil && settings.EventTracingEnabled
}

func commandDistinctID() string {
	return commandTelemetryUserFromEmail(commandTelemetryEmailLookup()).distinctIDOrLocal()
}

func commandTelemetryEmail() string {
	store, err := auth.NewKeychainStore(true)
	if err != nil {
		return ""
	}
	manager := auth.NewManager(store, auth.Config{ClientID: ""})
	status := manager.Status()
	if status == nil || !status.LoggedIn {
		return ""
	}
	return strings.TrimSpace(status.Email)
}

func commandTelemetryUserFromEmail(email string) commandTelemetryUser {
	email = strings.TrimSpace(email)
	return commandTelemetryUser{
		email:      email,
		distinctID: telemetry.UserDistinctID(email),
	}
}

func (u commandTelemetryUser) distinctIDOrLocal() string {
	if u.distinctID != "" {
		return u.distinctID
	}
	return localDistinctID()
}

func identifyCommandUser(ctx context.Context, client telemetry.Client) {
	if client == nil || !commandTelemetryEnabled() {
		return
	}
	identifyCommandUserWithIdentity(ctx, client, commandTelemetryUserFromEmail(commandTelemetryEmailLookup()))
}

func identifyCommandUserWithIdentity(ctx context.Context, client telemetry.Client, user commandTelemetryUser) {
	if user.distinctID == "" {
		return
	}
	client.Identify(ctx, user.distinctID, map[string]any{"email": user.email, "source": "cli"})
}

func aliasLocalToCommandUserWithIdentity(ctx context.Context, client telemetry.Client, user commandTelemetryUser) {
	if user.distinctID == "" {
		return
	}
	client.Alias(ctx, localDistinctID(), user.distinctID)
}

func localDistinctID() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return telemetry.LocalDistinctID("")
	}
	return telemetry.LocalDistinctID(home)
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
