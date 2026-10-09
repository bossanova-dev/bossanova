package main

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/recurser/boss/internal/client"
	"github.com/recurser/bossalib/displaystatus"
	"github.com/spf13/cobra"
)

func sessionPhaseCmd(connectClient func(*cobra.Command) (client.BossClient, error)) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "phase [<name>]",
		Short: "Report the current working phase of a chat",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			clear, _ := cmd.Flags().GetBool("clear")
			if clear == (len(args) == 1) {
				return fmt.Errorf("exactly one of a phase name or --clear is required")
			}
			phase := ""
			if !clear {
				var err error
				phase, err = displaystatus.NormalizePhase(args[0])
				if err != nil {
					return err
				}
			}
			chatID, _ := cmd.Flags().GetString("chat")
			if !cmd.Flags().Changed("chat") {
				chatID = osGetenv("BOSS_AGENT_SESSION_ID")
			}
			chatID = strings.TrimSpace(chatID)
			if chatID == "" {
				return fmt.Errorf("chat id is required: use --chat or BOSS_AGENT_SESSION_ID")
			}
			sessionID, _ := cmd.Flags().GetString("session")
			if !cmd.Flags().Changed("session") {
				sessionID = osGetenv("BOSS_SESSION_ID")
			}
			sessionID = strings.TrimSpace(sessionID)
			c, err := connectClient(cmd)
			if err != nil {
				return err
			}
			ctx, cancel := context.WithTimeout(cmd.Context(), 10*time.Second)
			defer cancel()
			if err := c.SetChatPhase(ctx, sessionID, chatID, phase); err != nil {
				return err
			}
			if clear {
				_, err = fmt.Fprintln(cmd.OutOrStdout(), "phase cleared")
			} else {
				_, err = fmt.Fprintf(cmd.OutOrStdout(), "phase set: %s\n", phase)
			}
			return err
		},
	}
	cmd.Flags().Bool("clear", false, "Clear the current working phase")
	cmd.Flags().String("session", "", "Session id (default BOSS_SESSION_ID)")
	cmd.Flags().String("chat", "", "Chat id (default BOSS_AGENT_SESSION_ID)")
	return cmd
}
