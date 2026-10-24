// Package daemonbin stages bossd in a stable, TCC-friendly location.
package daemonbin

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	BinDirName = "bin"
	BossdName  = "bossd"
)

// Homebrew Cellar path shape: <brewPrefix>/Cellar/<CellarFormulaName>/<version>/bin.
const (
	cellarDirName     = "Cellar"
	CellarFormulaName = "bossanova"
)

// Reason vocabulary shared by NeedsStage and Inspect. One definition keeps the
// CLI warning, `boss daemon doctor` and `boss daemon status` from inventing
// three different phrasings for the same fact (BOS-864).
const (
	ReasonStagedMissing       = "staged binary missing"
	ReasonStagedNotRegular    = "staged binary not regular"
	ReasonStagedNotExecutable = "staged binary not owner-executable"
	ReasonContentMismatch     = "binary content mismatch"
	ReasonSizeMismatch        = "binary size mismatch"
	ReasonSourceUnavailable   = "source binary unavailable"
	ReasonStagedUnreadable    = "staged binary unreadable"
	ReasonDaemonStartUnknown  = "daemon start time unknown"
	ReasonRunningBehindStaged = "daemon started before the staged binary was written"
)

// StagedPath returns the stable path to the staged bossd file. The file is
// always real rather than a symlink, because macOS TCC resolves symlinks.
func StagedPath(appDataDir string) string {
	return filepath.Join(appDataDir, BinDirName, BossdName)
}

// NeedsStage reports whether sourcePath must be copied to stagedPath.
func NeedsStage(sourcePath, stagedPath string) (bool, string, error) {
	sourceInfo, err := os.Stat(sourcePath)
	if err != nil {
		return false, "", fmt.Errorf("stat source binary: %w", err)
	}
	if !sourceInfo.Mode().IsRegular() {
		return false, "", fmt.Errorf("source binary is not regular")
	}

	stagedInfo, err := os.Lstat(stagedPath)
	if os.IsNotExist(err) {
		return true, ReasonStagedMissing, nil
	}
	if err != nil {
		return false, "", fmt.Errorf("stat staged binary: %w", err)
	}
	if !stagedInfo.Mode().IsRegular() {
		return true, ReasonStagedNotRegular, nil
	}
	if stagedInfo.Mode()&0o100 == 0 {
		return true, ReasonStagedNotExecutable, nil
	}

	sourceHash, err := fileSHA256(sourcePath)
	if err != nil {
		return false, "", fmt.Errorf("hash source binary: %w", err)
	}
	stagedHash, err := fileSHA256(stagedPath)
	if err != nil {
		return false, "", fmt.Errorf("hash staged binary: %w", err)
	}
	if sourceHash != stagedHash {
		return true, ReasonContentMismatch, nil
	}

	return false, "", nil
}

// HomebrewCellarPrefixes parses a Homebrew Cellar bin directory, returning the
// formula prefix (…/Cellar/bossanova/<version>) and the Homebrew prefix (the
// path above Cellar, e.g. /opt/homebrew). ok is false for any other shape.
//
// This is the single definition of the Cellar shape: `boss`'s own
// homebrewPrefixesForBinDir delegates here rather than re-deriving it, so the
// plugin-directory resolver and the staleness warning can never disagree about
// what "a Homebrew install" means.
func HomebrewCellarPrefixes(binDir string) (formulaPrefix, homebrewPrefix string, ok bool) {
	clean := filepath.Clean(binDir)
	parts := strings.Split(filepath.ToSlash(clean), "/")
	for i := 0; i+3 < len(parts); i++ {
		if parts[i] == cellarDirName && parts[i+1] == CellarFormulaName && parts[i+3] == BinDirName {
			return filepath.FromSlash(strings.Join(parts[:i+3], "/")), filepath.FromSlash(strings.Join(parts[:i], "/")), true
		}
	}
	return "", "", false
}

