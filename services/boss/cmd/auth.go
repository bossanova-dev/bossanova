package main

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/spf13/cobra"

	"github.com/recurser/boss/internal/auth"
	"github.com/recurser/boss/internal/client"
)

func loginCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "login",
		Short: "Log in to Bossanova cloud (WorkOS)",
		RunE: func(cmd *cobra.Command, args []string) error {
			return runLogin(cmd)
		},
	}
}

func logoutCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "logout",
		Short: "Log out and remove stored credentials",
		RunE: func(cmd *cobra.Command, args []string) error {
			return runLogout(cmd)
		},
	}
}

func authStatusCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "auth-status",
		Short: "Show authentication status",
		RunE: func(cmd *cobra.Command, args []string) error {
			return runAuthStatus(cmd)
		},
	}
}

// defaultWorkOSClientID is the production WorkOS device-code client that
// `boss login` uses when BOSS_WORKOS_CLIENT_ID is unset. WorkOS client IDs
// are public; the secret is held by WorkOS. Override for staging.
const defaultWorkOSClientID = "client_01KP805YXXAMZSN2YB4NGXS9XB"

func authConfig() auth.Config {
	return auth.Config{
		ClientID: envOr("BOSS_WORKOS_CLIENT_ID", defaultWorkOSClientID),
	}
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func newAuthManager(cmd *cobra.Command) (*auth.Manager, error) {
	var store auth.TokenStore
	if override := resolveE2ETokenStore(); override != nil {
		// Only reachable under the `e2e` build tag; see authstore_e2e.go.
		store = override
	} else {
		ks, err := auth.NewKeychainStore(allowInsecureKeyring(cmd))
		if err != nil {
			return nil, fmt.Errorf("open keychain: %w", err)
		}
		store = ks
	}

	cfg := authConfig()
	if cfg.ClientID == "" {
		return nil, fmt.Errorf("BOSS_WORKOS_CLIENT_ID must be set for cloud authentication")
	}

	return auth.NewManager(store, cfg), nil
}

// allowInsecureKeyring reads the --allow-insecure-keyring persistent flag if
// present. Returns false if the flag isn't registered (e.g. in tests).
func allowInsecureKeyring(cmd *cobra.Command) bool {
	if cmd == nil {
		return false
	}
	v, err := cmd.Flags().GetBool("allow-insecure-keyring")
	if err != nil {
		return false
	}
	return v
}

// newOptionalAuthManager returns an auth manager if BOSS_WORKOS_CLIENT_ID is set,
// or nil otherwise. Errors are swallowed so the TUI works without auth configured.
func newOptionalAuthManager(cmd *cobra.Command) *auth.Manager {
	mgr, err := newAuthManager(cmd)
	if err != nil {
		return nil
	}
	return mgr
}

func runLogin(cmd *cobra.Command) error {
	mgr, err := newAuthManager(cmd)
	if err != nil {
		return err
	}

	ctx := context.Background()

	if err := mgr.Login(ctx); err != nil {
		return fmt.Errorf("login: %w", err)
	}

	// Notify daemon so it can connect upstream immediately.
	notifyDaemonAuthChange("login")

	status := mgr.Status()
	if status.Email != "" {
		fmt.Printf("Logged in as %s\n", status.Email)
	} else {
		fmt.Println("Login successful!")
	}
	return nil
}

func runLogout(cmd *cobra.Command) error {
	mgr, err := newAuthManager(cmd)
	if err != nil {
		return err
	}

	if err := mgr.Logout(); err != nil {
		return fmt.Errorf("logout: %w", err)
	}

	// Notify daemon so it can disconnect upstream.
	notifyDaemonAuthChange("logout")

	fmt.Println("Logged out.")
	return nil
}

// notifyDaemonAuthChange is a best-effort notification to the daemon.
// Failures are ignored because the daemon may not be running.
func notifyDaemonAuthChange(action string) {
	socketPath, err := client.DefaultSocketPath()
	if err != nil {
		return
	}
	c := client.NewLocal(socketPath)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = c.NotifyAuthChange(ctx, action)
}

func runAuthStatus(cmd *cobra.Command) error {
	mgr, err := newAuthManager(cmd)
	if err != nil {
		return err
	}

	status := mgr.Status()
	if !status.LoggedIn {
		fmt.Println("Not logged in.")
		fmt.Println("Run 'boss login' to authenticate with Bossanova cloud.")
		return nil
	}

	fmt.Println("Logged in.")
	if status.Email != "" {
		fmt.Printf("  Email: %s\n", status.Email)
	}
	fmt.Printf("  Token expires: %s\n", status.ExpiresAt.Format(time.RFC3339))
	remaining := time.Until(status.ExpiresAt).Round(time.Second)
	if remaining > 0 {
		fmt.Printf("  Remaining: %s\n", remaining)
	} else {
		fmt.Println("  Token expired — will refresh on next request.")
	}
	return nil
}
