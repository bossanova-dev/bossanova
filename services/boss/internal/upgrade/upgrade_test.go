package upgrade

import (
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestNormalizeVersion(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		in      string
		want    string
		wantOK  bool
		wantDev bool
	}{
		{name: "plain semantic version", in: "1.2.3", want: "v1.2.3", wantOK: true},
		{name: "tag semantic version", in: "v1.2.3", want: "v1.2.3", wantOK: true},
		{name: "buildinfo string", in: "v1.2.3 (abc123) built 2026-05-18T00:00:00Z", want: "v1.2.3", wantOK: true},
		{name: "git describe", in: "v1.2.3-4-gabc123", wantOK: false},
		{name: "dirty git describe", in: "v1.2.3-4-gabc123-dirty", wantOK: false},
		{name: "dirty git describe without tag prefix", in: "1.2.3-4-gabc123-dirty", wantOK: false},
		{name: "dev", in: "dev", wantOK: false, wantDev: true},
		{name: "empty", in: "", wantOK: false},
		{name: "prerelease", in: "v1.2.3-beta.1", want: "v1.2.3-beta.1", wantOK: true},
		{name: "prerelease with g prefix", in: "v1.2.3-gamma.1", want: "v1.2.3-gamma.1", wantOK: true},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, ok, dev := NormalizeVersion(tt.in)
			if got != tt.want || ok != tt.wantOK || dev != tt.wantDev {
				t.Fatalf("NormalizeVersion(%q) = (%q, %v, %v), want (%q, %v, %v)", tt.in, got, ok, dev, tt.want, tt.wantOK, tt.wantDev)
			}
		})
	}
}

func TestCompareStableVersions(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		current string
		latest  string
		want    CompareResult
	}{
		{name: "current older", current: "v1.2.3", latest: "v1.2.4", want: CompareOlder},
		{name: "current equal", current: "v1.2.3", latest: "v1.2.3", want: CompareCurrent},
		{name: "current newer", current: "v1.2.4", latest: "v1.2.3", want: CompareNewer},
		{name: "prerelease latest ignored", current: "v1.2.3", latest: "v1.2.4-beta.1", want: CompareInvalid},
		{name: "invalid current", current: "dev", latest: "v1.2.3", want: CompareInvalid},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := CompareStableVersions(tt.current, tt.latest); got != tt.want {
				t.Fatalf("CompareStableVersions(%q, %q) = %v, want %v", tt.current, tt.latest, got, tt.want)
			}
		})
	}
}

func TestParseLatestStableRelease(t *testing.T) {
	t.Parallel()

	body := `[
		{"tag_name":"v1.3.0-beta.1","prerelease":true,"html_url":"https://example.test/beta"},
		{"tag_name":"v1.2.4","html_url":"https://example.test/stable"},
		{"tag_name":"v1.2.5","draft":true,"html_url":"https://example.test/draft"}
	]`

	release, err := parseLatestStableRelease([]byte(body))
	if err != nil {
		t.Fatalf("parseLatestStableRelease() error = %v", err)
	}
	if release.Version != "v1.2.4" || release.URL != "https://example.test/stable" {
		t.Fatalf("parseLatestStableRelease() = %+v, want v1.2.4 stable URL", release)
	}
}

func TestParseLatestStableReleaseSelectsHighestStable(t *testing.T) {
	t.Parallel()

	body := `[
		{"tag_name":"v1.2.4","html_url":"https://example.test/older"},
		{"tag_name":"v1.3.0-beta.1","prerelease":true,"html_url":"https://example.test/beta"},
		{"tag_name":"v1.2.6","html_url":"https://example.test/newer"},
		{"tag_name":"v1.2.5","draft":true,"html_url":"https://example.test/draft"}
	]`

	release, err := parseLatestStableRelease([]byte(body))
	if err != nil {
		t.Fatalf("parseLatestStableRelease() error = %v", err)
	}
	if release.Version != "v1.2.6" || release.URL != "https://example.test/newer" {
		t.Fatalf("parseLatestStableRelease() = %+v, want v1.2.6 newer URL", release)
	}
}

