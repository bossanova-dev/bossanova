// Package cloudorigin holds the trust table of Bossanova Cloud orchestrator
// origins and checks whether a stored Bossanova Cloud login may be sent to a
// given orchestrator URL.
package cloudorigin

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strings"
)

const (
	workOSIssuerPrefix  = "https://api.workos.com/user_management/"
	fallbackOriginLabel = "the configured orchestrator URL"
)

var (
	// ErrUnknownOrchestrator reports that an orchestrator URL is not a known
	// Bossanova Cloud origin.
	ErrUnknownOrchestrator = errors.New("unknown Bossanova Cloud orchestrator")

	// ErrEnvironmentMismatch reports that a stored login token was not issued
	// for the Bossanova Cloud environment matching the target orchestrator.
	ErrEnvironmentMismatch = errors.New("stored login environment mismatch")
)

// Environment identifies a Bossanova Cloud deployment and the WorkOS client
// that issues access tokens for it.
type Environment struct {
	Name               string
	WorkOSClientID     string
	OrchestratorOrigin string
}

func (e Environment) issuer() string {
	return workOSIssuerPrefix + e.WorkOSClientID
}

var knownEnvironments = []Environment{
	{
		Name:               "production",
		WorkOSClientID:     "client_01KP805YXXAMZSN2YB4NGXS9XB",
		OrchestratorOrigin: "https://orchestrator.bossanova.dev",
	},
	{
		Name:               "staging",
		WorkOSClientID:     "client_01KP805YA85J1BJ4FZN39R7MRT",
		OrchestratorOrigin: "https://orchestrator-staging.bossanova.dev",
	},
}

type refusalError struct {
	sentinel error
	msg      string
}

func (e *refusalError) Error() string { return e.msg }
func (e *refusalError) Unwrap() error { return e.sentinel }

// KnownOrchestrator reports whether rawURL targets a known Bossanova Cloud
// orchestrator origin.
//
// Only https URLs with no userinfo and either no port or port 443 match. Path,
// query, fragment, and trailing slash are ignored.
func KnownOrchestrator(rawURL string) (Environment, bool) {
	if rawURL == "" {
		return Environment{}, false
	}
	u, err := url.Parse(rawURL)
	if err != nil || u.Opaque != "" || u.User != nil {
		return Environment{}, false
	}
	scheme := strings.ToLower(u.Scheme)
	if scheme != "https" {
		return Environment{}, false
	}
	host := strings.ToLower(u.Hostname())
	if host == "" {
		return Environment{}, false
	}
	port := u.Port()
	if port != "" && port != "443" {
		return Environment{}, false
	}
	rawHost := strings.ToLower(u.Host)
	if port == "" && rawHost != host {
		return Environment{}, false
	}
	if port == "443" && rawHost != host+":443" {
		return Environment{}, false
	}

	origin := scheme + "://" + host
	for _, env := range knownEnvironments {
		if origin == env.OrchestratorOrigin {
			return env, true
		}
	}
	return Environment{}, false
}

// OriginString formats rawURL as scheme and host (plus port when non-default
// for http/https) for user-facing messages and logs. Userinfo, path, query,
// and fragment are never included. Unparseable or hostless inputs fall back to
// a generic placeholder so raw values that may carry secrets are never echoed.
func OriginString(rawURL string) string {
	u, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil || u.Opaque != "" || u.Scheme == "" || u.Host == "" {
		return fallbackOriginLabel
	}
	scheme := strings.ToLower(u.Scheme)
	host := strings.ToLower(u.Hostname())
	if host == "" {
		return fallbackOriginLabel
	}
	port := u.Port()
	if (scheme == "https" && port == "443") || (scheme == "http" && port == "80") {
		port = ""
	}
	var hostPort string
	if port != "" {
		hostPort = net.JoinHostPort(host, port)
	} else if strings.Contains(host, ":") {
		hostPort = "[" + host + "]"
	} else {
		hostPort = host
	}
	return scheme + "://" + hostPort
}

// CheckOrchestrator verifies that rawURL targets a known Bossanova Cloud
// orchestrator origin. When it does not, the returned error wraps
// ErrUnknownOrchestrator with a user-facing message naming only the sanitized
// origin.
func CheckOrchestrator(rawURL string) (Environment, error) {
	env, ok := KnownOrchestrator(rawURL)
	if !ok {
		return Environment{}, &refusalError{
			sentinel: ErrUnknownOrchestrator,
			msg:      fmt.Sprintf("%s is not a Bossanova Cloud orchestrator, so your Bossanova Cloud login is not sent there.", OriginString(rawURL)),
		}
	}
	return env, nil
}

// CheckStoredToken verifies that orchestratorURL is a known Bossanova Cloud
// orchestrator origin and that accessToken's unverified issuer matches that
// environment's WorkOS client.
//
// Reading the issuer unverified decides only where not to send a stored
// credential; the orchestrator still verifies the token signature and claims.
func CheckStoredToken(orchestratorURL, accessToken string) error {
	env, err := CheckOrchestrator(orchestratorURL)
	if err != nil {
		return err
	}

	iss, ok := decodeJWTIssuer(accessToken)
	if ok && iss == env.issuer() {
		return nil
	}
	if ok {
		if tokenEnv, known := environmentByIssuer(iss); known {
			return &refusalError{
				sentinel: ErrEnvironmentMismatch,
				msg:      fmt.Sprintf("your stored login was issued for Bossanova Cloud %s, not %s.", tokenEnv.Name, env.Name),
			}
		}
	}
	return &refusalError{
		sentinel: ErrEnvironmentMismatch,
		msg:      fmt.Sprintf("your stored login was not issued for Bossanova Cloud %s.", env.Name),
	}
}

func environmentByIssuer(iss string) (Environment, bool) {
	for _, env := range knownEnvironments {
		if iss == env.issuer() {
			return env, true
		}
	}
	return Environment{}, false
}

func decodeJWTIssuer(token string) (string, bool) {
	if token == "" {
		return "", false
	}
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return "", false
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return "", false
	}
	var claims struct {
		Iss string `json:"iss"`
	}
	if err := json.Unmarshal(payload, &claims); err != nil {
		return "", false
	}
	if claims.Iss == "" {
		return "", false
	}
	return claims.Iss, true
}
