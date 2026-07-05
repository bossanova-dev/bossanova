package credmaterialize

import "sync"

// keyedMutex provides one mutex per string key, created lazily. It lets the
// Materializer serialize all operations on a single account (keyed by
// "provider/accountID") without a global lock that would serialize unrelated
// accounts.
type keyedMutex struct {
	mu    sync.Mutex
	locks map[string]*sync.Mutex
}

// Lock acquires the per-key mutex and returns a func that releases it. The
// returned unlock func must be called exactly once (typically via defer).
func (k *keyedMutex) Lock(key string) func() {
	k.mu.Lock()
	if k.locks == nil {
		k.locks = make(map[string]*sync.Mutex)
	}
	m, ok := k.locks[key]
	if !ok {
		m = &sync.Mutex{}
		k.locks[key] = m
	}
	k.mu.Unlock()

	m.Lock()
	return m.Unlock
}
