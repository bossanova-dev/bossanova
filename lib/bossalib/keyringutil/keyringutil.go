// Package keyringutil supplies a per-install random passphrase for the
// 99designs/keyring file backend, shared across boss and bossd.
//
// On macOS / Windows the native credential store is used and the helpers in
// this package are never invoked. On Linux containers and CI where the file
// backend is the only option, the passphrase is generated on first use with
// mode 0600 and read back on subsequent runs — no hardcoded secret.
package keyringutil

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// Passphrase matches the signature of github.com/99designs/keyring.PromptFunc
// so callers can assign a Passphrase directly to FilePasswordFunc without
// this package importing the keyring library.
type Passphrase func(prompt string) (string, error)

// LegacyPassphrase is the pre-fix hardcoded value. Retained only so that
// callers with allowInsecure=true can reproduce the old behavior when no
// writable passphrase location is available.
const LegacyPassphrase = "bossanova"

// New returns a Passphrase that loads (or generates) a per-install random
// passphrase at $HOME/.config/bossanova/keyring.key. The file must be on
// persistent storage because the encrypted keyring data it guards is
// persistent — tmpfs locations like $XDG_RUNTIME_DIR would silently brick
// stored credentials on reboot.
//
// When no writable location is available and allowInsecure is false, the
// returned function errors out with a message pointing at the opt-in.
// When allowInsecure is true, it falls back to LegacyPassphrase.
func New(allowInsecure bool) Passphrase {
	return func(_ string) (string, error) {
		path, err := passphrasePath()
		if err != nil {
			if allowInsecure {
				return LegacyPassphrase, nil
			}
			return "", fmt.Errorf(
				"locate keyring passphrase file: %w; "+
					"pass --allow-insecure-keyring to use the legacy static password",
				err,
			)
		}
		pass, err := loadOrCreatePassphrase(path)
		if err != nil {
			if allowInsecure {
				return LegacyPassphrase, nil
			}
			return "", err
		}
		return pass, nil
	}
}

// passphrasePath returns the location of the keyring passphrase file.
func passphrasePath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("locate home directory: %w", err)
	}
	return filepath.Join(home, ".config", "bossanova", "keyring.key"), nil
}

// loadOrCreatePassphrase reads the passphrase file at path, or generates a
// new random passphrase and writes it with mode 0600 on first use.
//
// The passphrase is written to a sibling temp file and then atomically
// link(2)-ed into place. Two processes (e.g. boss + bossd) racing on a fresh
// install can't each write a different passphrase and silently overwrite one
// another — the loser of the link race falls back to reading the winner's
// value. A concurrent reader never observes an empty file at path because
// the file only appears once its contents are fully written.
func loadOrCreatePassphrase(path string) (string, error) {
	if pass, ok, err := readExisting(path); err != nil {
		return "", err
	} else if ok {
		return pass, nil
	}

	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("generate keyring passphrase: %w", err)
	}
	passphrase := hex.EncodeToString(buf)

	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("create keyring passphrase dir: %w", err)
	}
	tmp, err := os.CreateTemp(dir, ".keyring.key.*")
	if err != nil {
		return "", fmt.Errorf("create keyring passphrase temp: %w", err)
	}
	tmpPath := tmp.Name()
	defer func() { _ = os.Remove(tmpPath) }()
	if err := os.Chmod(tmpPath, 0o600); err != nil {
		_ = tmp.Close()
		return "", fmt.Errorf("chmod keyring passphrase temp: %w", err)
	}
	if _, err := tmp.Write([]byte(passphrase)); err != nil {
		_ = tmp.Close()
		return "", fmt.Errorf("write keyring passphrase temp: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return "", fmt.Errorf("close keyring passphrase temp: %w", err)
	}

	if err := os.Link(tmpPath, path); err != nil {
		if errors.Is(err, fs.ErrExist) {
			pass, ok, readErr := readExisting(path)
			if readErr != nil {
				return "", readErr
			}
			if !ok {
				return "", fmt.Errorf("keyring passphrase file %s is empty", path)
			}
			return pass, nil
		}
		return "", fmt.Errorf("link keyring passphrase: %w", err)
	}
	return passphrase, nil
}

// readExisting returns (passphrase, true, nil) if path holds a non-empty
// passphrase, (_, false, nil) if path does not exist, and an error otherwise
// (including when the file exists but is empty).
func readExisting(path string) (string, bool, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return "", false, nil
		}
		return "", false, fmt.Errorf("read keyring passphrase: %w", err)
	}
	if len(data) == 0 {
		return "", false, fmt.Errorf("keyring passphrase file %s is empty", path)
	}
	return string(data), true, nil
}
