package skillinstall

import (
	"crypto/rand"
	"crypto/sha1" // #nosec G505 -- Git blob identity requires SHA-1; owner=@recurser review-by=2027-01-17 issue=BOS-1694
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
)

// HealOperation is a bounded modification of one installed path.
type HealOperation string

const (
	HealWrite  HealOperation = "write"
	HealChmod  HealOperation = "chmod"
	HealRemove HealOperation = "remove"
	HealRelink HealOperation = "relink"
)

// HealFix names a payload-relative path; bare skill names denote top-level
// symlinks. Directory removals use a trailing slash and only remove empty dirs.
// Data is the authenticated payload, also used to derive executable permissions.
// ExpectedFingerprint must come from HealFingerprint before classification.
type HealFix struct {
	Path                string
	Operation           HealOperation
	Data                []byte
	ExpectedFingerprint string
}

// HealResult records applied paths and paths changed since classification.
type HealResult struct {
	Healed  []string
	Skipped []string
}

func healLocation(rel string) (string, bool, error) {
	clean := strings.TrimSuffix(rel, "/")
	if !fs.ValidPath(clean) || strings.Contains(clean, "\\") || !isBossSkill(strings.Split(clean, "/")[0]) {
		return "", false, fmt.Errorf("unsafe skill heal path %q", rel)
	}
	top := !strings.Contains(clean, "/") && !strings.HasSuffix(rel, "/")
	if top {
		return clean, true, nil
	}
	return path.Join(Namespace, clean), false, nil
}

// healParents rejects parent symlinks, even ones pointing inside the root. Root
// additionally confines every filesystem operation if a parent changes later.
func healParents(root *os.Root, name string, create bool) error {
	parent := path.Dir(name)
	if parent == "." {
		return nil
	}
	current := ""
	for _, part := range strings.Split(parent, "/") {
		current = path.Join(current, part)
		info, err := root.Lstat(current)
		if os.IsNotExist(err) && create {
			if err = root.Mkdir(current, 0o750); err != nil && !os.IsExist(err) {
				return err
			}
			info, err = root.Lstat(current)
		}
		if err != nil {
			return err
		}
		if !info.IsDir() {
			return fmt.Errorf("unsafe heal parent %q", current)
		}
	}
	return nil
}

// scopedHealRoot pins namespace operations to its directory descriptor, so a
// parent replacement cannot redirect a payload write elsewhere in the agent dir.
func scopedHealRoot(root *os.Root, name string, create bool) (*os.Root, string, error) {
	if !strings.HasPrefix(name, Namespace+"/") {
		return nil, name, nil
	}
	info, err := root.Lstat(Namespace)
	if os.IsNotExist(err) && create {
		if err = root.Mkdir(Namespace, 0o750); err != nil && !os.IsExist(err) {
			return nil, "", err
		}
		info, err = root.Lstat(Namespace)
	}
	if err != nil {
		return nil, "", err
	}
	if !info.IsDir() {
		return nil, "", fmt.Errorf("unsafe skill namespace")
	}
	scoped, err := root.OpenRoot(Namespace)
	if err != nil {
		return nil, "", err
	}
	opened, err := scoped.Stat(".")
	if err != nil || !os.SameFile(info, opened) {
		_ = scoped.Close()
		return nil, "", fmt.Errorf("skill namespace changed during heal")
	}
	return scoped, strings.TrimPrefix(name, Namespace+"/"), nil
}

func healFingerprint(root *os.Root, name string) (string, error) {
	scoped, relative, err := scopedHealRoot(root, name, false)
	if os.IsNotExist(err) {
		return "absent", nil
	}
	if err != nil {
		return "", err
	}
	if scoped != nil {
		defer func() { _ = scoped.Close() }()
		root, name = scoped, relative
	}

	if err := healParents(root, name, false); err != nil {
		if os.IsNotExist(err) {
			return "absent", nil
		}
		return "", err
	}
	info, err := root.Lstat(name)
	if os.IsNotExist(err) {
		return "absent", nil
	}
	if err != nil {
		return "", err
	}
	prefix := fmt.Sprintf("%o:", info.Mode())
	if info.Mode()&os.ModeSymlink != 0 {
		target, err := root.Readlink(name)
		return prefix + "symlink:" + target, err
	}
	if info.IsDir() {
		return prefix + "directory", nil
	}
	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("unsupported heal file %q", name)
	}
	data, err := root.ReadFile(name)
	if err != nil {
		return "", err
	}
	blob := append(fmt.Appendf(nil, "blob %d\x00", len(data)), data...)
	// #nosec G401 -- Git object identity must match hash-object and recorded repository blob IDs.
	// owner=@recurser review-by=2027-01-17 issue=BOS-1694
	return fmt.Sprintf("%sblob:%x", prefix, sha1.Sum(blob)), nil
}

// HealFingerprintBlob returns the Git blob ID captured for a regular file.
// It returns an empty string for absent paths, directories and symlinks.
func HealFingerprintBlob(fingerprint string) string {
	_, blob, ok := strings.Cut(fingerprint, ":blob:")
	if !ok {
		return ""
	}
	return blob
}

