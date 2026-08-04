// Package upstream — terminal_upload.go routes the BOS-661 chat file
// upload subprotocol between bosso and the daemon's chatupload.Manager.
//
// It is a sibling of terminal_stream.go for the same reason that file is a
// sibling of stream.go: upload traffic rides the dedicated TerminalStream
// so a 500 MiB transfer can never starve the control-plane DaemonStream,
// and keeping the dispatch here leaves the terminal input/resize/close
// path untouched.
//
// Two invariants are load-bearing and each has a test:
//
//   - An ack is emitted ONLY after chatupload.Manager.Chunk has returned
//     nil, i.e. after the bytes have reached the file handle. The ack is
//     what releases the browser's four-chunk window, so acking early would
//     let the client run ahead of the daemon's writer and silently defeat
//     the bounded-memory contract.
//   - Nothing on this path ever puts the generated absolute path, the
//     file contents, or an unrecognised error's text on the wire or in a
//     log line. Terminal failures are reported from chatupload's own
//     fixed, non-sensitive strings; anything else collapses to a generic
//     message.
package upstream

import (
	"context"
	"errors"
	"time"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/safego"
	"github.com/recurser/bossd/internal/chatupload"
)

// uploadDeliveryTimeout bounds one chat submission started by an
// upload_finish. See handleUploadFinish for why the delivery does not run
// on the stream context, and why this value is the browser's own stall
// timeout.
const uploadDeliveryTimeout = 60 * time.Second

// uploadManager is the subset of *chatupload.Manager this client drives.
// Hoisted to an interface so the dispatch can be tested with a stub that
// fails on demand; production passes the real manager.
type uploadManager interface {
	Start(attachID, uploadID, chatID, filename string, sizeBytes uint64) error
	Chunk(attachID, uploadID string, seq uint64, data []byte) (uint64, error)
	ClaimFinish(attachID, uploadID string) error
	Finish(ctx context.Context, attachID, uploadID string) (string, error)
	Cancel(attachID, uploadID string)
	CancelAttach(attachID string)
	CancelAll()
}

// UploadManager is the public re-export of the unexported interface above,
// so services/bossd/cmd can hand a *chatupload.Manager to the client config
// without this package exporting its internals. Mirrors ChatLookup.
type UploadManager = uploadManager

// handleUploadStart opens a new upload against a live attach. The chat id
// is resolved from the attach registry rather than taken from the wire:
// the client can only ever upload into the chat it is already attached to.
func (c *TerminalStreamClient) handleUploadStart(ctx context.Context, cmd *pb.TerminalUploadStart, outbound chan<- *pb.TerminalServerMessage) {
	attachID, uploadID := cmd.GetAttachId(), cmd.GetUploadId()
	if c.uploads == nil {
		c.failUpload(ctx, outbound, attachID, uploadID, "upload: not supported by this daemon", false)
		return
	}
	state := c.lookupAttach(attachID)
	if state == nil {
		// The attach died (or never existed) — treat it as transport loss
		// so the browser may resend the same file after reconnecting,
		// rather than telling the user the file itself was rejected.
		c.failUpload(ctx, outbound, attachID, uploadID, "upload: unknown attach", true)
		return
	}
	if err := c.uploads.Start(attachID, uploadID, state.chatID, cmd.GetFilename(), cmd.GetSizeBytes()); err != nil {
		c.sendUploadError(ctx, outbound, attachID, uploadID, err)
		return
	}
	c.logger.Debug().
		Str("attach_id", attachID).
		Str("upload_id", uploadID).
		Uint64("size_bytes", cmd.GetSizeBytes()).
		Msg("terminal stream: upload started")
}

