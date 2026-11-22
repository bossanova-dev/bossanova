package config

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"testing"
)

// replaceSettingsFile replaces path with data atomically. A non-nil
// beforeReplace runs after the new bytes are durable and before the rename; an
// error from it leaves the file untouched.
func replaceSettingsFile(path string, data []byte, syncParentDir func(string) error, beforeReplace func() error) error {
	if err := guardRealDefaultWrite(
		path,
		testing.Testing(),
		refusesDefaultSettingsWrite(),
		realDefaultPath,
		initEnvSettingsPath,
	); err != nil {
		return err
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return fmt.Errorf("create settings directory %q: %w", path, err)
	}

	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".*.tmp")
	if err != nil {
		return fmt.Errorf("create settings temporary file %q: %w", path, err)
	}
	tmpName := tmp.Name()
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.Remove(tmpName)
		}
	}()

	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write settings temporary file %q: %w", path, err)
	}
	if err := tmp.Chmod(0o644); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("set settings temporary file permissions %q: %w", path, err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("sync settings temporary file %q: %w", path, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close settings temporary file %q: %w", path, err)
	}
	if beforeReplace != nil {
		if err := beforeReplace(); err != nil {
			return err
		}
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("replace settings %q: %w", path, err)
	}
	cleanup = false
	if err := syncParentDir(dir); err != nil {
		return fmt.Errorf("sync settings directory for %q: %w", path, err)
	}
	return nil
}

func syncDir(dir string) error {
	cleaned := filepath.Clean(dir)
	f, err := os.Open(cleaned)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	return syncDirectory(f)
}

// syncDirectory preserves the one platform-specific directory-sync error that
// is safe to ignore while returning every other failure to the caller.
func syncDirectory(f interface{ Sync() error }) error {
	if err := f.Sync(); err != nil && err != io.ErrUnexpectedEOF {
		return err
	}
	return nil
}
