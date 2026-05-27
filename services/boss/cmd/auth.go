package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	"connectrpc.com/connect"
	"github.com/spf13/cobra"

	"github.com/recurser/boss/internal/auth"
	"github.com/recurser/boss/internal/client"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/telemetry"
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
const defaultCloudURL = "https://orchestrator.bossanova.dev"

const cloudGateMessage = "Bossanova Cloud requires an active subscription.\nLocal sessions are still available."

type cloudAccessClient interface {
	GetCloudAccessStatus(ctx context.Context) (*pb.CloudAccessStatus, error)
	CreateCheckoutSession(ctx context.Context, returnURL, cancelURL string) (string, error)
	RefreshCloudEntitlements(ctx context.Context) (*pb.CloudAccessStatus, error)
}

var openCloudCheckoutURL = auth.OpenBrowser

type cloudGateError struct {
	status *pb.CloudAccessStatus
	err    error
}

func (e *cloudGateError) Error() string {
	if e != nil && e.err != nil {
		return fmt.Sprintf("Bossanova Cloud status unavailable: %v\nLocal sessions are still available.", e.err)
	}
	if e != nil && e.status.GetMessage() != "" {
		return cloudGateMessage + "\n" + e.status.GetMessage()
	}
	return cloudGateMessage
}

func isCloudGateError(err error) bool {
	var gateErr *cloudGateError
	return errors.As(err, &gateErr)
}

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
	status := mgr.Status()
	captureAuthChangedWithEmail(cmd.Context(), commandTelemetryClient(cmd), "login", status.Email)

	// Notify daemon so it can connect upstream immediately.
	notifyDaemonAuthChange("login")

	token, err := mgr.AccessToken(ctx)
	if err != nil {
		return fmt.Errorf("access token: %w", err)
	}
	cloud := client.NewRemote(cloudURL(cmd), token)
	active := checkLoginCloudGateWithTelemetry(ctx, cloud, os.Stdout, commandTelemetryClient(cmd))
	if !active {
		return nil
	}

	if status.Email != "" {
		fmt.Printf("Logged in as %s\n", status.Email)
	} else {
		fmt.Println("Login successful!")
	}
	return nil
}

func cloudURL(cmd *cobra.Command) string {
	if cmd != nil && cmd.Root() != nil {
		if remote, err := cmd.Root().Flags().GetString("remote"); err == nil && remote != "" {
			return remote
		}
	}
	return envOr("BOSS_CLOUD_URL", defaultCloudURL)
}

func runLoginCloudGate(ctx context.Context, c cloudAccessClient, out io.Writer) {
	checkLoginCloudGate(ctx, c, out)
}

func checkLoginCloudGate(ctx context.Context, c cloudAccessClient, out io.Writer) bool {
	return checkLoginCloudGateWithTelemetry(ctx, c, out, nil)
}

func checkLoginCloudGateWithTelemetry(
	ctx context.Context,
	c cloudAccessClient,
	out io.Writer,
	telemetryClient telemetry.Client,
) bool {
	status, err := c.GetCloudAccessStatus(ctx)
	if err != nil {
		if connect.CodeOf(err) == connect.CodeUnavailable {
			return true
		}
		captureLoginCloudBillingEvent(
			ctx,
			telemetryClient,
			telemetry.EventCloudAccessDenied,
			nil,
			"cli_login",
			"billing_unavailable",
		)
		_, _ = fmt.Fprintf(out, "Cloud access status unavailable: %v\n", err)
		_, _ = fmt.Fprintln(out, cloudGateMessage)
		return false
	}
	if cloudAccessActive(status) {
		return true
	}
	if status.GetState() == pb.CloudAccessState_CLOUD_ACCESS_STATE_BILLING_UNAVAILABLE {
		return true
	}

	denialReason := cloudAccessDenialReason(status)
	captureLoginCloudBillingEvent(
		ctx,
		telemetryClient,
		telemetry.EventCloudAccessDenied,
		status,
		"cli_login",
		denialReason,
	)
	if status.GetState() == pb.CloudAccessState_CLOUD_ACCESS_STATE_PENDING_ENTITLEMENT_REFRESH {
		_, _ = fmt.Fprintln(out, cloudGateMessage)
		return false
	}
	checkoutURL, err := c.CreateCheckoutSession(ctx, cloudReturnURL(), cloudCancelURL())
	if err != nil {
		_, _ = fmt.Fprintf(out, "Cloud checkout unavailable: %v\n", err)
		_, _ = fmt.Fprintln(out, cloudGateMessage)
		return false
	}
	captureLoginCloudBillingEvent(
		ctx,
		telemetryClient,
		telemetry.EventCloudCheckoutStarted,
		status,
		"cli_login",
		denialReason,
	)
	if checkoutURL != "" {
		if err := openCloudCheckoutURL(checkoutURL); err != nil {
			_, _ = fmt.Fprintf(out, "Open checkout: %s\n", checkoutURL)
		}
	}
	_, _ = fmt.Fprintln(out, cloudGateMessage)
	return false
}

