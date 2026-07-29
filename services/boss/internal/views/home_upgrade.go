// Home's upgrade check, upgrade run, and upgrade banner. Split out of home.go
// (BOS-526); the declarations moved unchanged, except handleUpgradeKey, whose
// return type was later narrowed from tea.Model to HomeModel so handleKey can
// thread the model back on the declined path.

package views

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/recurser/boss/internal/upgrade"
	"github.com/recurser/bossalib/buildinfo"
)

// upgradeNow is the clock source used by the upgrade-check goroutine. Tests
// override it to pin a fixed instant.
var upgradeNow = time.Now

// upgradeCachePath returns the on-disk path for the upgrade banner cache,
// shared with the `boss upgrade` action via upgrade.DefaultCachePath. Returns
// "" if the user's cache directory is unavailable; callers treat an empty path
// as "cache disabled" and skip both reads and writes. Kept as a var so tests
// can point it at a temp file.
var upgradeCachePath = upgrade.DefaultCachePath

type upgradeCheckFunc func(context.Context, string) (upgrade.CheckResult, error)

// proofUpgradeRateLimit, when set via BOSS_PROOF_UPGRADE_RATE_LIMIT (proof
// scenarios only), makes the upgrade check yield a synthetic
// *upgrade.RateLimitError so the friendly rate-limit banner can be captured
// deterministically — without draining a real GitHub quota. Mirrors the
// BOSS_PROOF_HIDE_GUEST_OFFER seam in cloudpromo.go.
var proofUpgradeRateLimit = os.Getenv("BOSS_PROOF_UPGRADE_RATE_LIMIT") != ""

func checkUpgradeCmd(ctx context.Context) tea.Cmd {
	if proofUpgradeRateLimit {
		return func() tea.Msg {
			return upgradeCheckMsg{err: &upgrade.RateLimitError{Resets: upgradeNow().Add(30 * time.Minute)}}
		}
	}
	// Resolve the token lazily, inside the check, so a warm cache skips both the
	// network request and the `gh auth token` subprocess — and Init never blocks
	// on gh. The token authenticates the release check (5000/hr) so the banner
	// no longer 403s when the anonymous 60/hr/IP pool is drained.
	check := func(ctx context.Context, current string) (upgrade.CheckResult, error) {
		return upgrade.Checker{Token: upgrade.ResolveGitHubToken(ctx)}.Check(ctx, current)
	}
	return checkUpgradeCmdForVersion(ctx, buildinfo.Version, check)
}

func checkUpgradeCmdForVersion(ctx context.Context, current string, check upgradeCheckFunc) tea.Cmd {
	if ctx == nil {
		ctx = context.Background()
	}
	version, ok, dev := upgrade.NormalizeVersion(current)
	if !ok || dev {
		return func() tea.Msg {
			return upgradeCheckMsg{}
		}
	}
	if check == nil {
		checker := upgrade.Checker{}
		check = checker.Check
	}
	return func() tea.Msg {
		now := upgradeNow()
		cachePath := upgradeCachePath()
		if cachePath != "" {
			if entry, ok, err := upgrade.ReadFreshCache(cachePath, version, now, upgradeCacheTTL); err == nil && ok {
				return upgradeCheckMsg{
					current:   entry.CurrentVersion,
					latest:    entry.LatestVersion,
					url:       entry.ReleaseURL,
					available: upgrade.CompareStableVersions(entry.CurrentVersion, entry.LatestVersion) == upgrade.CompareOlder && !entry.Suppressed(now, entry.LatestVersion),
				}
			}
		}
		checkCtx, cancel := context.WithTimeout(ctx, upgradeCheckTimeout)
		defer cancel()
		result, err := check(checkCtx, version)
		available := result.Available
		if err == nil && cachePath != "" {
			entry := upgrade.CacheEntry{
				CheckedAt:      now,
				CurrentVersion: result.CurrentVersion,
				LatestVersion:  result.LatestVersion,
				ReleaseURL:     result.ReleaseURL,
			}
			// Preserve a prior snooze across the cache refresh — without
			// this the user's dismiss is silently discarded the moment
			// the cache TTL expires.
			if prior, ok, _ := upgrade.ReadCache(cachePath); ok && prior.SnoozedVersion == result.LatestVersion && !prior.SnoozedUntil.IsZero() && now.Before(prior.SnoozedUntil) {
				entry.SnoozedVersion = prior.SnoozedVersion
				entry.SnoozedUntil = prior.SnoozedUntil
				// An active snooze for this version suppresses the upgrade
				// prompt regardless of the prior availability check.
				available = false
			}
			_ = upgrade.WriteCache(cachePath, entry)
		}
		return upgradeCheckMsg{
			current:   result.CurrentVersion,
			latest:    result.LatestVersion,
			url:       result.ReleaseURL,
			available: available,
			err:       err,
		}
	}
}

