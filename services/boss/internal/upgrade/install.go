package upgrade

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/mod/semver"
)

var pluginBins = []string{
	"bossd-plugin-claude",
	"bossd-plugin-codex",
	"bossd-plugin-dependabot",
	"bossd-plugin-linear",
	"bossd-plugin-repair",
}

// MaxAssetSize caps a single downloaded asset so a compromised release host
// cannot fill the disk before checksum verification rejects the payload.
// 512 MiB is well above any plausible Go binary size for this project.
const MaxAssetSize = 512 * 1024 * 1024

func AssetNames(goos, goarch string) []string {
	suffix := "-" + goos + "-" + goarch
	names := make([]string, 0, 2+len(pluginBins))
	names = append(names, "boss"+suffix, "bossd"+suffix)
	for _, plugin := range pluginBins {
		names = append(names, plugin+suffix)
	}
	return names
}

func VerifySHA256(path, expected string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()

	sum := sha256.New()
	if _, err := io.Copy(sum, f); err != nil {
		return err
	}
	got := hex.EncodeToString(sum.Sum(nil))
	if got != expected {
		return fmt.Errorf("checksum mismatch for %s: expected %s got %s", path, expected, got)
	}
	return nil
}

func AtomicReplace(src, dest string) error {
	info, err := os.Stat(src)
	if err != nil {
		return err
	}
	if err := os.Chmod(src, info.Mode()|0o755); err != nil {
		return err
	}
	return os.Rename(src, dest)
}

type InstallPlan struct {
	Version    string
	ReleaseURL string
	GOOS       string
	GOARCH     string
	BinDir     string
	PluginDir  string
}

func (p InstallPlan) Validate() error {
	if p.Version == "" || p.ReleaseURL == "" || p.GOOS == "" || p.GOARCH == "" || p.BinDir == "" || p.PluginDir == "" {
		return fmt.Errorf("incomplete install plan")
	}
	version, ok, _ := NormalizeVersion(p.Version)
	if !ok {
		return fmt.Errorf("invalid upgrade version %q", p.Version)
	}
	if semver.Prerelease(version) != "" {
		return fmt.Errorf("prerelease upgrade version %q requires an explicit prerelease channel", p.Version)
	}
	releaseURL, err := url.Parse(p.ReleaseURL)
	if err != nil {
		return fmt.Errorf("invalid release URL: %w", err)
	}
	wantPath := "/bossanova-dev/bossanova/releases/download/" + p.Version
	if releaseURL.Scheme != "https" || strings.ToLower(releaseURL.Hostname()) != "github.com" || releaseURL.Path != wantPath || releaseURL.RawQuery != "" || releaseURL.Fragment != "" {
		return fmt.Errorf("invalid release URL %q", p.ReleaseURL)
	}
	if !supportedPlatform(p.GOOS, p.GOARCH) {
		return fmt.Errorf("unsupported upgrade platform %s/%s", p.GOOS, p.GOARCH)
	}
	if !filepath.IsAbs(p.BinDir) {
		return fmt.Errorf("install bin dir must be absolute")
	}
	if !filepath.IsAbs(p.PluginDir) {
		return fmt.Errorf("install plugin dir must be absolute")
	}
	info, err := os.Stat(p.BinDir)
	if err != nil {
		return fmt.Errorf("install bin dir: %w", err)
	}
	if !info.IsDir() {
		return fmt.Errorf("install bin dir is not a directory")
	}
	pluginInfo, err := os.Stat(p.PluginDir)
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("install plugin dir: %w", err)
	}
	if err == nil && !pluginInfo.IsDir() {
		return fmt.Errorf("install plugin dir is not a directory")
	}
	return nil
}

type Installer struct {
	HTTPClient *http.Client
	TempDir    string
	Replace    func(src, dest string) error
}

type replacement struct {
	src       string
	dest      string
	backup    string
	hadDest   bool
	installed bool
}

func (i Installer) Install(ctx context.Context, plan InstallPlan) error {
	if err := plan.Validate(); err != nil {
		return err
	}
	if err := os.MkdirAll(plan.PluginDir, 0o755); err != nil {
		return fmt.Errorf("create plugin dir: %w", err)
	}
	// Preflight write permissions before downloading hundreds of MB. Without
	// this a user whose binaries are owned by a package manager (e.g.
	// /usr/local/bin) gets the full download wasted, then fails on rename.
	if err := preflightWritable(plan.BinDir); err != nil {
		return fmt.Errorf("cannot write to install dir %s: %w; rerun with sudo or upgrade via your package manager", plan.BinDir, err)
	}
	if err := preflightWritable(plan.PluginDir); err != nil {
		return fmt.Errorf("cannot write to plugin dir %s: %w; rerun with sudo or upgrade via your package manager", plan.PluginDir, err)
	}
	replacements := make([]replacement, 0, len(AssetNames(plan.GOOS, plan.GOARCH)))
	cleanupStaged := true
	defer func() {
		if cleanupStaged {
			for _, item := range replacements {
				_ = os.Remove(item.src)
			}
		}
	}()
	for _, asset := range AssetNames(plan.GOOS, plan.GOARCH) {
		dest, err := assetDestination(plan, asset)
		if err != nil {
			return err
		}
		tmpPath, err := stagedPath(filepath.Dir(dest), asset)
		if err != nil {
			return fmt.Errorf("stage %s: %w", asset, err)
		}
		replacements = append(replacements, replacement{src: tmpPath, dest: dest})
		if err := i.downloadFile(ctx, plan.ReleaseURL+"/"+asset, tmpPath); err != nil {
			return fmt.Errorf("download %s: %w", asset, err)
		}
		checksum, err := i.downloadText(ctx, plan.ReleaseURL+"/"+asset+".sha256")
		if err != nil {
			return fmt.Errorf("download %s.sha256: %w", asset, err)
		}
		expected, err := parseSHA256(checksum)
		if err != nil {
			return fmt.Errorf("parse %s.sha256: %w", asset, err)
		}
		if err := VerifySHA256(tmpPath, expected); err != nil {
			return err
		}
	}
	if err := i.installReplacements(replacements); err != nil {
		return err
	}
	cleanupStaged = false
	return nil
}

