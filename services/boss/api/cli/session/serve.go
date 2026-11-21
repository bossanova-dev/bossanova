package session

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/recurser/boss/api/cli/cliutil"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/spf13/cobra"
)

// Serve RPCs are local-only; cloud clients do not offer this capability.
type sessionServeClient interface {
	StartSessionServe(context.Context, string) (*pb.SessionServe, error)
	StopSessionServe(context.Context, string) (*pb.SessionServe, error)
	GetSessionServe(context.Context, string) (*pb.SessionServe, error)
}

func (g *Group) sessionServeCmd() *cobra.Command {
	group := &cobra.Command{Use: "serve", Short: "Run the repo serve command and report its service links"}
	for _, operation := range []struct{ name, summary string }{
		{"start", "Start the repo serve command"},
		{"stop", "Stop the repo serve command"},
		{"status", "Show serve state and service links"},
	} {
		cmd := &cobra.Command{
			Use:   operation.name + " [session-id]",
			Short: operation.summary,
			Args:  cobra.MaximumNArgs(1),
			RunE:  func(cmd *cobra.Command, args []string) error { return g.runSessionServe(cmd, args, operation.name) },
		}
		cmd.Flags().Bool(cliutil.JSONFlagName, false, "Emit a stable JSON schema instead of text")
		if operation.name == "start" {
			cmd.Flags().Duration("wait", 0, "Wait for at least one service link (for example 30s)")
		}
		group.AddCommand(cmd)
	}
	return group
}
func (g *Group) runSessionServe(cmd *cobra.Command, args []string, operation string) error {
	asJSON, _ := cmd.Flags().GetBool(cliutil.JSONFlagName)
	id := strings.TrimSpace(g.Getenv("BOSS_SESSION_ID"))
	if len(args) > 0 {
		id = strings.TrimSpace(args[0])
	}
	if id == "" {
		return cliutil.EmitJSONFailure(cmd, asJSON, fmt.Errorf("session-id or BOSS_SESSION_ID is required"))
	}
	wait := time.Duration(0)
	if operation == "start" {
		wait, _ = cmd.Flags().GetDuration("wait")
	}
	if wait < 0 {
		return cliutil.EmitJSONFailure(cmd, asJSON, fmt.Errorf("--wait must not be negative"))
	}
	client, err := g.newClient(cmd)
	if err != nil {
		return cliutil.EmitJSONFailure(cmd, asJSON, err)
	}
	c, ok := client.(sessionServeClient)
	if !ok {
		return cliutil.EmitJSONFailure(cmd, asJSON, fmt.Errorf("serve commands require a local daemon"))
	}
	if len(args) > 0 {
		id, err = cliutil.ResolveSessionID(client, cmd.Context(), id)
		if err != nil {
			return cliutil.EmitJSONFailure(cmd, asJSON, err)
		}
	}
	var state *pb.SessionServe
	switch operation {
	case "start":
		state, err = c.StartSessionServe(cmd.Context(), id)
	case "stop":
		state, err = c.StopSessionServe(cmd.Context(), id)
	default:
		state, err = c.GetSessionServe(cmd.Context(), id)
	}
	if err == nil && wait > 0 && len(state.GetServiceLinks()) == 0 {
		state, err = waitForServe(cmd.Context(), c, id, wait)
	}
	if err != nil {
		return cliutil.EmitJSONFailure(cmd, asJSON, err)
	}
	return printSessionServe(cmd, id, state, asJSON)
}
func waitForServe(ctx context.Context, c sessionServeClient, id string, wait time.Duration) (*pb.SessionServe, error) {
	ctx, cancel := context.WithTimeout(ctx, wait)
	defer cancel()
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("waiting for service links: %w", ctx.Err())
		case <-ticker.C:
			state, err := c.GetSessionServe(ctx, id)
			if err != nil {
				return nil, err
			}
			if len(state.GetServiceLinks()) > 0 {
				return state, nil
			}
		}
	}
}
func printSessionServe(cmd *cobra.Command, id string, state *pb.SessionServe, asJSON bool) error {
	if asJSON {
		type serviceLinkJSON struct {
			Name string `json:"name"`
			URL  string `json:"url"`
		}
		links := make([]serviceLinkJSON, 0, len(state.GetServiceLinks()))
		for _, link := range state.GetServiceLinks() {
			links = append(links, serviceLinkJSON{Name: link.Name, URL: link.Url})
		}
		return cliutil.EmitJSON(cmd, struct {
			SessionID       string            `json:"session_id"`
			IsRunning       bool              `json:"is_running"`
			ServiceLinks    []serviceLinkJSON `json:"service_links"`
			LastError       string            `json:"last_error"`
			StartedAt       string            `json:"started_at"`
			TmuxSessionName string            `json:"tmux_session_name"`
		}{id, state.GetIsRunning(), links, state.GetLastError(), cliutil.Rfc3339OrEmpty(state.GetStartedAt()), state.GetTmuxSessionName()})
	}
	status := "stopped"
	if state.GetIsRunning() {
		status = "running"
	}
	out := cmd.OutOrStdout()
	if _, err := fmt.Fprintln(out, status); err != nil {
		return err
	}
	for _, link := range state.GetServiceLinks() {
		if _, err := fmt.Fprintf(out, "%s  %s\n", link.Name, link.Url); err != nil {
			return err
		}
	}
	if state.GetLastError() != "" {
		_, err := fmt.Fprintln(out, "last error:", state.GetLastError())
		return err
	}
	return nil
}