// IsHomebrewCellarBinary reports whether path is a binary inside a Homebrew
// Cellar keg for this formula — the released install path, and the only one a
// `brew upgrade` moves.
//
// The filepath.EvalSymlinks step is load-bearing. daemon.ResolveBossdPath
// prefers the binary next to the running executable, so on a released install
// it resolves to <brewPrefix>/bin/bossd, which is a *symlink* into the Cellar.
// Classifying the unresolved path returns false and silently disables the
// staleness warning on exactly the installs it exists for (BOS-864).
func IsHomebrewCellarBinary(path string) bool {
	if path == "" {
		return false
	}
	resolved := path
	if evaluated, err := filepath.EvalSymlinks(path); err == nil {
		resolved = evaluated
	}
	_, _, ok := HomebrewCellarPrefixes(filepath.Dir(resolved))
	return ok
}

// Staleness is the classifier's verdict about two independent facts that have
// historically been conflated (BOS-864):
//
//   - Is the staged *file* current? (StagedBehindSource)
//   - Is the *running process* executing those bytes? (RunningBehindStaged)
//
// Each carries its own "known" flag, because an undeterminable input must be
// reported as unknown and never as healthy.
type Staleness struct {
	// StagedBehindSource reports that the staged file does not match the
	// installed source binary. Meaningful only when StagedKnown is true.
	StagedBehindSource bool
	// StagedKnown is false when the staged-vs-source comparison could not be
	// made at all (unresolvable source, unreadable staged file).
	StagedKnown bool
	// RunningBehindStaged reports that the live daemon started before the
	// staged file was last written, so it is not executing those bytes.
	// Meaningful only when RunningKnown is true.
	RunningBehindStaged bool
	// RunningKnown is false when the running-image comparison could not be
	// made (no staged file to date, or a zero daemonStartedAt).
	RunningKnown bool
	// Reason is a short human phrase from the shared Reason* vocabulary.
	Reason string
	// StagedModTime is when the staged file was last replaced. Stage writes a
	// temp file and renames it, so this is exactly "when the current bytes
	// were placed". Zero when the staged file is missing or unreadable.
	StagedModTime time.Time
	// DaemonStartedAt echoes the timestamp the caller supplied.
	DaemonStartedAt time.Time
}

