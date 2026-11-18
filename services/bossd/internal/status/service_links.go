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
	links = slices.Clone(links)
	slices.SortFunc(links, func(a, b ServiceLink) int { return cmp.Or(cmp.Compare(a.Name, b.Name), cmp.Compare(a.URL, b.URL)) })
	t.mu.Lock()
	e := t.entries[sessionID]
	if (e == nil && len(links) == 0) || (e != nil && slices.Equal(e.ServiceLinks, links)) {
		t.mu.Unlock()
		return
	}
	if e == nil {
		e = &DisplayEntry{}
		t.entries[sessionID] = e
	}
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
