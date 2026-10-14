// Package reposecretstest provides an in-memory reposecrets.Ring for tests in
// any package, so no test ever touches a real OS keyring.
package reposecretstest

import (
	"errors"
	"sync"

	"github.com/recurser/bossd/internal/reposecrets"
)

// ErrInjected is the error a Ring returns for an operation a test made fail.
var ErrInjected = errors.New("reposecretstest: injected keyring failure")

// Ring is an in-memory reposecrets.Ring. FailSet / FailGet make the matching
// operation fail with ErrInjected; counters record how often the keyring was
// actually reached. Block makes calls on one key hang the way a keychain
// approval prompt does.
type Ring struct {
	mu      sync.Mutex
	items   map[string][]byte
	written []string
	FailSet bool
	FailGet bool
	Gets    int
	Sets    int
	Removes int

	gateMu  sync.Mutex
	gates   map[string]chan struct{}
	entered chan string
}

var _ reposecrets.Ring = (*Ring)(nil)

// NewRing returns an empty Ring.
func NewRing() *Ring {
	return &Ring{
		items:   make(map[string][]byte),
		gates:   make(map[string]chan struct{}),
		entered: make(chan string, 64),
	}
}

// Block makes every call on key wait, before it touches the ring, until the
// returned release is called. Calls on other keys are unaffected. release is
// idempotent.
func (r *Ring) Block(key string) (release func()) {
	gate := make(chan struct{})
	r.gateMu.Lock()
	r.gates[key] = gate
	r.gateMu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			r.gateMu.Lock()
			delete(r.gates, key)
			r.gateMu.Unlock()
			close(gate)
		})
	}
}

// Entered receives the key of each call that reached a Block gate, so a test
// can wait until a call is actually inside the ring.
func (r *Ring) Entered() <-chan string {
	return r.entered
}

func (r *Ring) wait(key string) {
	r.gateMu.Lock()
	gate := r.gates[key]
	r.gateMu.Unlock()
	if gate != nil {
		r.entered <- key
		<-gate
	}
}

// Get implements reposecrets.Ring.
func (r *Ring) Get(key string) ([]byte, error) {
	r.wait(key)
	r.mu.Lock()
	defer r.mu.Unlock()
	r.Gets++
	if r.FailGet {
		return nil, ErrInjected
	}
	data, ok := r.items[key]
	if !ok {
		return nil, reposecrets.ErrNotFound
	}
	return append([]byte(nil), data...), nil
}

// Set implements reposecrets.Ring.
func (r *Ring) Set(key string, data []byte) error {
	r.wait(key)
	r.mu.Lock()
	defer r.mu.Unlock()
	r.Sets++
	if r.FailSet {
		return ErrInjected
	}
	r.items[key] = append([]byte(nil), data...)
	r.written = append(r.written, string(data))
	return nil
}

// Writes returns every item value Set stored, in the order it stored them.
func (r *Ring) Writes() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.written...)
}

// Remove implements reposecrets.Ring.
func (r *Ring) Remove(key string) error {
	r.wait(key)
	r.mu.Lock()
	defer r.mu.Unlock()
	r.Removes++
	if _, ok := r.items[key]; !ok {
		return reposecrets.ErrNotFound
	}
	delete(r.items, key)
	return nil
}

// Has reports whether key holds an item.
func (r *Ring) Has(key string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	_, ok := r.items[key]
	return ok
}

// SetFailSet toggles FailSet under the lock.
func (r *Ring) SetFailSet(fail bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.FailSet = fail
}

// GetCount returns Gets under the lock.
func (r *Ring) GetCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.Gets
}

// NewStore returns a reposecrets.Store over ring.
func NewStore(ring *Ring) *reposecrets.Store {
	return reposecrets.New(func() (reposecrets.Ring, error) { return ring, nil })
}
