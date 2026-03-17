package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os/exec"
	"runtime"
	"strings"
	"time"
)

// Config holds OIDC provider configuration.
type Config struct {
	Issuer   string // e.g. "https://bossanova.us.auth0.com/"
	ClientID string // Auth0 SPA/native app client ID
	Audience string // e.g. "https://api.bossanova.dev"
}

// tokenResponse is the JSON body from the token endpoint.
type tokenResponse struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	IDToken      string `json:"id_token"`
	ExpiresIn    int    `json:"expires_in"`
	TokenType    string `json:"token_type"`
	Error        string `json:"error"`
	ErrorDesc    string `json:"error_description"`
}

// Login performs the PKCE authorization code flow:
//  1. Generate code_verifier + code_challenge.
//  2. Start a local HTTP server on a random port.
//  3. Open the browser to the authorization URL.
//  4. Wait for the callback with the authorization code.
//  5. Exchange the code for tokens.
func Login(ctx context.Context, cfg Config) (*Tokens, error) {
	// Generate PKCE pair.
	verifier, err := generateCodeVerifier()
	if err != nil {
		return nil, fmt.Errorf("generate verifier: %w", err)
	}
	challenge := codeChallenge(verifier)

	// Start local callback server.
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("listen: %w", err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	redirectURI := fmt.Sprintf("http://127.0.0.1:%d/callback", port)

	// Generate state for CSRF protection.
	state, err := randomString(32)
	if err != nil {
		return nil, fmt.Errorf("generate state: %w", err)
	}

	// Build authorization URL.
	authURL := buildAuthURL(cfg, redirectURI, state, challenge)

	// Channel for the authorization code.
	codeCh := make(chan string, 1)
	errCh := make(chan error, 1)

	mux := http.NewServeMux()
	mux.HandleFunc("/callback", func(w http.ResponseWriter, r *http.Request) {
		if errMsg := r.URL.Query().Get("error"); errMsg != "" {
			desc := r.URL.Query().Get("error_description")
			errCh <- fmt.Errorf("authorization error: %s — %s", errMsg, desc)
			_, _ = fmt.Fprintf(w, "<html><body><h1>Login failed</h1><p>%s</p><p>You can close this tab.</p></body></html>", desc)
			return
		}

		if r.URL.Query().Get("state") != state {
			errCh <- fmt.Errorf("state mismatch")
			http.Error(w, "State mismatch", http.StatusBadRequest)
			return
		}

		code := r.URL.Query().Get("code")
		if code == "" {
			errCh <- fmt.Errorf("no authorization code in callback")
			http.Error(w, "Missing code", http.StatusBadRequest)
			return
		}

		_, _ = fmt.Fprint(w, "<html><body><h1>Login successful!</h1><p>You can close this tab and return to the terminal.</p></body></html>")
		codeCh <- code
	})

	server := &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	go func() {
		if err := server.Serve(listener); err != nil && err != http.ErrServerClosed {
			errCh <- fmt.Errorf("callback server: %w", err)
		}
	}()
	defer func() {
		shutCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = server.Shutdown(shutCtx)
	}()

	// Open browser.
	if err := openBrowserFn(authURL); err != nil {
		// Don't fail — print URL for manual copy.
		fmt.Printf("Could not open browser. Please visit:\n%s\n", authURL)
	}

	// Wait for code or error.
	var code string
	select {
	case code = <-codeCh:
	case err := <-errCh:
		return nil, err
	case <-ctx.Done():
		return nil, ctx.Err()
	}

	// Exchange code for tokens.
	return exchangeCode(ctx, cfg, code, verifier, redirectURI)
}

// RefreshAccessToken uses a refresh token to get a new access token.
func RefreshAccessToken(ctx context.Context, cfg Config, refreshToken string) (*Tokens, error) {
	tokenURL := strings.TrimSuffix(cfg.Issuer, "/") + "/oauth/token"

	data := url.Values{
		"grant_type":    {"refresh_token"},
		"client_id":     {cfg.ClientID},
		"refresh_token": {refreshToken},
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, tokenURL, strings.NewReader(data.Encode()))
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("token request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}

	var tokenResp tokenResponse
	if err := json.Unmarshal(body, &tokenResp); err != nil {
		return nil, fmt.Errorf("parse response: %w", err)
	}

	if tokenResp.Error != "" {
		return nil, fmt.Errorf("token error: %s — %s", tokenResp.Error, tokenResp.ErrorDesc)
	}

	tokens := &Tokens{
		AccessToken: tokenResp.AccessToken,
		IDToken:     tokenResp.IDToken,
		ExpiresAt:   time.Now().Add(time.Duration(tokenResp.ExpiresIn) * time.Second),
	}
	// Keep old refresh token if a new one wasn't issued.
	if tokenResp.RefreshToken != "" {
		tokens.RefreshToken = tokenResp.RefreshToken
	} else {
		tokens.RefreshToken = refreshToken
	}
	return tokens, nil
}

func buildAuthURL(cfg Config, redirectURI, state, challenge string) string {
	base := strings.TrimSuffix(cfg.Issuer, "/") + "/authorize"
	params := url.Values{
		"response_type":         {"code"},
		"client_id":             {cfg.ClientID},
		"redirect_uri":          {redirectURI},
		"scope":                 {"openid profile email offline_access"},
		"state":                 {state},
		"code_challenge":        {challenge},
		"code_challenge_method": {"S256"},
		"audience":              {cfg.Audience},
	}
	return base + "?" + params.Encode()
}

func exchangeCode(ctx context.Context, cfg Config, code, verifier, redirectURI string) (*Tokens, error) {
	tokenURL := strings.TrimSuffix(cfg.Issuer, "/") + "/oauth/token"

	data := url.Values{
		"grant_type":    {"authorization_code"},
		"client_id":     {cfg.ClientID},
		"code":          {code},
		"redirect_uri":  {redirectURI},
		"code_verifier": {verifier},
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, tokenURL, strings.NewReader(data.Encode()))
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("token request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}

	var tokenResp tokenResponse
	if err := json.Unmarshal(body, &tokenResp); err != nil {
		return nil, fmt.Errorf("parse response: %w", err)
	}

	if tokenResp.Error != "" {
		return nil, fmt.Errorf("token error: %s — %s", tokenResp.Error, tokenResp.ErrorDesc)
	}

	return &Tokens{
		AccessToken:  tokenResp.AccessToken,
		RefreshToken: tokenResp.RefreshToken,
		IDToken:      tokenResp.IDToken,
		ExpiresAt:    time.Now().Add(time.Duration(tokenResp.ExpiresIn) * time.Second),
	}, nil
}

// generateCodeVerifier creates a random 32-byte, base64url-encoded string.
func generateCodeVerifier() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// codeChallenge computes S256(verifier).
func codeChallenge(verifier string) string {
	h := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(h[:])
}

// randomString returns a cryptographically random base64url string of n bytes.
func randomString(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// openBrowserFn opens a URL in the default browser. Variable for testing.
var openBrowserFn = openBrowserDefault

func openBrowserDefault(url string) error {
	switch runtime.GOOS {
	case "darwin":
		return exec.Command("open", url).Start()
	case "linux":
		return exec.Command("xdg-open", url).Start()
	case "windows":
		return exec.Command("rundll32", "url.dll,FileProtocolHandler", url).Start()
	default:
		return fmt.Errorf("unsupported platform: %s", runtime.GOOS)
	}
}
