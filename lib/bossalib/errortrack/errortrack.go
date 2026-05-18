// Package errortrack wraps Sentry error tracking with PII scrubbing, HTTP
// middleware, ConnectRPC interceptor, and safego recover path. Empty DSN
// disables capture as a noop.
package errortrack

import (
	"fmt"
	"sync"
	"time"

	"github.com/getsentry/sentry-go"
	"github.com/recurser/bossalib/safego"
)

// Opts configures Sentry-backed error tracking.
type Opts struct {
	DSN         string
	App         string
	Environment string
	Release     string
	SampleRate  float64
	Transport   sentry.Transport
}

var (
	closeOnce        sync.Mutex
	nextGeneration   uint64
	activeGeneration uint64
)

// Init initializes error tracking and returns a close function.
func Init(opts Opts) (func(), error) {
	noopClose := func() {}
	if opts.DSN == "" {
		return noopClose, nil
	}

	closeOnce.Lock()
	defer closeOnce.Unlock()

	if opts.SampleRate == 0 {
		opts.SampleRate = 1.0
	}

	if err := sentry.Init(sentry.ClientOptions{
		Dsn:              opts.DSN,
		Environment:      opts.Environment,
		Release:          opts.Release,
		SampleRate:       opts.SampleRate,
		TracesSampleRate: 0,
		EnableTracing:    false,
		SendDefaultPII:   false,
		BeforeSend:       beforeSend(opts.App),
		Transport:        opts.Transport,
	}); err != nil {
		return noopClose, fmt.Errorf("errortrack init: %w", err)
	}

	sentry.ConfigureScope(func(scope *sentry.Scope) {
		scope.SetTag("app", opts.App)
	})
	hub := sentry.CurrentHub().Clone()

	nextGeneration++
	generation := nextGeneration
	activeGeneration = generation

	safego.RegisterRecoverHook(func(r any, _ []byte) {
		hub.Recover(r)
	})

	closed := false
	return func() {
		closeOnce.Lock()
		defer closeOnce.Unlock()
		if closed {
			return
		}
		closed = true
		hub.Flush(2 * time.Second)
		if client := hub.Client(); client != nil {
			client.Close()
		}
		if activeGeneration == generation {
			activeGeneration = 0
			safego.RegisterRecoverHook(nil)
		}
	}, nil
}
