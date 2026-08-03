package auth

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"time"

	"github.com/recurser/bossalib/authtoken"
)

var (
	workosAPIBase    = "https://api.workos.com"
	workosHTTPClient = http.DefaultClient
)

// DeviceCodeResponse holds the response from the WorkOS device authorization endpoint.
type DeviceCodeResponse struct {
	DeviceCode              string `json:"device_code"`
	UserCode                string `json:"user_code"`
	VerificationURI         string `json:"verification_uri"`
	VerificationURIComplete string `json:"verification_uri_complete"`
	ExpiresIn               int    `json:"expires_in"`
	Interval                int    `json:"interval"`
}

// deviceAuthResponse is the JSON body from the WorkOS authenticate endpoint.
type deviceAuthResponse struct {
	AccessToken  string         `json:"access_token"`
	RefreshToken string         `json:"refresh_token"`
	ExpiresIn    int            `json:"expires_in"`
	User         deviceAuthUser `json:"user"`
	Error        string         `json:"error"`
	ErrorDesc    string         `json:"error_description"`
}

type deviceAuthUser struct {
	ID    string `json:"id"`
	Email string `json:"email"`
}

// DeviceCodeResult holds the result of a successful device code login.
type DeviceCodeResult struct {
	Tokens *Tokens
	Email  string // User email from WorkOS response.
}

// RequestDeviceCode initiates the device authorization flow with WorkOS.
func RequestDeviceCode(ctx context.Context, cfg Config) (*DeviceCodeResponse, error) {
	deviceURL := workosAPIBase + "/user_management/authorize/device"

	data := url.Values{
		"client_id": {cfg.ClientID},
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, deviceURL, strings.NewReader(data.Encode()))
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := workosHTTPClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("device code request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("device code request failed (HTTP %d): %s", resp.StatusCode, string(body))
	}

	var dcResp DeviceCodeResponse
	if err := json.Unmarshal(body, &dcResp); err != nil {
		return nil, fmt.Errorf("parse response: %w", err)
	}

	return &dcResp, nil
}

// PollForToken polls the WorkOS authenticate endpoint until the user completes
// login, the code expires, or the context is cancelled.
func PollForToken(ctx context.Context, cfg Config, deviceCode string, interval int) (*DeviceCodeResult, error) {
	authURL := workosAPIBase + "/user_management/authenticate"
	pollInterval := time.Duration(interval) * time.Second

	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(pollInterval):
		}

		data := url.Values{
			"client_id":   {cfg.ClientID},
			"grant_type":  {"urn:ietf:params:oauth:grant-type:device_code"},
			"device_code": {deviceCode},
		}

		req, err := http.NewRequestWithContext(ctx, http.MethodPost, authURL, strings.NewReader(data.Encode()))
		if err != nil {
			return nil, fmt.Errorf("build request: %w", err)
		}
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

		resp, err := workosHTTPClient.Do(req)
		if err != nil {
			return nil, fmt.Errorf("token request: %w", err)
		}

		body, err := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if err != nil {
			return nil, fmt.Errorf("read response: %w", err)
		}

		// Non-JSON responses (e.g. 502 HTML error pages) should fail fast
		// rather than producing a confusing "parse response" error.
		var authResp deviceAuthResponse
		if err := json.Unmarshal(body, &authResp); err != nil {
			return nil, fmt.Errorf("token request failed (HTTP %d): %s", resp.StatusCode, string(body))
		}

		switch authResp.Error {
		case "":
			// Success.
			tokens := &Tokens{
				AccessToken:  authResp.AccessToken,
				RefreshToken: authResp.RefreshToken,
				ExpiresAt:    authtoken.AccessTokenExpiry(authResp.AccessToken, authResp.ExpiresIn, time.Now()),
				Email:        authResp.User.Email,
			}
			return &DeviceCodeResult{
				Tokens: tokens,
				Email:  authResp.User.Email,
			}, nil
		case "authorization_pending":
			continue
		case "slow_down":
			pollInterval += 5 * time.Second
			continue
		case "access_denied":
			return nil, fmt.Errorf("access denied: %s", authResp.ErrorDesc)
		case "expired_token":
			return nil, fmt.Errorf("device code expired: %s", authResp.ErrorDesc)
		default:
			return nil, fmt.Errorf("unexpected error: %s — %s", authResp.Error, authResp.ErrorDesc)
		}
	}
}

