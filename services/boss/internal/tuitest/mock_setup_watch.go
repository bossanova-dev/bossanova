package tuitest

import (
	"sync"
	"time"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

type setupWatchScript struct {
	frames  []*pb.WatchSessionSetupResponse
	delay   time.Duration
	settled <-chan struct{}
}

// SetSetupWatchScript seeds replay and live frames for a session's setup watcher.
// The returned function releases terminal setup-settled frames after the caller
// has observed the output; it is safe to call more than once.
func (m *MockDaemon) SetSetupWatchScript(sessionID string, frames []*pb.WatchSessionSetupResponse, delay time.Duration) func() {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.setupWatchScripts == nil {
		m.setupWatchScripts = make(map[string]setupWatchScript)
	}
	settled := make(chan struct{})
	m.setupWatchScripts[sessionID] = setupWatchScript{frames: cloneMsgs(frames), delay: delay, settled: settled}
	return sync.OnceFunc(func() { close(settled) })
}
