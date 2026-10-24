// Package homeprocess implements the operating-system services injected into Home.
package homeprocess

import (
	"context"
	"os/exec"
	"time"

	daemonrpc "github.com/recurser/boss/internal/adapter/daemonrpc"
	"github.com/recurser/boss/internal/daemonlife"
	"github.com/recurser/boss/internal/upgrade"
)

type Command = exec.Cmd
type CheckResult = upgrade.CheckResult
type CacheEntry = upgrade.CacheEntry
type RateLimitError = upgrade.RateLimitError
type Status = daemonlife.Status

const CacheTTL = upgrade.CacheTTL
const DefaultSnoozeDuration = upgrade.DefaultSnoozeDuration
const CompareOlder = upgrade.CompareOlder
const LifecyclePollInterval = daemonlife.LifecyclePollInterval
const LifecycleShutdownTimeout = daemonlife.LifecycleShutdownTimeout
const LifecycleStartupTimeout = daemonlife.LifecycleStartupTimeout

var NormalizeVersion = upgrade.NormalizeVersion
var CompareStableVersions = upgrade.CompareStableVersions

// System provides the production process, daemon and upgrade services.
type System struct{}

func (System) Command(name string, args ...string) *Command { return exec.Command(name, args...) }
func (System) LookPath(name string) (string, error)         { return exec.LookPath(name) }
func (System) DefaultSocketPath() (string, error)           { return daemonrpc.DefaultSocketPath() }
func (System) SocketReachable(path string) bool             { return daemonlife.IsSocketReachable(path) }
func (System) DaemonStatus() (*Status, error)               { return daemonlife.GetStatus() }
func (System) CachePath() string                            { return upgrade.DefaultCachePath() }
func (System) Check(ctx context.Context, current string) (CheckResult, error) {
	return upgrade.Checker{Token: upgrade.ResolveGitHubToken(ctx)}.Check(ctx, current)
}
func (System) ReadFreshCache(path, current string, now time.Time, ttl time.Duration) (CacheEntry, bool, error) {
	return upgrade.ReadFreshCache(path, current, now, ttl)
}
func (System) RecordCheck(path string, r CheckResult, now time.Time) (bool, error) {
	return upgrade.RecordCheck(path, r, now)
}
func (System) SnoozeUpgrade(path, current, latest, url string, now time.Time, duration time.Duration) error {
	return upgrade.SnoozeUpgrade(path, current, latest, url, now, duration)
}
