package upgrade

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"time"
)

// DefaultSnoozeDuration is how long an upgrade banner stays dismissed after
// the user presses the dismiss key. Survives cache TTL expiry because the
// snooze fields are preserved across cache refresh when SnoozedVersion still
// matches the latest release.
const DefaultSnoozeDuration = 7 * 24 * time.Hour

type CacheEntry struct {
	CheckedAt      time.Time `json:"checked_at"`
	CurrentVersion string    `json:"current_version"`
	LatestVersion  string    `json:"latest_version"`
	ReleaseURL     string    `json:"release_url"`
	SnoozedUntil   time.Time `json:"snoozed_until,omitempty"`
	SnoozedVersion string    `json:"snoozed_version,omitempty"`
}

// ReadFreshCache returns the entry only if it matches the current version and
// is within the TTL. Use ReadCache for read paths that need to preserve
// snooze state across TTL expiry.
func ReadFreshCache(path, current string, now time.Time, ttl time.Duration) (CacheEntry, bool, error) {
	entry, ok, err := ReadCache(path)
	if err != nil || !ok {
		return CacheEntry{}, false, err
	}
	if entry.CurrentVersion != current {
		return CacheEntry{}, false, nil
	}
	if now.Sub(entry.CheckedAt) > ttl {
		return CacheEntry{}, false, nil
	}
	return entry, true, nil
}

// ReadCache returns the cache entry regardless of TTL or current-version
// match. Returns (zero, false, nil) when the file does not exist or contains
// unparsable JSON. The caller is responsible for any freshness checks.
func ReadCache(path string) (CacheEntry, bool, error) {
	body, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return CacheEntry{}, false, nil
		}
		return CacheEntry{}, false, err
	}
	var entry CacheEntry
	if err := json.Unmarshal(body, &entry); err != nil {
		return CacheEntry{}, false, nil
	}
	return entry, true, nil
}

// WriteCache atomically writes the entry to path. Concurrent writers from
// two boss processes can race on the read-modify-write cycle, but each
// individual write is atomic via os.Rename on the same filesystem; a 24h-TTL
// banner cache tolerates the race.
func WriteCache(path string, entry CacheEntry) error {
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return err
	}

	body, err := json.MarshalIndent(entry, "", "  ")
	if err != nil {
		return err
	}

	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, body, 0600); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}

	return nil
}

func (e CacheEntry) Suppressed(now time.Time, latestVersion string) bool {
	return e.SnoozedVersion == latestVersion && !e.SnoozedUntil.IsZero() && now.Before(e.SnoozedUntil)
}

// SnoozeUpgrade dismisses the upgrade banner for the given release until
// now+duration. The dismissal is preserved across cache TTL refresh as long
// as the latest version remains the snoozed version.
func SnoozeUpgrade(path, current, latest, url string, now time.Time, duration time.Duration) error {
	if path == "" {
		return nil
	}
	entry, _, _ := ReadCache(path)
	if entry.CurrentVersion != current {
		entry = CacheEntry{
			CheckedAt:      now,
			CurrentVersion: current,
			LatestVersion:  latest,
			ReleaseURL:     url,
		}
	} else {
		entry.LatestVersion = latest
		entry.ReleaseURL = url
	}
	entry.SnoozedVersion = latest
	entry.SnoozedUntil = now.Add(duration)
	return WriteCache(path, entry)
}
