package agent

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"sync"
	"syscall"
	"time"

	"github.com/rs/zerolog"

	"github.com/recurser/bossalib/safego"
)

// ErrLogPathSymlink is returned when a tailed log path is a symlink.
var ErrLogPathSymlink = errors.New("log path is a symlink; refusing to follow")

// pollInterval is the fallback file-poll cadence on platforms without
// inotify/fsevents. PR1 uses polling unconditionally for portability;
// inotify integration is a follow-up optimisation if profiling shows
// it matters.
const pollInterval = 100 * time.Millisecond

// Tailer follows NDJSON log files written by an agent plugin and fans
// the parsed OutputLines out to in-process subscribers. One Tailer
// owns many tails (one per active session); each tail has its own
// ringbuffer and subscribers fanout.
//
//	log file ──read──► NDJSON parse ──► ringBuffer (1000 lines)
//	                               └──► subscribers (broadcast)
type Tailer struct {
	mu     sync.RWMutex
	tails  map[string]*tail
	logger zerolog.Logger
}

func NewTailer(logger zerolog.Logger) *Tailer {
	return &Tailer{
		tails:  make(map[string]*tail),
		logger: logger,
	}
}

// Open begins tailing logPath and indexes it under sessionID. Subsequent
// Subscribe / History calls reference this sessionID. Refuses symlinks
// (ErrLogPathSymlink). Idempotent: re-opening the same sessionID is a
// no-op.
func (t *Tailer) Open(sessionID, logPath string) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if _, ok := t.tails[sessionID]; ok {
		return nil
	}

	f, err := openLogNoFollowReadOnly(logPath)
	if err != nil {
		return err
	}

	tlCtx, cancel := context.WithCancel(context.Background()) //nolint:gosec // cancel stored in tl.cancel; called by Close
	tl := &tail{
		sessionID: sessionID,
		path:      logPath,
		file:      f,
		ring:      newRingBuffer(DefaultRingBufferSize),
		subs:      newSubscribers(),
		done:      make(chan struct{}),
		cancel:    cancel,
	}
	t.tails[sessionID] = tl
	safego.Go(t.logger, func() { tl.run(tlCtx, t.logger) })
	return nil
}

// Close stops tailing and closes any open subscribers for sessionID.
func (t *Tailer) Close(sessionID string) {
	t.mu.Lock()
	tl, ok := t.tails[sessionID]
	if ok {
		delete(t.tails, sessionID)
	}
	t.mu.Unlock()
	if !ok {
		return
	}
	tl.cancel()
	<-tl.done
	tl.subs.close()
	_ = tl.file.Close()
}

// Subscribe returns a channel of OutputLines for sessionID. The channel
// is closed when the tailer is closed or ctx is cancelled.
func (t *Tailer) Subscribe(ctx context.Context, sessionID string) (<-chan OutputLine, error) {
	t.mu.RLock()
	tl, ok := t.tails[sessionID]
	t.mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("tailer: session %s not open", sessionID)
	}
	return tl.subs.add(ctx), nil
}

// History returns the buffered OutputLines for sessionID.
func (t *Tailer) History(sessionID string) []OutputLine {
	t.mu.RLock()
	tl, ok := t.tails[sessionID]
	t.mu.RUnlock()
	if !ok {
		return nil
	}
	return tl.ring.lines()
}

// --- internal types ---

type tail struct {
	sessionID string
	path      string
	file      *os.File
	ring      *ringBuffer
	subs      *subscribers
	done      chan struct{}
	cancel    context.CancelFunc
}

func (tl *tail) run(ctx context.Context, logger zerolog.Logger) {
	defer close(tl.done)
	reader := bufio.NewReader(tl.file)
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}
		line, err := reader.ReadString('\n')
		if errors.Is(err, io.EOF) || (err != nil && len(line) == 0) {
			// Wait for more data.
			select {
			case <-ctx.Done():
				return
			case <-time.After(pollInterval):
				continue
			}
		}
		if err != nil && !errors.Is(err, io.EOF) {
			logger.Warn().Err(err).Str("session", tl.sessionID).Msg("tailer read error")
			continue
		}
		// Strip trailing newline.
		if n := len(line); n > 0 && line[n-1] == '\n' {
			line = line[:n-1]
		}
		if line == "" {
			continue
		}
		var entry struct {
			TS   string `json:"ts"`
			Text string `json:"text"`
		}
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			logger.Warn().Err(err).Str("session", tl.sessionID).Str("line", line).Msg("malformed NDJSON line")
			continue
		}
		ts, _ := time.Parse(time.RFC3339Nano, entry.TS)
		out := OutputLine{Text: entry.Text, Timestamp: ts}
		tl.ring.add(out)
		tl.subs.broadcast(out)
	}
}

// openLogNoFollowReadOnly opens path read-only with O_NOFOLLOW.
func openLogNoFollowReadOnly(path string) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		var pe *os.PathError
		if errors.As(err, &pe) {
			if pe.Err == syscall.ELOOP || pe.Err == syscall.EMLINK {
				return nil, fmt.Errorf("%w: %s", ErrLogPathSymlink, path)
			}
		}
		return nil, fmt.Errorf("open log: %w", err)
	}
	return f, nil
}
