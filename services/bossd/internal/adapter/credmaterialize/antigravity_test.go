package credmaterialize

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/rs/zerolog"
)

func TestMaterializeAntigravityProjectsHomeAndIsolatesCredentials(t *testing.T) {
	baseDir := t.TempDir()
	hostHome := t.TempDir()

	// Seed a fake host home with dotfiles, Library/Keychains, .gemini/config,
	// and .gemini/antigravity-cli state including an ambient token file.
	mustWrite := func(rel, content string) {
		t.Helper()
		p := filepath.Join(hostHome, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatalf("mkdir %s: %v", rel, err)
		}
		if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
			t.Fatalf("write %s: %v", rel, err)
		}
	}
	mustWrite(".gitconfig", "[user]\n\tname = Test\n")
	mustWrite("Library/Keychains/login.keychain-db", "ambient-keychain")
	mustWrite("Library/Caches/tool.cache", "cached")
	mustWrite(".gemini/config/config.json", `{"ok":true}`)
	mustWrite(".gemini/antigravity-cli/settings.json", `{"model":"gemini"}`)
	mustWrite(".gemini/antigravity-cli/brain/conv-1/log.jsonl", `{}`)
	mustWrite(".gemini/antigravity-cli/antigravity-oauth-token", `{"token":{"refresh_token":"ambient-secret"}}`)

	stored := []byte(`{"token":{"access_token":"acct-access","token_type":"Bearer","refresh_token":"acct-refresh"},"auth_method":"consumer","id_token_email":"acct@example.com"}`)
	store := &fakeStore{blob: stored}

	m, err := New(store, zerolog.Nop(), WithBaseDir(baseDir), WithAntigravityBaseHome(hostHome))
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	mat, persist, err := m.MaterializeAntigravity(context.Background(), "acct-1")
	if err != nil {
		t.Fatalf("MaterializeAntigravity: %v", err)
	}
	if mat.Env["HOME"] != mat.HomeDir || mat.HomeDir == "" {
		t.Fatalf("HOME = %q, HomeDir = %q", mat.Env["HOME"], mat.HomeDir)
	}
	if mat.Env["SSH_CLIENT"] != "1" {
		t.Fatalf("SSH_CLIENT = %q, want 1", mat.Env["SSH_CLIENT"])
	}

	// Token file must hold the account's credential with 0600 perms, not the ambient one.
	tokenPath := filepath.Join(mat.HomeDir, ".gemini", "antigravity-cli", "antigravity-oauth-token")
	got, err := os.ReadFile(tokenPath)
	if err != nil {
		t.Fatalf("read token file: %v", err)
	}
	if !bytes.Equal(got, stored) {
		t.Fatalf("token file = %q, want %q", got, stored)
	}
	info, err := os.Stat(tokenPath)
	if err != nil {
		t.Fatalf("stat token file: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("token mode = %#o, want 0600", info.Mode().Perm())
	}

	// Keychains must NOT be projected into the account home.
	if _, err := os.Lstat(filepath.Join(mat.HomeDir, "Library", "Keychains")); !os.IsNotExist(err) {
		t.Fatalf("Library/Keychains was projected into account home (err=%v)", err)
	}
	// Shared dotfiles, Library/Caches, .gemini/config, and brain/ must be projected.
	for _, rel := range []string{
		".gitconfig",
		"Library/Caches/tool.cache",
		".gemini/config/config.json",
		".gemini/antigravity-cli/settings.json",
		".gemini/antigravity-cli/brain/conv-1/log.jsonl",
	} {
		if _, err := os.Stat(filepath.Join(mat.HomeDir, rel)); err != nil {
			t.Errorf("expected projected path %s to be readable: %v", rel, err)
		}
	}

	// PersistBack with unchanged file is a no-op.
	if err := persist(context.Background()); err != nil {
		t.Fatalf("persist unchanged: %v", err)
	}
	if store.saveCalls != 0 {
		t.Fatalf("unchanged persist saved %d times, want 0", store.saveCalls)
	}

	// Simulate agy refreshing the access token on disk and persisting back.
	refreshed := []byte(`{"token":{"access_token":"acct-access-2","token_type":"Bearer","refresh_token":"acct-refresh"},"auth_method":"consumer","id_token_email":"acct@example.com"}`)
	if err := os.WriteFile(tokenPath, refreshed, 0o600); err != nil {
		t.Fatalf("write refreshed token: %v", err)
	}
	if err := persist(context.Background()); err != nil {
		t.Fatalf("persist refreshed: %v", err)
	}
	if store.saveCalls != 1 {
		t.Fatalf("refreshed persist saved %d times, want 1", store.saveCalls)
	}
	if !bytes.Equal(store.blob, refreshed) {
		t.Fatalf("stored blob = %q, want %q", store.blob, refreshed)
	}

	// RemoveAgentAccount deletes the per-account directory.
	if err := m.RemoveAgentAccount(context.Background(), "antigravity", "acct-1"); err != nil {
		t.Fatalf("RemoveAgentAccount: %v", err)
	}
	if _, err := os.Stat(mat.HomeDir); !os.IsNotExist(err) {
		t.Fatalf("expected account dir to be removed, err=%v", err)
	}
}

