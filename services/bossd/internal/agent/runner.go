// Package claude manages Claude CLI subprocess lifecycle for coding sessions.
package agent

import (
	"context"
	"sync"
	"time"

	bossanovav1 "github.com/recurser/bossalib/gen/bossanova/v1"
)

// DefaultRingBufferSize is the number of output lines kept in memory per session.
const DefaultRingBufferSize = 1000

// OutputLine is a single line of output from a Claude process.
type OutputLine struct {
	Text      string
	Timestamp time.Time
}

// AgentRunner manages Claude CLI subprocesses.
type AgentRunner interface {
	// Start spawns a Claude CLI process in workDir with the given plan.
	// If resume is non-nil, it resumes an existing Claude session.
	// If sessionID is non-empty, it is passed via --session-id and used as the tracking key.
	// When sessionID is empty, a generated claude-<timestamp> ID is used instead.
	// model is an opaque agent model id; "" means the plugin's default model.
	// extraEnv is an allowlisted, never-logged env overlay (proof credentials
	// + non-secret proof constants) applied to the spawned process; nil/empty
	// leaves the inherited environment unchanged.
	// Returns the session ID assigned to this process.
	Start(ctx context.Context, workDir, plan string, resume *string, sessionID, model string, extraEnv map[string]string) (string, error)

	// Stop terminates the Claude process for the given session.
	Stop(sessionID string) error

	// IsRunning reports whether a Claude process is active for the session.
	IsRunning(sessionID string) bool

	// ExitError returns the exit error for a completed session.
	// Returns nil if the session is still running, exited successfully,
	// or is unknown.
	ExitError(sessionID string) error

	// Subscribe returns a channel that receives output lines for the session.
	// The channel is closed when the process exits or the caller cancels ctx.
	Subscribe(ctx context.Context, sessionID string) (<-chan OutputLine, error)

	// History returns the buffered output lines for a session.
	History(sessionID string) []OutputLine
}

// AgentDispatcher extends AgentRunner with explicit by-agent routing for
// callers that already know which agent should service the call (e.g. the
// session lifecycle reading sess.AgentName). The agentSessionID parameter
// remains the agent-side tracking key forwarded to the plugin — empty for
// fresh runs, non-empty for resume.
type AgentDispatcher interface {
	AgentRunner
	StartByAgent(ctx context.Context, agentName, workDir, plan string, resume *string, agentSessionID, model string, extraEnv map[string]string) (string, error)
	StopByAgent(ctx context.Context, agentName, agentSessionID string) error
	IsRunningByAgent(agentName, agentSessionID string) bool
}

// ContextualStopper is implemented by runners whose stop can be BOUNDED by the
// caller's context. AgentRunner.Stop cannot: it takes no context, and
// PluginRunner.Stop therefore issued its StopRun RPC on context.Background().
//
// That was survivable while every stop came from a caller with nothing waiting
// on it. It stopped being survivable once a failed bootstrap had to stop the run
// it had just spawned: an unresponsive plugin would block StartSession forever,
// holding the per-target lock, so the half-started row and its worktree were
// never cleaned up — the exact daemon-wedge shape BOS-717 exists to remove,
// reintroduced by the cleanup meant to prevent an orphan.
//
// Kept separate from AgentRunner rather than widening Stop, matching how
// HeadlessCapabilityProfileRunner and friends are layered here: every legacy
// Stop call stays byte-for-byte unchanged.
type ContextualStopper interface {
	StopWithContext(ctx context.Context, agentSessionID string) error
}

// HeadlessCapabilityProfileRunner is implemented only by runners that can
// carry an explicit operation-surface requirement to their plugin. Keeping it
// separate from AgentRunner leaves every legacy Start call byte-for-byte
// unchanged.
type HeadlessCapabilityProfileRunner interface {
	StartWithHeadlessCapabilityProfile(ctx context.Context, workDir, plan string, resume *string, sessionID, model string, extraEnv map[string]string, profile bossanovav1.HeadlessCapabilityProfile) (string, error)
}

// HeadlessLaunchOptions carries daemon-owned policy that every panel-less
// launch must forward to its plugin as one atomic request.
type HeadlessLaunchOptions struct {
	HeadlessCapabilityProfile bossanovav1.HeadlessCapabilityProfile
}

// HeadlessLaunchOptionsRunner is implemented by runners that preserve the
// complete panel-less launch contract instead of silently dropping a
// capability profile.
type HeadlessLaunchOptionsRunner interface {
	StartWithHeadlessLaunchOptions(ctx context.Context, workDir, plan string, resume *string, sessionID, model string, extraEnv map[string]string, options HeadlessLaunchOptions) (string, error)
}

// HeadlessCapabilityProfilePreflightRunner is implemented only by runners
// that can validate a required headless operation surface without starting a
// run.
//
// workDir is the directory the gated run will execute in. It is passed so the
// preflight profiles the same repo-level agent configuration the run will load
// — agents discover per-repo config relative to their working directory — and
// an empty value preserves the historical inherit-the-daemon-cwd behaviour.
type HeadlessCapabilityProfilePreflightRunner interface {
	PreflightHeadlessCapabilityProfile(ctx context.Context, workDir, model string, extraEnv map[string]string, profile bossanovav1.HeadlessCapabilityProfile) error
}

