package skillinstall

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

// payloadRecordFile names the record of which payload last wrote an installed
// tree. It sits beside the update lock at the install root, never under
// Namespace, so the drift walk of the namespace directory cannot report it as
// an unexpected installed file and extract's namespace cleanup cannot remove it
// out from under a waiting writer.
const payloadRecordFile = ".bossanova-skills.payload.json"

// PayloadOrigin is where a payload's bytes came from.
type PayloadOrigin string

const (
	// OriginCheckout is a payload snapshotted from a checkout's skill sources.
	OriginCheckout PayloadOrigin = "checkout"
	// OriginEmbedded is a payload compiled into the writing binary.
	OriginEmbedded PayloadOrigin = "embedded"
)

// PayloadWriter is whether a person asked for the write or a process did it on
// its own.
type PayloadWriter string

const (
	// WriterExplicit is an operator command or consent: `boss skills install`,
	// `boss skills sync`, an accepted interactive prompt, or a self-heal run
	// with the checkout explicitly trusted.
	WriterExplicit PayloadWriter = "explicit"
	// WriterUnattended is a background refresh no operator asked for: the
	// headless CLI self-heal from the embedded payload, or a plugin start.
	WriterUnattended PayloadWriter = "unattended"
)

// PayloadRecord identifies the payload that wrote an installed tree, so a later
// unattended writer can tell whether its own payload is older.
type PayloadRecord struct {
	Origin PayloadOrigin `json:"origin"`
	// Revision is the payload's git revision: a full SHA where known, the
	// build's short SHA for an embedded payload, or "unknown".
	Revision string `json:"revision"`
	// Version is the writing binary's version string.
	Version string `json:"version"`
	// Dirty reports that the payload's bytes may differ from Revision (an
	// uncommitted checkout edit, or a binary built from a dirty tree).
	Dirty  bool          `json:"dirty,omitempty"`
	Writer PayloadWriter `json:"writer"`
	// Pinned reports that an unattended refresh moved the tree forward from an
	// explicit or checkout install (or from a refresh already pinned to one)
	// and so inherits that install's floor: a later writer whose order is
	// unprovable holds over it exactly as it would over the install itself.
	Pinned bool `json:"pinned,omitempty"`
}

// vouched reports whether the record carries an explicit or checkout floor
// that an unattended writer of unprovable order must not replace.
func (r PayloadRecord) vouched() bool {
	return r.Writer == WriterExplicit || r.Origin == OriginCheckout || r.Pinned
}

// EmbeddedPayloadRecord describes a payload compiled into the writing binary
// from its stamped build info. A blank commit is "unknown" (not a revision), and
// a `-dirty` version marks the payload dirty, so neither is ever comparable.
func EmbeddedPayloadRecord(commit, version string, writer PayloadWriter) PayloadRecord {
	revision := commit
	if strings.TrimSpace(revision) == "" {
		revision = "unknown"
	}
	return PayloadRecord{
		Origin:   OriginEmbedded,
		Revision: revision,
		Version:  version,
		Dirty:    strings.HasSuffix(version, "-dirty"),
		Writer:   writer,
	}
}

// Comparable reports whether the record's revision names a commit whose
// content the payload is. An unstamped, dev-fallback or dirty payload is not a
// revision, so nothing may be proven from it.
func (r PayloadRecord) Comparable() bool {
	switch r.Revision {
	case "", "unknown":
		return false
	}
	return r.Version != "dev" && !r.Dirty
}

// String renders the record for a one-line operator message.
func (r PayloadRecord) String() string {
	revision := r.Revision
	if revision == "" {
		revision = "unknown"
	}
	label := fmt.Sprintf("%s %s", r.Origin, revision)
	if r.Version != "" && r.Version != revision {
		label += " (" + r.Version + ")"
	}
	if r.Dirty {
		label += " dirty"
	}
	if r.Pinned {
		label += " pinned"
	}
	return label
}

// sameRevision reports whether two comparable records name one commit. An
// embedded payload carries a short SHA and a checkout a full one, so a prefix
// match of at least seven hex digits counts.
func sameRevision(a, b PayloadRecord) bool {
	if !a.Comparable() || !b.Comparable() {
		return false
	}
	shorter, longer := a.Revision, b.Revision
	if len(shorter) > len(longer) {
		shorter, longer = longer, shorter
	}
	return len(shorter) >= 7 && strings.HasPrefix(longer, shorter)
}

// Ordering is an oracle's answer about whether a payload is older than the one
// that wrote the installed tree.
type Ordering int

