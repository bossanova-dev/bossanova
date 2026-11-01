// Package ghusage keeps the on-disk ledger of GitHub calls made with the shared
// token, attributed to the cron, stage or subsystem that made them, so the
// daemon can report who spent the quota over the last hour.
//
// The ledger is a directory of hour-bucketed JSON Lines files named
// <YYYYMMDDHH UTC>.jsonl. Each record is one short line,
//
//	{"at": <unix seconds>, "label": "<attribution>", "quota": "core"|"graphql"}
//
// appended with O_APPEND in a single write, so several processes, including
// non-Go gate and agent shims, can append to the same bucket without a lock.
package ghusage

import (
	"context"
	"path/filepath"
	"time"
)

// DirName is the ledger directory under the daemon's app data dir.
const DirName = "github-usage"

// The two GitHub quotas a gh call can spend: REST calls (`gh api repos/...`)
// spend "core"; `gh pr ...` and `gh api graphql` spend "graphql".
const (
	QuotaCore    = "core"
	QuotaGraphQL = "graphql"
)

// Labels for the bossd subsystems that spend the quota. Cron gates and agent
// sessions write their own records as cron:<name> and session:<kind>.
const (
	// LabelUnattributed is recorded for a call whose context carries no label,
	// so per-label counts always add up to the total.
	LabelUnattributed = "daemon:unattributed"
	LabelFinalize     = "stage:finalize"
	LabelRepair       = "stage:repair"
	LabelPoller       = "daemon:poller"
	LabelCallbacks    = "daemon:callbacks"
	LabelMerge        = "daemon:merge"
	LabelVerifyEvent  = "daemon:verify-event"
)

// DefaultWindow is the report window when the caller names none.
const DefaultWindow = time.Hour

// retention is how far back the ledger is kept: Prune removes a bucket once
// its whole hour is older than this, so any window up to MaxWindow is served
// from complete buckets.
const retention = 2 * time.Hour

// MaxWindow is the longest window Read can answer from what Prune keeps.
const MaxWindow = retention

// maxLabelBytes keeps every record line short enough for one atomic append.
const maxLabelBytes = 128

// bucketLayout names an hour bucket file, in UTC.
const bucketLayout = "2006010215"

const bucketExt = ".jsonl"

// Record is one ledger line: one GitHub call.
type Record struct {
	At    int64  `json:"at"`
	Label string `json:"label"`
	Quota string `json:"quota"`
}

// Dir returns the ledger directory under appDataDir.
func Dir(appDataDir string) string { return filepath.Join(appDataDir, DirName) }

// PluginLabel attributes a call made on behalf of the named plugin.
func PluginLabel(name string) string { return "plugin:" + name }

type labelKey struct{}

// WithLabel attributes every GitHub call made under ctx to label. An inner
// WithLabel overrides an outer one, so a subsystem labelled at its entry point
// keeps its own label when a broader caller invoked it.
func WithLabel(ctx context.Context, label string) context.Context {
	if label == "" {
		return ctx
	}
	return context.WithValue(ctx, labelKey{}, label)
}

// LabelFrom returns the attribution carried by ctx, or LabelUnattributed.
func LabelFrom(ctx context.Context) string {
	if ctx != nil {
		if label, ok := ctx.Value(labelKey{}).(string); ok && label != "" {
			return label
		}
	}
	return LabelUnattributed
}

func bucketName(t time.Time) string {
	return t.UTC().Format(bucketLayout) + bucketExt
}

// bucketStart parses a bucket file name back to the start of its hour.
func bucketStart(name string) (time.Time, bool) {
	if filepath.Ext(name) != bucketExt {
		return time.Time{}, false
	}
	t, err := time.ParseInLocation(bucketLayout, name[:len(name)-len(bucketExt)], time.UTC)
	if err != nil {
		return time.Time{}, false
	}
	return t, true
}
