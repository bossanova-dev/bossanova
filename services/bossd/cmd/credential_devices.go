package main

import (
	"context"
	"time"

	"github.com/rs/zerolog/log"
)

// credentialEnrollmentWorker serializes enrollment and distribution reconnects.
// A buffered nudge preserves one retry when the connection flaps during a pass.
// With refresh set, it also re-evaluates readiness after each pass and on every
// interval tick; a tick only evaluates and never requests enrollment.
type credentialEnrollmentWorker struct {
	nudge     chan struct{}
	enroll    func(context.Context) error
	reconnect func(context.Context) error
	refresh   func(context.Context)
	interval  time.Duration
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
	var tick <-chan time.Time
	if w.refresh != nil && w.interval > 0 {
		ticker := time.NewTicker(w.interval)
		defer ticker.Stop()
		tick = ticker.C
	}
	for {
		pass := false
		select {
		case <-ctx.Done():
			return
		case <-w.nudge:
			pass = true
		case <-tick:
		}
		if ctx.Err() != nil {
			return
		}
		if pass {
			w.runPass(ctx)
		}
		w.refreshReadiness(ctx)
	}
}

func (w *credentialEnrollmentWorker) refreshReadiness(ctx context.Context) {
	if w.refresh == nil {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, w.timeout)
	defer cancel()
	w.refresh(ctx)
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
