package credmaterialize

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const (
	providerAntigravity       = "antigravity"
	antigravityTokenFileName  = "antigravity-oauth-token"
	antigravityGeminiDirName  = ".gemini"
	antigravityCLIDirName     = "antigravity-cli"
	antigravityLibraryDirName = "Library"
)

// WithAntigravityBaseHome overrides the host user's home directory projected
// into per-account Antigravity homes. Tests pass a temp dir so nothing touches
// the real home directory.
func WithAntigravityBaseHome(dir string) Option {
	return func(m *Materializer) { m.antigravityBaseHome = dir }
}

func (m *Materializer) antigravityAccountDir(accountID string) string {
	return filepath.Join(m.accountsRoot(), providerAntigravity, accountID)
}

// MaterializeAntigravity writes the Antigravity OAuth token file inside a
// per-account projected HOME directory and returns HOME + SSH_CLIENT=1 plus a
// PersistBack closure.
//
// SSH_CLIENT=1 instructs agy's composite token storage to bypass the OS keyring
// (macOS Keychain / Linux Secret Service) and use its file-based token store at
// $HOME/.gemini/antigravity-cli/antigravity-oauth-token, which stays isolated
// per account. Top-level entries from the real home directory, including
// Library/Keychains, are symlinked into the account home so git, ssh, boss and
// agy's shared conversations/brain/settings remain accessible. Projecting
// Library/Keychains matters on macOS: gh and git credential helpers keep their
// tokens in $HOME/Library/Keychains/login.keychain-db, and without it they fail
// with HTTP 401 because the shared ~/.config/gh/hosts.yml holds no token.
func (m *Materializer) MaterializeAntigravity(ctx context.Context, accountID string) (Materialized, PersistBack, error) {
	if err := validateAccountID(accountID); err != nil {
		return Materialized{}, nil, err
	}
	unlock := m.locks.Lock(lockKey(providerAntigravity, accountID))
	defer unlock()

	var (
		accountHome string
		tokenPath   string
		writtenHash [sha256.Size]byte
	)
	work := func() error {
		blob, err := m.store.LoadCredential(ctx, accountID)
		if err != nil {
			return fmt.Errorf("load antigravity credential for %q: %w", accountID, err)
		}
		if !validAntigravityBlob(blob) {
			return fmt.Errorf("invalid antigravity credential for %q", accountID)
		}

		accountHome = m.antigravityAccountDir(accountID)
		geminiDir := filepath.Join(accountHome, antigravityGeminiDirName)
		cliDir := filepath.Join(geminiDir, antigravityCLIDirName)
		tokenPath = filepath.Join(cliDir, antigravityTokenFileName)

		if err := m.assertNoSymlinkChain(m.baseDir, m.accountsRoot(), filepath.Dir(accountHome), accountHome, geminiDir, cliDir); err != nil {
			return err
		}
		if err := os.MkdirAll(cliDir, 0o700); err != nil {
			return fmt.Errorf("create antigravity account dir: %w", err)
		}
		for _, d := range []string{m.accountsRoot(), filepath.Dir(accountHome), accountHome, geminiDir, cliDir} {
			// #nosec G302 -- Chmod(d, 0o700) on the credential dir chain; 0700 is the tightest usable dir mode
			if err := os.Chmod(d, 0o700); err != nil {
				return fmt.Errorf("enforce 0700 on antigravity dir %q: %w", d, err)
			}
		}
		m.writeCacheDirTag()

		if err := m.projectAntigravityBaseHome(ctx, accountHome); err != nil {
			return fmt.Errorf("project antigravity base home: %w", err)
		}

		toWrite := blob
		if reconciled, ok := m.reconcileAntigravityToken(ctx, accountID, tokenPath, blob); ok {
			toWrite = reconciled
		}
		if err := atomicWriteFile(tokenPath, toWrite); err != nil {
			return fmt.Errorf("write antigravity token file: %w", err)
		}
		writtenHash = sha256.Sum256(toWrite)
		m.recordAuthWrite(tokenPath, writtenHash)
		return nil
	}

	if locker, ok := m.store.(CredentialStoreLocker); ok {
		if err := locker.WithCredentialLock(accountID, work); err != nil {
			return Materialized{}, nil, err
		}
	} else if err := work(); err != nil {
		return Materialized{}, nil, err
	}

	recorded := writtenHash
	persist := func(pctx context.Context) error {
		return m.persistAntigravityBack(pctx, accountID, tokenPath, &recorded)
	}
	return Materialized{
		Env: map[string]string{
			"HOME":       accountHome,
			"SSH_CLIENT": "1",
		},
		HomeDir: accountHome,
	}, persist, nil
}

