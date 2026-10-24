package main

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	bossanovav1 "github.com/recurser/bossalib/gen/bossanova/v1"
)

const headlessCapabilityCacheTTL = 10 * time.Minute

// Only verified runtime surfaces enter the cache. Trust writes happen before
// this layer, so a hit skips the expensive inventory without skipping launch setup.
type headlessCapabilityCache struct {
	mu      sync.Mutex
	entries map[[sha256.Size]byte]capabilityCacheEntry
	pending map[[sha256.Size]byte]*capabilityProbe
	now     func() time.Time
}

type capabilityCacheEntry struct {
	surface runtimeOperationSurface
	expires time.Time
}

type capabilityProbe struct {
	done    chan struct{}
	surface runtimeOperationSurface
	err     error
}

func (c *headlessCapabilityCache) probe(ctx context.Context, profile bossanovav1.HeadlessCapabilityProfile, target codexRuntimeTarget, run func() (runtimeOperationSurface, error)) (runtimeOperationSurface, error) {
	key, err := capabilityFingerprint(profile, target)
	if err != nil {
		// An unreadable config cannot establish cache identity. Let the runtime
		// diagnose it rather than treating a fingerprint failure as a cached success.
		return run()
	}
	c.mu.Lock()
	now := time.Now
	if c.now != nil {
		now = c.now
	}
	for key, entry := range c.entries {
		if !now().Before(entry.expires) {
			delete(c.entries, key)
		}
	}
	if entry, ok := c.entries[key]; ok {
		c.mu.Unlock()
		return entry.surface, nil
	}
	if pending, ok := c.pending[key]; ok {
		c.mu.Unlock()
		select {
		case <-ctx.Done():
			return runtimeOperationSurface{}, ctx.Err()
		case <-pending.done:
			return pending.surface, pending.err
		}
	}
	if c.pending == nil {
		c.pending = make(map[[sha256.Size]byte]*capabilityProbe)
	}
	pending := &capabilityProbe{done: make(chan struct{})}
	c.pending[key] = pending
	c.mu.Unlock()

	pending.surface, pending.err = run()
	c.mu.Lock()
	if pending.err == nil {
		if c.entries == nil {
			c.entries = make(map[[sha256.Size]byte]capabilityCacheEntry)
		}
		c.entries[key] = capabilityCacheEntry{surface: pending.surface, expires: now().Add(headlessCapabilityCacheTTL)}
	}
	delete(c.pending, key)
	close(pending.done)
	c.mu.Unlock()
	return pending.surface, pending.err
}

type capabilityConfig struct {
	Present  bool
	Contents []byte
}

// Paths are omitted for worktrees deliberately: siblings with identical repo
// configuration share a verdict. Home remains part of the identity.
func capabilityFingerprint(profile bossanovav1.HeadlessCapabilityProfile, target codexRuntimeTarget) ([sha256.Size]byte, error) {
	home, err := filepath.Abs(target.Home)
	if err != nil {
		return [sha256.Size]byte{}, err
	}
	home, err = filepath.EvalSymlinks(home)
	if err != nil {
		return [sha256.Size]byte{}, err
	}
	configs := make([]capabilityConfig, 0, 2)
	paths := []string{filepath.Join(home, "config.toml"), filepath.Join(target.WorkDir, ".codex", "config.toml")}
	for i, path := range paths {
		if i == 1 && target.WorkDir == "" {
			path = ""
		}
		var config capabilityConfig
		if path != "" {
			content, err := os.ReadFile(path)
			if err != nil && !errors.Is(err, os.ErrNotExist) {
				return [sha256.Size]byte{}, err
			}
			config.Present = err == nil
			config.Contents = content
		}
		configs = append(configs, config)
	}
	env := make([]string, 0, len(target.ExtraEnv))
	for name, value := range target.ExtraEnv {
		env = append(env, name+"="+value)
	}
	sort.Strings(env)
	// JSON length-frames each field and sorts no secret-bearing text into logs.
	payload, err := json.Marshal(struct {
		Profile                    bossanovav1.HeadlessCapabilityProfile
		Home, Model, Effort, Trust string
		Env                        []string
		Configs                    []capabilityConfig
	}{profile, home, target.Model, target.Effort, target.ProjectTrustOverride, env, configs})
	if err != nil {
		return [sha256.Size]byte{}, err
	}
	return sha256.Sum256(payload), nil
}