// Inspect classifies the staged binary against its source and against the
// running daemon. It is a pure comparison and never writes: no code path here
// calls Stage, so no re-stage loop and no silent dev-build downgrade is
// expressible.
//
// A zero daemonStartedAt means "unknown" and leaves RunningKnown false. On
// error the returned Staleness has both known flags false, so an error can
// never be misread as a healthy verdict.
func Inspect(sourcePath, stagedPath string, daemonStartedAt time.Time) (Staleness, error) {
	result := Staleness{DaemonStartedAt: daemonStartedAt}

	stagedInfo, stagedErr := os.Lstat(stagedPath)
	stagedMissing := stagedErr != nil && errors.Is(stagedErr, os.ErrNotExist)
	if stagedErr != nil && !stagedMissing {
		result.Reason = ReasonStagedUnreadable
		return result, fmt.Errorf("stat staged binary: %w", stagedErr)
	}
	if !stagedMissing {
		result.StagedModTime = stagedInfo.ModTime()
	}

	// Running-vs-staged is answerable only with both a staged file and a
	// recorded daemon start time.
	switch {
	case stagedMissing:
		result.Reason = ReasonStagedMissing
	case daemonStartedAt.IsZero():
		result.Reason = ReasonDaemonStartUnknown
	default:
		result.RunningKnown = true
		result.RunningBehindStaged = daemonStartedAt.Before(result.StagedModTime)
	}

	sourceInfo, sourceErr := os.Stat(sourcePath)
	sourceUsable := sourceErr == nil && sourceInfo.Mode().IsRegular()
	switch {
	case !sourceUsable:
		// The source is unavailable, so staged-vs-source stays unknown. The
		// running-vs-staged verdict computed above is independent and stands.
		if result.RunningBehindStaged {
			result.Reason = ReasonRunningBehindStaged
		} else {
			result.Reason = ReasonSourceUnavailable
		}
		return result, nil
	case stagedMissing:
		result.StagedKnown = true
		result.StagedBehindSource = true
		result.Reason = ReasonStagedMissing
		return result, nil
	case !stagedInfo.Mode().IsRegular():
		result.StagedKnown = true
		result.StagedBehindSource = true
		result.Reason = ReasonStagedNotRegular
		return result, nil
	case stagedInfo.Mode()&0o100 == 0:
		result.StagedKnown = true
		result.StagedBehindSource = true
		result.Reason = ReasonStagedNotExecutable
		return result, nil
	case sourceInfo.Size() != stagedInfo.Size():
		// Stat-only screen: a version bump changes size in practice, so the
		// common case never hashes two large binaries on the CLI hot path.
		result.StagedKnown = true
		result.StagedBehindSource = true
		result.Reason = ReasonSizeMismatch
		return result, nil
	}

	// Equal sizes: hash to confirm, so a same-version `brew reinstall` never
	// produces a false warning.
	sourceHash, err := fileSHA256(sourcePath)
	if err != nil {
		return Staleness{DaemonStartedAt: daemonStartedAt, Reason: ReasonSourceUnavailable}, fmt.Errorf("hash source binary: %w", err)
	}
	stagedHash, err := fileSHA256(stagedPath)
	if err != nil {
		return Staleness{DaemonStartedAt: daemonStartedAt, Reason: ReasonStagedUnreadable}, fmt.Errorf("hash staged binary: %w", err)
	}

	result.StagedKnown = true
	result.StagedBehindSource = sourceHash != stagedHash
	switch {
	case result.StagedBehindSource:
		result.Reason = ReasonContentMismatch
	case result.RunningBehindStaged:
		result.Reason = ReasonRunningBehindStaged
	case result.RunningKnown:
		// Fully current on both axes. Leave the unknown reason in place when
		// RunningKnown is false so a zero start time never reads as healthy.
		result.Reason = ""
	}
	return result, nil
}

// Stage atomically copies sourcePath to stagedPath with executable permissions.
func Stage(sourcePath, stagedPath string) error {
	dir := filepath.Dir(stagedPath)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create staged binary directory: %w", err)
	}

	sourceInfo, err := os.Stat(sourcePath)
	if err != nil {
		return fmt.Errorf("stat source binary: %w", err)
	}
	if !sourceInfo.Mode().IsRegular() {
		return fmt.Errorf("source binary is not regular")
	}

	// #nosec G304 -- caller-supplied source path is the binary being staged.
	// owner=@recurser review-by=2027-01-18 issue=BOS-696
	source, err := os.Open(sourcePath)
	if err != nil {
		return fmt.Errorf("open source binary: %w", err)
	}
	defer func() { _ = source.Close() }()

	temporary, err := os.CreateTemp(dir, ".bossd-stage-*")
	if err != nil {
		return fmt.Errorf("create staged binary temp file: %w", err)
	}
	temporaryPath := temporary.Name()
	committed := false
	defer func() {
		if !committed {
			_ = os.Remove(temporaryPath)
		}
	}()

	if _, err := io.Copy(temporary, source); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("copy source binary: %w", err)
	}
	if err := temporary.Chmod(0o755); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("chmod staged binary: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("close staged binary: %w", err)
	}
	if err := os.Rename(temporaryPath, stagedPath); err != nil {
		return fmt.Errorf("replace staged binary: %w", err)
	}
	committed = true

	return nil
}

func fileSHA256(path string) (string, error) {
	// #nosec G304 -- caller-supplied path is a source or staged binary being verified.
	// owner=@recurser review-by=2027-01-18 issue=BOS-696
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer func() { _ = file.Close() }()

	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return "", err
	}
	return fmt.Sprintf("%x", hash.Sum(nil)), nil
}
