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

	verdict, err := mgr.Login(ctx)
	if err != nil {
		return fmt.Errorf("login: %w", err)
	}

	// The cloud gate needs a usable access token, so it only runs once the
	// write has been verified. A denied gate still counts as a successful
	// login for telemetry and for the daemon, exactly as it did before.
	if verdict.Verified() {
		if cloudURL := cloudURL(cmd); cloudURL != "" {
			cloudAccess := resolveE2ECloudAccessClient()
			if cloudAccess == nil {
				token, err := mgr.AccessToken(ctx)
				if err != nil {
					return fmt.Errorf("access token: %w", err)
				}
				cloudAccess = client.NewRemote(cloudURL, token)
			}
			active := checkLoginCloudGateWithTelemetry(ctx, cloudAccess, cmd.OutOrStdout(), commandTelemetryClient(cmd))
			if !active {
				daemonNote := announceLoginSuccess(cmd, verdict.Email, notifyDaemonAuthChange)
				writeDaemonLoginNote(cmd, daemonNote)
				return nil
			}
		}
	}

	return renderLoginVerdict(cmd, verdict, verdict.Email, notifyDaemonAuthChange)
}

// announceLoginSuccess records the login for telemetry and nudges the daemon
// so it can connect upstream immediately. Both are best-effort side effects
// that only fire once the credential write has been verified.
//
// It returns the operator-facing note for whatever the daemon reported about
// the credentials it reloaded, or "" when there is nothing to say. The note is
// returned rather than printed here so each caller can emit it AFTER its own
// success line: the login did succeed, and a warning printed above "Logged in
// as ..." would read as though it had not.
func announceLoginSuccess(cmd *cobra.Command, email string, notify func(string) *pb.NotifyAuthChangeResponse) string {
	captureAuthChangedWithEmail(cmd.Context(), commandTelemetryClient(cmd), "login", email)
	if notify == nil {
		return ""
	}
	return renderDaemonLoginVerdict(notify("login"))
}

// renderDaemonLoginVerdict turns the daemon's post-login verdict into one line
// of operator-facing text, or "" when there is nothing worth saying.
//
// It is deliberately pure and total. Silence is the answer for nil (no daemon
// running, remote mode, or the RPC failed) and for OUTCOME_UNSPECIFIED (an
// older daemon, or one with no orchestrator configured) — an unknown verdict
// must never be rendered as a reassurance the daemon did not give. It is also
// the answer for OUTCOME_OK, which needs no commentary beyond the success line
// the caller already printed.
//
// Nothing here interpolates credential material: the only value that reaches
// the output is reloginReason, an enumerated persisted marker, and it is
// funnelled through auth.ReloginReasonDescription rather than printed raw.
func renderDaemonLoginVerdict(resp *pb.NotifyAuthChangeResponse) string {
	if resp == nil {
		return ""
	}
	switch resp.GetOutcome() {
	case pb.NotifyAuthChangeResponse_OUTCOME_CREDENTIALS_FLAGGED:
		// The reason arrives over the wire from a daemon this CLI does not
		// version-lock, so it is matched against the reasons THIS binary
		// knows and anything else renders without a cause. Handing an
		// unrecognised (or empty) value to auth.ReloginReasonDescription
		// would print "the stored refresh token was rejected" for it: that
		// helper is total by design for in-process callers, whose input is
		// always one of the two constants, and it must stay that way for
		// them — so the narrowing belongs here, at the trust boundary, not
		// in the helper. Reporting a specific cause the daemon never named
		// is exactly the "unknown verdict rendered as a claim" this
		// function's contract forbids.
		switch resp.GetReloginReason() {
		case auth.ReloginReasonRefreshOutcomeUnknown, auth.ReloginReasonRefreshTokenRejected:
			return fmt.Sprintf(
				"Warning: the daemon reloaded your credentials but they are still marked for re-login (%s), so background sync stays paused. Run `boss auth-status` for detail.",
				auth.ReloginReasonDescription(resp.GetReloginReason()),
			)
		default:
			return "Warning: the daemon reloaded your credentials but they are still marked for re-login, so background sync stays paused. Run `boss auth-status` for detail."
		}

	case pb.NotifyAuthChangeResponse_OUTCOME_CREDENTIALS_MISSING:
		return "Warning: the daemon found no usable credentials after this login, so background sync stays paused. This usually means the daemon reads a different keyring backend than this CLI wrote to."

	case pb.NotifyAuthChangeResponse_OUTCOME_REGISTER_FAILED:
		// Deliberately says nothing about the credentials. This outcome also
		// arrives when the daemon could not re-read the record at all and then
		// failed to register with the stale cache, and "the daemon accepted
		// your credentials" would be an assertion nothing verified. What the
		// operator needs either way is the same: the register failed, and it
		// retries on its own.
		return "Note: the daemon could not reach the orchestrator to re-register after this login; it will retry in the background."

	default:
		return ""
	}
}