func TestParseLatestStableReleaseNoStable(t *testing.T) {
	t.Parallel()

	body := `[
		{"tag_name":"v1.3.0-beta.1","prerelease":true,"html_url":"https://example.test/beta"}
	]`

	if _, err := parseLatestStableRelease([]byte(body)); err == nil {
		t.Fatal("parseLatestStableRelease() error = nil, want error")
	}
}

func TestCacheRoundTripAndTTL(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "upgrade-cache.json")
	now := time.Date(2026, 5, 18, 0, 0, 0, 0, time.UTC)
	entry := CacheEntry{
		CheckedAt:      now,
		CurrentVersion: "v1.2.3",
		LatestVersion:  "v1.2.4",
		ReleaseURL:     "https://example.test/release",
	}

	if err := WriteCache(path, entry); err != nil {
		t.Fatalf("WriteCache() error = %v", err)
	}

	got, ok, err := ReadFreshCache(path, "v1.2.3", now.Add(23*time.Hour), 24*time.Hour)
	if err != nil {
		t.Fatalf("ReadFreshCache() error = %v", err)
	}
	if !ok {
		t.Fatal("ReadFreshCache() ok = false, want true")
	}
	if got.LatestVersion != "v1.2.4" {
		t.Fatalf("ReadFreshCache() LatestVersion = %q, want v1.2.4", got.LatestVersion)
	}

	if _, ok, err := ReadFreshCache(path, "v1.2.3", now.Add(25*time.Hour), 24*time.Hour); err != nil || ok {
		t.Fatalf("ReadFreshCache() after TTL = (_, %v, %v), want (_, false, nil)", ok, err)
	}
}

func TestCacheEntrySuppressed(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 5, 18, 0, 0, 0, 0, time.UTC)
	entry := CacheEntry{
		SnoozedVersion: "v1.2.4",
		SnoozedUntil:   now.Add(time.Hour),
	}

	if !entry.Suppressed(now, "v1.2.4") {
		t.Fatal("Suppressed(now, v1.2.4) = false, want true")
	}
	if entry.Suppressed(now.Add(2*time.Hour), "v1.2.4") {
		t.Fatal("Suppressed(after snooze, v1.2.4) = true, want false")
	}
	if entry.Suppressed(now, "v1.2.5") {
		t.Fatal("Suppressed(now, v1.2.5) = true, want false")
	}
}

func TestSnoozeUpgradeWritesSnoozeFields(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "upgrade-cache.json")
	now := time.Date(2026, 5, 18, 0, 0, 0, 0, time.UTC)

	if err := SnoozeUpgrade(path, "v1.2.3", "v1.2.4", "https://example.test/release", now, DefaultSnoozeDuration); err != nil {
		t.Fatalf("SnoozeUpgrade() error = %v", err)
	}

	entry, ok, err := ReadCache(path)
	if err != nil || !ok {
		t.Fatalf("ReadCache() = (_, %v, %v), want (entry, true, nil)", ok, err)
	}
	if entry.SnoozedVersion != "v1.2.4" {
		t.Fatalf("SnoozedVersion = %q, want v1.2.4", entry.SnoozedVersion)
	}
	if !entry.SnoozedUntil.Equal(now.Add(DefaultSnoozeDuration)) {
		t.Fatalf("SnoozedUntil = %v, want %v", entry.SnoozedUntil, now.Add(DefaultSnoozeDuration))
	}
	if entry.CurrentVersion != "v1.2.3" {
		t.Fatalf("CurrentVersion = %q, want v1.2.3", entry.CurrentVersion)
	}
	if !entry.Suppressed(now, "v1.2.4") {
		t.Fatal("Snoozed entry should suppress the banner for the snoozed version")
	}
}