func captureLoginCloudBillingEvent(
	ctx context.Context,
	client telemetry.Client,
	event telemetry.Event,
	status *pb.CloudAccessStatus,
	entryPoint string,
	denialReason string,
) {
	if client == nil {
		return
	}
	if !commandTelemetryEnabled() {
		return
	}
	props := map[string]any{
		"product_area":       "billing",
		"cloud_access_state": cloudAccessStateAnalyticsName(status),
		"entry_point":        entryPoint,
		"denial_reason":      denialReason,
	}
	if status.GetWorkosOrgId() != "" {
		props["workos_org_id"] = status.GetWorkosOrgId()
	}
	client.Capture(ctx, event, commandDistinctID(), props)
}

func cloudAccessStateAnalyticsName(status *pb.CloudAccessStatus) string {
	switch status.GetState() {
	case pb.CloudAccessState_CLOUD_ACCESS_STATE_ACTIVE:
		return "active"
	case pb.CloudAccessState_CLOUD_ACCESS_STATE_NEEDS_SUBSCRIPTION:
		return "needs_subscription"
	case pb.CloudAccessState_CLOUD_ACCESS_STATE_PENDING_ENTITLEMENT_REFRESH:
		return "pending_entitlement_refresh"
	case pb.CloudAccessState_CLOUD_ACCESS_STATE_PAST_DUE:
		return "past_due"
	case pb.CloudAccessState_CLOUD_ACCESS_STATE_CANCELED:
		return "canceled"
	case pb.CloudAccessState_CLOUD_ACCESS_STATE_BILLING_UNAVAILABLE:
		return "billing_unavailable"
	case pb.CloudAccessState_CLOUD_ACCESS_STATE_UNSPECIFIED:
		return "unspecified"
	default:
		return "unknown"
	}
}

func cloudAccessDenialReason(status *pb.CloudAccessStatus) string {
	switch status.GetState() {
	case pb.CloudAccessState_CLOUD_ACCESS_STATE_NEEDS_SUBSCRIPTION:
		return "subscription_required"
	case pb.CloudAccessState_CLOUD_ACCESS_STATE_PAST_DUE:
		return "past_due"
	case pb.CloudAccessState_CLOUD_ACCESS_STATE_CANCELED:
		return "subscription_canceled"
	case pb.CloudAccessState_CLOUD_ACCESS_STATE_BILLING_UNAVAILABLE:
		return "billing_unavailable"
	case pb.CloudAccessState_CLOUD_ACCESS_STATE_PENDING_ENTITLEMENT_REFRESH:
		return "entitlement_pending"
	default:
		return "access_unavailable"
	}
}

func requireActiveCloudAccess(ctx context.Context, c cloudAccessClient) error {
	status, err := c.GetCloudAccessStatus(ctx)
	if err != nil {
		if connect.CodeOf(err) == connect.CodeUnavailable {
			return nil
		}
		return &cloudGateError{err: err}
	}
	if cloudAccessActive(status) || status.GetState() == pb.CloudAccessState_CLOUD_ACCESS_STATE_BILLING_UNAVAILABLE {
		return nil
	}
	return &cloudGateError{status: status}
}

func cloudAccessActive(status *pb.CloudAccessStatus) bool {
	return status.GetState() == pb.CloudAccessState_CLOUD_ACCESS_STATE_ACTIVE
}

func cloudReturnURL() string {
	return envOr("BOSS_CLOUD_RETURN_URL", "https://app.bossanova.dev/subscribe/success")
}

func cloudCancelURL() string {
	return envOr("BOSS_CLOUD_CANCEL_URL", "https://app.bossanova.dev/subscribe/canceled")
}

func runLogout(cmd *cobra.Command) error {
	mgr, err := newAuthManager(cmd)
	if err != nil {
		return err
	}
	status := mgr.Status()

	if err := mgr.Logout(); err != nil {
		return fmt.Errorf("logout: %w", err)
	}
	captureAuthChangedWithEmail(cmd.Context(), commandTelemetryClient(cmd), "logout", status.Email)

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