func TestMaterializeAntigravityEnforces0700OnPreExistingDirChain(t *testing.T) {
	baseDir := t.TempDir()
	hostHome := t.TempDir()
	stored := []byte(`{"token":{"access_token":"acct-access","refresh_token":"acct-refresh"}}`)
	store := &fakeStore{blob: stored}

	m, err := New(store, zerolog.Nop(), WithBaseDir(baseDir), WithAntigravityBaseHome(hostHome))
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	accountsRoot := filepath.Join(baseDir, "accounts")
	providerDir := filepath.Join(accountsRoot, "antigravity")
	accountHome := filepath.Join(providerDir, "acct-perm")
	geminiDir := filepath.Join(accountHome, ".gemini")
	cliDir := filepath.Join(geminiDir, "antigravity-cli")
	dirs := []string{accountsRoot, providerDir, accountHome, geminiDir, cliDir}

	if err := os.MkdirAll(cliDir, 0o755); err != nil {
		t.Fatalf("mkdir pre-existing chain: %v", err)
	}
	for _, d := range dirs {
		if err := os.Chmod(d, 0o755); err != nil {
			t.Fatalf("chmod %s: %v", d, err)
		}
	}

	if _, _, err := m.MaterializeAntigravity(context.Background(), "acct-perm"); err != nil {
		t.Fatalf("MaterializeAntigravity: %v", err)
	}

	for _, d := range dirs {
		info, err := os.Stat(d)
		if err != nil {
			t.Fatalf("stat %s: %v", d, err)
		}
		if got := info.Mode().Perm(); got != 0o700 {
			t.Errorf("dir %s mode = %#o, want 0700", d, got)
		}
	}
}

func TestAntigravityTokenReadIgnoresSymlinkHardLinkAndFIFO(t *testing.T) {
	stored := []byte(`{"token":{"access_token":"acct-access","refresh_token":"acct-refresh"}}`)
	foreign := []byte(`{"token":{"access_token":"foreign-access","refresh_token":"foreign-refresh"}}`)

	cases := []struct {
		name   string
		tamper func(t *testing.T, tokenPath, foreignPath string)
	}{
		{
			name: "symlink",
			tamper: func(t *testing.T, tokenPath, foreignPath string) {
				t.Helper()
				if err := os.Remove(tokenPath); err != nil {
					t.Fatalf("remove token: %v", err)
				}
				if err := os.Symlink(foreignPath, tokenPath); err != nil {
					t.Fatalf("symlink token: %v", err)
				}
			},
		},
		{
			name: "hardlink",
			tamper: func(t *testing.T, tokenPath, foreignPath string) {
				t.Helper()
				if err := os.Remove(tokenPath); err != nil {
					t.Fatalf("remove token: %v", err)
				}
				if err := os.Link(foreignPath, tokenPath); err != nil {
					t.Fatalf("hardlink token: %v", err)
				}
			},
		},
		{
			name: "fifo",
			tamper: func(t *testing.T, tokenPath, _ string) {
				t.Helper()
				if err := os.Remove(tokenPath); err != nil {
					t.Fatalf("remove token: %v", err)
				}
				if err := syscall.Mkfifo(tokenPath, 0o600); err != nil {
					t.Fatalf("mkfifo token: %v", err)
				}
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			baseDir := t.TempDir()
			hostHome := t.TempDir()
			foreignPath := filepath.Join(t.TempDir(), "foreign-token")
			if err := os.WriteFile(foreignPath, foreign, 0o600); err != nil {
				t.Fatalf("write foreign token: %v", err)
			}

			store := &fakeStore{blob: append([]byte(nil), stored...)}
			m, err := New(store, zerolog.Nop(), WithBaseDir(baseDir), WithAntigravityBaseHome(hostHome))
			if err != nil {
				t.Fatalf("New: %v", err)
			}

			mat, persist, err := m.MaterializeAntigravity(context.Background(), "acct-safe")
			if err != nil {
				t.Fatalf("initial MaterializeAntigravity: %v", err)
			}
			tokenPath := filepath.Join(mat.HomeDir, ".gemini", "antigravity-cli", "antigravity-oauth-token")

			tc.tamper(t, tokenPath, foreignPath)

			// PersistBack must ignore the tampered file rather than folding foreign bytes into the store.
			if err := persist(context.Background()); err != nil {
				t.Fatalf("persist tampered (%s): %v", tc.name, err)
			}
			if store.saveCalls != 0 {
				t.Fatalf("persist tampered (%s) saved %d times, want 0", tc.name, store.saveCalls)
			}
			if !bytes.Equal(store.blob, stored) {
				t.Fatalf("store blob changed on persist (%s): %q", tc.name, store.blob)
			}

			// Re-materializing must not reconcile the tampered file into the store,
			// and must atomically replace it with a regular file containing the stored credential.
			if _, _, err := m.MaterializeAntigravity(context.Background(), "acct-safe"); err != nil {
				t.Fatalf("re-materialize tampered (%s): %v", tc.name, err)
			}
			if store.saveCalls != 0 {
				t.Fatalf("reconcile tampered (%s) saved %d times, want 0", tc.name, store.saveCalls)
			}
			if !bytes.Equal(store.blob, stored) {
				t.Fatalf("store blob changed on reconcile (%s): %q", tc.name, store.blob)
			}
			info, err := os.Lstat(tokenPath)
			if err != nil {
				t.Fatalf("lstat replaced token: %v", err)
			}
			if !info.Mode().IsRegular() {
				t.Fatalf("replaced token mode = %v, want regular file", info.Mode())
			}
			got, err := os.ReadFile(tokenPath)
			if err != nil {
				t.Fatalf("read replaced token: %v", err)
			}
			if !bytes.Equal(got, stored) {
				t.Fatalf("replaced token = %q, want %q", got, stored)
			}
		})
	}
}