func TestSnoozeUpgradePreservesAcrossExistingEntry(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "upgrade-cache.json")
	now := time.Date(2026, 5, 18, 0, 0, 0, 0, time.UTC)
	original := CacheEntry{
		CheckedAt:      now,
		CurrentVersion: "v1.2.3",
		LatestVersion:  "v1.2.4",
		ReleaseURL:     "https://example.test/old",
	}
	if err := WriteCache(path, original); err != nil {
		t.Fatalf("WriteCache() error = %v", err)
	}

	if err := SnoozeUpgrade(path, "v1.2.3", "v1.2.4", "https://example.test/new", now.Add(time.Minute), DefaultSnoozeDuration); err != nil {
		t.Fatalf("SnoozeUpgrade() error = %v", err)
	}

	entry, ok, err := ReadCache(path)
	if err != nil || !ok {
		t.Fatalf("ReadCache() = (_, %v, %v), want (entry, true, nil)", ok, err)
	}
	if !entry.CheckedAt.Equal(now) {
		t.Fatalf("CheckedAt = %v, want %v (snooze must not clobber CheckedAt of an existing fresh entry)", entry.CheckedAt, now)
	}
	if entry.ReleaseURL != "https://example.test/new" {
		t.Fatalf("ReleaseURL = %q, want updated URL", entry.ReleaseURL)
	}
}

func TestCheckerCheckAvailable(t *testing.T) {
	t.Parallel()

	body := `[
		{"tag_name":"v1.3.0-beta.1","prerelease":true,"html_url":"https://example.test/beta"},
		{"tag_name":"v1.2.4","html_url":"https://example.test/stable"}
	]`
	client := &http.Client{
		Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			if got, want := req.URL.String(), "https://api.github.com/repos/bossanova-dev/bossanova/releases?per_page=100"; got != want {
				t.Fatalf("request URL = %q, want %q", got, want)
			}
			if got, want := req.Header.Get("Accept"), "application/vnd.github+json"; got != want {
				t.Fatalf("Accept = %q, want %q", got, want)
			}
			if got := req.Header.Get("User-Agent"); !strings.HasPrefix(got, "boss-upgrade-check/") {
				t.Fatalf("User-Agent = %q, want prefix boss-upgrade-check/", got)
			}
			return &http.Response{
				StatusCode: http.StatusOK,
				Body:       io.NopCloser(strings.NewReader(body)),
				Header:     make(http.Header),
				Request:    req,
			}, nil
		}),
	}

	got, err := (Checker{HTTPClient: client}).Check(context.Background(), "v1.2.3")
	if err != nil {
		t.Fatalf("Check() error = %v", err)
	}
	if !got.Available {
		t.Fatal("Check() Available = false, want true")
	}
	if got.CurrentVersion != "v1.2.3" || got.LatestVersion != "v1.2.4" || got.ReleaseURL != "https://example.test/stable" {
		t.Fatalf("Check() = %+v, want current v1.2.3 latest v1.2.4 stable URL", got)
	}
}

func TestCheckerCheckPaginatesUntilStableRelease(t *testing.T) {
	t.Parallel()

	requests := []string{}
	client := &http.Client{
		Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			requests = append(requests, req.URL.String())
			if got, want := req.Header.Get("Accept"), "application/vnd.github+json"; got != want {
				t.Fatalf("Accept = %q, want %q", got, want)
			}
			if got := req.Header.Get("User-Agent"); !strings.HasPrefix(got, "boss-upgrade-check/") {
				t.Fatalf("User-Agent = %q, want prefix boss-upgrade-check/", got)
			}
			header := make(http.Header)
			switch req.URL.String() {
			case "https://api.github.com/repos/bossanova-dev/bossanova/releases?per_page=100":
				header.Set("Link", `<https://api.github.com/repos/bossanova-dev/bossanova/releases?per_page=100&page=2>; rel="next"`)
				return &http.Response{
					StatusCode: http.StatusOK,
					Body:       io.NopCloser(strings.NewReader(`[{"tag_name":"v1.3.0-beta.1","prerelease":true,"html_url":"https://example.test/beta"}]`)),
					Header:     header,
					Request:    req,
				}, nil
			case "https://api.github.com/repos/bossanova-dev/bossanova/releases?per_page=100&page=2":
				return &http.Response{
					StatusCode: http.StatusOK,
					Body:       io.NopCloser(strings.NewReader(`[{"tag_name":"v1.2.4","html_url":"https://example.test/stable"}]`)),
					Header:     header,
					Request:    req,
				}, nil
			default:
				t.Fatalf("unexpected request URL %q", req.URL.String())
				return nil, nil
			}
		}),
	}

	got, err := (Checker{HTTPClient: client}).Check(context.Background(), "v1.2.3")
	if err != nil {
		t.Fatalf("Check() error = %v", err)
	}
	if !got.Available {
		t.Fatal("Check() Available = false, want true")
	}
	if got.LatestVersion != "v1.2.4" || got.ReleaseURL != "https://example.test/stable" {
		t.Fatalf("Check() = %+v, want latest v1.2.4 stable URL", got)
	}
	if len(requests) != 2 {
		t.Fatalf("requests = %v, want 2 paginated requests", requests)
	}
}