// writeDaemonLoginNote emits a non-empty daemon note on stderr, leaving stdout
// to carry only the login result itself so a caller piping stdout still gets a
// clean answer.
func writeDaemonLoginNote(cmd *cobra.Command, note string) {
	if note == "" {
		return
	}
	_, _ = fmt.Fprintln(cmd.ErrOrStderr(), note)
}

// renderLoginVerdict turns a verification verdict into operator-facing output
// and an exit code. It is the single seam the CLI's login reporting is tested
// through: `notify` is injected so a test can drive daemon notifications and
// their verdicts without a daemon, the login result prints to
// cmd.OutOrStdout(), and the daemon's note prints to cmd.ErrOrStderr() — both
// capturable. Only the enumerated outcome and reason are rendered — the
// verdict's Err is never printed, because it may wrap a keyring error whose
// text embeds record bytes.
func renderLoginVerdict(cmd *cobra.Command, verdict auth.LoginVerification, email string, notify func(string) *pb.NotifyAuthChangeResponse) error {
	out := cmd.OutOrStdout()

	switch verdict.Outcome {
	case auth.LoginVerified:
		daemonNote := announceLoginSuccess(cmd, email, notify)
		if email != "" {
			_, _ = fmt.Fprintf(out, "Logged in as %s\n", email)
		} else {
			_, _ = fmt.Fprintln(out, "Login successful!")
		}
		// After the success line: the credential write DID land, and the
		// daemon's complaint is about what it could then do with it.
		writeDaemonLoginNote(cmd, daemonNote)
		return nil

	case auth.LoginVerifyInconclusive:
		// The write may well have landed, so this still counts as a login for
		// telemetry — but nothing downstream may assume a usable credential,
		// so the daemon is not notified and no success line is printed.
		captureAuthChangedWithEmail(cmd.Context(), commandTelemetryClient(cmd), "login", email)
		_, _ = fmt.Fprintf(out, "%s\n", verdict.Note())
		return nil

	case auth.LoginVerifyRecordNotUpdated:
		// Nothing was stored. No telemetry, no daemon notification, non-zero exit.
		return fmt.Errorf("login: %s", verdict.Note())

	default:
		// A verdict that was never filled in means verification did not run.
		// Report it rather than reporting a success nobody checked.
		return fmt.Errorf("login: %s", verdict.Note())
	}
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

// The organization surface below mirrors the GitHub App methods: the TUI reaches
// it by type-asserting App.cloudAccess, so these have to hang off the same
// client rather than off the daemon-local BossClient. Each dials a fresh remote
// the same way its neighbours do, so a token refresh is picked up per call.

func (c *authCloudAccessClient) ListOrganizations(ctx context.Context) ([]*pb.Organization, error) {
	remote, err := c.remote(ctx)
	if err != nil {
		return nil, err
	}
	return remote.ListOrganizations(ctx)
}

func (c *authCloudAccessClient) GetRepoOrganization(ctx context.Context, repoOriginURL string) (*pb.RepoOrganizationMapping, error) {
	remote, err := c.remote(ctx)
	if err != nil {
		return nil, err
	}
	return remote.GetRepoOrganization(ctx, repoOriginURL)
}

func (c *authCloudAccessClient) SetRepoOrganization(ctx context.Context, repoOriginURL, organizationID string) (*pb.RepoOrganizationMapping, error) {
	remote, err := c.remote(ctx)
	if err != nil {
		return nil, err
	}
	return remote.SetRepoOrganization(ctx, repoOriginURL, organizationID)
}

func (c *authCloudAccessClient) ClearRepoOrganization(ctx context.Context, repoOriginURL, organizationID string) error {
	remote, err := c.remote(ctx)
	if err != nil {
		return err
	}
	return remote.ClearRepoOrganization(ctx, repoOriginURL, organizationID)
}

func (c *authCloudAccessClient) GetGitHubAppInstallURL(ctx context.Context, returnURL string) (string, error) {
	remote, err := c.remote(ctx)
	if err != nil {
		return "", err
	}
	return remote.GetGitHubAppInstallURL(ctx, returnURL)
}

func (c *authCloudAccessClient) ListGitHubAppRepos(ctx context.Context) ([]*pb.GitHubAppRepoStatus, error) {
	remote, err := c.remote(ctx)
	if err != nil {
		return nil, err
	}
	return remote.ListGitHubAppRepos(ctx)
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

	if err := mgr.Logout(cmd.Context()); err != nil {
		return fmt.Errorf("logout: %w", err)
	}
	captureAuthChangedWithEmail(cmd.Context(), commandTelemetryClient(cmd), "logout", status.Email)

	// Notify daemon so it can disconnect upstream. Logout evaluates no
	// credentials, so the daemon has no verdict to report and there is nothing
	// to render.
	_ = notifyDaemonAuthChange("logout")

	fmt.Println("Logged out.")
	return nil
}

// notifyDaemonAuthChange is a best-effort notification to the daemon. Failures
// are ignored because the daemon may not be running — but the daemon's verdict
// on the credentials it reloaded is now returned rather than discarded, so a
// login that leaves the daemon unable to work can say so.
//
// Every failure path returns nil, which renderDaemonLoginVerdict renders as
// silence. That is the only safe default: "we could not ask" and "the daemon
// said it is fine" must not produce the same reassuring output.
func notifyDaemonAuthChange(action string) *pb.NotifyAuthChangeResponse {
	socketPath, err := daemonAuthSocketPath()
	if err != nil {
		return nil
	}
	c := newDaemonAuthNotifier(socketPath)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	resp, err := c.NotifyAuthChange(ctx, action)
	if err != nil {
		return nil
	}
	return resp
}

// daemonAuthNotifier is the sliver of the daemon client notifyDaemonAuthChange
// actually uses. It exists so the two failure paths above — no socket path and
// a failing RPC — are testable without a daemon; both are load-bearing,
// because `boss login` must stay successful and silent when no daemon is
// listening.
type daemonAuthNotifier interface {
	NotifyAuthChange(ctx context.Context, action string) (*pb.NotifyAuthChangeResponse, error)
}

// Indirections for tests only; production wiring is the local daemon client.
// Tests mutating these must not call t.Parallel(): they are shared mutable
// package state (same convention as the mcp.go seams, see saveMcpSeams).
var (
	daemonAuthSocketPath  = client.DefaultSocketPath
	newDaemonAuthNotifier = func(socketPath string) daemonAuthNotifier {
		return client.NewLocal(socketPath)
	}
)

func runAuthStatus(cmd *cobra.Command) error {
	mgr, err := newAuthManager(cmd)
	if err != nil {
		return err
	}

	_, _ = fmt.Fprint(cmd.OutOrStdout(), renderAuthStatus(mgr.Status()))
	return nil
}

// renderAuthStatus writes the three states `boss auth-status` can report:
// logged in, never logged in, and — since BOS-659 — credentials that are
// still stored but can no longer be used, so the user must sign in again.
//
// The retained state deliberately reads differently from both neighbours: it
// keeps the known identity on screen (that is the whole point of no longer
// deleting the keychain entry) while making clear the session is not usable.
// Only the enumerated, non-secret reason is rendered as prose — never a raw
// WorkOS payload, network error, or any token material.
func renderAuthStatus(status *auth.Status) string {
	var b strings.Builder
	if status.NeedsRelogin {
		b.WriteString("Sign in required.\n")
		if status.Email != "" {
			fmt.Fprintf(&b, "  Account: %s\n", status.Email)
		}
		fmt.Fprintf(&b, "  Reason: %s.\n", auth.ReloginReasonDescription(status.ReloginReason))
		b.WriteString("  Your stored credentials were kept, but they can no longer be used.\n")
		b.WriteString("Run 'boss login' to sign in again. Local sessions are still available.\n")
		return b.String()
	}

	if !status.LoggedIn {
		b.WriteString("Not logged in.\n")
		b.WriteString("Run 'boss login' to authenticate with Bossanova cloud.\n")
		return b.String()
	}

	b.WriteString("Logged in.\n")
	if status.Email != "" {
		fmt.Fprintf(&b, "  Email: %s\n", status.Email)
	}
	fmt.Fprintf(&b, "  Token expires: %s\n", status.ExpiresAt.Format(time.RFC3339))
	remaining := time.Until(status.ExpiresAt).Round(time.Second)
	if remaining > 0 {
		fmt.Fprintf(&b, "  Remaining: %s\n", remaining)
	} else {
		b.WriteString("  Token expired — will refresh on next request.\n")
	}
	return b.String()
}