func TestProjectAntigravityBaseHomeRejectsLibrarySymlinkAndTightensPerms(t *testing.T) {
	stored := []byte(`{"token":{"access_token":"acct-access","refresh_token":"acct-refresh"}}`)

	t.Run("rejects symlinked Library", func(t *testing.T) {
		baseDir := t.TempDir()
		hostHome := t.TempDir()
		outsideDir := t.TempDir()

		if err := os.MkdirAll(filepath.Join(hostHome, "Library", "Caches"), 0o700); err != nil {
			t.Fatalf("mkdir host Library/Caches: %v", err)
		}
		if err := os.WriteFile(filepath.Join(hostHome, "Library", "Caches", "tool.cache"), []byte("cached"), 0o600); err != nil {
			t.Fatalf("write host cache: %v", err)
		}

		accountHome := filepath.Join(baseDir, "accounts", "antigravity", "acct-lib-sym")
		if err := os.MkdirAll(accountHome, 0o700); err != nil {
			t.Fatalf("mkdir accountHome: %v", err)
		}
		if err := os.Symlink(outsideDir, filepath.Join(accountHome, "Library")); err != nil {
			t.Fatalf("symlink Library: %v", err)
		}

		m, err := New(&fakeStore{blob: stored}, zerolog.Nop(), WithBaseDir(baseDir), WithAntigravityBaseHome(hostHome))
		if err != nil {
			t.Fatalf("New: %v", err)
		}

		if _, _, err := m.MaterializeAntigravity(context.Background(), "acct-lib-sym"); err == nil {
			t.Fatal("expected MaterializeAntigravity to reject symlinked accountHome/Library, got nil")
		}
		if _, err := os.Lstat(filepath.Join(outsideDir, "Caches")); !os.IsNotExist(err) {
			t.Fatalf("expected outsideDir/Caches to remain untouched, err=%v", err)
		}
	})

	t.Run("tightens pre-existing Library perms to 0700", func(t *testing.T) {
		baseDir := t.TempDir()
		hostHome := t.TempDir()

		if err := os.MkdirAll(filepath.Join(hostHome, "Library", "Caches"), 0o700); err != nil {
			t.Fatalf("mkdir host Library/Caches: %v", err)
		}

		acctLib := filepath.Join(baseDir, "accounts", "antigravity", "acct-lib-perm", "Library")
		if err := os.MkdirAll(acctLib, 0o755); err != nil {
			t.Fatalf("mkdir acctLib: %v", err)
		}
		if err := os.Chmod(acctLib, 0o755); err != nil {
			t.Fatalf("chmod acctLib: %v", err)
		}

		m, err := New(&fakeStore{blob: stored}, zerolog.Nop(), WithBaseDir(baseDir), WithAntigravityBaseHome(hostHome))
		if err != nil {
			t.Fatalf("New: %v", err)
		}

		if _, _, err := m.MaterializeAntigravity(context.Background(), "acct-lib-perm"); err != nil {
			t.Fatalf("MaterializeAntigravity: %v", err)
		}
		info, err := os.Stat(acctLib)
		if err != nil {
			t.Fatalf("stat acctLib: %v", err)
		}
		if got := info.Mode().Perm(); got != 0o700 {
			t.Fatalf("acctLib mode = %#o, want 0700", got)
		}
	})
}