// handleUploadChunk writes one in-order slice and acknowledges it. The ack
// is sent only after Chunk reports the write succeeded — see the package
// comment; this ordering is the backpressure contract.
func (c *TerminalStreamClient) handleUploadChunk(ctx context.Context, cmd *pb.TerminalUploadChunk, outbound chan<- *pb.TerminalServerMessage) {
	attachID, uploadID := cmd.GetAttachId(), cmd.GetUploadId()
	if c.uploads == nil {
		c.failUpload(ctx, outbound, attachID, uploadID, "upload: not supported by this daemon", false)
		return
	}
	received, err := c.uploads.Chunk(attachID, uploadID, cmd.GetSeq(), cmd.GetData())
	if err != nil {
		// Chunk already removed the partial file; the result below is the
		// upload's single terminal frame.
		c.sendUploadError(ctx, outbound, attachID, uploadID, err)
		return
	}
	c.sendServerMessage(ctx, outbound, &pb.TerminalServerMessage{
		Msg: &pb.TerminalServerMessage_UploadAck{UploadAck: &pb.TerminalUploadAck{
			AttachId:      attachID,
			UploadId:      uploadID,
			Seq:           cmd.GetSeq(),
			BytesReceived: received,
		}},
	})
}

// handleUploadFinish closes the file and prefills the path into the chat's
// composer, then reports the terminal result.
//
// It runs on its own goroutine because chatupload.Manager.Finish delivers
// through the daemon's wake-and-deliver chat path, which drives tmux and may
// wake an asleep session — seconds, not microseconds. Doing that on the
// reader goroutine would stall terminal input, resize, close AND the
// BOS-376 ping/pong heartbeat for the whole window, so a large upload
// could tear down the very stream carrying it.
//
// The goroutine is joined, not fire-and-forget: uploadWG is what
// openStream's teardown waits on before closing `outbound`, so a finish in
// flight can never send on a closed channel.
//
// The spawn is GATED by a synchronous ClaimFinish on this reader goroutine.
// Without that gate the spawn rate is the client's frame rate: repeating
// upload_finish for one id would start a goroutine per ~30-byte frame, each
// one able to park on a full `outbound`, and nothing would bound the pile.
// Claiming first means at most one delivery goroutine ever exists per live
// upload, and live uploads are already bounded by the manager's admission
// limits.
func (c *TerminalStreamClient) handleUploadFinish(ctx context.Context, cmd *pb.TerminalUploadFinish, outbound chan<- *pb.TerminalServerMessage) {
	attachID, uploadID := cmd.GetAttachId(), cmd.GetUploadId()
	if c.uploads == nil {
		c.failUpload(ctx, outbound, attachID, uploadID, "upload: not supported by this daemon", false)
		return
	}
	if err := c.uploads.ClaimFinish(attachID, uploadID); err != nil {
		c.sendUploadError(ctx, outbound, attachID, uploadID, err)
		return
	}
	c.uploadWG.Add(1)
	_ = safego.Go(c.logger, func() {
		defer c.uploadWG.Done()
		// The submission gets its OWN bounded context rather than the
		// reader's stream context.
		//
		// openStream's teardown cancels the stream context BEFORE joining
		// this goroutine (cancel() → <-readerDone → waitUploads()), so every
		// ordinary stream cycle — heartbeat miss, CycleStream, re-auth,
		// reconnect — used to poison an in-flight delivery. chatupload.Finish
		// then treats the cancelled context as UNCONFIRMED, which is
		// reported PERMANENT (can_retry=0) and retains the file for the full
		// 24h TTL: a routine reconnect turned into an orphan file plus a
		// message the user cannot recover from. Because teardown already
		// waits for us, cancelling us first bought nothing.
		//
		// The timeout is what keeps teardown bounded, and it is set to the
		// browser's own per-upload stall timeout (UPLOAD_STALL_TIMEOUT_MS in
		// services/web/src/lib/wsAttach.ts), past which nobody is still
		// waiting for this result anyway.
		//
		// It bounds the chat SUBMIT, not the whole of Finish. The fsync and
		// close that precede the submit take no context, so waitUploads()
		// can block for this deadline PLUS a flush of up to MaxUploadBytes.
		// That flush is bounded by the disk rather than by us, and cutting
		// it short would publish a path to a torn file, so it is left to
		// complete.
		deliverCtx, cancelDeliver := context.WithTimeout(context.WithoutCancel(ctx), uploadDeliveryTimeout)
		defer cancelDeliver()
		// The returned path is deliberately discarded: it is the one
		// value on this path that must never reach a log line.
		if _, err := c.uploads.Finish(deliverCtx, attachID, uploadID); err != nil {
			c.sendUploadError(ctx, outbound, attachID, uploadID, err)
			return
		}
		c.logger.Info().
			Str("attach_id", attachID).
			Str("upload_id", uploadID).
			Msg("terminal stream: upload delivered to chat")
		c.sendServerMessage(ctx, outbound, &pb.TerminalServerMessage{
			Msg: &pb.TerminalServerMessage_UploadResult{UploadResult: &pb.TerminalUploadResult{
				AttachId: attachID,
				UploadId: uploadID,
				IsOk:     true,
			}},
		})
	})
}

