package main

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/spf13/cobra"

	"github.com/recurser/boss/internal/auth"
)

func loginCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "login",
		Short: "Log in to Bossanova cloud (Auth0 PKCE)",
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

func authConfig() auth.Config {
	return auth.Config{
		Issuer:   envOr("BOSS_OIDC_ISSUER", ""),
		ClientID: envOr("BOSS_OIDC_CLIENT_ID", ""),
		Audience: envOr("BOSS_OIDC_AUDIENCE", ""),
	}
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func newAuthManager() (*auth.Manager, error) {
	store, err := auth.NewKeychainStore()
	if err != nil {
		return nil, fmt.Errorf("open keychain: %w", err)
	}

	cfg := authConfig()
	if cfg.Issuer == "" || cfg.ClientID == "" {
		return nil, fmt.Errorf("BOSS_OIDC_ISSUER and BOSS_OIDC_CLIENT_ID must be set for cloud authentication")
	}

	return auth.NewManager(store, cfg), nil
}

func runLogin(_ *cobra.Command) error {
	mgr, err := newAuthManager()
	if err != nil {
		return err
	}

	fmt.Println("Opening browser for authentication...")
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	if err := mgr.Login(ctx); err != nil {
		return fmt.Errorf("login: %w", err)
	}

	status := mgr.Status()
	if status.Email != "" {
		fmt.Printf("Logged in as %s\n", status.Email)
	} else {
		fmt.Println("Login successful!")
	}
	return nil
}

func runLogout(_ *cobra.Command) error {
	mgr, err := newAuthManager()
	if err != nil {
		return err
	}

	if err := mgr.Logout(); err != nil {
		return fmt.Errorf("logout: %w", err)
	}
	fmt.Println("Logged out.")
	return nil
}

func runAuthStatus(_ *cobra.Command) error {
	mgr, err := newAuthManager()
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
