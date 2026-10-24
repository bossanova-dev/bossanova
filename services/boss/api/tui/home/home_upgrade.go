// Home's upgrade check, upgrade run, and upgrade banner. Split out of home.go
// (BOS-526); the declarations moved unchanged, except handleUpgradeKey, whose
// return type was later narrowed from tea.Model to Model so handleKey can
// thread the model back on the declined path.

package home

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/recurser/boss/api/tui/ui"

	tea "charm.land/bubbletea/v2"
	upgrade "github.com/recurser/boss/internal/homeprocess"
	"github.com/recurser/bossalib/buildinfo"
)

// upgradeNow is the clock source used by the upgrade-check goroutine. Tests
// override it to pin a fixed instant.
var upgradeNow = time.Now

// upgradeCachePath returns the on-disk path for the upgrade banner cache,
// shared with the `boss upgrade` action via system.CachePath. Returns
// "" if the user's cache directory is unavailable; callers treat an empty path
// as "cache disabled" and skip both reads and writes. Kept as a var so tests
// can point it at a temp file.
var upgradeCachePath = System.CachePath

type upgradeCheckFunc func(context.Context, string) (upgrade.CheckResult, error)

// proofUpgradeRateLimit, when set via BOSS_PROOF_UPGRADE_RATE_LIMIT (proof
// scenarios only), makes the upgrade check yield a synthetic
// *upgrade.RateLimitError so the friendly rate-limit banner can be captured
// deterministically — without draining a real GitHub quota. Mirrors the
// BOSS_PROOF_HIDE_GUEST_OFFER seam in cloudpromo.go.
var proofUpgradeRateLimit = os.Getenv("BOSS_PROOF_UPGRADE_RATE_LIMIT") != ""

func checkUpgradeCmd(ctx context.Context, services ...Runtime) tea.Cmd {
	svc := System
	if len(services) > 0 && services[0] != nil {
		svc = services[0]
	}
	if proofUpgradeRateLimit {
		return func() tea.Msg {
			return upgradeCheckMsg{Err: &upgrade.RateLimitError{Resets: upgradeNow().Add(30 * time.Minute)}}
		}
	}
	// Resolve the token lazily, inside the check, so a warm cache skips both the
	// network request and the `gh auth token` subprocess — and Init never blocks
	// on gh. The token authenticates the release check (5000/hr) so the banner
	// no longer 403s when the anonymous 60/hr/IP pool is drained.
	check := func(ctx context.Context, current string) (upgrade.CheckResult, error) {
		return svc.Check(ctx, current)
	}
	return checkUpgradeCmdForVersion(ctx, buildinfo.Version, check, services...)
}

func checkUpgradeCmdForVersion(ctx context.Context, current string, check upgradeCheckFunc, services ...Runtime) tea.Cmd {
	svc := System
	cachePathFunc := upgradeCachePath
	if len(services) > 0 && services[0] != nil {
		svc = services[0]
		cachePathFunc = svc.CachePath
	}
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
		check = svc.Check
	}
	return func() tea.Msg {
		now := upgradeNow()
		cachePath := cachePathFunc()
		if cachePath != "" {
			if entry, ok, err := svc.ReadFreshCache(cachePath, version, now, upgradeCacheTTL); err == nil && ok {
				return upgradeCheckMsg{
					Current:   entry.CurrentVersion,
					Latest:    entry.LatestVersion,
					URL:       entry.ReleaseURL,
					Available: upgrade.CompareStableVersions(entry.CurrentVersion, entry.LatestVersion) == upgrade.CompareOlder && !entry.Suppressed(now, entry.LatestVersion),
				}
			}
		}
		checkCtx, cancel := context.WithTimeout(ctx, upgradeCheckTimeout)
		defer cancel()
		result, err := check(checkCtx, version)
		available := result.Available
		if err == nil && cachePath != "" {
			if snoozed, _ := svc.RecordCheck(cachePath, result, now); snoozed {
				available = false
			}
		}
		return upgradeCheckMsg{
			Current:   result.CurrentVersion,
			Latest:    result.LatestVersion,
			URL:       result.ReleaseURL,
			Available: available,
			Err:       err,
		}
	}
}

