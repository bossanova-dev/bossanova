package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"strings"
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

type appCloudAccessClient interface {
	cloudAccessClient
	CreateBillingPortalSession(ctx context.Context, returnURL string) (string, error)
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

	mgr := auth.NewManager(store, cfg)
	if email := resolveE2ELoginEmail(); email != "" {
		mgr.SetE2ELogin(email)
	}
	return mgr, nil
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

	if cloudURL := cloudURL(cmd); cloudURL != "" {
		cloudAccess := resolveE2ECloudAccessClient()
		if cloudAccess == nil {
			token, err := mgr.AccessToken(ctx)
			if err != nil {
				return fmt.Errorf("access token: %w", err)
			}
			cloudAccess = client.NewRemote(cloudURL, token)
		}
		active := checkLoginCloudGateWithTelemetry(ctx, cloudAccess, os.Stdout, commandTelemetryClient(cmd))
		if !active {
			return nil
		}
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
	if cloud := os.Getenv("BOSS_CLOUD_URL"); cloud != "" {
		return cloud
	}
	if cloud, ok := os.LookupEnv("BOSSD_ORCHESTRATOR_URL"); ok {
		return cloud
	}
	return defaultCloudURL
}

type authCloudAccessClient struct {
	mgr *auth.Manager
	url string
}

func newAuthCloudAccessClient(mgr *auth.Manager, url string) appCloudAccessClient {
	if cloudAccess := resolveE2ECloudAccessClient(); cloudAccess != nil {
		if appCloudAccess, ok := cloudAccess.(appCloudAccessClient); ok {
			return appCloudAccess
		}
	}
	return &authCloudAccessClient{mgr: mgr, url: url}
}

func (c *authCloudAccessClient) remote(ctx context.Context) (*client.RemoteClient, error) {
	token, err := c.mgr.AccessToken(ctx)
	if err != nil {
		return nil, err
	}
	if token == "" {
		return nil, errors.New("not logged in")
	}
	return client.NewRemote(c.url, token), nil
}

func (c *authCloudAccessClient) GetCloudAccessStatus(ctx context.Context) (*pb.CloudAccessStatus, error) {
	remote, err := c.remote(ctx)
	if err != nil {
		return nil, err
	}
	return remote.GetCloudAccessStatus(ctx)
}

func (c *authCloudAccessClient) CreateCheckoutSession(ctx context.Context, returnURL, cancelURL string) (string, error) {
	remote, err := c.remote(ctx)
	if err != nil {
		return "", err
	}
	return remote.CreateCheckoutSession(ctx, returnURL, cancelURL)
}

func (c *authCloudAccessClient) CreateBillingPortalSession(ctx context.Context, returnURL string) (string, error) {
	remote, err := c.remote(ctx)
	if err != nil {
		return "", err
	}
	return remote.CreateBillingPortalSession(ctx, returnURL)
}

func (c *authCloudAccessClient) RefreshCloudEntitlements(ctx context.Context) (*pb.CloudAccessStatus, error) {
	remote, err := c.remote(ctx)
	if err != nil {
		return nil, err
	}
	return remote.RefreshCloudEntitlements(ctx)
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
	subscribeURL := cloudSubscribeURL()
	if subscribeURL != "" {
		if err := openCloudCheckoutURL(subscribeURL); err != nil {
			_, _ = fmt.Fprintf(out, "Open subscription page: %s\n", subscribeURL)
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
	return cloudURLWithSource(envOr("BOSS_CLOUD_RETURN_URL", "https://app.bossanova.dev/subscribe/success"), "cli")
}

func cloudCancelURL() string {
	return cloudURLWithSource(envOr("BOSS_CLOUD_CANCEL_URL", "https://app.bossanova.dev/subscribe/canceled"), "cli")
}

func cloudSubscribeURL() string {
	return cloudURLWithSource(envOr("BOSS_CLOUD_SUBSCRIBE_URL", defaultCloudSubscribeURL()), "cli")
}

func defaultCloudSubscribeURL() string {
	if web := os.Getenv("BOSS_WEB_URL"); web != "" {
		return strings.TrimRight(web, "/") + "/subscribe"
	}
	if port := os.Getenv("BOSS_WEB_PORT"); port != "" {
		return "http://localhost:" + port + "/subscribe"
	}
	remote := envOr("BOSS_CLOUD_URL", envOr("BOSSD_ORCHESTRATOR_URL", ""))
	if strings.Contains(remote, "localhost") || strings.Contains(remote, "127.0.0.1") {
		port := os.Getenv("BOSS_WEB_PORT")
		if port == "" {
			port = "5151"
		}
		return "http://localhost:" + port + "/subscribe"
	}
	if strings.Contains(remote, "orchestrator-staging.bossanova.dev") {
		return "https://app-staging.bossanova.dev/subscribe"
	}
	return "https://app.bossanova.dev/subscribe"
}

func cloudURLWithSource(rawURL, source string) string {
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return rawURL
	}
	values := parsed.Query()
	values.Set("source", source)
	parsed.RawQuery = values.Encode()
	return parsed.String()
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
