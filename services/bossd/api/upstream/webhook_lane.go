package upstream

import (
	"context"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/kit/safego"
)

// webhookLaneDepth bounds the webhooks queued behind the one in flight. It
// matches outboundBufferSize; bosso stops waiting for an ack after 10 s, so a
// deeper queue would only hold work bosso has already given up on.
const webhookLaneDepth = outboundBufferSize

// webhookJob is one queued webhook command.
type webhookJob struct {
	cmdID string
	ev    *pb.WebhookEvent
}

// webhookLane takes webhook handling off the single command reader
// (BOS-2252). Dispatch makes GitHub round-trips, so running it inline let a
// webhook burst starve every other command kind past bosso's 30 s deadline.
// One worker drains a bounded FIFO, which keeps webhook order and today's
// GitHub load; the reader only enqueues.
type webhookLane struct {
	queue chan webhookJob
	done  <-chan struct{}
}

// enqueue hands job to the worker without blocking. It reports false when the
// queue is full, so the reader can nack instead of wedging.
func (l *webhookLane) enqueue(job webhookJob) bool {
	select {
	case l.queue <- job:
		return true
	default:
		return false
	}
}

// startWebhookLane starts the per-stream webhook worker. ctx bounds the
// worker and every Dispatch it runs; acks go to outbound. The caller must
// cancel ctx and call joinWebhookLane before closing outbound.
func (c *StreamClient) startWebhookLane(ctx context.Context, outbound chan<- *pb.DaemonEvent) {
	lane := &webhookLane{queue: make(chan webhookJob, webhookLaneDepth)}
	lane.done = safego.Go(c.logger, func() {
		c.runWebhookLane(ctx, lane.queue, outbound)
	})
	c.webhookLane = lane
}

// joinWebhookLane waits for the worker started by startWebhookLane to exit
// and forgets the lane. Every worker receive and send also selects on
// ctx.Done(), so once ctx is cancelled the join cannot wedge behind a
// stalled writer.
func (c *StreamClient) joinWebhookLane() {
	if c.webhookLane == nil {
		return
	}
	<-c.webhookLane.done
	c.webhookLane = nil
}

func (c *StreamClient) runWebhookLane(ctx context.Context, queue <-chan webhookJob, outbound chan<- *pb.DaemonEvent) {
	for {
		var job webhookJob
		select {
		case <-ctx.Done():
			return
		case job = <-queue:
		}
		if ctx.Err() != nil {
			return
		}
		ack := webhookAck(job.cmdID, true, "")
		if err := c.webhooks.Dispatch(ctx, job.ev); err != nil {
			ack = webhookAck(job.cmdID, false, err.Error())
		}
		select {
		case <-ctx.Done():
			return
		case outbound <- ack:
		}
	}
}