// HealFingerprint captures installed bytes, permissions, and link identity.
// Capture it before deciding the direction, then supply it unchanged to HealPaths.
func HealFingerprint(dir, rel string) (string, error) {
	name, _, err := healLocation(rel)
	if err != nil {
		return "", err
	}
	root, err := os.OpenRoot(dir)
	if os.IsNotExist(err) {
		return "absent", nil
	}
	if err != nil {
		return "", err
	}
	defer func() { _ = root.Close() }()
	return healFingerprint(root, name)
}

// HealPaths holds the agent update lock for the batch, checks each fingerprint,
// and atomically replaces individual files. It never changes the payload record.
func HealPaths(dir string, fixes []HealFix) (result HealResult, err error) {
	// Reject malformed operations before any mutation (including creating a lock).
	for _, fix := range fixes {
		_, top, e := healLocation(fix.Path)
		if e != nil {
			return result, e
		}
		switch fix.Operation {
		case HealRelink:
			if !top {
				return result, fmt.Errorf("relink requires top-level skill: %q", fix.Path)
			}
		case HealWrite, HealChmod:
			if top || strings.HasSuffix(fix.Path, "/") {
				return result, fmt.Errorf("write requires payload file: %q", fix.Path)
			}
		case HealRemove:
		default:
			return result, fmt.Errorf("unknown heal operation %q", fix.Operation)
		}
		if fix.ExpectedFingerprint == "" {
			return result, fmt.Errorf("missing heal fingerprint: %q", fix.Path)
		}
	}
	lock, err := acquireUpdateLock(dir)
	if err != nil {
		return result, err
	}
	defer func() {
		if e := lock.Unlock(); err == nil && e != nil {
			err = e
		}
	}()
	root, err := os.OpenRoot(dir)
	if err != nil {
		return result, err
	}
	defer func() { _ = root.Close() }()
	for _, fix := range fixes {
		name, top, _ := healLocation(fix.Path)
		observed, e := healFingerprint(root, name)
		if e != nil {
			return result, e
		}
		if observed != fix.ExpectedFingerprint {
			result.Skipped = append(result.Skipped, fix.Path)
			continue
		}
		if top && observed != "absent" && !strings.Contains(observed, ":symlink:") {
			return result, fmt.Errorf("heal refuses real top-level entry %q", fix.Path)
		}
		if e = applyHeal(root, name, fix); e != nil {
			return result, fmt.Errorf("heal %s: %w", fix.Path, e)
		}
		result.Healed = append(result.Healed, fix.Path)
	}
	return result, nil
}

func applyHeal(root *os.Root, name string, fix HealFix) error {
	scoped, relative, err := scopedHealRoot(root, name, fix.Operation != HealRemove)
	if os.IsNotExist(err) && fix.Operation == HealRemove {
		return nil
	}
	if err != nil {
		return err
	}
	if scoped != nil {
		defer func() { _ = scoped.Close() }()
		root, name = scoped, relative
	}

	if fix.Operation == HealRemove {
		err := root.Remove(name)
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	if err := healParents(root, name, true); err != nil {
		return err
	}
	token := make([]byte, 16)
	if _, err := rand.Read(token); err != nil {
		return err
	}
	temp := path.Join(path.Dir(name), fmt.Sprintf(".boss-heal-%x", token))
	defer func() { _ = root.Remove(temp) }()
	if fix.Operation == HealRelink {
		if err := root.Symlink(filepath.Join(Namespace, fix.Path), temp); err != nil {
			return err
		}
	} else {
		if fix.Operation == HealChmod {
			data, err := root.ReadFile(name)
			if err != nil {
				return err
			}
			if string(data) != string(fix.Data) {
				return fmt.Errorf("chmod payload differs from installed bytes")
			}
		}
		file, err := root.OpenFile(temp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err != nil {
			return err
		}
		_, writeErr := file.Write(fix.Data)
		if writeErr == nil {
			writeErr = file.Chmod(skillFileMode("skills/"+fix.Path, fix.Data))
		}
		closeErr := file.Close()
		if writeErr != nil {
			return writeErr
		}
		if closeErr != nil {
			return closeErr
		}
	}
	return root.Rename(temp, name)
}

// StampPayloadIfCurrent claims a complete tree only while it still matches fsys.
// A concurrent update cannot be relabeled as the caller's payload.
func StampPayloadIfCurrent(dir string, fsys fs.FS, record PayloadRecord) (stamped bool, err error) {
	lock, err := acquireUpdateLock(dir)
	if err != nil {
		return false, err
	}
	defer func() {
		if e := lock.Unlock(); err == nil && e != nil {
			err = e
		}
	}()
	if !IsInstalled(dir) {
		return false, nil
	}
	report, err := driftReportLocked(dir, fsys)
	if err != nil {
		return false, err
	}
	if len(report.Entries) != 0 {
		return false, nil
	}
	if err := stampPayloadRecordLocked(dir, record); err != nil {
		return false, err
	}
	return true, nil
}
