package main

import (
	"context"
	"time"

	"github.com/rs/zerolog/log"
)

// credentialEnrollmentWorker serializes enrollment and distribution reconnects.
// A buffered nudge preserves one retry when the connection flaps during a pass.
type credentialEnrollmentWorker struct {
	nudge     chan struct{}
	enroll    func(context.Context) error
	reconnect func(context.Context) error
	timeout   time.Duration
}

func newCredentialEnrollmentWorker(enroll, reconnect func(context.Context) error) *credentialEnrollmentWorker {
	return &credentialEnrollmentWorker{nudge: make(chan struct{}, 1), enroll: enroll, reconnect: reconnect, timeout: 10 * time.Second}
}

func (w *credentialEnrollmentWorker) Nudge() {
	if w == nil {
		return
	}
	select {
	case w.nudge <- struct{}{}:
	default:
	}
}

// Run waits for an in-flight pass, including uncancellable keyring calls, before
// returning. The daemon's bounded shutdown join accounts for a wedged keyring.
func (w *credentialEnrollmentWorker) Run(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-w.nudge:
		}
		if ctx.Err() != nil {
			return
		}
		w.runPass(ctx)
	}
}

func (w *credentialEnrollmentWorker) runPass(ctx context.Context) {
	ctx, cancel := context.WithTimeout(ctx, w.timeout)
	defer cancel()
	if err := w.enroll(ctx); err != nil {
		log.Warn().Err(err).Msg("device enrollment failed; retrying on next upstream connection")
	}
	if w.reconnect != nil {
		if err := w.reconnect(ctx); err != nil {
			log.Warn().Err(err).Msg("credential distribution reconnect failed")
		}
	}
}

func (w *daemonWiring) onUpstreamHandshake() {
	w.terminalStreamClient.CycleStream()
	w.credentialEnrollment.Nudge()
}
