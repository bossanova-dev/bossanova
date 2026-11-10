package upstream

import (
	"context"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

type resultScopeKey struct{}
type commandResultScope struct {
	repos []string
	stop  func() bool
}

// Bind the actual result, never the caller-supplied correlation ID: IDs can be
// reused while an earlier asynchronous command is still in flight.
func (c *StreamClient) bindResultScope(ctx context.Context, ev *pb.DaemonEvent) {
	result := ev.GetResult()
	repos, ok := ctx.Value(resultScopeKey{}).([]string)
	if result == nil || !ok {
		return
	}
	scope := &commandResultScope{repos: repos}
	scope.stop = context.AfterFunc(ctx, func() { c.resultRepos.Delete(result) })
	c.resultRepos.Store(result, scope)
	// Cancellation may have run the cleanup before Store.
	if ctx.Err() != nil {
		c.resultRepos.Delete(result)
		scope.stop()
	}
}

func (c *StreamClient) takeResultScope(ev *pb.DaemonEvent) []string {
	value, _ := c.resultRepos.LoadAndDelete(ev.GetResult())
	scope, ok := value.(*commandResultScope)
	if !ok {
		return nil
	}
	scope.stop()
	return scope.repos
}

func (c *StreamClient) sendCommandResult(ctx context.Context, outbound chan<- *pb.DaemonEvent, ev *pb.DaemonEvent) {
	select {
	case <-ctx.Done():
		c.takeResultScope(ev)
	case outbound <- ev:
	}
}
