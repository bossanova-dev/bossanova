package skillinstall

import (
	"crypto/rand"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

const (
	stageDirPrefix  = ".bossanova-stage-"
	tempLinkPrefix  = ".bossanova-link-"
	backupDirPrefix = ".bossanova-prev-"
)

// replaceDirAtomic installs a fully staged directory at target. When target
// already exists as a directory, it swaps stage and target atomically so no
// reader ever observes a missing directory or a half-written mix of old and
// new files. On filesystems that do not support atomic directory exchange, it
// falls back to rename-out + rename-in with rollback.
func replaceDirAtomic(stage, target string) error {
	info, err := os.Lstat(target)
	if err != nil {
		if os.IsNotExist(err) {
			return os.Rename(stage, target)
		}
		return err
	}
	if !info.IsDir() {
		if err := os.Remove(target); err != nil {
			return err
		}
		return os.Rename(stage, target)
	}
	err = swapDirAtomic(stage, target)
	if err == nil {
		return nil
	}
	if !errors.Is(err, syscall.ENOSYS) && !errors.Is(err, syscall.ENOTSUP) && !errors.Is(err, syscall.EINVAL) {
		return err
	}
	return replaceDirFallback(stage, target)
}

func replaceDirFallback(stage, target string) error {
	token := make([]byte, 8)
	if _, err := rand.Read(token); err != nil {
		return err
	}
	backup := filepath.Join(filepath.Dir(stage), fmt.Sprintf("%s%x", backupDirPrefix, token))
	if err := os.Rename(target, backup); err != nil {
		return err
	}
	if err := os.Rename(stage, target); err != nil {
		_ = os.Rename(backup, target)
		return err
	}
	return os.RemoveAll(backup)
}

// replaceSymlinkAtomic ensures link is a symlink pointing to target. If link
// already points to target it is left completely untouched; otherwise a sibling
// temporary symlink is created and atomically renamed over link.
func replaceSymlinkAtomic(dir, target, link string) error {
	if existing, err := os.Readlink(link); err == nil && existing == target {
		return nil
	}
	if info, err := os.Lstat(link); err == nil && info.IsDir() {
		if err := os.RemoveAll(link); err != nil {
			return fmt.Errorf("remove legacy skill dir %q: %w", filepath.Base(link), err)
		}
	}
	token := make([]byte, 8)
	if _, err := rand.Read(token); err != nil {
		return fmt.Errorf("generate symlink token: %w", err)
	}
	tmpLink := filepath.Join(dir, fmt.Sprintf("%s%x", tempLinkPrefix, token))
	defer func() { _ = os.Remove(tmpLink) }()
	if err := os.Symlink(target, tmpLink); err != nil {
		return fmt.Errorf("create temp skill symlink: %w", err)
	}
	if err := os.Rename(tmpLink, link); err != nil {
		return fmt.Errorf("replace skill symlink: %w", err)
	}
	return nil
}