var runUpgradeCmd = func(executable string) tea.Cmd {
	return func() tea.Msg {
		cmd := exec.Command(executable, "upgrade", "--yes", "--no-restart")
		output, err := cmd.CombinedOutput()
		return upgradeRunMsg{output: string(output), err: err}
	}
}

func (h HomeModel) handleUpgradeCheck(msg upgradeCheckMsg) (tea.Model, tea.Cmd) {
	h.upgradeChecking = false
	if msg.err != nil {
		// A drained anonymous rate limit is actionable, not a transient
		// blip: surface a short hint pointing at a token. Other check
		// errors stay silent (today's behavior) to avoid banner noise.
		var rlErr *upgrade.RateLimitError
		if errors.As(msg.err, &rlErr) {
			h.upgradeError = rlErr.Error()
		}
		return h, nil
	}
	h.upgradeError = ""
	h.upgradeAvailable = msg.available
	h.upgradeCurrent = msg.current
	h.upgradeLatest = msg.latest
	h.upgradeURL = msg.url
	return h, nil
}

func (h HomeModel) handleUpgradeRun(msg upgradeRunMsg) (tea.Model, tea.Cmd) {
	h.upgrading = false
	if msg.err != nil {
		h.upgradeError = upgradeRunError(msg)
		return h, nil
	}
	h.upgradeError = ""
	h.upgradeDone = true
	h.restartPrompt = true
	return h, nil
}

// handleUpgradeKey covers the restart prompt, the cloud-subscription "u" key,
// and the upgrade banner's [u]pgrade/[d]ismiss keys. It returns handled=false
// when the key belongs to a later stage, leaving the model untouched.
func (h HomeModel) handleUpgradeKey(msg tea.KeyMsg) (HomeModel, tea.Cmd, bool) {
	if h.restartPrompt {
		switch msg.String() {
		case "r", "enter":
			h.restarting = true
			h.upgradeError = ""
			return h, restartDaemonCmd(), true
		case "esc":
			h.restartPrompt = false
			return h, nil, true
		}
	}
	if msg.String() == "u" && h.canOpenCloudSubscription() && !h.upgradeAvailable {
		return h, func() tea.Msg { return startSubscriptionFlowMsg{} }, true
	}
	if h.upgradeAvailable {
		switch msg.String() {
		case "u":
			executable, err := os.Executable()
			if err != nil {
				h.upgradeError = err.Error()
				return h, nil, true
			}
			h.upgradeError = ""
			h.upgrading = true
			return h, runUpgradeCmd(executable), true
		case "d":
			h.upgradeAvailable = false
			if path := upgradeCachePath(); path != "" {
				_ = upgrade.SnoozeUpgrade(
					path,
					h.upgradeCurrent,
					h.upgradeLatest,
					h.upgradeURL,
					upgradeNow(),
					upgrade.DefaultSnoozeDuration,
				)
			}
			return h, nil, true
		}
	}
	return h, nil, false
}

func (h HomeModel) upgradeStatusView() string {
	var lines []string
	if h.upgradeError != "" {
		lines = append(lines, renderError("Upgrade: "+h.upgradeError, h.statusWrapWidth()))
	}
	switch {
	case h.upgrading:
		lines = append(lines, h.statusLine(nil, h.spinner.View()+statusUpgrading))
	case h.restarting:
		lines = append(lines, h.statusLine(nil, h.spinner.View()+statusRestartingDaemon))
	case h.restartPrompt:
		lines = append(lines, h.statusLine(colorWarning, statusUpgradeRestartPrompt))
	case h.upgradeDone:
		lines = append(lines, h.statusLine(nil, statusUpgradeDone))
	case h.upgradeAvailable:
		lines = append(lines, h.statusLine(colorWarning, fmt.Sprintf(
			statusUpgradeAvailableFormat,
			h.upgradeCurrent,
			h.upgradeLatest,
		)))
	}
	return strings.Join(lines, "\n")
}

func upgradeRunError(msg upgradeRunMsg) string {
	output := strings.TrimSpace(msg.output)
	if output == "" {
		return msg.err.Error()
	}
	return msg.err.Error() + "\n" + output
}

func (h HomeModel) withUpgradeStatus(content string) string {
	status := h.upgradeStatusView()
	if status == "" {
		return content
	}
	if content == "" {
		return status
	}
	return content + "\n" + status
}