func TestCheckerCheckScansAllPagesForHighestStableRelease(t *testing.T) {
	t.Parallel()

	requests := []string{}
	client := &http.Client{
		Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			requests = append(requests, req.URL.String())
			header := make(http.Header)
			switch req.URL.String() {
			case "https://api.github.com/repos/bossanova-dev/bossanova/releases?per_page=100":
				header.Set("Link", `<https://api.github.com/repos/bossanova-dev/bossanova/releases?per_page=100&page=2>; rel="next"`)
				return &http.Response{
					StatusCode: http.StatusOK,
					Body:       io.NopCloser(strings.NewReader(`[{"tag_name":"v1.2.4","html_url":"https://example.test/lower"}]`)),
					Header:     header,
					Request:    req,
				}, nil
			case "https://api.github.com/repos/bossanova-dev/bossanova/releases?per_page=100&page=2":
				return &http.Response{
					StatusCode: http.StatusOK,
					Body:       io.NopCloser(strings.NewReader(`[{"tag_name":"v2.0.0","html_url":"https://example.test/higher"}]`)),
					Header:     header,
					Request:    req,
				}, nil
			default:
				t.Fatalf("unexpected request URL %q", req.URL.String())
				return nil, nil
			}
		}),
	}

	got, err := (Checker{HTTPClient: client}).Check(context.Background(), "v1.2.3")
	if err != nil {
		t.Fatalf("Check() error = %v", err)
	}
	if got.LatestVersion != "v2.0.0" || got.ReleaseURL != "https://example.test/higher" {
		t.Fatalf("Check() = %+v, want highest stable v2.0.0", got)
	}
	if len(requests) != 2 {
		t.Fatalf("requests = %v, want 2 paginated requests", requests)
	}
}

func TestCheckerCheckReadsLargeReleasePage(t *testing.T) {
	t.Parallel()

	largeBody := `[{"tag_name":"v1.2.4","html_url":"https://example.test/stable","body":"` + strings.Repeat("x", 1<<20) + `"}]`
	client := &http.Client{
		Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode: http.StatusOK,
				Body:       io.NopCloser(strings.NewReader(largeBody)),
				Header:     make(http.Header),
				Request:    req,
			}, nil
		}),
	}

	got, err := (Checker{HTTPClient: client}).Check(context.Background(), "v1.2.3")
	if err != nil {
		t.Fatalf("Check() error = %v", err)
	}
	if got.LatestVersion != "v1.2.4" || got.ReleaseURL != "https://example.test/stable" {
		t.Fatalf("Check() = %+v, want large page stable release", got)
	}
}

func TestVerifySHA256(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "boss")
	if err := os.WriteFile(path, []byte("binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := VerifySHA256(path, "9a3a45d01531a20e89ac6ae10b0b0beb0492acd7216a368aa062d1a5fecaf9cd"); err != nil {
		t.Fatalf("VerifySHA256 valid: %v", err)
	}
	if err := VerifySHA256(path, "bad"); err == nil {
		t.Fatal("VerifySHA256 accepted wrong checksum")
	}
}

