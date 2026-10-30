package taskorchestrator

import (
	"context"

	"github.com/recurser/bossalib/kit/safego"
)

// Start begins the poll loop asynchronously; Done closes after it and its merge workers return.
// Repos are staggered across the poll interval so that API calls are spread evenly.
func (o *Orchestrator) Start(ctx context.Context) {
	safego.Go(o.logger, func() {
		defer close(o.done)
		o.run(ctx)
		o.workers.Wait()
	})
}

// Done returns a channel closed after the poll loop and every merge worker return.
func (o *Orchestrator) Done() <-chan struct{} { return o.done }

// goWorker is called only by the poll loop, so all Adds finish before Start waits.
func (o *Orchestrator) goWorker(fn func()) {
	o.workers.Add(1)
	safego.Go(o.logger, func() {
		defer o.workers.Done()
		fn()
	})
}
