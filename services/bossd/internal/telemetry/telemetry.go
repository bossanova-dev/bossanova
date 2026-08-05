package telemetry

import (
	"context"
	"os"
	"sync"

	"github.com/recurser/bossalib/config"
	"github.com/recurser/bossalib/safego"
	libtelemetry "github.com/recurser/bossalib/telemetry"
	"github.com/rs/zerolog"
)

// Client applies telemetry settings changed through UpdateSettings without
// requiring a daemon restart. All daemon services share this client.
type Client struct {
	mu        sync.RWMutex
	config    libtelemetry.Config
	current   libtelemetry.Client
	retired   sync.WaitGroup
	closeOnce sync.Once
	closed    bool
}

// NewClient creates a telemetry client backed by the supplied settings.
func NewClient(settings config.Settings) *Client {
	cfg := ConfigFromSettings(settings)
	return &Client{config: cfg, current: libtelemetry.New(cfg)}
}

// Update replaces the underlying client after telemetry settings change.
func (c *Client) Update(settings config.Settings) {
	c.Refresh(settings)
}

// Refresh replaces the underlying client only when its configuration changed.
// Capture calls this after loading settings so direct config saves made by the
// TUI take effect without waiting for a daemon restart.
func (c *Client) Refresh(settings config.Settings) {
	if c == nil {
		return
	}
	cfg := ConfigFromSettings(settings)
	c.mu.Lock()
	if c.closed || c.config == cfg {
		c.mu.Unlock()
		return
	}
	previous := c.current
	c.config = cfg
	c.current = libtelemetry.New(cfg)
	if previous != nil {
		c.retired.Add(1)
	}
	c.mu.Unlock()
	if previous != nil {
		safego.Go(zerolog.Nop(), func() {
			defer c.retired.Done()
			previous.Close()
		})
	}
}

func (c *Client) Capture(ctx context.Context, event libtelemetry.Event, distinctID string, properties map[string]any) {
	if c == nil {
		return
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.current != nil {
		c.current.Capture(ctx, event, distinctID, properties)
	}
}

func (c *Client) Identify(ctx context.Context, distinctID string, properties map[string]any) {
	if c == nil {
		return
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.current != nil {
		c.current.Identify(ctx, distinctID, properties)
	}
}

func (c *Client) Alias(ctx context.Context, alias, distinctID string) {
	if c == nil {
		return
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.current != nil {
		c.current.Alias(ctx, alias, distinctID)
	}
}

func (c *Client) Close() {
	if c == nil {
		return
	}
	c.closeOnce.Do(func() {
		c.mu.Lock()
		c.closed = true
		current := c.current
		c.current = nil
		c.mu.Unlock()
		if current != nil {
			current.Close()
		}
		c.retired.Wait()
	})
}

// Capture records an opted-in daemon event. Telemetry is best-effort and
// disabled when configuration cannot be loaded.
func Capture(ctx context.Context, client libtelemetry.Client, event libtelemetry.Event, props map[string]any) {
	if client == nil {
		return
	}
	settings, err := config.Load()
	if err != nil {
		return
	}
	if refreshingClient, ok := client.(interface{ Refresh(config.Settings) }); ok {
		refreshingClient.Refresh(settings)
	}
	if !settings.EventTracingEnabled {
		return
	}
	withSource := make(map[string]any, len(props)+1)
	for key, value := range props {
		withSource[key] = value
	}
	withSource["source"] = "daemon"
	hostname, err := os.Hostname()
	if err != nil {
		hostname = ""
	}
	client.Capture(ctx, event, libtelemetry.DaemonDistinctID(hostname), withSource)
}

func ConfigFromSettings(s config.Settings) libtelemetry.Config {
	return libtelemetry.FromSettings(s, "bossd")
}
