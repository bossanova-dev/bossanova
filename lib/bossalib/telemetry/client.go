package telemetry

import (
	"context"
	"fmt"

	"github.com/posthog/posthog-go"
	"github.com/recurser/bossalib/config"
	"github.com/rs/zerolog/log"
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
	Alias(ctx context.Context, alias, distinctID string)
	Close()
}

type noopClient struct{}

func (noopClient) Capture(context.Context, Event, string, map[string]any) {}
func (noopClient) Identify(context.Context, string, map[string]any)       {}
func (noopClient) Alias(context.Context, string, string)                  {}
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
	inner, err := posthog.NewWithConfig(cfg.ProjectToken, postHogConfig(cfg.Host))
	if err != nil {
		log.Warn().Err(err).Msg("posthog telemetry disabled")
		return noopClient{}
	}
	return &postHogClient{cfg: cfg, inner: inner}
}

func postHogConfig(host string) posthog.Config {
	return posthog.Config{
		Endpoint: host,
		Logger:   postHogLogger{},
	}
}

type postHogLogger struct{}

func (postHogLogger) Debugf(format string, args ...interface{}) {
	log.Debug().Str("component", "posthog").Msg(fmt.Sprintf(format, args...))
}

func (postHogLogger) Logf(format string, args ...interface{}) {
	log.Info().Str("component", "posthog").Msg(fmt.Sprintf(format, args...))
}

func (postHogLogger) Warnf(format string, args ...interface{}) {
	log.Warn().Str("component", "posthog").Msg(fmt.Sprintf(format, args...))
}

func (postHogLogger) Errorf(format string, args ...interface{}) {
	log.Error().Str("component", "posthog").Msg(fmt.Sprintf(format, args...))
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
		log.Warn().Err(err).Str("event", string(event)).Msg("posthog capture enqueue failed")
	}
}

func (c *postHogClient) Identify(ctx context.Context, distinctID string, properties map[string]any) {
	_ = ctx
	if distinctID == "" {
		return
	}
	props := FilterIdentifyProperties(properties)
	if err := c.inner.Enqueue(posthog.Identify{
		DistinctId: distinctID,
		Properties: posthog.Properties(props),
	}); err != nil {
		log.Warn().Err(err).Msg("posthog identify enqueue failed")
	}
}

func (c *postHogClient) Alias(_ context.Context, alias, distinctID string) {
	if c == nil || c.inner == nil || alias == "" || distinctID == "" || alias == distinctID {
		return
	}
	if err := c.inner.Enqueue(posthog.Alias{Alias: alias, DistinctId: distinctID}); err != nil {
		log.Warn().Err(err).Msg("posthog alias enqueue failed")
	}
}

func (c *postHogClient) Close() {
	if err := c.inner.Close(); err != nil {
		log.Warn().Err(err).Msg("posthog close failed")
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

func FilterIdentifyProperties(properties map[string]any) map[string]any {
	filtered := FilterProperties(properties)
	if email, ok := properties["email"]; ok && isSafeScalar(email) {
		filtered["email"] = email
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
