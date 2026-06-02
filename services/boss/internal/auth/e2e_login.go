//go:build e2e

package auth

import (
	"context"
	"time"
)

// SetE2ELogin installs fake device-code login behavior for e2e tests. It only
// affects the TUI login path (StartLogin/PollLogin); the package-level Login
// used by the `boss login` CLI command is unaffected and would still hit the
// real WorkOS endpoint. Add a CLI seam here if a CLI-login e2e test is needed.
func (m *Manager) SetE2ELogin(email string) {
	m.startLogin = func(context.Context) (*DeviceCodeResponse, error) {
		return &DeviceCodeResponse{
			DeviceCode:              "e2e-device-code",
			UserCode:                "E2E-CODE",
			VerificationURIComplete: "https://auth.example.test/device",
			ExpiresIn:               int((2 * time.Minute).Seconds()),
			Interval:                1,
		}, nil
	}
	m.pollLogin = func(ctx context.Context, _ string, interval int) error {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Duration(interval) * time.Second):
		}
		return m.store.Save(&Tokens{
			AccessToken:  "e2e-access-token",
			RefreshToken: "e2e-refresh-token",
			Email:        email,
			ExpiresAt:    time.Now().Add(1 * time.Hour),
		})
	}
}
