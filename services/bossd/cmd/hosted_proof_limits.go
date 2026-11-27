package main

import (
	"sync"
	"time"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	server "github.com/recurser/bossd/api/daemon"
)

const hostedProofLimitsTTL = time.Hour

type hostedProofLimitsCache struct {
	mu      sync.Mutex
	now     func() time.Time
	entries map[string]hostedProofLimitsEntry
}

type hostedProofLimitsEntry struct {
	limits   server.HostedProofLimits
	storedAt time.Time
}

func (c *hostedProofLimitsCache) clock() time.Time {
	if c.now == nil {
		return time.Now()
	}
	return c.now()
}

func (c *hostedProofLimitsCache) get(origin string) (server.HostedProofLimits, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	entry, ok := c.freshLocked(origin)
	return entry.limits, ok
}

func (c *hostedProofLimitsCache) store(origin string, limits server.HostedProofLimits) {
	if limits == (server.HostedProofLimits{}) {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.putLocked(origin, limits)
}

func (c *hostedProofLimitsCache) refused(origin string, refusal *server.HostedProofLimitError) {
	if refusal == nil || refusal.Max <= 0 {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	entry, _ := c.freshLocked(origin)
	limits := entry.limits
	switch refusal.Limit {
	case "file-count":
		limits.FileCount = refusal.Max
	case "file-bytes":
		limits.FileBytes = refusal.Max
	case "page-bytes":
		limits.PageBytes = refusal.Max
	default:
		return
	}
	c.putLocked(origin, limits)
}

func (c *hostedProofLimitsCache) freshLocked(origin string) (hostedProofLimitsEntry, bool) {
	entry, ok := c.entries[origin]
	if !ok {
		return hostedProofLimitsEntry{}, false
	}
	if c.clock().Sub(entry.storedAt) >= hostedProofLimitsTTL {
		delete(c.entries, origin)
		return hostedProofLimitsEntry{}, false
	}
	return entry, true
}

func (c *hostedProofLimitsCache) putLocked(origin string, limits server.HostedProofLimits) {
	if c.entries == nil {
		c.entries = make(map[string]hostedProofLimitsEntry)
	}
	c.entries[origin] = hostedProofLimitsEntry{limits: limits, storedAt: c.clock()}
}

func hostedProofLimitsFrom(limits *pb.ProofUploadLimits) server.HostedProofLimits {
	if limits == nil {
		return server.HostedProofLimits{}
	}
	return server.HostedProofLimits{
		FileCount: int64(max(limits.MaxFileCount, 0)),
		FileBytes: max(limits.MaxFileBytes, 0),
		PageBytes: max(limits.MaxPageBytes, 0),
	}
}
