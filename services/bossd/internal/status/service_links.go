package status

import (
	"cmp"
	"slices"
	"time"
)

// ServiceLink is a named https URL reported by a running serve command.
type ServiceLink struct{ Name, URL string }

// SetServiceLinks stores an owned, sorted copy and publishes only changes.
func (t *DisplayTracker) SetServiceLinks(sessionID string, links []ServiceLink) {
	t.mu.RLock()
	e := t.entries[sessionID]
	running := e != nil && e.ServeRunning
	serveErr := ""
	if e != nil {
		serveErr = e.ServeError
	}
	t.mu.RUnlock()
	t.SetServeState(sessionID, running, serveErr, links)
}

// SetServeState stores the live serve status, error and sorted links,
// publishing only when any of them changes.
func (t *DisplayTracker) SetServeState(sessionID string, running bool, serveErr string, links []ServiceLink) {
	links = slices.Clone(links)
	slices.SortFunc(links, func(a, b ServiceLink) int { return cmp.Or(cmp.Compare(a.Name, b.Name), cmp.Compare(a.URL, b.URL)) })
	t.mu.Lock()
	e := t.entries[sessionID]
	if (e == nil && !running && serveErr == "" && len(links) == 0) ||
		(e != nil && e.ServeRunning == running && e.ServeError == serveErr && slices.Equal(e.ServiceLinks, links)) {
		t.mu.Unlock()
		return
	}
	if e == nil {
		e = &DisplayEntry{}
		t.entries[sessionID] = e
	}
	e.ServeRunning = running
	e.ServeError = serveErr
	e.ServiceLinks = links
	e.UpdatedAt = time.Now()
	if e.isEmpty() {
		delete(t.entries, sessionID)
	}
	publish := t.onAttentionChange
	t.mu.Unlock()
	t.scheduleRecompute(sessionID)
	if publish != nil {
		publish(sessionID)
	}
}
