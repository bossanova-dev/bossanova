package github

import (
	"context"
	"errors"
	"os"
	"strconv"
	"strings"

	"github.com/recurser/bossd/internal/ghusage"
)

// InstallationTokenSource is what the adapter needs from bossd's per-repo
// GitHub credential source.
type InstallationTokenSource interface {
	// Token returns the repo's installation token, or ok=false meaning "use
	// the user's gh login".
	Token(ctx context.Context, nwo string) (InstallationToken, bool)
	// Invalidate drops a token GitHub rejected so the next Token call mints
	// afresh.
	Invalidate(nwo string, token string)
}

// InstallationToken is one repo's GitHub App installation token. Its String
// and GoString omit the token.
type InstallationToken struct {
	Value          string
	InstallationID int64
	// Permissions is GitHub's granted permissions, e.g. "statuses": "write".
	Permissions map[string]string
}

// String renders the token's metadata without the token.
func (t InstallationToken) String() string {
	return "github.InstallationToken{InstallationID:" + strconv.FormatInt(t.InstallationID, 10) + "}"
}

// GoString renders the token's metadata without the token, for %#v.
func (t InstallationToken) GoString() string { return t.String() }

// route says which identity a Provider method's gh calls may use.
type route int

const (
	// routeOperator always uses the user's gh login, so GitHub attributes the
	// action to the operator. It is the zero value: an unrouted call is a
	// user call, which is the pre-BOS-2265 behaviour.
	routeOperator route = iota
	// routeRead uses the repo's installation token when the source has one.
	routeRead
	// routeStatusWrite uses the installation token only when it was granted
	// statuses: write.
	routeStatusWrite
	// routeNone marks a method that makes no gh call.
	routeNone
)

// methodRoutes maps every exported *Provider method to its route. A method
// missing from it runs on the user's login; TestEveryProviderMethodHasARoute
// fails on one.
var methodRoutes = map[string]route{
	"GetPRStatus":               routeRead,
	"GetCheckResults":           routeRead,
	"GetCheckSet":               routeRead,
	"GetReviewComments":         routeRead,
	"GetReviewObservation":      routeRead,
	"GetReviewInlineComments":   routeRead,
	"BlockingThreadAuthors":     routeRead,
	"FindPRCommentsWithMarker":  routeRead,
	"ListOpenPRs":               routeRead,
	"ListClosedPRs":             routeRead,
	"SearchPRsByTitleTag":       routeRead,
	"GetBranchHeadSHA":          routeRead,
	"ListWorkflowRuns":          routeRead,
	"GetFailedCheckLogs":        routeRead,
	"GetPRMergeCommit":          routeRead,
	"GetAllowedMergeStrategies": routeOperator, // merge settings need contents: write; the installation token is read-only
	"OpenIssueURL":              routeRead,

	"PostCommitStatus": routeStatusWrite,

	"CreateDraftPR":      routeOperator,
	"MarkReadyForReview": routeOperator,
	"MergePR":            routeOperator,
	"UpdatePRTitle":      routeOperator,
	"UpdatePRBody":       routeOperator,
	// QuotaStatus reports the user's quotas, which the crons and gates spend.
	"QuotaStatus": routeOperator,

	"InvalidatePR":          routeNone,
	"InvalidateRepo":        routeNone,
	"SetWebhookHealth":      routeNone,
	"SetInstallationTokens": routeNone,
}

type routeCtxKey struct{}

// routeInfo is the route the outermost Provider method put on a call's ctx.
type routeInfo struct {
	method string
	route  route
	// nwo is the repo's owner/name, "" when it is not on github.com.
	nwo string
}

// withRoute marks ctx with method's route for the repo nwo. The outermost
// method wins, so an operator action's internal reads keep its user login.
func withRoute(ctx context.Context, method, nwo string) context.Context {
	if _, ok := ctx.Value(routeCtxKey{}).(routeInfo); ok {
		return ctx
	}
	return context.WithValue(ctx, routeCtxKey{}, routeInfo{method: method, route: methodRoutes[method], nwo: nwo})
}

// ghIdentity is the credential one gh call runs as.
type ghIdentity struct {
	// key is ghusage.IdentityUser or ghusage.InstallationIdentity; it keys the
	// breaker, the quota readings and the ledger.
	key   string
	token string
	nwo   string
}

var userIdentity = ghIdentity{key: ghusage.IdentityUser}

func (id ghIdentity) isUser() bool { return id.token == "" }

type identityKey struct{}

func withIdentity(ctx context.Context, id ghIdentity) context.Context {
	return context.WithValue(ctx, identityKey{}, id)
}

// identityFrom returns the identity the gh funnel resolved for ctx, the user
// login when none was.
func identityFrom(ctx context.Context) ghIdentity {
	if id, ok := ctx.Value(identityKey{}).(ghIdentity); ok {
		return id
	}
	return userIdentity
}

