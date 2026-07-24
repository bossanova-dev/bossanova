package upgrade

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/recurser/bossalib/buildinfo"
	"golang.org/x/mod/semver"
)

const defaultRepo = "bossanova-dev/bossanova"

const githubReleasesPerPage = 100

var errNoStableRelease = errors.New("no stable release found")

// UserAgent identifies boss to GitHub. Including the build version helps
// release-telemetry and rate-limit auditing on the API side.
var UserAgent = "boss-upgrade-check/" + buildinfo.Version

// VerifyReleaseTag confirms that the given release tag exists on GitHub by
// issuing a HEAD against the canonical release page. Used by --check
// --version so users learn about typos before the install flow downloads
// anything. When token is non-empty an Authorization: Bearer header is sent so
// the request draws on the authenticated (5000/hr) rate-limit budget.
func VerifyReleaseTag(ctx context.Context, client *http.Client, repo, version, token string) error {
	if client == nil {
		client = http.DefaultClient
	}
	if repo == "" {
		repo = defaultRepo
	}
	url := fmt.Sprintf("https://github.com/%s/releases/tag/%s", repo, version)
	resp, err := releaseRequest(ctx, client, http.MethodHead, url, token)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusNotFound {
		return fmt.Errorf("release %s not found", version)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		if rl := rateLimitError(resp); rl != nil {
			return rl
		}
		return fmt.Errorf("verify release %s: HTTP %d", version, resp.StatusCode)
	}
	return nil
}

type Release struct {
	Version string
	URL     string
}

type CheckResult struct {
	CurrentVersion string
	LatestVersion  string
	ReleaseURL     string
	Available      bool
	Reason         string
}

type Checker struct {
	HTTPClient *http.Client
	Repo       string
	// Token, when non-empty, authenticates the release check via an
	// Authorization: Bearer header (5000/hr) instead of the anonymous
	// 60/hr/IP budget that frequently 403s on busy machines.
	Token string
	Now   func() time.Time
}

// RateLimitError reports that the GitHub REST API returned a rate-limit
// response — HTTP 403 or 429 with X-RateLimit-Remaining: 0. It is distinguished
// from a generic non-2xx error so callers can render an actionable message
// (set a token, wait for the reset) instead of a bare "HTTP 403".
type RateLimitError struct {
	Resource string
	Resets   time.Time
}

func (e *RateLimitError) Error() string {
	msg := "github rate limit reached"
	if !e.Resets.IsZero() {
		msg += "; resets at " + e.Resets.Local().Format("15:04")
	}
	return msg + "; set GITHUB_TOKEN or run `gh auth login`"
}

// rateLimitError returns a *RateLimitError when resp is a GitHub rate-limit
// response (403/429 with X-RateLimit-Remaining: 0), else nil. A genuine
// non-rate-limit 403 (X-RateLimit-Remaining absent or non-zero) returns nil so
// the caller falls through to the generic error.
func rateLimitError(resp *http.Response) *RateLimitError {
	if resp.StatusCode != http.StatusForbidden && resp.StatusCode != http.StatusTooManyRequests {
		return nil
	}
	if resp.Header.Get("X-RateLimit-Remaining") != "0" {
		return nil
	}
	rl := &RateLimitError{Resource: resp.Header.Get("X-RateLimit-Resource")}
	if reset := resp.Header.Get("X-RateLimit-Reset"); reset != "" {
		if secs, err := strconv.ParseInt(reset, 10, 64); err == nil {
			rl.Resets = time.Unix(secs, 0)
		}
	}
	return rl
}

type githubRelease struct {
	TagName    string `json:"tag_name"`
	Draft      bool   `json:"draft"`
	Prerelease bool   `json:"prerelease"`
	HTMLURL    string `json:"html_url"`
}

func parseLatestStableRelease(body []byte) (Release, error) {
	var releases []githubRelease
	if err := json.Unmarshal(body, &releases); err != nil {
		return Release{}, err
	}
	return latestStableFromReleases(releases)
}

func latestStableFromReleases(releases []githubRelease) (Release, error) {
	var latest Release
	for _, release := range releases {
		if release.Draft || release.Prerelease {
			continue
		}
		version, ok, _ := NormalizeVersion(release.TagName)
		if !ok || semver.Prerelease(version) != "" {
			continue
		}
		if latest.Version == "" || semver.Compare(version, latest.Version) > 0 {
			latest = Release{Version: version, URL: release.HTMLURL}
		}
	}

	if latest.Version != "" {
		return latest, nil
	}

	return Release{}, errNoStableRelease
}

