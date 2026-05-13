package telemetry

import (
	"context"
	"log/slog"

	"github.com/posthog/posthog-go"
	"github.com/recurser/bossalib/config"
)

type Config struct {
	Enabled      bool
	ProjectToken string
	Host         string
	App          string
	Environment  string
}

type Client interface {
	Capture(ctx context.Context, event Event, distinctID string, properties map[string]any)
	Identify(ctx context.Context, distinctID string, properties map[string]any)
	Close()
}

type noopClient struct{}

func (noopClient) Capture(context.Context, Event, string, map[string]any) {}
func (noopClient) Identify(context.Context, string, map[string]any)       {}
func (noopClient) Close()                                                 {}

type postHogClient struct {
	cfg   Config
	inner posthog.Client
}

func FromSettings(settings config.Settings, app string) Config {
	cfg := Config{
		Enabled:      settings.EventTracingEnabled,
		ProjectToken: settings.PostHogProjectToken,
		Host:         settings.PostHogHost,
		App:          app,
		Environment:  "local",
	}
	if cfg.Enabled {
		if cfg.ProjectToken == "" {
			cfg.ProjectToken = ProductionProjectToken
		}
		if cfg.Host == "" {
			cfg.Host = DefaultHost
		}
	}
	return cfg
}

func FromEnv(app, environment, token, host string) Config {
	if host == "" {
		host = DefaultHost
	}
	return Config{
		Enabled:      token != "",
		ProjectToken: token,
		Host:         host,
		App:          app,
		Environment:  environment,
	}
}

func New(cfg Config) Client {
	if !cfg.Enabled || cfg.ProjectToken == "" {
		return noopClient{}
	}
	inner, err := posthog.NewWithConfig(cfg.ProjectToken, posthog.Config{Endpoint: cfg.Host})
	if err != nil {
		slog.Warn("posthog telemetry disabled", "error", err)
		return noopClient{}
	}
	return &postHogClient{cfg: cfg, inner: inner}
}

func (c *postHogClient) Capture(ctx context.Context, event Event, distinctID string, properties map[string]any) {
	_ = ctx
	if !IsAllowed(event) || distinctID == "" {
		return
	}
	props := FilterProperties(properties)
	props["app"] = c.cfg.App
	props["environment"] = c.cfg.Environment
	if err := c.inner.Enqueue(posthog.Capture{
		DistinctId: distinctID,
		Event:      string(event),
		Properties: posthog.Properties(props),
	}); err != nil {
		slog.Warn("posthog capture enqueue failed", "event", event, "error", err)
	}
}

func (c *postHogClient) Identify(ctx context.Context, distinctID string, properties map[string]any) {
	_ = ctx
	if distinctID == "" {
		return
	}
	props := FilterProperties(properties)
	if err := c.inner.Enqueue(posthog.Identify{
		DistinctId: distinctID,
		Properties: posthog.Properties(props),
	}); err != nil {
		slog.Warn("posthog identify enqueue failed", "error", err)
	}
}

func (c *postHogClient) Close() {
	if err := c.inner.Close(); err != nil {
		slog.Warn("posthog close failed", "error", err)
	}
}

func FilterProperties(properties map[string]any) map[string]any {
	filtered := make(map[string]any, len(properties))
	for key, value := range properties {
		if !isAllowedProperty(key) || !isSafeScalar(value) {
			continue
		}
		filtered[key] = value
	}
	return filtered
}

func isAllowedProperty(key string) bool {
	switch key {
	case "action",
		"authenticated",
		"command",
		"context_has_error",
		"ok",
		"report_id",
		"resume",
		"source",
		"status":
		return true
	}
	return false
}

func isSafeScalar(value any) bool {
	switch value.(type) {
	case string,
		bool,
		int, int8, int16, int32, int64,
		uint, uint8, uint16, uint32, uint64, uintptr,
		float32, float64:
		return true
	default:
		return false
	}
}