// ghTokenFrom is the installation token a gh call under ctx must run with, ""
// for the user login. defaultRunGH and in-package test fakes read it.
func ghTokenFrom(ctx context.Context) string { return identityFrom(ctx).token }

// tokenSourceBox lets an interface value sit in an atomic.Pointer.
type tokenSourceBox struct{ src InstallationTokenSource }

// SetInstallationTokens attaches the per-repo installation token source. It is
// safe to call after construction and concurrently with calls; nil detaches
// it, so every call uses the user's login.
func (p *Provider) SetInstallationTokens(src InstallationTokenSource) {
	if src == nil {
		p.tokens.Store(nil)
		return
	}
	p.tokens.Store(&tokenSourceBox{src: src})
}

// resolveIdentity picks the identity for one gh call from the route on ctx.
func (p *Provider) resolveIdentity(ctx context.Context) ghIdentity {
	info, ok := ctx.Value(routeCtxKey{}).(routeInfo)
	box := p.tokens.Load()
	if !ok || box == nil || info.nwo == "" || (info.route != routeRead && info.route != routeStatusWrite) {
		return userIdentity
	}
	tok, ok := box.src.Token(ctx, info.nwo)
	if !ok || tok.Value == "" {
		return userIdentity
	}
	if info.route == routeStatusWrite && tok.Permissions["statuses"] != "write" {
		return userIdentity
	}
	return ghIdentity{key: ghusage.InstallationIdentity(tok.InstallationID), token: tok.Value, nwo: info.nwo}
}

// gh runs one gh invocation as the identity the call's route allows. An
// installation token GitHub refuses (bad credentials, or a permission the
// token lacks) is retried once on the user login; see retryAsUser.
func (p *Provider) gh(ctx context.Context, args ...string) (string, error) {
	id := p.resolveIdentity(ctx)
	out, err := p.ghAs(withIdentity(ctx, id), id, args...)
	if id.isUser() || !isInstallationRefusal(err) {
		return out, err
	}
	return p.retryAsUser(ctx, id, err, args)
}

// retryAsUser re-runs a call GitHub refused for installation id once on the
// user login, through the user's breaker. A bad-credentials refusal also
// invalidates the token. It never logs the token or GitHub's response.
func (p *Provider) retryAsUser(ctx context.Context, id ghIdentity, refusal error, args []string) (string, error) {
	shape := "not_accessible_by_integration"
	if isBadCredentials(refusal) {
		shape = "bad_credentials"
		if box := p.tokens.Load(); box != nil {
			box.src.Invalidate(id.nwo, id.token)
		}
	}
	info, _ := ctx.Value(routeCtxKey{}).(routeInfo)
	p.logger.Warn().Str("method", info.method).Str("repo", id.nwo).Str("identity", id.key).Str("refusal", shape).
		Msg("github installation token refused; retrying once on the user login")
	return p.ghAs(withIdentity(ctx, userIdentity), userIdentity, args...)
}

// isInstallationRefusal reports whether an installation-token call failed
// because GitHub refused the token: HTTP 401 / bad credentials, or 403
// "resource not accessible by integration". A rate-limit refusal is not one.
func isInstallationRefusal(err error) bool {
	var limited *rateLimitedError
	if err == nil || errors.As(err, &limited) {
		return false
	}
	return isBadCredentials(err) || strings.Contains(strings.ToLower(ghResponseText(err)), "resource not accessible by integration")
}

func isBadCredentials(err error) bool {
	msg := strings.ToLower(ghResponseText(err))
	return strings.Contains(msg, "http 401") || strings.Contains(msg, "bad credentials")
}

// tokenEnvVars are every variable gh reads a token from.
var tokenEnvVars = []string{"GH_TOKEN", "GITHUB_TOKEN", "GH_ENTERPRISE_TOKEN", "GITHUB_ENTERPRISE_TOKEN"}

// ghEnv is the environment for a gh process under ctx: nil (inherit the
// daemon's) for the user login, otherwise the daemon's environment with every
// inherited token removed and GH_TOKEN set to the installation token. The
// token is set only on that process, never on the daemon.
func ghEnv(ctx context.Context) []string {
	token := ghTokenFrom(ctx)
	if token == "" {
		return nil
	}
	environ := os.Environ()
	env := make([]string, 0, len(environ)+1)
	for _, kv := range environ {
		name, _, _ := strings.Cut(kv, "=")
		if !isTokenEnvVar(name) {
			env = append(env, kv)
		}
	}
	return append(env, "GH_TOKEN="+token)
}

func isTokenEnvVar(name string) bool {
	for _, v := range tokenEnvVars {
		if name == v {
			return true
		}
	}
	return false
}