const (
	// OrderUnknown means the order could not be proven either way.
	OrderUnknown Ordering = iota
	// OrderNotOlder means the payload provably contains the installed
	// payload's revision (it is the same commit or a descendant).
	OrderNotOlder
	// OrderOlder means the installed payload provably contains the payload's
	// revision and not the other way round.
	OrderOlder
)

// OrderFunc answers how payload relates to installed. It is injected rather
// than implemented here because the proof (git ancestry) needs a checkout this
// package must not reach for, and because revisiondrift imports this package,
// so this package cannot import it back.
type OrderFunc func(installed, payload PayloadRecord) Ordering

var releaseVersionPattern = regexp.MustCompile(`^v(\d+)\.(\d+)\.(\d+)$`)

// ReleaseVersionOrder orders two records by their release versions. It proves
// an order only when both are clean release tags (vX.Y.Z); every other version
// string is a build label, not a fact about history, and answers OrderUnknown.
func ReleaseVersionOrder(installed, payload PayloadRecord) Ordering {
	left, ok := parseReleaseVersion(installed.Version)
	if !ok || installed.Dirty {
		return OrderUnknown
	}
	right, ok := parseReleaseVersion(payload.Version)
	if !ok || payload.Dirty {
		return OrderUnknown
	}
	for i := range left {
		switch {
		case right[i] > left[i]:
			return OrderNotOlder
		case right[i] < left[i]:
			return OrderOlder
		}
	}
	return OrderNotOlder
}

func parseReleaseVersion(version string) ([3]int, bool) {
	match := releaseVersionPattern.FindStringSubmatch(version)
	if match == nil {
		return [3]int{}, false
	}
	var parts [3]int
	for i := range parts {
		n, err := strconv.Atoi(match[i+1])
		if err != nil {
			return [3]int{}, false
		}
		parts[i] = n
	}
	return parts, true
}

// RefreshDecision is the verdict of the no-downgrade rule for one tree.
type RefreshDecision struct {
	// Refresh is true when the payload may replace the installed bytes.
	Refresh bool
	// Recorded is false for a legacy tree nothing has stamped.
	Recorded bool
	// Installed is the record found on the tree (zero when Recorded is false).
	Installed PayloadRecord
	// Ordering is the oracle's answer, OrderUnknown when it was not consulted.
	Ordering Ordering
	// Reason is a short phrase naming the matrix row that decided.
	Reason string
}

// DecideRefresh applies the unattended no-downgrade matrix. An unattended
// writer may move a tree forward, never backward: it replaces the installed
// bytes only when its payload is provably not older than the one that wrote
// them, or when nothing recorded which payload that was.
func DecideRefresh(installed PayloadRecord, recorded bool, payload PayloadRecord, order OrderFunc) RefreshDecision {
	decision := RefreshDecision{Recorded: recorded, Installed: installed}
	if !recorded {
		decision.Refresh = true
		decision.Reason = "no payload record (legacy tree)"
		return decision
	}
	if sameRevision(installed, payload) {
		decision.Refresh = true
		decision.Reason = "same revision"
		return decision
	}
	if order != nil {
		decision.Ordering = order(installed, payload)
	}
	switch decision.Ordering {
	case OrderNotOlder:
		decision.Refresh = true
		decision.Reason = "payload is not older than the installed payload"
		return decision
	case OrderOlder:
		decision.Reason = "payload is older than the installed payload"
		return decision
	case OrderUnknown:
	}
	if installed.vouched() {
		decision.Reason = "payload order is unprovable over an explicit or checkout install"
		if installed.Pinned {
			decision.Reason = "payload order is unprovable over a refresh pinned to an explicit or checkout install"
		}
		return decision
	}
	decision.Refresh = true
	decision.Reason = "payload order is unprovable over an unattended embedded refresh"
	return decision
}

// guardedStamp is the record an unattended refresh stamps after decision let
// payload replace the tree. Moving forward from a vouched record keeps its
// floor (Pinned), so the tree does not become an ordinary unattended refresh
// that any writer of unprovable order may replace. A legacy tree's Installed
// is the zero record, which is not vouched, so its stamp is never pinned.
func guardedStamp(payload PayloadRecord, decision RefreshDecision) PayloadRecord {
	if decision.Installed.vouched() {
		payload.Pinned = true
	}
	return payload
}

// ReadPayloadRecord returns the record stamped on the tree at dir. A missing
// record is a legacy tree (recorded false, nil error). A record that exists but
// does not parse is treated the same way: the matrix's legacy row is today's
// behaviour, and the next write replaces it.
func ReadPayloadRecord(dir string) (record PayloadRecord, recorded bool, err error) {
	data, err := os.ReadFile(filepath.Join(dir, payloadRecordFile))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return PayloadRecord{}, false, nil
		}
		return PayloadRecord{}, false, fmt.Errorf("read skill payload record: %w", err)
	}
	if err := json.Unmarshal(data, &record); err != nil {
		return PayloadRecord{}, false, nil
	}
	return record, true, nil
}

