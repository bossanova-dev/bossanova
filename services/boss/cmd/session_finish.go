package main

import (
	"context"
	"fmt"
	"strings"
	"time"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/spf13/cobra"

	"github.com/recurser/boss/internal/client"
)

// sessionFinishTimeout bounds the RPC. The daemon defers the requester's own
// finish, so the call returns as soon as the request is recorded; the bound only
// matters when the request joins a finalize already in flight.
const sessionFinishTimeout = 30 * time.Second

// sessionFinishJSON is the stable `boss session finish --json` envelope.
// outcome is "pending" when the daemon will finalize the run once the session
// settles, "finished" when it finalized synchronously.
type sessionFinishJSON struct {
	Outcome                string `json:"outcome"`
	BlockingAgentSessionID string `json:"blocking_agent_session_id"`
}

func sessionFinishCmd(connectClient func(*cobra.Command) (client.BossClient, error)) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "finish",
		Short: "Tell the daemon this unattended run is done",
		Long: "Tell the daemon an unattended run is done. The daemon finalizes the run " +
			"(records its outcome and cleans it up) once every chat is idle and the " +
			"session's agent output has gone quiet, instead of waiting for the idle sweep. " +
			"Run it from inside the session: the ids default to BOSS_SESSION_ID and " +
			"BOSS_AGENT_SESSION_ID.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			asJSON, _ := cmd.Flags().GetBool(jsonFlagName)
			sessionID, _ := cmd.Flags().GetString("session")
			if !cmd.Flags().Changed("session") {
				sessionID = osGetenv("BOSS_SESSION_ID")
			}
			sessionID = strings.TrimSpace(sessionID)
			if sessionID == "" {
				return emitJSONFailure(cmd, asJSON, fmt.Errorf("session id is required: use --session or BOSS_SESSION_ID"))
			}
			chatID, _ := cmd.Flags().GetString("chat")
			if !cmd.Flags().Changed("chat") {
				chatID = osGetenv("BOSS_AGENT_SESSION_ID")
			}
			chatID = strings.TrimSpace(chatID)
			if chatID == "" {
				return emitJSONFailure(cmd, asJSON, fmt.Errorf("chat id is required: use --chat or BOSS_AGENT_SESSION_ID"))
			}
			c, err := connectClient(cmd)
			if err != nil {
				return emitJSONFailure(cmd, asJSON, err)
			}
			ctx, cancel := context.WithTimeout(cmd.Context(), sessionFinishTimeout)
			defer cancel()
			resp, err := c.FinishUnattendedRun(ctx, &pb.FinishUnattendedRunRequest{
				SessionId:               sessionID,
				RequesterAgentSessionId: chatID,
			})
			if err != nil {
				return emitJSONFailure(cmd, asJSON, err)
			}
			out := sessionFinishJSON{Outcome: "finished"}
			if resp.GetIsDeferred() {
				out = sessionFinishJSON{Outcome: "pending", BlockingAgentSessionID: resp.GetBlockingAgentSessionId()}
			}
			if asJSON {
				return emitJSON(cmd, out)
			}
			if out.Outcome == "pending" {
				_, err = fmt.Fprintf(cmd.OutOrStdout(), "finish pending (waiting on %s)\n", out.BlockingAgentSessionID)
			} else {
				_, err = fmt.Fprintln(cmd.OutOrStdout(), "finished")
			}
			return err
		},
	}
	cmd.Flags().String("session", "", "Session id (default BOSS_SESSION_ID)")
	cmd.Flags().String("chat", "", "Requesting chat id (default BOSS_AGENT_SESSION_ID)")
	cmd.Flags().Bool(jsonFlagName, false, `Emit {"outcome","blocking_agent_session_id"} as JSON on stdout`)
	return cmd
}
