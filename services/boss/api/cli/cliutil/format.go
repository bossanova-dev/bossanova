package cliutil

import (
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func Rfc3339OrEmpty(ts *timestamppb.Timestamp) string {
	if ts == nil || !ts.IsValid() || ts.AsTime().IsZero() {
		return ""
	}
	return ts.AsTime().UTC().Format(time.RFC3339)
}

func OrDash(s string) string {
	if strings.TrimSpace(s) == "" {
		return "-"
	}
	return s
}

func ReadPromptFlag(cmd *cobra.Command, required bool) (string, bool, error) {
	promptSet := cmd.Flags().Changed("prompt")
	fileSet := cmd.Flags().Changed("prompt-file")

	if promptSet && fileSet {
		return "", false, fmt.Errorf("cannot use both --prompt and --prompt-file")
	}
	if !promptSet && !fileSet {
		if required {
			return "", false, fmt.Errorf("one of --prompt or --prompt-file is required")
		}
		return "", false, nil
	}

	if promptSet {
		v, _ := cmd.Flags().GetString("prompt")
		return v, true, nil
	}

	path, _ := cmd.Flags().GetString("prompt-file")
	if path == "-" {
		b, err := io.ReadAll(cmd.InOrStdin())
		if err != nil {
			return "", false, fmt.Errorf("read prompt from stdin: %w", err)
		}
		return string(b), true, nil
	}
	// #nosec G304 -- operator-supplied --prompt-file path; trusted runtime value, holds no secret.
	// owner=@recurser review-by=2027-01-18 issue=BOS-28
	b, err := os.ReadFile(path)
	if err != nil {
		return "", false, fmt.Errorf("read prompt file: %w", err)
	}
	return string(b), true, nil
}

// BoolLabel formats a boolean for human-readable command tables.
func BoolLabel(b bool) string {
	if b {
		return "yes"
	}
	return "no"
}