func (i Installer) installReplacements(replacements []replacement) error {
	replace := i.Replace
	if replace == nil {
		replace = AtomicReplace
	}
	for idx := range replacements {
		item := &replacements[idx]
		if _, err := os.Stat(item.dest); err == nil {
			backup, err := backupPath(filepath.Dir(item.dest), filepath.Base(item.dest))
			if err != nil {
				rollbackReplacements(replacements)
				return fmt.Errorf("backup %s: %w", item.dest, err)
			}
			if err := os.Rename(item.dest, backup); err != nil {
				rollbackReplacements(replacements)
				return fmt.Errorf("backup %s: %w", item.dest, err)
			}
			item.backup = backup
			item.hadDest = true
		} else if !os.IsNotExist(err) {
			rollbackReplacements(replacements)
			return fmt.Errorf("stat %s: %w", item.dest, err)
		}
	}
	for idx := range replacements {
		item := &replacements[idx]
		if err := replace(item.src, item.dest); err != nil {
			rollbackReplacements(replacements)
			return fmt.Errorf("replace %s: %w", item.dest, err)
		}
		item.installed = true
	}
	for _, item := range replacements {
		if item.hadDest {
			_ = os.Remove(item.backup)
		}
	}
	return nil
}

func rollbackReplacements(replacements []replacement) {
	for idx := len(replacements) - 1; idx >= 0; idx-- {
		item := replacements[idx]
		if item.installed {
			_ = os.Remove(item.dest)
		}
		if item.hadDest {
			_ = os.Rename(item.backup, item.dest)
		}
	}
}

func (i Installer) client() *http.Client {
	if i.HTTPClient != nil {
		return i.HTTPClient
	}
	return http.DefaultClient
}

func (i Installer) downloadFile(ctx context.Context, url, path string) error {
	resp, err := i.get(ctx, url)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()

	f, err := os.OpenFile(path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	// Cap the download — a compromised release host could otherwise fill
	// the disk before checksum verification rejects the payload.
	limited := io.LimitReader(resp.Body, MaxAssetSize+1)
	n, err := io.Copy(f, limited)
	if err != nil {
		_ = f.Close()
		return err
	}
	if n > MaxAssetSize {
		_ = f.Close()
		return fmt.Errorf("asset exceeds %d byte limit", MaxAssetSize)
	}
	if err := f.Close(); err != nil {
		return err
	}
	return nil
}

func (i Installer) downloadText(ctx context.Context, url string) (string, error) {
	resp, err := i.get(ctx, url)
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", err
	}
	return string(body), nil
}

func (i Installer) get(ctx context.Context, url string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := i.client().Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		_ = resp.Body.Close()
		return nil, fmt.Errorf("unexpected HTTP status %s", resp.Status)
	}
	return resp, nil
}

func parseSHA256(content string) (string, error) {
	fields := strings.Fields(content)
	if len(fields) == 0 {
		return "", fmt.Errorf("empty checksum")
	}
	return fields[0], nil
}

func assetDestination(plan InstallPlan, asset string) (string, error) {
	suffix := "-" + plan.GOOS + "-" + plan.GOARCH
	if !strings.HasSuffix(asset, suffix) {
		return "", fmt.Errorf("asset %q does not match platform suffix %q", asset, suffix)
	}
	name := strings.TrimSuffix(asset, suffix)
	switch {
	case name == "boss" || name == "bossd":
		return filepath.Join(plan.BinDir, name), nil
	case strings.HasPrefix(name, "bossd-plugin-"):
		return filepath.Join(plan.PluginDir, name), nil
	default:
		return "", fmt.Errorf("unknown upgrade asset %q", asset)
	}
}

func stagedPath(dir, asset string) (string, error) {
	f, err := os.CreateTemp(dir, ".boss-upgrade-"+asset+"-*")
	if err != nil {
		return "", err
	}
	path := f.Name()
	if err := f.Close(); err != nil {
		_ = os.Remove(path)
		return "", err
	}
	return path, nil
}

// backupPath reserves a unique on-disk name in dir for the backup of a
// destination file. There is a small TOCTOU window between the Remove here
// and the Rename in installReplacements where another process could create
// a file at the same path; the .boss-upgrade-backup- prefix in a user-owned
// install directory makes this practically a non-issue.
func backupPath(dir, name string) (string, error) {
	f, err := os.CreateTemp(dir, ".boss-upgrade-backup-"+name+"-*")
	if err != nil {
		return "", err
	}
	path := f.Name()
	if err := f.Close(); err != nil {
		_ = os.Remove(path)
		return "", err
	}
	if err := os.Remove(path); err != nil {
		return "", err
	}
	return path, nil
}

func supportedPlatform(goos, goarch string) bool {
	switch goos + "/" + goarch {
	case "darwin/amd64", "darwin/arm64", "linux/amd64":
		return true
	default:
		return false
	}
}

// preflightWritable verifies the caller can create and remove a file inside
// dir. Used to fail fast on permission-denied installs before downloading.
func preflightWritable(dir string) error {
	f, err := os.CreateTemp(dir, ".boss-upgrade-permcheck-*")
	if err != nil {
		return err
	}
	name := f.Name()
	_ = f.Close()
	return os.Remove(name)
}