func (m *Materializer) projectAntigravityBaseHome(ctx context.Context, accountHome string) error {
	acctGemini := filepath.Join(accountHome, antigravityGeminiDirName)
	baseHome := m.antigravityBaseHome
	if baseHome == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return m.ensureAntigravityConfigDir(ctx, "", acctGemini)
		}
		baseHome = home
	}
	if _, err := os.Stat(baseHome); err != nil {
		if os.IsNotExist(err) {
			return m.ensureAntigravityConfigDir(ctx, "", acctGemini)
		}
		return err
	}

	// Project top-level entries except Library and .gemini, which are projected
	// entry by entry below.
	if err := projectDirectoryEntries(baseHome, accountHome, map[string]bool{
		antigravityLibraryDirName: true,
		antigravityGeminiDirName:  true,
	}); err != nil {
		return err
	}

	// On macOS, project every ~/Library entry, Keychains included, so host CLIs
	// such as gh and git credential helpers can read the login keychain. agy is
	// kept off the OS keyring by SSH_CLIENT=1, not by hiding Keychains. Library
	// stays a real per-account directory rather than one symlink because account
	// homes materialized before Keychains was projected already hold a real
	// Library/, and projectDirectoryEntries never replaces a non-symlink; linking
	// entry by entry lets those homes pick up Keychains on their next run.
	baseLib := filepath.Join(baseHome, antigravityLibraryDirName)
	if info, err := os.Stat(baseLib); err == nil && info.IsDir() {
		acctLib := filepath.Join(accountHome, antigravityLibraryDirName)
		if err := m.assertNoSymlinkChain(acctLib); err != nil {
			return err
		}
		if err := os.MkdirAll(acctLib, 0o700); err != nil {
			return err
		}
		// #nosec G302 -- Chmod(acctLib, 0o700) on the projected Library dir; 0700 is the tightest usable dir mode
		if err := os.Chmod(acctLib, 0o700); err != nil {
			return fmt.Errorf("enforce 0700 on antigravity dir %q: %w", acctLib, err)
		}
		if err := projectDirectoryEntries(baseLib, acctLib, nil); err != nil {
			return err
		}
	}

	// Project ~/.gemini entries except antigravity-cli and config (config is
	// materialized as a real directory so mcp_config.json can merge user and
	// repo servers while keeping skills/ and config.json symlinked).
	baseGemini := filepath.Join(baseHome, antigravityGeminiDirName)
	if info, err := os.Stat(baseGemini); err == nil && info.IsDir() {
		if err := projectDirectoryEntries(baseGemini, acctGemini, map[string]bool{
			antigravityCLIDirName:    true,
			antigravityConfigDirName: true,
		}); err != nil {
			return err
		}
	}
	if err := m.ensureAntigravityConfigDir(ctx, baseGemini, acctGemini); err != nil {
		return err
	}

	// Project ~/.gemini/antigravity-cli entries except the account-local token
	// file and its hash sidecar (so brain/, conversations/, cache/, settings.json
	// are shared across accounts for transcript inspection and cross-account resume).
	baseCLI := filepath.Join(baseGemini, antigravityCLIDirName)
	if info, err := os.Stat(baseCLI); err == nil && info.IsDir() {
		acctCLI := filepath.Join(accountHome, antigravityGeminiDirName, antigravityCLIDirName)
		if err := projectDirectoryEntries(baseCLI, acctCLI, map[string]bool{
			antigravityTokenFileName: true,
			authHashFileName:         true,
		}); err != nil {
			return err
		}
	}
	return nil
}