// HeadlessCapabilityProfileDispatcher is the explicit-by-agent equivalent for
// session lifecycle launches. It is intentionally opt-in: a required profile
// on an agent without support fails closed instead of being silently dropped.
type HeadlessCapabilityProfileDispatcher interface {
	AgentDispatcher
	StartByAgentWithHeadlessCapabilityProfile(ctx context.Context, agentName, workDir, plan string, resume *string, agentSessionID, model string, extraEnv map[string]string, profile bossanovav1.HeadlessCapabilityProfile) (string, error)
}

// HeadlessLaunchOptionsDispatcher is the explicit-by-agent form used by the
// lifecycle for panel-less launches.
type HeadlessLaunchOptionsDispatcher interface {
	AgentDispatcher
	StartByAgentWithHeadlessLaunchOptions(ctx context.Context, agentName, workDir, plan string, resume *string, agentSessionID, model string, extraEnv map[string]string, options HeadlessLaunchOptions) (string, error)
}

// AgentNameResolver is implemented by dispatchers that can report which
// agent name a launch will actually route to, including the empty-name
// fallbacks (single loaded runner, else the configured default agent).
type AgentNameResolver interface {
	ResolveAgentName(agentName string) string
}

// HeadlessCapabilityProfilePreflightDispatcher validates a required profile
// through explicit by-agent routing before lifecycle worktree side effects.
// Keeping it optional preserves AgentDispatcher compatibility for all
// unprofiled callers.
type HeadlessCapabilityProfilePreflightDispatcher interface {
	AgentDispatcher
	PreflightByAgentWithHeadlessCapabilityProfile(
		ctx context.Context,
		agentName, workDir, model string,
		extraEnv map[string]string,
		profile bossanovav1.HeadlessCapabilityProfile,
	) error
}

// --- Ring Buffer ---

// ringBuffer is a fixed-size circular buffer of OutputLine entries.
type ringBuffer struct {
	mu    sync.RWMutex
	buf   []OutputLine
	size  int
	head  int // next write position
	count int // total items written (for overflow detection)
}

func newRingBuffer(size int) *ringBuffer {
	return &ringBuffer{
		buf:  make([]OutputLine, size),
		size: size,
	}
}

// add appends a line to the ring buffer.
func (rb *ringBuffer) add(line OutputLine) {
	rb.mu.Lock()
	defer rb.mu.Unlock()

	rb.buf[rb.head] = line
	rb.head = (rb.head + 1) % rb.size
	rb.count++
}

// lines returns all stored lines in chronological order.
func (rb *ringBuffer) lines() []OutputLine {
	rb.mu.RLock()
	defer rb.mu.RUnlock()

	if rb.count == 0 {
		return nil
	}

	n := rb.count
	if n > rb.size {
		n = rb.size
	}

	result := make([]OutputLine, n)
	if rb.count <= rb.size {
		// Buffer hasn't wrapped yet.
		copy(result, rb.buf[:n])
	} else {
		// Buffer has wrapped: read from head (oldest) to end, then start to head.
		start := rb.head // oldest entry
		copied := copy(result, rb.buf[start:])
		copy(result[copied:], rb.buf[:start])
	}

	return result
}

// --- Subscribers ---

// subscribers manages broadcast channels for output streaming.
type subscribers struct {
	mu     sync.RWMutex
	chans  []chan OutputLine
	closed bool
}

func newSubscribers() *subscribers {
	return &subscribers{}
}

// add creates a new subscription channel. The channel is removed when ctx is cancelled.
func (s *subscribers) add(ctx context.Context) <-chan OutputLine {
	ch := make(chan OutputLine, 64)

	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		close(ch)
		return ch
	}
	s.chans = append(s.chans, ch)
	s.mu.Unlock()

	// Remove the channel when the context is cancelled.
	go func() {
		<-ctx.Done()
		s.remove(ch)
	}() // no safego: trivial cleanup, cannot panic

	return ch
}

// broadcast sends a line to all subscriber channels.
func (s *subscribers) broadcast(line OutputLine) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	for _, ch := range s.chans {
		select {
		case ch <- line:
		default:
			// Slow consumer — drop the line rather than blocking.
		}
	}
}

// close closes all subscriber channels.
func (s *subscribers) close() {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.closed = true
	for _, ch := range s.chans {
		close(ch)
	}
	s.chans = nil
}

// remove removes a specific channel from subscribers.
func (s *subscribers) remove(ch chan OutputLine) {
	s.mu.Lock()
	defer s.mu.Unlock()

	for i, c := range s.chans {
		if c == ch {
			s.chans = append(s.chans[:i], s.chans[i+1:]...)
			close(ch)
			return
		}
	}
}
