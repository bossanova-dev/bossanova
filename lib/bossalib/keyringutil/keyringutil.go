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
// Creation uses O_CREATE|O_EXCL so two processes (e.g. boss + bossd) racing
// on a fresh install can't each write a different passphrase and silently
// overwrite one another — the loser of the race falls back to reading the
// winner's value.
func loadOrCreatePassphrase(path string) (string, error) {
	if data, err := os.ReadFile(path); err == nil {
		if len(data) == 0 {
			return "", fmt.Errorf("keyring passphrase file %s is empty", path)
		}
		return string(data), nil
	} else if !errors.Is(err, fs.ErrNotExist) {
		return "", fmt.Errorf("read keyring passphrase: %w", err)
	}

	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("generate keyring passphrase: %w", err)
	}
	passphrase := hex.EncodeToString(buf)

	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return "", fmt.Errorf("create keyring passphrase dir: %w", err)
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		if errors.Is(err, fs.ErrExist) {
			data, readErr := os.ReadFile(path)
			if readErr != nil {
				return "", fmt.Errorf("read keyring passphrase after race: %w", readErr)
			}
			if len(data) == 0 {
				return "", fmt.Errorf("keyring passphrase file %s is empty", path)
			}
			return string(data), nil
		}
		return "", fmt.Errorf("create keyring passphrase: %w", err)
	}
	if _, err := f.Write([]byte(passphrase)); err != nil {
		_ = f.Close()
		return "", fmt.Errorf("write keyring passphrase: %w", err)
	}
	if err := f.Close(); err != nil {
		return "", fmt.Errorf("close keyring passphrase: %w", err)
	}
	return passphrase, nil
}