func TestAtomicReplaceReplacesDestination(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	dest := filepath.Join(dir, "boss")
	src := filepath.Join(dir, "boss.new")
	if err := os.WriteFile(dest, []byte("old"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(src, []byte("new"), 0o755); err != nil {
		t.Fatal(err)
	}

	if err := AtomicReplace(src, dest); err != nil {
		t.Fatalf("AtomicReplace() error = %v", err)
	}

	got, err := os.ReadFile(dest)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "new" {
		t.Fatalf("dest = %q, want new", got)
	}
}

func TestInstallPlanValidateRejectsInvalidPlans(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	valid := InstallPlan{
		Version:    "v1.2.3",
		ReleaseURL: "https://github.com/bossanova-dev/bossanova/releases/download/v1.2.3",
		GOOS:       "darwin",
		GOARCH:     "arm64",
		BinDir:     dir,
		PluginDir:  filepath.Join(dir, "plugins"),
	}

	tests := []struct {
		name string
		mut  func(*InstallPlan)
	}{
		{name: "invalid version", mut: func(p *InstallPlan) { p.Version = "dev" }},
		{name: "prerelease version", mut: func(p *InstallPlan) {
			p.Version = "v1.2.3-beta.1"
			p.ReleaseURL = "https://github.com/bossanova-dev/bossanova/releases/download/v1.2.3-beta.1"
		}},
		{name: "relative bin dir", mut: func(p *InstallPlan) { p.BinDir = "bin" }},
		{name: "missing bin dir", mut: func(p *InstallPlan) { p.BinDir = filepath.Join(dir, "missing") }},
		{name: "non directory bin dir", mut: func(p *InstallPlan) {
			path := filepath.Join(dir, "boss")
			if err := os.WriteFile(path, []byte("binary"), 0o755); err != nil {
				t.Fatal(err)
			}
			p.BinDir = path
		}},
		{name: "non https release url", mut: func(p *InstallPlan) {
			p.ReleaseURL = "http://github.com/bossanova-dev/bossanova/releases/download/v1.2.3"
		}},
		{name: "non github release url", mut: func(p *InstallPlan) {
			p.ReleaseURL = "https://example.test/bossanova/releases/download/v1.2.3"
		}},
		{name: "wrong release repo", mut: func(p *InstallPlan) {
			p.ReleaseURL = "https://github.com/other/bossanova/releases/download/v1.2.3"
		}},
		{name: "release tag mismatch", mut: func(p *InstallPlan) {
			p.ReleaseURL = "https://github.com/bossanova-dev/bossanova/releases/download/v1.2.4"
		}},
		{name: "unsupported platform", mut: func(p *InstallPlan) {
			p.GOOS = "freebsd"
			p.GOARCH = "amd64"
		}},
		{name: "empty goos", mut: func(p *InstallPlan) { p.GOOS = "" }},
		{name: "empty goarch", mut: func(p *InstallPlan) { p.GOARCH = "" }},
		{name: "empty plugin dir", mut: func(p *InstallPlan) { p.PluginDir = "" }},
		{name: "relative plugin dir", mut: func(p *InstallPlan) { p.PluginDir = "plugins" }},
		{name: "non directory plugin dir", mut: func(p *InstallPlan) {
			path := filepath.Join(dir, "plugin-file")
			if err := os.WriteFile(path, []byte("not a directory"), 0o644); err != nil {
				t.Fatal(err)
			}
			p.PluginDir = path
		}},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			plan := valid
			tt.mut(&plan)
			if err := plan.Validate(); err == nil {
				t.Fatal("Validate() error = nil, want error")
			}
		})
	}
}

