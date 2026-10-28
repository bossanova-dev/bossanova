//go:build e2e

package main

import (
	"encoding/json"
	"fmt"

	"github.com/recurser/boss/api/tui/ui"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/spf13/cobra"
)

func addProofCommands(root *cobra.Command) { root.AddCommand(proofPaletteCmd()) }

func proofPaletteCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use: "__proof-palette", Hidden: true, Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			palette := make(map[string]string)
			for name, intent := range map[string]pb.DisplayIntent{
				"info":    pb.DisplayIntent_DISPLAY_INTENT_INFO,
				"success": pb.DisplayIntent_DISPLAY_INTENT_SUCCESS,
				"warning": pb.DisplayIntent_DISPLAY_INTENT_WARNING,
				"danger":  pb.DisplayIntent_DISPLAY_INTENT_DANGER,
				"muted":   pb.DisplayIntent_DISPLAY_INTENT_MUTED,
			} {
				r, g, b, _ := ui.StyleForIntent(intent).GetForeground().RGBA()
				palette[name] = fmt.Sprintf("#%02x%02x%02x", r>>8, g>>8, b>>8)
			}
			return json.NewEncoder(cmd.OutOrStdout()).Encode(palette)
		},
	}
	cmd.Flags().Bool("json", false, "Print intent colours as JSON")
	return cmd
}