// handleUploadCancel abandons an upload and removes its partial file. No
// result frame is emitted: the client asked for this, and exactly one
// terminal result per upload id is the protocol's contract — a cancel
// racing an already-sent failure must not produce a second one. Cancel is
// safe for an unknown id.
func (c *TerminalStreamClient) handleUploadCancel(cmd *pb.TerminalUploadCancel) {
	if c.uploads == nil {
		return
	}
	c.uploads.Cancel(cmd.GetAttachId(), cmd.GetUploadId())
	c.logger.Debug().
		Str("attach_id", cmd.GetAttachId()).
		Str("upload_id", cmd.GetUploadId()).
		Str("reason", cmd.GetReason()).
		Msg("terminal stream: upload cancelled")
}

// sendUploadError maps a manager error onto the wire result. A
// *chatupload.Error carries a fixed, non-sensitive message and its own
// retryable classification; anything else is reported as a generic
// permanent failure rather than echoing an unbounded error string back to
// the browser.
func (c *TerminalStreamClient) sendUploadError(ctx context.Context, outbound chan<- *pb.TerminalServerMessage, attachID, uploadID string, err error) {
	msg, retryable := "upload failed", false
	var uploadErr *chatupload.Error
	if errors.As(err, &uploadErr) {
		msg, retryable = uploadErr.Msg, uploadErr.Retryable
	}
	c.failUpload(ctx, outbound, attachID, uploadID, msg, retryable)
}

// failUpload logs and emits one terminal failure result. Only ids and the
// already-sanitised reason are logged — never the filename, the generated
// path, or any payload byte.
func (c *TerminalStreamClient) failUpload(ctx context.Context, outbound chan<- *pb.TerminalServerMessage, attachID, uploadID, reason string, retryable bool) {
	c.logger.Warn().
		Str("attach_id", attachID).
		Str("upload_id", uploadID).
		Str("reason", reason).
		Bool("retryable", retryable).
		Msg("terminal stream: upload failed")
	c.sendServerMessage(ctx, outbound, &pb.TerminalServerMessage{
		Msg: &pb.TerminalServerMessage_UploadResult{UploadResult: &pb.TerminalUploadResult{
			AttachId:     attachID,
			UploadId:     uploadID,
			IsOk:         false,
			ErrorMessage: reason,
			CanRetry:     retryable,
		}},
	})
}

// cancelAttachUploads drops every in-flight upload belonging to one
// attach. Called from the per-attach pump's teardown, which covers a
// browser detach, a PTY exit, and a stream-wide close alike.
func (c *TerminalStreamClient) cancelAttachUploads(attachID string) {
	if c.uploads == nil {
		return
	}
	c.uploads.CancelAttach(attachID)
}

// cancelAllUploads drops every in-flight upload on stream teardown, so an
// upload whose attach vanished without a pump exit still cannot leave a
// partial file behind.
func (c *TerminalStreamClient) cancelAllUploads() {
	if c.uploads == nil {
		return
	}
	c.uploads.CancelAll()
}

// waitUploads blocks until every in-flight finish goroutine has returned.
// MUST be called after the reader has exited (the reader is the only
// spawner) and before `outbound` is closed.
func (c *TerminalStreamClient) waitUploads() { c.uploadWG.Wait() }
