package home

import (
	"context"
	"time"

	"github.com/recurser/boss/internal/homeprocess"
)

// Runtime supplies Home's operating-system, daemon and upgrade operations.
type Runtime interface {
	Command(string, ...string) *homeprocess.Command
	LookPath(string) (string, error)
	DefaultSocketPath() (string, error)
	SocketReachable(string) bool
	DaemonStatus() (*homeprocess.Status, error)
	CachePath() string
	Check(context.Context, string) (homeprocess.CheckResult, error)
	ReadFreshCache(string, string, time.Time, time.Duration) (homeprocess.CacheEntry, bool, error)
	RecordCheck(string, homeprocess.CheckResult, time.Time) (bool, error)
	SnoozeUpgrade(string, string, string, string, time.Time, time.Duration) error
}

var System Runtime = homeprocess.System{}