// writePayloadRecord replaces the record atomically: a reader sees either the
// previous record or the complete new one, never a torn write.
func writePayloadRecord(dir string, record PayloadRecord) error {
	data, err := json.MarshalIndent(record, "", "  ")
	if err != nil {
		return fmt.Errorf("encode skill payload record: %w", err)
	}
	tmp, err := os.CreateTemp(dir, payloadRecordFile+".*.tmp")
	if err != nil {
		return fmt.Errorf("create skill payload record: %w", err)
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()
	if _, err := tmp.Write(append(data, '\n')); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write skill payload record: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close skill payload record: %w", err)
	}
	if err := os.Chmod(tmpName, 0o644); err != nil {
		return fmt.Errorf("chmod skill payload record: %w", err)
	}
	if err := os.Rename(tmpName, filepath.Join(dir, payloadRecordFile)); err != nil {
		return fmt.Errorf("replace skill payload record: %w", err)
	}
	return nil
}

// stampPayloadRecordLocked writes record over a tree that already matches its
// payload, skipping the write when the stamp is already exactly that record.
// The caller holds the update lock.
func stampPayloadRecordLocked(dir string, record PayloadRecord) error {
	existing, recorded, err := ReadPayloadRecord(dir)
	if err != nil {
		return err
	}
	if recorded && existing == record {
		return nil
	}
	return writePayloadRecord(dir, record)
}

func removePayloadRecord(dir string) error {
	if err := os.Remove(filepath.Join(dir, payloadRecordFile)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("remove skill payload record: %w", err)
	}
	return nil
}

// GuardedResult reports what EnsureUpdatedGuarded did.
type GuardedResult struct {
	// Updated is true when the tree was rewritten from the payload.
	Updated bool
	// Held is true when the tree differed from the payload but the
	// no-downgrade rule refused the write; the installed bytes are unchanged.
	Held bool
	// Decision is the matrix verdict. Zero when no decision was needed (the
	// tree was absent or already current).
	Decision RefreshDecision
}

// EnsureUpdatedGuarded is EnsureUpdated for unattended writers. It refreshes an
// installed tree that differs from fsys only when DecideRefresh allows it, and
// stamps payload on the tree after a complete write or over a matching tree
// nothing has stamped yet (plan R4's legacy migration). The ordering decision and
// the write happen under one hold of the update lock, so two concurrent writers
// cannot interleave one's check with the other's write.
func EnsureUpdatedGuarded(dir string, fsys fs.FS, payload PayloadRecord, order OrderFunc) (GuardedResult, error) {
	return ensureUpdated(dir, fsys, &payload, func(installed PayloadRecord, recorded bool) RefreshDecision {
		return DecideRefresh(installed, recorded, payload, order)
	})
}

// InstalledRefreshDecision is the DecideRefresh verdict for replacing the tree
// at dir with payload, read without taking the update lock. The interactive
// prompt consults it so a stale binary does not offer a downgrade as an
// "Update"; the accepted write itself stays an explicit ExtractRecorded.
func InstalledRefreshDecision(dir string, payload PayloadRecord, order OrderFunc) (RefreshDecision, error) {
	installed, recorded, err := ReadPayloadRecord(dir)
	if err != nil {
		return RefreshDecision{}, err
	}
	return DecideRefresh(installed, recorded, payload, order), nil
}

// ExtractRecorded is Extract that stamps record on the tree once it is
// complete. Explicit install paths use it so the next unattended writer can
// tell whether its own payload is older.
func ExtractRecorded(dir string, fsys fs.FS, record PayloadRecord) (err error) {
	lock, err := acquireUpdateLock(dir)
	if err != nil {
		return err
	}
	defer func() {
		if unlockErr := lock.Unlock(); err == nil && unlockErr != nil {
			err = fmt.Errorf("release skill update lock: %w", unlockErr)
		}
	}()
	return extract(dir, fsys, &record)
}

// EnsureUpdatedRecorded is EnsureUpdated that stamps record after a refresh,
// and over an installed tree that already matches fsys. It keeps
// EnsureUpdated's overwrite-on-difference contract, so it is for explicit
// writers only; unattended writers use EnsureUpdatedGuarded.
func EnsureUpdatedRecorded(dir string, fsys fs.FS, record PayloadRecord) (bool, error) {
	result, err := ensureUpdated(dir, fsys, &record, nil)
	return result.Updated, err
}