func TestInstallerInstallDownloadsVerifiesAndReplacesAssets(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	binDir := filepath.Join(dir, "bin")
	pluginDir := filepath.Join(dir, "plugins")
	if err := os.Mkdir(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(binDir, "boss"), []byte("old boss"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(binDir, "bossd"), []byte("old bossd"), 0o755); err != nil {
		t.Fatal(err)
	}

	contents := map[string][]byte{}
	for _, asset := range AssetNames("darwin", "arm64") {
		contents[asset] = []byte("new " + asset)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		name := filepath.Base(r.URL.Path)
		if strings.HasSuffix(name, ".sha256") {
			asset := strings.TrimSuffix(name, ".sha256")
			body, ok := contents[asset]
			if !ok {
				http.NotFound(w, r)
				return
			}
			sum := sha256.Sum256(body)
			_, _ = fmt.Fprintf(w, "%x  %s\n", sum, asset)
			return
		}
		body, ok := contents[name]
		if !ok {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write(body)
	}))
	defer server.Close()

	plan := InstallPlan{
		Version:    "v1.2.3",
		ReleaseURL: "https://github.com/bossanova-dev/bossanova/releases/download/v1.2.3",
		GOOS:       "darwin",
		GOARCH:     "arm64",
		BinDir:     binDir,
		PluginDir:  pluginDir,
	}
	if err := (Installer{HTTPClient: rewriteClient(t, server.URL)}).Install(context.Background(), plan); err != nil {
		t.Fatalf("Install() error = %v", err)
	}

	assertFileContent(t, filepath.Join(binDir, "boss"), "new boss-darwin-arm64")
	assertFileContent(t, filepath.Join(binDir, "bossd"), "new bossd-darwin-arm64")
	for _, plugin := range pluginBins {
		assertFileContent(t, filepath.Join(pluginDir, plugin), "new "+plugin+"-darwin-arm64")
	}
}

func TestInstallerInstallRollsBackOnReplaceFailure(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	binDir := filepath.Join(dir, "bin")
	pluginDir := filepath.Join(dir, "plugins")
	if err := os.Mkdir(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(pluginDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(binDir, "boss"), []byte("old boss"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(binDir, "bossd"), []byte("old bossd"), 0o755); err != nil {
		t.Fatal(err)
	}

	contents := map[string][]byte{}
	for _, asset := range AssetNames("darwin", "arm64") {
		contents[asset] = []byte("new " + asset)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		name := filepath.Base(r.URL.Path)
		if strings.HasSuffix(name, ".sha256") {
			asset := strings.TrimSuffix(name, ".sha256")
			body := contents[asset]
			sum := sha256.Sum256(body)
			_, _ = fmt.Fprintf(w, "%x  %s\n", sum, asset)
			return
		}
		_, _ = w.Write(contents[name])
	}))
	defer server.Close()

	plan := InstallPlan{
		Version:    "v1.2.3",
		ReleaseURL: "https://github.com/bossanova-dev/bossanova/releases/download/v1.2.3",
		GOOS:       "darwin",
		GOARCH:     "arm64",
		BinDir:     binDir,
		PluginDir:  pluginDir,
	}
	calls := 0
	installer := Installer{
		HTTPClient: rewriteClient(t, server.URL),
		Replace: func(src, dest string) error {
			calls++
			if calls == 2 {
				return fmt.Errorf("injected replace failure")
			}
			return AtomicReplace(src, dest)
		},
	}
	err := installer.Install(context.Background(), plan)
	if err == nil {
		t.Fatal("Install() error = nil, want replace failure")
	}

	assertFileContent(t, filepath.Join(binDir, "boss"), "old boss")
	assertFileContent(t, filepath.Join(binDir, "bossd"), "old bossd")
}

func TestVerifyReleaseTag(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		status  int
		wantErr string
	}{
		{name: "found", status: 200},
		{name: "not found", status: 404, wantErr: "not found"},
		{name: "server error", status: 500, wantErr: "HTTP 500"},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			client := &http.Client{
				Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
					if req.Method != http.MethodHead {
						t.Fatalf("method = %q, want HEAD", req.Method)
					}
					return &http.Response{
						StatusCode: tt.status,
						Body:       io.NopCloser(strings.NewReader("")),
						Header:     make(http.Header),
						Request:    req,
					}, nil
				}),
			}
			err := VerifyReleaseTag(context.Background(), client, "", "v1.2.3")
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("VerifyReleaseTag() error = %v, want nil", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("VerifyReleaseTag() error = %v, want containing %q", err, tt.wantErr)
			}
		})
	}
}