// projectDirectoryEntries creates symlinks in dstDir for each entry in srcDir
// not listed in skip. Existing non-symlink entries in dstDir are left untouched.
func projectDirectoryEntries(srcDir, dstDir string, skip map[string]bool) error {
	entries, err := os.ReadDir(srcDir)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		name := entry.Name()
		if skip[name] {
			continue
		}
		srcPath := filepath.Join(srcDir, name)
		dstPath := filepath.Join(dstDir, name)
		info, err := os.Lstat(dstPath)
		if err == nil {
			if info.Mode()&os.ModeSymlink == 0 {
				continue
			}
			if target, rerr := os.Readlink(dstPath); rerr == nil && target == srcPath {
				continue
			}
			_ = os.Remove(dstPath)
		} else if !os.IsNotExist(err) {
			return err
		}
		if err := os.Symlink(srcPath, dstPath); err != nil && !os.IsExist(err) {
			return err
		}
	}
	return nil
}

func (m *Materializer) readSafeAntigravityToken(tokenPath string) ([]byte, bool, error) {
	info, err := os.Lstat(tokenPath)
	if err != nil {
		return nil, false, err
	}
	if !info.Mode().IsRegular() {
		m.logger.Warn().Str("path", tokenPath).
			Msg("credmaterialize: antigravity token is not a regular file; ignoring it")
		return nil, false, nil
	}
	if authFileHasMultipleLinks(tokenPath, info) {
		m.logger.Warn().Str("path", tokenPath).
			Msg("credmaterialize: antigravity token has multiple hard links; ignoring it")
		return nil, false, nil
	}
	// #nosec G304 -- reads account-local antigravity token file under lock; internally-derived path
	current, err := os.ReadFile(tokenPath)
	if err != nil {
		return nil, false, err
	}
	return current, true, nil
}

func (m *Materializer) reconcileAntigravityToken(ctx context.Context, accountID, tokenPath string, stored []byte) ([]byte, bool) {
	lastSum, ok := m.lastAuthWrite(tokenPath)
	if !ok {
		return nil, false
	}
	if sha256.Sum256(stored) != lastSum {
		// Store was updated externally (e.g. reauth or cloud sync); prefer stored.
		return nil, false
	}
	current, ok, err := m.readSafeAntigravityToken(tokenPath)
	if err != nil || !ok {
		return nil, false
	}
	if sha256.Sum256(current) == lastSum || !validAntigravityBlob(current) {
		return nil, false
	}
	if err := m.store.SaveCredential(ctx, accountID, current); err != nil {
		m.logger.Warn().Err(err).Str("account_id", accountID).
			Msg("credmaterialize: failed to persist refreshed antigravity token; using stored copy")
		return nil, false
	}
	return current, true
}

func (m *Materializer) persistAntigravityBack(ctx context.Context, accountID, tokenPath string, recorded *[sha256.Size]byte) error {
	unlock := m.locks.Lock(lockKey(providerAntigravity, accountID))
	defer unlock()

	persist := func() error {
		current, ok, err := m.readSafeAntigravityToken(tokenPath)
		if err != nil {
			return fmt.Errorf("read antigravity token at %q for persist-back: %w", tokenPath, err)
		}
		if !ok {
			return nil
		}
		currentHash := sha256.Sum256(current)
		if currentHash == *recorded {
			return nil
		}
		if !validAntigravityBlob(current) {
			return nil
		}
		if err := m.store.SaveCredential(ctx, accountID, current); err != nil {
			return fmt.Errorf("save refreshed antigravity credential: %w", err)
		}
		*recorded = currentHash
		m.recordAuthWrite(tokenPath, currentHash)
		return nil
	}
	if locker, ok := m.store.(CredentialStoreLocker); ok {
		return locker.WithCredentialLock(accountID, persist)
	}
	return persist()
}

func validAntigravityBlob(blob []byte) bool {
	var payload struct {
		Token struct {
			AccessToken  string `json:"access_token"`
			RefreshToken string `json:"refresh_token"`
		} `json:"token"`
	}
	if json.Unmarshal(blob, &payload) != nil {
		return false
	}
	return strings.TrimSpace(payload.Token.RefreshToken) != "" || strings.TrimSpace(payload.Token.AccessToken) != ""
}
