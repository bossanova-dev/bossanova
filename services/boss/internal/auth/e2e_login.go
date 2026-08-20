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
		// The access/refresh values are deliberately distinct from the ones
		// resolveE2ETokenStore seeds for an already-signed-in fixture. If the
		// seam re-saved the seeded token, a save that silently did nothing
		// would still satisfy verification's equality leg and the no-op would
		// go unnoticed.
		tokens := &Tokens{
			AccessToken:  "e2e-login-access-token",
			RefreshToken: "e2e-login-refresh-token",
			Email:        email,
			ExpiresAt:    time.Now().Add(1 * time.Hour),
		}
		// Record what we handed the store so commitLogin can verify this
		// branch on the same terms as the production one.
		m.lastE2ETokens = tokens
		return m.store.Save(tokens)
	}
}