func TestInstallerInstallRejectsUnwritableBinDir(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root bypasses file-mode permission checks")
	}
	t.Parallel()

	dir := t.TempDir()
	binDir := filepath.Join(dir, "bin")
	pluginDir := filepath.Join(dir, "plugins")
	if err := os.Mkdir(binDir, 0o500); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(pluginDir, 0o755); err != nil {
		t.Fatal(err)
	}
	plan := InstallPlan{
		Version:    "v1.2.3",
		ReleaseURL: "https://github.com/bossanova-dev/bossanova/releases/download/v1.2.3",
		GOOS:       "darwin",
		GOARCH:     "arm64",
		BinDir:     binDir,
		PluginDir:  pluginDir,
	}
	httpCalled := false
	installer := Installer{
		HTTPClient: &http.Client{
			Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
				httpCalled = true
				return nil, fmt.Errorf("unexpected HTTP request: %s", req.URL)
			}),
		},
	}
	err := installer.Install(context.Background(), plan)
	if err == nil {
		t.Fatal("Install() error = nil, want preflight permission error")
	}
	if !strings.Contains(err.Error(), "cannot write") {
		t.Fatalf("Install() error = %v, want write-permission message", err)
	}
	if httpCalled {
		t.Fatal("Install() should refuse before any HTTP traffic")
	}
}

func TestInstallerInstallRejectsOversizedAsset(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	binDir := filepath.Join(dir, "bin")
	pluginDir := filepath.Join(dir, "plugins")
	if err := os.Mkdir(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(pluginDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// Asset whose advertised size is just past the cap. We can't actually
	// stream half a gig in a test, so we synthesise a response whose body
	// claims to be MaxAssetSize+1 bytes via a counting reader.
	oversizedReader := func() io.ReadCloser {
		return io.NopCloser(&countingReader{remaining: int64(MaxAssetSize) + 8})
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, ".sha256") {
			_, _ = fmt.Fprint(w, "deadbeef  asset\n")
			return
		}
		body := oversizedReader()
		defer func() { _ = body.Close() }()
		_, _ = io.Copy(w, body)
	}))
	defer server.Close()

	plan := InstallPlan{
		Version:    "v1.2.3",
		ReleaseURL: "https://github.com/bossanova-dev/bossanova/releases/download/v1.2.3",
		GOOS:       "darwin",
		GOARCH:     "arm64",
		BinDir:     binDir,
		PluginDir:  pluginDir,
	}
	err := (Installer{HTTPClient: rewriteClient(t, server.URL)}).Install(context.Background(), plan)
	if err == nil {
		t.Fatal("Install() error = nil, want oversized-asset error")
	}
	if !strings.Contains(err.Error(), "byte limit") {
		t.Fatalf("Install() error = %v, want byte-limit message", err)
	}
	matches, err := filepath.Glob(filepath.Join(binDir, ".boss-upgrade-*"))
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) != 0 {
		t.Fatalf("staged files after failed install = %v, want none", matches)
	}
}

type countingReader struct {
	remaining int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	if c.remaining <= 0 {
		return 0, io.EOF
	}
	n := int64(len(p))
	if n > c.remaining {
		n = c.remaining
	}
	c.remaining -= n
	for i := int64(0); i < n; i++ {
		p[i] = 0
	}
	return int(n), nil
}

func TestAssetNames(t *testing.T) {
	t.Parallel()

	got := AssetNames("darwin", "arm64")
	want := []string{
		"boss-darwin-arm64",
		"bossd-darwin-arm64",
		"bossd-plugin-claude-darwin-arm64",
		"bossd-plugin-codex-darwin-arm64",
		"bossd-plugin-dependabot-darwin-arm64",
		"bossd-plugin-linear-darwin-arm64",
		"bossd-plugin-repair-darwin-arm64",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("AssetNames() = %#v, want %#v", got, want)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func rewriteClient(t *testing.T, serverURL string) *http.Client {
	t.Helper()

	target, err := url.Parse(serverURL)
	if err != nil {
		t.Fatal(err)
	}
	return &http.Client{
		Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			clone := req.Clone(req.Context())
			clone.URL.Scheme = target.Scheme
			clone.URL.Host = target.Host
			clone.Host = target.Host
			return http.DefaultTransport.RoundTrip(clone)
		}),
	}
}

func assertFileContent(t *testing.T, path, want string) {
	t.Helper()

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != want {
		t.Fatalf("%s = %q, want %q", path, got, want)
	}
}
