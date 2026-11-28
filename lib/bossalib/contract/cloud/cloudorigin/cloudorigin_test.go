package cloudorigin_test

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/recurser/bossalib/contract/cloud/cloudorigin"
)

const (
	prodClientID    = "client_01KP805YXXAMZSN2YB4NGXS9XB"
	stagingClientID = "client_01KP805YA85J1BJ4FZN39R7MRT"
	prodOrigin      = "https://orchestrator.bossanova.dev"
	stagingOrigin   = "https://orchestrator-staging.bossanova.dev"
)

func makeTestJWT(t *testing.T, claims any) string {
	t.Helper()
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"RS256","typ":"JWT"}`))
	payloadBytes, err := json.Marshal(claims)
	if err != nil {
		t.Fatalf("marshal claims: %v", err)
	}
	payload := base64.RawURLEncoding.EncodeToString(payloadBytes)
	sig := base64.RawURLEncoding.EncodeToString([]byte("test-signature-material"))
	return header + "." + payload + "." + sig
}

func TestKnownOrchestrator(t *testing.T) {
	accepted := []struct {
		name         string
		rawURL       string
		wantEnvName  string
		wantClientID string
		wantOrigin   string
	}{
		{
			name:         "production exact",
			rawURL:       "https://orchestrator.bossanova.dev",
			wantEnvName:  "production",
			wantClientID: prodClientID,
			wantOrigin:   prodOrigin,
		},
		{
			name:         "staging exact",
			rawURL:       "https://orchestrator-staging.bossanova.dev",
			wantEnvName:  "staging",
			wantClientID: stagingClientID,
			wantOrigin:   stagingOrigin,
		},
		{
			name:         "production trailing slash",
			rawURL:       "https://orchestrator.bossanova.dev/",
			wantEnvName:  "production",
			wantClientID: prodClientID,
			wantOrigin:   prodOrigin,
		},
		{
			name:         "staging trailing slash",
			rawURL:       "https://orchestrator-staging.bossanova.dev/",
			wantEnvName:  "staging",
			wantClientID: stagingClientID,
			wantOrigin:   stagingOrigin,
		},
		{
			name:         "production with path query and fragment",
			rawURL:       "https://orchestrator.bossanova.dev/bossanova.v1.OrchestratorService/RegisterDaemon?foo=bar#frag",
			wantEnvName:  "production",
			wantClientID: prodClientID,
			wantOrigin:   prodOrigin,
		},
		{
			name:         "uppercase scheme and host",
			rawURL:       "HTTPS://ORCHESTRATOR.BOSSANOVA.DEV/rpc",
			wantEnvName:  "production",
			wantClientID: prodClientID,
			wantOrigin:   prodOrigin,
		},
		{
			name:         "mixed case staging host",
			rawURL:       "HtTpS://Orchestrator-Staging.Bossanova.Dev/rpc",
			wantEnvName:  "staging",
			wantClientID: stagingClientID,
			wantOrigin:   stagingOrigin,
		},
		{
			name:         "production explicit 443",
			rawURL:       "https://orchestrator.bossanova.dev:443",
			wantEnvName:  "production",
			wantClientID: prodClientID,
			wantOrigin:   prodOrigin,
		},
		{
			name:         "staging explicit 443 with path",
			rawURL:       "https://orchestrator-staging.bossanova.dev:443/api/v1",
			wantEnvName:  "staging",
			wantClientID: stagingClientID,
			wantOrigin:   stagingOrigin,
		},
	}

	for _, tc := range accepted {
		t.Run("accept/"+tc.name, func(t *testing.T) {
			env, ok := cloudorigin.KnownOrchestrator(tc.rawURL)
			if !ok {
				t.Fatalf("KnownOrchestrator(%q) = false, want true", tc.rawURL)
			}
			if env.Name != tc.wantEnvName {
				t.Errorf("Name = %q, want %q", env.Name, tc.wantEnvName)
			}
			if env.WorkOSClientID != tc.wantClientID {
				t.Errorf("WorkOSClientID = %q, want %q", env.WorkOSClientID, tc.wantClientID)
			}
			if env.OrchestratorOrigin != tc.wantOrigin {
				t.Errorf("OrchestratorOrigin = %q, want %q", env.OrchestratorOrigin, tc.wantOrigin)
			}

			checkedEnv, err := cloudorigin.CheckOrchestrator(tc.rawURL)
			if err != nil {
				t.Fatalf("CheckOrchestrator(%q) unexpected error: %v", tc.rawURL, err)
			}
			if checkedEnv != env {
				t.Errorf("CheckOrchestrator(%q) = %+v, want %+v", tc.rawURL, checkedEnv, env)
			}
		})
	}

	rejected := []struct {
		name   string
		rawURL string
	}{
		{name: "http production", rawURL: "http://orchestrator.bossanova.dev"},
		{name: "http staging", rawURL: "http://orchestrator-staging.bossanova.dev"},
		{name: "http explicit 443", rawURL: "http://orchestrator.bossanova.dev:443"},
		{name: "userinfo username only", rawURL: "https://user@orchestrator.bossanova.dev"},
		{name: "userinfo username and password", rawURL: "https://user:pass@orchestrator.bossanova.dev"},
		{name: "empty userinfo", rawURL: "https://@orchestrator.bossanova.dev"},
		{name: "suffix look-alike", rawURL: "https://orchestrator.bossanova.dev.attacker.example"},
		{name: "subdomain look-alike", rawURL: "https://evil.orchestrator.bossanova.dev"},
		{name: "staging subdomain look-alike", rawURL: "https://staging.orchestrator.bossanova.dev"},
		{name: "non-443 port 8443", rawURL: "https://orchestrator.bossanova.dev:8443"},
		{name: "non-443 port 80", rawURL: "https://orchestrator.bossanova.dev:80"},
		{name: "leading zero port", rawURL: "https://orchestrator.bossanova.dev:0443"},
		{name: "dangling colon port", rawURL: "https://orchestrator.bossanova.dev:"},
		{name: "trailing dot fqdn", rawURL: "https://orchestrator.bossanova.dev."},
		{name: "empty", rawURL: ""},
		{name: "whitespace", rawURL: "   "},
		{name: "leading whitespace", rawURL: " https://orchestrator.bossanova.dev"},
		{name: "trailing whitespace", rawURL: "https://orchestrator.bossanova.dev "},
		{name: "missing scheme", rawURL: "orchestrator.bossanova.dev"},
		{name: "opaque url", rawURL: "https:orchestrator.bossanova.dev"},
		{name: "malformed scheme", rawURL: "://orchestrator.bossanova.dev"},
		{name: "empty host", rawURL: "https:///path"},
		{name: "loopback ipv4 http", rawURL: "http://127.0.0.1:8080"},
		{name: "loopback ipv4 https", rawURL: "https://127.0.0.1:443"},
		{name: "loopback localhost https 443", rawURL: "https://localhost:443"},
		{name: "loopback localhost http", rawURL: "http://localhost:8080"},
		{name: "loopback ipv6", rawURL: "https://[::1]:443"},
		{name: "other bossanova web host", rawURL: "https://app.bossanova.dev"},
		{name: "other bossanova mcp host", rawURL: "https://mcp.bossanova.dev"},
	}

	for _, tc := range rejected {
		t.Run("reject/"+tc.name, func(t *testing.T) {
			if env, ok := cloudorigin.KnownOrchestrator(tc.rawURL); ok {
				t.Fatalf("KnownOrchestrator(%q) = (%+v, true), want false", tc.rawURL, env)
			}
			if _, err := cloudorigin.CheckOrchestrator(tc.rawURL); !errors.Is(err, cloudorigin.ErrUnknownOrchestrator) {
				t.Fatalf("CheckOrchestrator(%q) err = %v, want ErrUnknownOrchestrator", tc.rawURL, err)
			}
		})
	}
}

func TestCheckStoredToken(t *testing.T) {
	prodToken := makeTestJWT(t, map[string]any{
		"iss": "https://api.workos.com/user_management/" + prodClientID,
		"sub": "user_prod_secret_subject",
		"aud": "bosso",
	})
	stagingToken := makeTestJWT(t, map[string]any{
		"iss": "https://api.workos.com/user_management/" + stagingClientID,
		"sub": "user_staging_secret_subject",
		"aud": "bosso",
	})
	unknownClientToken := makeTestJWT(t, map[string]any{
		"iss": "https://api.workos.com/user_management/client_UNKNOWN_999",
		"sub": "user_other",
	})
	foreignIssuerToken := makeTestJWT(t, map[string]any{
		"iss": "https://auth.attacker.example/issuer",
		"sub": "user_attacker",
	})
	missingIssuerToken := makeTestJWT(t, map[string]any{
		"sub": "user_no_iss",
	})
	nonStringIssuerToken := makeTestJWT(t, map[string]any{
		"iss": 12345,
	})
	malformedJSONToken := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"RS256"}`)) +
		"." + base64.RawURLEncoding.EncodeToString([]byte(`{not-json`)) +
		".sig"

	t.Run("accept matching environment tokens", func(t *testing.T) {
		cases := []struct {
			name  string
			url   string
			token string
		}{
			{"production exact", prodOrigin, prodToken},
			{"production uppercase and port 443", "HTTPS://ORCHESTRATOR.BOSSANOVA.DEV:443/path", prodToken},
			{"staging exact", stagingOrigin, stagingToken},
			{"staging trailing slash", stagingOrigin + "/", stagingToken},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				if err := cloudorigin.CheckStoredToken(tc.url, tc.token); err != nil {
					t.Fatalf("CheckStoredToken(%q) = %v, want nil", tc.url, err)
				}
			})
		}
	})

	t.Run("reject cross-environment tokens", func(t *testing.T) {
		cases := []struct {
			name     string
			url      string
			token    string
			wantMsg  string
			forbidIn []string
		}{
			{
				name:     "production token on staging origin",
				url:      stagingOrigin,
				token:    prodToken,
				wantMsg:  "your stored login was issued for Bossanova Cloud production, not staging.",
				forbidIn: []string{prodToken, prodClientID, "user_prod_secret_subject", "test-signature-material"},
			},
			{
				name:     "staging token on production origin",
				url:      prodOrigin,
				token:    stagingToken,
				wantMsg:  "your stored login was issued for Bossanova Cloud staging, not production.",
				forbidIn: []string{stagingToken, stagingClientID, "user_staging_secret_subject", "test-signature-material"},
			},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				err := cloudorigin.CheckStoredToken(tc.url, tc.token)
				if !errors.Is(err, cloudorigin.ErrEnvironmentMismatch) {
					t.Fatalf("CheckStoredToken err = %v, want ErrEnvironmentMismatch", err)
				}
				if err.Error() != tc.wantMsg {
					t.Errorf("err.Error() = %q, want %q", err.Error(), tc.wantMsg)
				}
				for _, forbidden := range tc.forbidIn {
					if strings.Contains(err.Error(), forbidden) {
						t.Errorf("error message leaked sensitive material %q: %q", forbidden, err.Error())
					}
				}
			})
		}
	})

	t.Run("reject unknown issuer and undecodable tokens", func(t *testing.T) {
		cases := []struct {
			name     string
			url      string
			token    string
			wantEnv  string
			forbidIn []string
		}{
			{
				name:     "unknown workos client",
				url:      prodOrigin,
				token:    unknownClientToken,
				wantEnv:  "production",
				forbidIn: []string{unknownClientToken, "client_UNKNOWN_999"},
			},
			{
				name:     "foreign issuer",
				url:      stagingOrigin,
				token:    foreignIssuerToken,
				wantEnv:  "staging",
				forbidIn: []string{foreignIssuerToken, "attacker.example"},
			},
			{
				name:     "missing iss claim",
				url:      prodOrigin,
				token:    missingIssuerToken,
				wantEnv:  "production",
				forbidIn: []string{missingIssuerToken, "user_no_iss"},
			},
			{
				name:     "non-string iss claim",
				url:      prodOrigin,
				token:    nonStringIssuerToken,
				wantEnv:  "production",
				forbidIn: []string{nonStringIssuerToken, "12345"},
			},
			{
				name:    "empty token",
				url:     prodOrigin,
				token:   "",
				wantEnv: "production",
			},
			{
				name:     "non-jwt opaque token",
				url:      prodOrigin,
				token:    "super-secret-bearer-value",
				wantEnv:  "production",
				forbidIn: []string{"super-secret-bearer-value"},
			},
			{
				name:     "two-part token",
				url:      prodOrigin,
				token:    "part1.part2",
				wantEnv:  "production",
				forbidIn: []string{"part1", "part2"},
			},
			{
				name:     "invalid base64 payload",
				url:      stagingOrigin,
				token:    "hdr.!!!bad-base64!!!.sig",
				wantEnv:  "staging",
				forbidIn: []string{"bad-base64", "hdr", "sig"},
			},
			{
				name:     "malformed json payload",
				url:      prodOrigin,
				token:    malformedJSONToken,
				wantEnv:  "production",
				forbidIn: []string{malformedJSONToken, "not-json"},
			},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				err := cloudorigin.CheckStoredToken(tc.url, tc.token)
				if !errors.Is(err, cloudorigin.ErrEnvironmentMismatch) {
					t.Fatalf("CheckStoredToken err = %v, want ErrEnvironmentMismatch", err)
				}
				if !strings.Contains(err.Error(), tc.wantEnv) {
					t.Errorf("err.Error() = %q, want it to mention %q", err.Error(), tc.wantEnv)
				}
				for _, forbidden := range tc.forbidIn {
					if strings.Contains(err.Error(), forbidden) {
						t.Errorf("error message leaked sensitive material %q: %q", forbidden, err.Error())
					}
				}
			})
		}
	})

	t.Run("reject unknown orchestrator before token inspection", func(t *testing.T) {
		cases := []struct {
			name     string
			url      string
			token    string
			wantMsg  string
			forbidIn []string
		}{
			{
				name:     "foreign https host strips path and query",
				url:      "https://evil.example/secret-path?api_key=leak",
				token:    prodToken,
				wantMsg:  "https://evil.example is not a Bossanova Cloud orchestrator, so your Bossanova Cloud login is not sent there.",
				forbidIn: []string{prodToken, "secret-path", "api_key", "leak"},
			},
			{
				name:     "foreign host with userinfo and non-default port",
				url:      "https://alice:hunter2@evil.example:8443/rpc",
				token:    prodToken,
				wantMsg:  "https://evil.example:8443 is not a Bossanova Cloud orchestrator, so your Bossanova Cloud login is not sent there.",
				forbidIn: []string{prodToken, "alice", "hunter2", "/rpc"},
			},
			{
				name:     "loopback server",
				url:      "http://127.0.0.1:8080/api",
				token:    prodToken,
				wantMsg:  "http://127.0.0.1:8080 is not a Bossanova Cloud orchestrator, so your Bossanova Cloud login is not sent there.",
				forbidIn: []string{prodToken, "/api"},
			},
			{
				name:     "known host over http",
				url:      "http://orchestrator.bossanova.dev/rpc",
				token:    prodToken,
				wantMsg:  "http://orchestrator.bossanova.dev is not a Bossanova Cloud orchestrator, so your Bossanova Cloud login is not sent there.",
				forbidIn: []string{prodToken, "/rpc"},
			},
			{
				name:     "known host on non-default port",
				url:      "https://orchestrator.bossanova.dev:80/rpc",
				token:    prodToken,
				wantMsg:  "https://orchestrator.bossanova.dev:80 is not a Bossanova Cloud orchestrator, so your Bossanova Cloud login is not sent there.",
				forbidIn: []string{prodToken, "/rpc"},
			},
			{
				name:     "unparseable url falls back safely",
				url:      "://secret-in-broken-url",
				token:    "raw-secret-token",
				wantMsg:  "the configured orchestrator URL is not a Bossanova Cloud orchestrator, so your Bossanova Cloud login is not sent there.",
				forbidIn: []string{"secret-in-broken-url", "raw-secret-token"},
			},
			{
				name:     "empty url falls back safely",
				url:      "",
				token:    "raw-secret-token",
				wantMsg:  "the configured orchestrator URL is not a Bossanova Cloud orchestrator, so your Bossanova Cloud login is not sent there.",
				forbidIn: []string{"raw-secret-token"},
			},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				err := cloudorigin.CheckStoredToken(tc.url, tc.token)
				if !errors.Is(err, cloudorigin.ErrUnknownOrchestrator) {
					t.Fatalf("CheckStoredToken(%q) err = %v, want ErrUnknownOrchestrator", tc.url, err)
				}
				if err.Error() != tc.wantMsg {
					t.Errorf("err.Error() = %q, want %q", err.Error(), tc.wantMsg)
				}
				for _, forbidden := range tc.forbidIn {
					if strings.Contains(err.Error(), forbidden) {
						t.Errorf("error message leaked sensitive material %q: %q", forbidden, err.Error())
					}
				}
			})
		}
	})
}

func TestOriginString(t *testing.T) {
	cases := []struct {
		rawURL string
		want   string
	}{
		{"https://orchestrator.bossanova.dev/path?q=1#f", "https://orchestrator.bossanova.dev"},
		{"HTTPS://EVIL.EXAMPLE:443/path", "https://evil.example"},
		{"http://evil.example:80/path", "http://evil.example"},
		{"https://evil.example:8443/path", "https://evil.example:8443"},
		{"https://user:pass@evil.example/path", "https://evil.example"},
		{"http://[::1]:8080/path", "http://[::1]:8080"},
		{"https://[::1]:443/path", "https://[::1]"},
		{"", "the configured orchestrator URL"},
		{"://broken", "the configured orchestrator URL"},
		{"just-a-token", "the configured orchestrator URL"},
	}
	for _, tc := range cases {
		if got := cloudorigin.OriginString(tc.rawURL); got != tc.want {
			t.Errorf("OriginString(%q) = %q, want %q", tc.rawURL, got, tc.want)
		}
	}
}
