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
	distinctID := ""
	if email != "" {
		// The funnel namespace, not the retired user-<hash> one. The CLI never
		// holds a WorkOS sub — only the keychain email — so this is the email:
		// half of the funnel identity, which the bosso JIT user-creation
		// callback merges into user:<sub>.
		distinctID = telemetry.FunnelDistinctID("", email)
	}
	return commandTelemetryUser{
		email:      email,
		distinctID: distinctID,
	}
}

// distinctIDOrLocal is the id CLI events are captured on. It resolves through
// the one shared local-surface definition, so a CLI event and a TUI event for
// the same logged-in human are the same PostHog person by construction rather
// than by two call sites agreeing.
func (u commandTelemetryUser) distinctIDOrLocal() string {
	return telemetry.LocalFunnelDistinctID(userHomeDirOrEmpty(), u.email)
}

func identifyCommandUser(ctx context.Context, client telemetry.Client) {
	if client == nil || !commandTelemetryEnabled() {
		return
	}
	identifyCommandUserWithIdentity(ctx, client, commandTelemetryUserFromEmail(commandTelemetryEmailLookup()))
}

// identifyCommandUserWithIdentity writes person properties. It must target the
// same funnel id the events carry, or the properties land on a person nothing
// else ever writes to.
func identifyCommandUserWithIdentity(ctx context.Context, client telemetry.Client, user commandTelemetryUser) {
	if user.distinctID == "" {
		return
	}
	client.Identify(ctx, user.distinctID, map[string]any{"email": user.email, "source": "cli"})
}

// aliasLocalToCommandUserWithIdentity bridges this machine's pre-login identity
// into the funnel person at login. It is the same alias that already shipped,
// pointed at the funnel id instead of the retired user-<hash> one — a
// forward-only bridge, not a migration: no alias is written from a pre-change
// user-<hash> person, matching the precedent recorded in
// services/web/src/analytics/AuthAnalytics.tsx.
//
// It bridges the CLI login path only. This is the whole boss binary's only
// Client.Alias call site, so a login performed from inside the running TUI
// still flips views' distinct id to the funnel id with nothing bridging the
// pre-login local-<hash> person.
func aliasLocalToCommandUserWithIdentity(ctx context.Context, client telemetry.Client, user commandTelemetryUser) {
	if user.distinctID == "" {
		return
	}
	client.Alias(ctx, localDistinctID(), user.distinctID)
}

// localDistinctID is the pre-login per-machine identity. It survives the move
// to the funnel namespace because it is what the login-time alias bridges
// forward: without it, everything a human did before logging in would be
// stranded on a person no funnel ever reaches.
func localDistinctID() string {
	return telemetry.LocalDistinctID(userHomeDirOrEmpty())
}

func userHomeDirOrEmpty() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return home
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
