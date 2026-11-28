package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/recurser/bossd/api/upstream"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
)

func TestResolveUpstreamConfigRefusesUnknownOrchestratorWithoutUserJWT(t *testing.T) {
	t.Setenv("BOSSD_ORCHESTRATOR_URL", "https://untrusted.example/path?secret=abc")
	t.Setenv("BOSSD_USER_JWT", "")

	var logBuf bytes.Buffer
	origLogger := log.Logger
	log.Logger = zerolog.New(&logBuf)
	t.Cleanup(func() { log.Logger = origLogger })

	if cfg := resolveUpstreamConfig(); cfg != nil {
		t.Fatalf("resolveUpstreamConfig() = %+v, want nil (local-only)", cfg)
	}

	logged := logBuf.String()
	for _, want := range []string{"https://untrusted.example", "BOSSD_USER_JWT", "local-only"} {
		if !strings.Contains(logged, want) {
			t.Fatalf("error log missing %q:\n%s", want, logged)
		}
	}
	if strings.Contains(logged, "secret=abc") || strings.Contains(logged, "/path") {
		t.Fatalf("error log leaked path or query string:\n%s", logged)
	}

	t.Setenv("BOSSD_USER_JWT", "explicit-jwt")
	cfg := resolveUpstreamConfig()
	if cfg == nil {
		t.Fatal("resolveUpstreamConfig() with BOSSD_USER_JWT = nil, want non-nil config")
	}
	if cfg.UserJWT != "explicit-jwt" {
		t.Fatalf("cfg.UserJWT = %q, want explicit-jwt", cfg.UserJWT)
	}
}

func TestWireUpstreamTokensExplicitJWTBypassesKeychainOnCloud(t *testing.T) {
	const explicitJWT = "explicit-cloud-jwt"
	for _, origin := range []string{
		"https://orchestrator.bossanova.dev",
		"https://orchestrator-staging.bossanova.dev",
	} {
		t.Run(origin, func(t *testing.T) {
			cfg := &upstream.Config{
				OrchestratorURL: origin,
				UserJWT:         explicitJWT,
			}
			tokens, authToken := wireUpstreamTokens(cfg)
			if authToken != explicitJWT {
				t.Fatalf("authToken = %q, want %q", authToken, explicitJWT)
			}
			if tokens.provider != nil {
				t.Fatalf("tokens.provider = %T, want nil so streams do not consult keychain", tokens.provider)
			}
			if tokens.reloader != nil {
				t.Fatalf("tokens.reloader = %T, want nil", tokens.reloader)
			}
			if got := tokens.userToken(); got != explicitJWT {
				t.Fatalf("tokens.userToken() = %q, want %q", got, explicitJWT)
			}
			if got := upstream.RegisterJWT(tokens.provider, cfg); got != explicitJWT {
				t.Fatalf("RegisterJWT() = %q, want %q", got, explicitJWT)
			}
		})
	}
}
