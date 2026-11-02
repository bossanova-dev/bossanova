package ghusage

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/rs/zerolog"
)

// Ledger appends, reads and prunes the hour-bucketed call ledger in one
// directory. Its methods are safe for concurrent use, and a nil *Ledger records
// nothing.
type Ledger struct {
	dir    string
	now    func() time.Time
	logger zerolog.Logger

	warnOnce sync.Once
}

// Option configures a Ledger.
type Option func(*Ledger)

// WithNow overrides the clock Record stamps records with (for testing).
func WithNow(now func() time.Time) Option {
	return func(l *Ledger) { l.now = now }
}

// New returns a Ledger over dir. The directory is created on first write.
func New(dir string, logger zerolog.Logger, opts ...Option) *Ledger {
	l := &Ledger{dir: dir, now: time.Now, logger: logger}
	for _, opt := range opts {
		opt(l)
	}
	return l
}

// Record appends one call to the current hour's bucket. identity is whose
// quota it spent; empty means IdentityUser. Metering must never fail a GitHub
// call, so a write error is logged once and otherwise swallowed.
func (l *Ledger) Record(label, quota, identity string) {
	l.write(label, quota, identity, "")
}

// RecordSkip appends one attempt that was refused without contacting GitHub,
// with outcome OutcomeBreaker or OutcomeFloor, against identity's quota. It
// fails like Record: a write error is logged once and otherwise swallowed.
func (l *Ledger) RecordSkip(label, quota, identity, outcome string) {
	l.write(label, quota, identity, outcome)
}

func (l *Ledger) write(label, quota, identity, outcome string) {
	if l == nil {
		return
	}
	if err := l.append(label, quota, identity, outcome); err != nil {
		l.warnOnce.Do(func() {
			l.logger.Warn().Err(err).Str("dir", l.dir).
				Msg("github usage ledger: write failed; calls are not being metered")
		})
	}
}

func (l *Ledger) append(label, quota, identity, outcome string) error {
	if label == "" {
		label = LabelUnattributed
	}
	if identity == "" {
		identity = IdentityUser
	}
	if len(label) > maxLabelBytes {
		label = label[:maxLabelBytes]
	}
	now := l.now()
	line, err := json.Marshal(Record{At: now.Unix(), Label: label, Quota: quota, Identity: identity, Outcome: outcome})
	if err != nil {
		return err
	}
	line = append(line, '\n')
	path := filepath.Join(l.dir, bucketName(now))
	f, err := openAppend(path)
	if errors.Is(err, fs.ErrNotExist) {
		if mkErr := os.MkdirAll(l.dir, 0o700); mkErr != nil {
			return mkErr
		}
		f, err = openAppend(path)
	}
	if err != nil {
		return err
	}
	// One write per record: with O_APPEND each short line lands whole even
	// when other processes append to the same bucket.
	_, werr := f.Write(line)
	cerr := f.Close()
	return errors.Join(werr, cerr)
}

func openAppend(path string) (*os.File, error) {
	// #nosec G304 -- path is the daemon-owned ledger dir joined with a generated bucket name
	return os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
}

// Count is what one label did against one quota: the calls it sent to
// GitHub and the attempts the breaker or the background floor refused.
type Count struct {
	Label        string
	Quota        string
	Calls        int64
	BreakerSkips int64
	FloorSkips   int64
}

// Skips is every refused attempt in c.
func (c Count) Skips() int64 { return c.BreakerSkips + c.FloorSkips }

// add counts one record's outcome. An outcome this reader does not know counts
// as neither a call nor a skip, so a future outcome cannot inflate the calls.
func (c *Count) add(outcome string) bool {
	switch outcome {
	case "", OutcomeSent:
		c.Calls++
	case OutcomeBreaker:
		c.BreakerSkips++
	case OutcomeFloor:
		c.FloorSkips++
	default:
		return false
	}
	return true
}

// Usage is the ledger's per-label, per-quota counts over a window.
type Usage struct {
	WindowStart time.Time
	WindowEnd   time.Time
	// Counts is sorted by Calls descending, then Skips descending, then
	// Label and Quota.
	Counts []Count
}

// Read counts the calls and skips recorded in the window ending at now. It
// reads every bucket the window touches, drops records outside the window and
// lines that do not parse (a torn or foreign line must not hide the rest), and
// treats a missing bucket as an hour with no calls.
func (l *Ledger) Read(now time.Time, window time.Duration) (Usage, error) {
	if window <= 0 {
		window = DefaultWindow
	}
	start := now.Add(-window)
	usage := Usage{WindowStart: start, WindowEnd: now, Counts: []Count{}}
	type key struct{ label, quota string }
	counts := map[key]*Count{}
	from, to := start.Unix(), now.Unix()
	for hour := start.UTC().Truncate(time.Hour); !hour.After(now); hour = hour.Add(time.Hour) {
		err := readBucket(filepath.Join(l.dir, bucketName(hour)), func(r Record) {
			if r.At < from || r.At > to {
				return
			}
			k := key{r.Label, r.Quota}
			c, ok := counts[k]
			if !ok {
				c = &Count{Label: r.Label, Quota: r.Quota}
			}
			if c.add(r.Outcome) && !ok {
				counts[k] = c
			}
		})
		if err != nil {
			return Usage{}, err
		}
	}
	for _, c := range counts {
		usage.Counts = append(usage.Counts, *c)
	}
	sort.Slice(usage.Counts, func(i, j int) bool {
		a, b := usage.Counts[i], usage.Counts[j]
		if a.Calls != b.Calls {
			return a.Calls > b.Calls
		}
		if a.Skips() != b.Skips() {
			return a.Skips() > b.Skips()
		}
		if a.Label != b.Label {
			return a.Label < b.Label
		}
		return a.Quota < b.Quota
	})
	return usage, nil
}

// readBucket calls fn for every well-formed record in one bucket file.
func readBucket(path string, fn func(Record)) error {
	// #nosec G304 -- path is the daemon-owned ledger dir joined with a generated bucket name
	f, err := os.Open(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("open github usage bucket: %w", err)
	}
	defer func() { _ = f.Close() }()
	r := bufio.NewReader(f)
	for {
		line, err := r.ReadBytes('\n')
		if rec, ok := parseRecord(line); ok {
			fn(rec)
		}
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("read github usage bucket: %w", err)
		}
	}
}

func parseRecord(line []byte) (Record, bool) {
	var rec Record
	if err := json.Unmarshal(line, &rec); err != nil {
		return Record{}, false
	}
	if rec.At <= 0 || rec.Quota == "" {
		return Record{}, false
	}
	if rec.Label == "" {
		rec.Label = LabelUnattributed
	}
	if rec.Identity == "" {
		rec.Identity = IdentityUser
	}
	return rec, true
}

// Prune deletes every bucket whose hour ended more than the retention period
// before now and reports how many it removed. Files that are not buckets are
// left alone, and a missing ledger directory is not an error.
func (l *Ledger) Prune(now time.Time) (int, error) {
	entries, err := os.ReadDir(l.dir)
	if errors.Is(err, fs.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("list github usage ledger: %w", err)
	}
	cutoff := now.Add(-retention)
	removed := 0
	var errs []error
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		hour, ok := bucketStart(e.Name())
		if !ok || !hour.Add(time.Hour).Before(cutoff) {
			continue
		}
		if err := os.Remove(filepath.Join(l.dir, e.Name())); err != nil && !errors.Is(err, fs.ErrNotExist) {
			errs = append(errs, err)
			continue
		}
		removed++
	}
	return removed, errors.Join(errs...)
}
