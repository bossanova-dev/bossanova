package taskorchestrator

import (
	"context"

	"github.com/recurser/bossd/internal/ghusage"
)

// withMergeLabel attributes the GitHub calls of a task auto-merge to
// daemon:merge in the usage ledger.
func withMergeLabel(ctx context.Context) context.Context {
	return ghusage.WithLabel(ctx, ghusage.LabelMerge)
}