var runUpgradeCmd = func(executable string) tea.Cmd {
	return func() tea.Msg {
		cmd := System.Command(executable, "upgrade", "--yes", "--no-restart")
		output, err := cmd.CombinedOutput()
		return upgradeRunMsg{Output: string(output), Err: err}
	}
}

func (h Model) handleUpgradeCheck(msg upgradeCheckMsg) (tea.Model, tea.Cmd) {
	h.upgradeChecking = false
	if msg.Err != nil {
		// A drained anonymous rate limit is actionable, not a transient
		// blip: surface a short hint pointing at a token. Other check
		// errors stay silent (today's behavior) to avoid banner noise.
		var rlErr *upgrade.RateLimitError
		if errors.As(msg.Err, &rlErr) {
			h.upgradeError = rlErr.Error()
		}
		return h, nil
	}
	h.upgradeError = ""
	h.upgradeAvailable = msg.Available
	h.upgradeCurrent = msg.Current
	h.upgradeLatest = msg.Latest
	h.upgradeURL = msg.URL
	return h, nil
}

func (h Model) handleUpgradeRun(msg upgradeRunMsg) (tea.Model, tea.Cmd) {
	h.Upgrading = false
	if msg.Err != nil {
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
func (h Model) handleUpgradeKey(msg tea.KeyMsg) (Model, tea.Cmd, bool) {
	if h.restartPrompt {
		switch msg.String() {
		case "r", "enter":
			h.Restarting = true
			h.upgradeError = ""
			return h, restartDaemonCmd(h.runtime), true
		case "esc":
			h.restartPrompt = false
			return h, nil, true
		}
	}
	if msg.String() == "u" && h.canOpenCloudSubscription() && !h.upgradeAvailable {
		return h, func() tea.Msg { return StartSubscriptionFlowMsg{} }, true
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
			h.Upgrading = true
			return h, h.runUpgrade(executable), true
		case "d":
			h.upgradeAvailable = false
			cachePath := upgradeCachePath
			svc := System
			if h.runtime != nil {
				svc = h.runtime
				cachePath = svc.CachePath
			}
			if path := cachePath(); path != "" {
				_ = svc.SnoozeUpgrade(
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

func (h Model) upgradeStatusView() string {
	var lines []string
	if h.upgradeError != "" {
		lines = append(lines, renderError("Upgrade: "+h.upgradeError, h.statusWrapWidth()))
	}
	switch {
	case h.Upgrading:
		lines = append(lines, h.statusLine(nil, h.Spinner.View()+statusUpgrading))
	case h.Restarting:
		lines = append(lines, h.statusLine(nil, h.Spinner.View()+statusRestartingDaemon))
	case h.restartPrompt:
		lines = append(lines, h.statusLine(ui.ColorWarning, statusUpgradeRestartPrompt))
	case h.upgradeDone:
		lines = append(lines, h.statusLine(nil, statusUpgradeDone))
	case h.upgradeAvailable:
		lines = append(lines, h.statusLine(ui.ColorWarning, fmt.Sprintf(
			statusUpgradeAvailableFormat,
			h.upgradeCurrent,
			h.upgradeLatest,
		)))
	}
	return strings.Join(lines, "\n")
}

func upgradeRunError(msg upgradeRunMsg) string {
	output := strings.TrimSpace(msg.Output)
	if output == "" {
		return msg.Err.Error()
	}
	return msg.Err.Error() + "\n" + output
}

func (h Model) withUpgradeStatus(content string) string {
	status := h.upgradeStatusView()
	if status == "" {
		return content
	}
	if content == "" {
		return status
	}
	return content + "\n" + status
}

func (h Model) runUpgrade(executable string) tea.Cmd {
	if h.runtime == nil {
		return runUpgradeCmd(executable)
	}
	return func() tea.Msg {
		output, err := h.runtime.Command(executable, "upgrade", "--yes", "--no-restart").CombinedOutput()
		return upgradeRunMsg{Output: string(output), Err: err}
	}
}