func (c Checker) Check(ctx context.Context, current string) (CheckResult, error) {
	repo := c.Repo
	if repo == "" {
		repo = defaultRepo
	}
	client := c.HTTPClient
	if client == nil {
		client = http.DefaultClient
	}

	latest, err := latestStableRelease(ctx, client, repo, c.Token)
	if err != nil {
		return CheckResult{}, err
	}

	result := CheckResult{
		CurrentVersion: current,
		LatestVersion:  latest.Version,
		ReleaseURL:     latest.URL,
	}
	switch CompareStableVersions(current, latest.Version) {
	case CompareOlder:
		result.Available = true
	case CompareCurrent:
		result.Reason = "current"
	case CompareNewer:
		result.Reason = "newer-than-release"
	default:
		result.Reason = "invalid-current-version"
	}

	return result, nil
}

func latestStableRelease(ctx context.Context, client *http.Client, repo, token string) (Release, error) {
	nextURL := fmt.Sprintf("https://api.github.com/repos/%s/releases?per_page=%d", repo, githubReleasesPerPage)
	var latest Release
	for nextURL != "" {
		releases, link, err := fetchReleasePage(ctx, client, nextURL, token)
		if err != nil {
			return Release{}, err
		}
		pageLatest, err := latestStableFromReleases(releases)
		if err == nil {
			if latest.Version == "" || semver.Compare(pageLatest.Version, latest.Version) > 0 {
				latest = pageLatest
			}
			nextURL = nextReleasePageURL(link)
			continue
		}
		if !errors.Is(err, errNoStableRelease) {
			return Release{}, err
		}
		nextURL = nextReleasePageURL(link)
	}
	if latest.Version != "" {
		return latest, nil
	}
	return Release{}, errNoStableRelease
}

// releaseRequest issues method against url with boss's standard headers, adding
// Authorization: Bearer when token is non-empty. On a 401 (a bad or expired
// token) it retries once anonymously, so a stale GITHUB_TOKEN/GH_TOKEN in the
// environment degrades to the prior unauthenticated behavior instead of turning
// the upgrade check into a hard failure. The caller owns closing resp.Body.
func releaseRequest(ctx context.Context, client *http.Client, method, url, token string) (*http.Response, error) {
	resp, err := doReleaseRequest(ctx, client, method, url, token)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode == http.StatusUnauthorized && token != "" {
		_ = resp.Body.Close()
		return doReleaseRequest(ctx, client, method, url, "")
	}
	return resp, nil
}

func doReleaseRequest(ctx context.Context, client *http.Client, method, url, token string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, url, nil)
	if err != nil {
		return nil, err
	}
	if method == http.MethodGet {
		req.Header.Set("Accept", "application/vnd.github+json")
	}
	req.Header.Set("User-Agent", UserAgent)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	return client.Do(req)
}

func fetchReleasePage(ctx context.Context, client *http.Client, pageURL, token string) ([]githubRelease, string, error) {
	resp, err := releaseRequest(ctx, client, http.MethodGet, pageURL, token)
	if err != nil {
		return nil, "", err
	}
	defer func() {
		_ = resp.Body.Close()
	}()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		if rl := rateLimitError(resp); rl != nil {
			return nil, "", rl
		}
		return nil, "", fmt.Errorf("github releases: HTTP %d", resp.StatusCode)
	}

	var releases []githubRelease
	if err := json.NewDecoder(resp.Body).Decode(&releases); err != nil {
		return nil, "", err
	}
	return releases, resp.Header.Get("Link"), nil
}

func nextReleasePageURL(linkHeader string) string {
	for _, link := range strings.Split(linkHeader, ",") {
		parts := strings.Split(link, ";")
		if len(parts) < 2 {
			continue
		}
		urlPart := strings.TrimSpace(parts[0])
		if !strings.HasPrefix(urlPart, "<") || !strings.HasSuffix(urlPart, ">") {
			continue
		}
		for _, param := range parts[1:] {
			if strings.TrimSpace(param) == `rel="next"` {
				return strings.TrimSuffix(strings.TrimPrefix(urlPart, "<"), ">")
			}
		}
	}
	return ""
}
