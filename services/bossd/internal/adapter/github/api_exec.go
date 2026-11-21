package github

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"
)

const apiUserTokenTTL = time.Minute

type apiExecConfig struct {
	baseURL   string
	transport http.RoundTripper
	fallback  ghFunc
	userToken func(context.Context) (string, error)
}

type apiExecutor struct {
	client       *http.Client
	baseURL      string
	fallback     ghFunc
	userToken    func(context.Context) (string, error)
	now          func() time.Time
	tokenMu      sync.Mutex
	token        string
	tokenUntil   time.Time
	tokenPending chan struct{}
}

func newAPIExecutor(c apiExecConfig) *apiExecutor {
	if c.baseURL == "" {
		c.baseURL = "https://api.github.com/"
	}
	if c.transport == nil {
		if transport, ok := http.DefaultTransport.(*http.Transport); ok {
			clone := transport.Clone()
			clone.MaxIdleConnsPerHost = 32
			c.transport = clone
		} else {
			c.transport = http.DefaultTransport
		}
	}
	if c.fallback == nil {
		c.fallback = defaultRunGH
	}
	if c.userToken == nil {
		c.userToken = resolveAPIUserToken
	}
	return &apiExecutor{client: &http.Client{Transport: c.transport}, baseURL: c.baseURL, fallback: c.fallback, userToken: c.userToken, now: time.Now}
}

func withAPIExecConfig(c apiExecConfig) ProviderOption {
	return func(p *Provider) { p.runGH = newAPIExecutor(c).run }
}

func resolveAPIUserToken(ctx context.Context) (string, error) {
	cmd := exec.CommandContext(ctx, "gh", "auth", "token", "--hostname", "github.com")
	cmd.WaitDelay = ghWaitDelay
	var out bytes.Buffer
	cmd.Stdout = &out
	// Credential helper diagnostics may contain tokens. Never return them.
	if err := cmd.Run(); err != nil {
		return "", errors.New("GitHub user token unavailable")
	}
	return strings.TrimSpace(out.String()), nil
}

func (e *apiExecutor) userCredential(ctx context.Context) (string, error) {
	for {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		e.tokenMu.Lock()
		if e.token != "" && e.now().Before(e.tokenUntil) {
			token := e.token
			e.tokenMu.Unlock()
			return token, nil
		}
		if pending := e.tokenPending; pending != nil {
			e.tokenMu.Unlock()
			select {
			case <-ctx.Done():
				return "", ctx.Err()
			case <-pending:
				continue
			}
		}
		e.tokenPending = make(chan struct{})
		e.token = ""
		e.tokenMu.Unlock()

		token, err := e.userToken(ctx)
		e.tokenMu.Lock()
		if err == nil {
			e.token = strings.TrimSpace(token)
			e.tokenUntil = e.now().Add(apiUserTokenTTL)
		}
		token = e.token
		close(e.tokenPending)
		e.tokenPending = nil
		e.tokenMu.Unlock()
		return token, err
	}
}

func (e *apiExecutor) forgetUserToken(token string) {
	e.tokenMu.Lock()
	defer e.tokenMu.Unlock()
	if e.token == token {
		e.token = ""
	}
}

func (e *apiExecutor) run(ctx context.Context, args ...string) (string, error) {
	shape, ok := parseAPIArgs(args)
	if !ok || (os.Getenv("GH_HOST") != "" && os.Getenv("GH_HOST") != "github.com") {
		return e.fallback(ctx, args...)
	}
	token := ghTokenFrom(ctx)
	user := token == ""
	if user {
		var err error
		token, err = e.userCredential(ctx)
		if ctx.Err() != nil {
			return "", apiTransportError(args, ctx.Err(), token)
		}
		if err != nil || token == "" {
			return e.fallback(ctx, args...)
		}
	}
	req, err := shape.request(ctx, e.baseURL)
	if err != nil {
		return e.fallback(ctx, args...)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	// A caller-supplied Authorization header has the same precedence as gh's
	// explicit -H override. It lives on the request so redirects strip it.
	shape.applyHeaders(req)
	var out strings.Builder
	for {
		resp, err := e.client.Do(req)
		if err != nil {
			return "", apiTransportError(args, err, token)
		}
		body, readErr := io.ReadAll(resp.Body)
		closeErr := resp.Body.Close()
		if readErr != nil {
			return "", apiTransportError(args, readErr, token)
		}
		if closeErr != nil {
			return "", apiTransportError(args, closeErr, token)
		}
		if user && resp.StatusCode == http.StatusUnauthorized {
			e.forgetUserToken(token)
		}
		output := apiResponseOutput(resp, body, shape.include)
		out.WriteString(output)
		if stderr := apiResponseError(resp.StatusCode, body, shape.path == "graphql"); stderr != "" {
			return "", &ghError{args: args, stdout: out.String(), stderr: redactAPIToken(stderr, token), err: errors.New("exit status 1")}
		}
		next := nextAPIPage(resp.Header.Get("Link"))
		if !shape.paginate || next == "" {
			return out.String(), nil
		}
		nextURL, err := req.URL.Parse(next)
		if err != nil {
			return "", apiTransportError(args, err, token)
		}
		// Pagination only spends the resolved credential on its original API
		// origin. Redirects use net/http's own sensitive-header policy.
		if nextURL.Scheme != req.URL.Scheme || nextURL.Host != req.URL.Host {
			return "", apiTransportError(args, errors.New("GitHub pagination changed origin"), token)
		}
		req = req.Clone(ctx)
		req.URL = nextURL
		req.Method = http.MethodGet
		req.Body = nil
		req.GetBody = nil
		req.ContentLength = 0
	}
}

func apiTransportError(args []string, err error, token string) *ghError {
	// Keep cancellation discoverable without allowing a transport's error text
	// to echo a credential into a persisted blocked reason.
	return &ghError{args: args, stderr: redactAPIToken(err.Error(), token), err: apiRequestError{cause: err}}
}

type apiRequestError struct{ cause error }

func (e apiRequestError) Error() string { return "GitHub request failed" }
func (e apiRequestError) Unwrap() error { return e.cause }

func redactAPIToken(s, token string) string {
	if token != "" {
		return strings.ReplaceAll(s, token, "[REDACTED]")
	}
	return s
}

func apiJSONBody(fields map[string]any, graphql bool) ([]byte, error) {
	if graphql {
		grouped := map[string]any{}
		variables := map[string]any{}
		for key, value := range fields {
			if key == "query" || key == "operationName" {
				grouped[key] = value
			} else {
				variables[key] = value
			}
		}
		if len(variables) > 0 {
			grouped["variables"] = variables
		}
		return json.Marshal(grouped)
	}
	return json.Marshal(fields)
}
