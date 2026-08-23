// Package switchdispatchtest exposes bossd's internal switch dispatcher to
// cross-service integration tests.
package switchdispatchtest

import (
	"context"
	"time"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossd/internal/server"
	"github.com/recurser/bossd/internal/upstream"
)

// ExpiredSwitchResult drives bossd's real switch budget expiry and packages the
// resulting failure as the daemon stream would.
func ExpiredSwitchResult(ctx context.Context, commandID string, budget time.Duration) *pb.CommandResult {
	return upstream.SwitchCommandResultForTest(commandID, server.ExpireSwitchBudgetForTest(ctx, budget))
}
