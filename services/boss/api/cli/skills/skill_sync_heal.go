package skills

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	libskillinstall "github.com/recurser/bossalib/contract/plugin/skillinstall"
)

// checkoutSkillBlob preserves every byte, including trailing newlines. The
// text-oriented checkoutGitOutput deliberately trims its output.
func checkoutSkillBlob(repoRoot, object string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), checkoutGitTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", "-C", repoRoot, "cat-file", "blob", object)
	cmd.WaitDelay = checkoutGitWaitDelay
	return cmd.Output()
}

func (g *Group) healSkillGateTree(out io.Writer, agent, dir string, payload selectedSkillPayload, record libskillinstall.PayloadRecord) (libskillinstall.DriftReport, error) {
	report, err := libskillinstall.SourceDriftReportFS(dir, payload.fsys)
	if err != nil || len(report.Entries) == 0 {
		return report, err
	}
	repoRoot := repoRootFromSourceRoot(payload.srcRoot)
	reason := ""
	if os.Getenv("BOSS_SKIP_SKILLS") != "" {
		reason = "BOSS_SKIP_SKILLS is set"
	}
	trusted, trustErr := TrustedSkillSourceRoot(payload.srcRoot)
	if reason == "" && (trustErr != nil || !trusted) {
		reason = "checkout is untrusted"
	}
	base, baseErr := checkoutGitOutput(repoRoot, "merge-base", "HEAD", "origin/HEAD")
	_, unexplained, fallback := classifySkillGateDrift(repoRoot, report.Entries)
	if reason == "" && (baseErr != nil || base == "" || fallback) {
		reason = "origin/HEAD unavailable"
	}
	if reason != "" {
		_, _ = fmt.Fprintf(out, "boss skills gate: %s heal skipped — %s\n", agent, reason)
		return report, nil
	}
	fixes := make([]libskillinstall.HealFix, 0, len(unexplained))
	for _, entry := range unexplained {
		fix, ok := g.skillGateHealFix(repoRoot, dir, base, payload.fsys, entry)
		if ok {
			fixes = append(fixes, fix)
		}
	}
	if len(fixes) == 0 {
		return report, nil
	}
	result, err := libskillinstall.HealPaths(dir, fixes)
	if err != nil {
		return report, err
	}
	if len(result.Healed) > 0 {
		_, _ = fmt.Fprintf(out, "boss skills gate: %s healed %d path(s) from origin/HEAD merge-base %.12s: %s\n", agent, len(result.Healed), base, strings.Join(result.Healed, ", "))
	}
	if len(result.Skipped) > 0 {
		_, _ = fmt.Fprintf(out, "boss skills gate: %s skipped %d path(s) changed during heal: %s\n", agent, len(result.Skipped), strings.Join(result.Skipped, ", "))
	}
	report, err = libskillinstall.SourceDriftReportFS(dir, payload.fsys)
	if err == nil && len(report.Entries) == 0 && len(result.Healed) > 0 {
		_, err = libskillinstall.StampPayloadIfCurrent(dir, payload.fsys, record)
	}
	return report, err
}

func (g *Group) skillGateHealFix(repoRoot, dir, base string, frozen fs.FS, entry libskillinstall.DriftEntry) (libskillinstall.HealFix, bool) {
	fingerprint, err := libskillinstall.HealFingerprint(dir, entry.Path)
	if err != nil {
		return libskillinstall.HealFix{}, false
	}
	direction := skillDriftUnknown
	blob := libskillinstall.HealFingerprintBlob(fingerprint)
	switch entry.Kind {
	case libskillinstall.DriftAbsent:
		if fingerprint == "absent" {
			direction = skillDriftLossless
		}
	case libskillinstall.DriftMode:
		if blob != "" {
			direction = skillDriftLossless
		}
	case libskillinstall.DriftBrokenSymlink:
		if fingerprint == "absent" || strings.Contains(fingerprint, ":symlink:") {
			direction = skillDriftLossless
		}
	case libskillinstall.DriftContent, libskillinstall.DriftUnexpected:
		if blob != "" && !isTopLevelSkillKey(entry.Path) {
			direction = g.resolveSkillDriftBlobAncestry(repoRoot, blob, filepath.Join(libskillinstall.SourceRelPath, "skills", entry.Path))
		}
	}
	if direction != skillDriftLossless && direction != skillDriftBehind {
		return libskillinstall.HealFix{}, false
	}
	fix := libskillinstall.HealFix{Path: entry.Path, ExpectedFingerprint: fingerprint}
	sourcePath := filepath.ToSlash(filepath.Join(libskillinstall.SourceRelPath, "skills", entry.Path))
	if isTopLevelSkillKey(entry.Path) {
		listing, err := checkoutGitOutput(repoRoot, "ls-tree", base, "--", sourcePath)
		if err != nil || listing == "" {
			return fix, false
		}
		fix.Operation = libskillinstall.HealRelink
		return fix, true
	}
	if strings.HasSuffix(entry.Path, "/") {
		return fix, false
	}
	listing, err := checkoutGitOutput(repoRoot, "ls-tree", base, "--", sourcePath)
	if err != nil {
		return fix, false
	}
	if entry.Kind == libskillinstall.DriftUnexpected {
		if listing != "" {
			return fix, false
		}
		fix.Operation = libskillinstall.HealRemove
		return fix, true
	}
	data, err := checkoutSkillBlob(repoRoot, base+":"+sourcePath)
	if err != nil {
		return fix, false
	}
	snapshot, err := fs.ReadFile(frozen, "skills/"+entry.Path)
	if err != nil || !bytes.Equal(data, snapshot) {
		return fix, false
	}
	fix.Data = data
	fix.Operation = libskillinstall.HealWrite
	if entry.Kind == libskillinstall.DriftMode {
		fix.Operation = libskillinstall.HealChmod
	}
	return fix, true
}