// Login performs the WorkOS device code flow:
//  1. Request a device code from WorkOS.
//  2. Display the user code and open the browser to the verification URL.
//  3. Poll for token completion.
func Login(ctx context.Context, cfg Config) (*DeviceCodeResult, error) {
	dcResp, err := RequestDeviceCode(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("request device code: %w", err)
	}

	// Set timeout from the device code expiry.
	loginCtx, cancel := context.WithTimeout(ctx, time.Duration(dcResp.ExpiresIn)*time.Second)
	defer cancel()

	// Display the user code.
	fmt.Printf("\nYour authentication code: %s\n\n", dcResp.UserCode)
	fmt.Printf("Visit: %s\n\n", dcResp.VerificationURIComplete)
	fmt.Println("Waiting for authentication...")

	// Open browser to verification URL.
	if err := openBrowserFn(dcResp.VerificationURIComplete); err != nil {
		fmt.Printf("Could not open browser. Please visit the URL above.\n")
	}

	return PollForToken(loginCtx, cfg, dcResp.DeviceCode, dcResp.Interval)
}

// RefreshAccessToken uses a refresh token to get a new access token from WorkOS.
func RefreshAccessToken(ctx context.Context, cfg Config, refreshToken string) (*Tokens, error) {
	authURL := workosAPIBase + "/user_management/authenticate"

	data := url.Values{
		"grant_type":    {"refresh_token"},
		"client_id":     {cfg.ClientID},
		"refresh_token": {refreshToken},
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, authURL, strings.NewReader(data.Encode()))
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := workosHTTPClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("token request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		// The response body is never interpolated: this error reaches the
		// user through Manager.AccessToken, and a WorkOS error body can echo
		// token material back at us. Mirrors the daemon's rule in
		// services/bossd/internal/upstream/tokens.go.
		return nil, fmt.Errorf("token refresh failed (HTTP %d)", resp.StatusCode)
	}

	var authResp deviceAuthResponse
	if err := json.Unmarshal(body, &authResp); err != nil {
		return nil, fmt.Errorf("parse response: %w", err)
	}

	if authResp.Error != "" {
		return nil, fmt.Errorf("token error: %s — %s", authResp.Error, authResp.ErrorDesc)
	}

	tokens := &Tokens{
		AccessToken: authResp.AccessToken,
		ExpiresAt:   authtoken.AccessTokenExpiry(authResp.AccessToken, authResp.ExpiresIn, time.Now()),
		Email:       authResp.User.Email,
	}
	// Keep old refresh token if a new one wasn't issued.
	if authResp.RefreshToken != "" {
		tokens.RefreshToken = authResp.RefreshToken
	} else {
		tokens.RefreshToken = refreshToken
	}
	return tokens, nil
}

// openBrowserFn opens a URL in the default browser. Variable for testing.
var openBrowserFn = openBrowserDefault

// OpenBrowser opens a URL in the default browser.
func OpenBrowser(url string) error {
	return openBrowserFn(url)
}

func openBrowserDefault(rawURL string) error {
	cmd, err := openBrowserCommand(rawURL, runtime.GOOS, isWSL(), exec.LookPath)
	if err != nil {
		return err
	}
	return cmd.Start()
}

func openBrowserCommand(rawURL, goos string, wsl bool, lookPath func(string) (string, error)) (*exec.Cmd, error) {
	switch goos {
	case "darwin":
		return exec.Command("open", rawURL), nil
	case "linux":
		if wsl {
			if _, err := lookPath("wslview"); err == nil {
				return exec.Command("wslview", rawURL), nil
			}
			if _, err := lookPath("cmd.exe"); err == nil {
				// #nosec G204 -- WSL cmd.exe /c start "" <url>; app-generated OAuth URL; argv + quoteCmdURL guards metachars
				// owner=@recurser review-by=2027-01-18 issue=BOS-28
				return exec.Command("cmd.exe", "/c", "start", "", quoteCmdURL(rawURL)), nil
			}
		}
		if _, err := lookPath("xdg-open"); err != nil {
			return nil, fmt.Errorf("no browser opener found: xdg-open is not installed")
		}
		return exec.Command("xdg-open", rawURL), nil
	case "windows":
		return exec.Command("rundll32", "url.dll,FileProtocolHandler", rawURL), nil
	default:
		return nil, fmt.Errorf("unsupported platform: %s", goos)
	}
}

func quoteCmdURL(rawURL string) string {
	return `"` + strings.ReplaceAll(rawURL, `"`, `%22`) + `"`
}

func isWSL() bool {
	data, err := os.ReadFile("/proc/version")
	if err != nil {
		return false
	}
	return strings.Contains(strings.ToLower(string(data)), "microsoft")
}
