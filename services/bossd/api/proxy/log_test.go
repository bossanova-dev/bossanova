package proxy

import (
	"bytes"
	"sync"
)

// syncBuffer is a mutex-guarded log sink. The daemon logs from the RPC
// goroutine, the lifecycle goroutine and the background draft-PR step, so a bare
// bytes.Buffer would race.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}
