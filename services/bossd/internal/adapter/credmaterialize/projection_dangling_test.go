package credmaterialize

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestMaterializeCodexKeepsDanglingScratchAccountLocal(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink semantics differ on Windows")
	}
	base := t.TempDir()
	nested := filepath.Join(base, ".tmp", "marketplaces", "marketplace-backup-x", "root", "site")
	if err := os.MkdirAll(nested, 0o700); err != nil {
		t.Fatal(err)
	}
	broken := filepath.Join(nested, "upgrading.md")
	if err := os.Symlink("../docs/install", broken); err != nil {
		t.Fatal(err)
	}
	m := newTestMaterializerWithCodexHome(t, &fakeStore{blob: codexBlob(t, fakeAccess, fakeID, fakeRefresh)}, base)
	res, _, err := m.MaterializeCodex(context.Background(), "acct-1")
	if err != nil {
		t.Fatalf("materialize with dangling scratch: %v", err)
	}
	scratch := filepath.Join(res.HomeDir, ".tmp")
	if _, err := os.Lstat(scratch); !os.IsNotExist(err) {
		t.Fatalf("scratch was projected: %v", err)
	}
	canonicalBase, err := filepath.EvalSymlinks(base)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(canonicalBase, ".tmp"), scratch); err != nil {
		t.Fatal(err)
	}
	if _, _, err := m.MaterializeCodex(context.Background(), "acct-1"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(scratch); !os.IsNotExist(err) {
		t.Fatalf("legacy scratch projection remains: %v", err)
	}
	if got, err := os.Readlink(broken); err != nil || got != "../docs/install" {
		t.Fatalf("base scratch changed: %q, %v", got, err)
	}
	if err := os.Mkdir(scratch, 0o700); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(scratch, "local")
	if err := os.WriteFile(marker, []byte("account"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := m.MaterializeCodex(context.Background(), "acct-1"); err != nil {
		t.Fatal(err)
	}
	info, err := os.Lstat(scratch)
	if err != nil || !info.IsDir() {
		t.Fatalf("account scratch replaced: %v", err)
	}
	if got, err := os.ReadFile(marker); err != nil || string(got) != "account" {
		t.Fatalf("account scratch changed: %q, %v", got, err)
	}
}

func TestMaterializeCodexSkipsOptionalDanglingSymlink(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink semantics differ on Windows")
	}
	base := t.TempDir()
	skills := filepath.Join(base, "skills")
	if err := os.Mkdir(skills, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("missing", filepath.Join(skills, "broken")); err != nil {
		t.Fatal(err)
	}
	m := newTestMaterializerWithCodexHome(t, &fakeStore{blob: codexBlob(t, fakeAccess, fakeID, fakeRefresh)}, base)
	res, _, err := m.MaterializeCodex(context.Background(), "acct-1")
	if err != nil {
		t.Fatalf("materialize with optional dangling link: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(res.HomeDir, "skills")); !os.IsNotExist(err) {
		t.Fatalf("unsafe skills projected: %v", err)
	}
	info, err := os.Lstat(filepath.Join(res.HomeDir, authFileName))
	if err != nil || !info.Mode().IsRegular() {
		t.Fatalf("account auth is not a regular file: %v", err)
	}
}

func TestMaterializeCodexRemovesProjectionWhenDangling(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink semantics differ on Windows")
	}
	base := t.TempDir()
	skills := filepath.Join(base, "skills")
	if err := os.Mkdir(skills, 0o700); err != nil {
		t.Fatal(err)
	}
	m, home := projectedOnce(t, base, "skills")
	if err := os.Symlink("missing", filepath.Join(skills, "broken")); err != nil {
		t.Fatal(err)
	}
	if _, _, err := m.MaterializeCodex(context.Background(), "acct-1"); err != nil {
		t.Fatalf("withdraw dangling projection: %v", err)
	}
	projection := filepath.Join(home, "skills")
	if _, err := os.Lstat(projection); !os.IsNotExist(err) {
		t.Fatalf("unsafe projection remains: %v", err)
	}
	if err := os.WriteFile(filepath.Join(skills, "missing"), []byte("safe"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := m.MaterializeCodex(context.Background(), "acct-1"); err != nil {
		t.Fatalf("restore repaired projection: %v", err)
	}
	info, err := os.Lstat(projection)
	if err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("repaired skills not projected: %v", err)
	}
}

func TestMaterializeCodexRejectsRequiredDanglingSymlinkBeforeCredentialWrite(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink semantics differ on Windows")
	}
	base := t.TempDir()
	plugins := filepath.Join(base, "plugins")
	if err := os.Mkdir(plugins, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("missing", filepath.Join(plugins, "broken")); err != nil {
		t.Fatal(err)
	}
	m := newTestMaterializerWithCodexHome(t, &fakeStore{blob: codexBlob(t, fakeAccess, fakeID, fakeRefresh)}, base)
	if _, _, err := m.MaterializeCodex(context.Background(), "acct-1"); !errors.Is(err, errDanglingSymlink) {
		t.Fatalf("required dangling link error = %v", err)
	}
	if _, err := os.Lstat(filepath.Join(m.codexAccountDir("acct-1"), authFileName)); !os.IsNotExist(err) {
		t.Fatalf("credential written after rejection: %v", err)
	}
}

func TestWalkProjectedDirectoryDistinguishesDanglingSymlink(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink semantics differ on Windows")
	}
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	broken := filepath.Join(base, "broken")
	if err := os.Symlink("missing", broken); err != nil {
		t.Fatal(err)
	}
	err = walkProjectedDirectory(broken, base, nil, nil)
	if !errors.Is(err, errDanglingSymlink) || !errors.Is(err, fs.ErrNotExist) || errors.Is(err, errSymlinkOutsideBase) {
		t.Fatalf("dangling link classification = %v", err)
	}
	outside := filepath.Join(base, "outside")
	if err := os.Symlink(t.TempDir(), outside); err != nil {
		t.Fatal(err)
	}
	err = walkProjectedDirectory(outside, base, nil, nil)
	if !errors.Is(err, errSymlinkOutsideBase) || errors.Is(err, errDanglingSymlink) {
		t.Fatalf("outside link classification = %v", err)
	}
}
