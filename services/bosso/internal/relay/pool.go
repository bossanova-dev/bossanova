// Package relay provides a daemon connection pool for proxying RPCs
// from the orchestrator to registered daemons.
package relay

import (
	"net/http"
	"sync"
	"time"

	"github.com/recurser/bossalib/gen/bossanova/v1/bossanovav1connect"
)

// Pool manages ConnectRPC client connections to registered daemons.
// It creates clients lazily and caches them by daemon ID.
type Pool struct {
	mu      sync.RWMutex
	clients map[string]bossanovav1connect.DaemonServiceClient
}

// NewPool creates a new daemon connection pool.
func NewPool() *Pool {
	return &Pool{
		clients: make(map[string]bossanovav1connect.DaemonServiceClient),
	}
}

// Register creates (or replaces) a DaemonServiceClient for the given endpoint.
// Called when a daemon registers with the orchestrator and provides an endpoint.
func (p *Pool) Register(daemonID, endpoint string) {
	httpClient := &http.Client{Timeout: 30 * time.Second}
	client := bossanovav1connect.NewDaemonServiceClient(httpClient, endpoint)

	p.mu.Lock()
	p.clients[daemonID] = client
	p.mu.Unlock()
}

// Get returns the DaemonServiceClient for a daemon, or nil if not registered.
func (p *Pool) Get(daemonID string) bossanovav1connect.DaemonServiceClient {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.clients[daemonID]
}

// Remove removes a daemon's client from the pool.
func (p *Pool) Remove(daemonID string) {
	p.mu.Lock()
	delete(p.clients, daemonID)
	p.mu.Unlock()
}
