// Package keyringutil supplies a per-install random passphrase for the
// 99designs/keyring file backend, shared across boss and bossd, and a small
// helper for selecting the keyring backend via the BOSS_KEYRING_BACKEND
// environment variable.
//
// On macOS / Windows the native credential store is used by default and the
// passphrase helpers here are never invoked. On Linux containers and CI where
// the file backend is the only option, the passphrase is generated on first
// use with mode 0600 and read back on subsequent runs — no hardcoded secret.
//
// Local-dev workflows on macOS can set BOSS_KEYRING_BACKEND=file to skip the
// system Keychain prompt and use the same encrypted file backend that Linux
// already uses.
package keyringutil

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/99designs/keyring"
)

// Passphrase matches the signature of github.com/99designs/keyring.PromptFunc
// so callers can assign a Passphrase directly to FilePasswordFunc without
// this package importing the keyring library.
type Passphrase func(prompt string) (string, error)

// LegacyPassphrase is the pre-fix hardcoded value. Retained only so that
// callers with allowInsecure=true can reproduce the old behavior when no
// writable passphrase location is available.
const LegacyPassphrase = "bossanova"

// backendEnv is the env var that selects which 99designs/keyring backend to
// use. Empty / unset means "let the library pick the platform default".
const backendEnv = "BOSS_KEYRING_BACKEND"

// Backends returns the AllowedBackends slice to thread into keyring.Config,
// derived from BOSS_KEYRING_BACKEND. A nil return means "no override" — the
// keyring library will pick the platform default (Keychain on macOS,
// secret-service / file on Linux, Wincred on Windows).
//
// Recognized values (case-insensitive):
//
//	file            — file backend on any platform. The intended escape hatch
//	                  for macOS local dev: skips the system Keychain prompt
//	                  and uses the per-install passphrase from New().
//	keychain        — macOS Keychain (explicit).
//	secret-service  — Linux secret-service / libsecret (explicit).
//
// Unknown values fall back to nil with a warning so a typo in the dev shell
// doesn't stop boss from starting.
func Backends() []keyring.BackendType {
	raw := strings.ToLower(strings.TrimSpace(os.Getenv(backendEnv)))
	if raw == "" {
		return nil
	}
	switch raw {
	case "file":
		return []keyring.BackendType{keyring.FileBackend}
	case "keychain":
		return []keyring.BackendType{keyring.KeychainBackend}
	case "secret-service":
		return []keyring.BackendType{keyring.SecretServiceBackend}
	default:
		// raw originates from os.Getenv so gosec marks it as tainted, but
		// slog's structured handlers quote attribute values — there is no
		// log-line injection vector here.
		slog.Warn("ignoring unknown keyring backend env value; using platform default", //nolint:gosec // see comment
			"env", backendEnv, "value", raw)
		return nil
	}
}

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
