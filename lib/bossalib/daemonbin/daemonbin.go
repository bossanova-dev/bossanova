// Package daemonbin stages bossd in a stable, TCC-friendly location.
package daemonbin

import (
	"crypto/sha256"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

const (
	BinDirName = "bin"
	BossdName  = "bossd"
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
		return true, "staged binary missing", nil
	}
	if err != nil {
		return false, "", fmt.Errorf("stat staged binary: %w", err)
	}
	if !stagedInfo.Mode().IsRegular() {
		return true, "staged binary not regular", nil
	}
	if stagedInfo.Mode()&0o100 == 0 {
		return true, "staged binary not owner-executable", nil
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
		return true, "binary content mismatch", nil
	}

	return false, "", nil
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
