package main

import (
	"time"

	"github.com/rs/zerolog"
)

const startupPhaseSlowAfter = 5 * time.Second

type startupPhaseResult struct {
	name    string
	elapsed time.Duration
}

type startupPhaseTimer struct {
	logger    zerolog.Logger
	started   time.Time
	slowAfter time.Duration
	phases    []startupPhaseResult
}

func (t *startupPhaseTimer) phase(name string, fn func() error) error {
	started := time.Now()
	warned := make(chan struct{})
	watchdog := time.AfterFunc(t.slowAfter, func() {
		defer close(warned)
		t.logger.Warn().Str("phase", name).Dur("elapsed", time.Since(started)).Msg("startup phase still running")
	})
	err := fn()
	elapsed := time.Since(started)
	// Stop cannot cancel a callback already running; join it before logging
	// completion so no watchdog outlives the phase or the logger's cleanup.
	if !watchdog.Stop() {
		<-warned
	}
	t.phases = append(t.phases, startupPhaseResult{name: name, elapsed: elapsed})
	t.logger.Debug().Str("phase", name).Dur("elapsed", elapsed).Err(err).Msg("startup phase done")
	return err
}

func (t *startupPhaseTimer) ready() {
	phases := zerolog.Dict()
	var slowest startupPhaseResult
	for _, phase := range t.phases {
		phases.Dur(phase.name, phase.elapsed)
		if phase.elapsed > slowest.elapsed {
			slowest = phase
		}
	}
	t.logger.Info().Dur("total", time.Since(t.started)).Str("slowest_phase", slowest.name).
		Dur("slowest_elapsed", slowest.elapsed).Dict("phases", phases).Msg("daemon ready to serve")
}

func startupVoidPhase(fn func()) func() error {
	return func() error { fn(); return nil }
}
