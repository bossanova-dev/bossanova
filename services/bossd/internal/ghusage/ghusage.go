// Package ghusage keeps the on-disk ledger of GitHub calls made with the shared
// token, attributed to the cron, stage or subsystem that made them, so the
// daemon can report who spent the quota over the last hour.
//
// The ledger is a directory of hour-bucketed JSON Lines files named
// <YYYYMMDDHH UTC>.jsonl. Each record is one short line,
//
//	{"at": <unix seconds>, "label": "<attribution>", "quota": "core"|"graphql", "identity": "<credential>"}
//
// with an "outcome" field only on an attempt that was refused without
// contacting GitHub ("breaker" or "floor"); a line without one is a sent call,
// appended with O_APPEND in a single write, so several processes, including
// non-Go gate and agent shims, can append to the same bucket without a lock.
package ghusage

import (
	"context"
	"path/filepath"
	"strconv"
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

// IdentityUser is the identity of a call made with the operator's own gh
// login. A call made with a GitHub App installation token records
// InstallationIdentity(id) instead. A record never carries the token itself.
const IdentityUser = "user"

// InstallationIdentity names the installation token pool of installation id.
func InstallationIdentity(id int64) string {
	return "installation:" + strconv.FormatInt(id, 10)
}

// Labels for the bossd subsystems that spend the quota. Cron gates and agent
// sessions write their own records as cron:<name> and session:<kind>.
const (
	// LabelUnattributed is recorded for a call whose context carries no label,
	// so per-label counts always add up to the total.
	LabelUnattributed = "daemon:unattributed"
	LabelFinalize     = "stage:finalize"
	LabelRepair       = "stage:repair"
	// LabelPRPoller and LabelDisplayPoller are the two session pollers.
	LabelPRPoller      = "daemon:pr-poller"
	LabelDisplayPoller = "daemon:display-poller"
	// LabelReconcileTitle and LabelReconcilePRLookup are reconcile's two
	// GitHub reads: the no-commits title repair and the find-PR listing.
	LabelReconcileTitle    = "daemon:reconcile-title"
	LabelReconcilePRLookup = "daemon:reconcile-pr-lookup"
	LabelCallbacks         = "daemon:callbacks"
	LabelMerge             = "daemon:merge"
	LabelVerifyEvent       = "daemon:verify-event"
	LabelPRState           = "daemon:pr-state"
	// LabelPRWait attributes the reads a WaitPRState call makes, so a wait's
	// cost shows apart from one-shot PR-state reads.
	LabelPRWait = "daemon:pr-wait"
)

// Outcomes of a metered attempt. A record with no outcome, or OutcomeSent, is
// a call sent to GitHub; the others are attempts refused without contacting
// it: by the rate-limit breaker while a quota is paused, or by the background
// quota floor.
const (
	OutcomeSent    = "sent"
	OutcomeBreaker = "breaker"
	OutcomeFloor   = "floor"
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

// Record is one ledger line: one GitHub call, or one refused attempt when
// Outcome is set.
type Record struct {
	At    int64  `json:"at"`
	Label string `json:"label"`
	Quota string `json:"quota"`
	// Identity is whose quota the call spent: IdentityUser or an
	// InstallationIdentity. Records written before it existed read as
	// IdentityUser.
	Identity string `json:"identity,omitempty"`
	Outcome  string `json:"outcome,omitempty"`
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
