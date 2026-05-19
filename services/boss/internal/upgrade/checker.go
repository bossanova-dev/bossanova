package upgrade

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
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
// anything.
func VerifyReleaseTag(ctx context.Context, client *http.Client, repo, version string) error {
	if client == nil {
		client = http.DefaultClient
	}
	if repo == "" {
		repo = defaultRepo
	}
	url := fmt.Sprintf("https://github.com/%s/releases/tag/%s", repo, version)
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", UserAgent)
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusNotFound {
		return fmt.Errorf("release %s not found", version)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
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
	Now        func() time.Time
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

	latest, err := latestStableRelease(ctx, client, repo)
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

func latestStableRelease(ctx context.Context, client *http.Client, repo string) (Release, error) {
	nextURL := fmt.Sprintf("https://api.github.com/repos/%s/releases?per_page=%d", repo, githubReleasesPerPage)
	var latest Release
	for nextURL != "" {
		releases, link, err := fetchReleasePage(ctx, client, nextURL)
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

func fetchReleasePage(ctx context.Context, client *http.Client, pageURL string) ([]githubRelease, string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, pageURL, nil)
	if err != nil {
		return nil, "", err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", UserAgent)

	resp, err := client.Do(req)
	if err != nil {
		return nil, "", err
	}
	defer func() {
		_ = resp.Body.Close()
	}()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
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
