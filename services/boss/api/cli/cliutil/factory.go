package cliutil

import "github.com/spf13/cobra"

// ClientFactory resolves command transport flags at execution time.
type ClientFactory[C any] func(*cobra.Command) (C, error)
