package skills

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	libskillinstall "github.com/recurser/bossalib/contract/plugin/skillinstall"
)

type originTipFile struct {
	blob       string
	executable bool
}

type skillOriginTip struct {
	files  map[string]originTipFile
	dirs   map[string]bool
	behind int
}

func (g *Group) loadSkillOriginTip(repoRoot string) (skillOriginTip, bool) {
	tip := skillOriginTip{behind: skillCheckoutBehindCount(repoRoot)}
	if tip.behind == 0 {
		return tip, false
	}
	prefix := filepath.ToSlash(filepath.Join(libskillinstall.SourceRelPath, "skills"))
	listing, err := g.skillDriftHistoryGit(repoRoot, "ls-tree", "-r", "-t", "--full-tree", "origin/HEAD", "--", prefix)
	if err != nil || strings.TrimSpace(listing) == "" {
		return tip, false
	}
	tip.files = make(map[string]originTipFile)
	tip.dirs = make(map[string]bool)
	for line := range strings.SplitSeq(strings.TrimSpace(listing), "\n") {
		meta, path, ok := strings.Cut(line, "\t")
		fields := strings.Fields(meta)
		if !ok || len(fields) != 3 {
			return skillOriginTip{}, false
		}
		rel, ok := strings.CutPrefix(path, prefix+"/")
		if !ok {
			continue
		}
		switch fields[1] {
		case "tree":
			tip.dirs[rel] = true
		case "blob":
			file := originTipFile{executable: fields[0] == "100755"}
			if fields[0] == "100644" || fields[0] == "100755" {
				file.blob = fields[2]
			}
			tip.files[rel] = file
		}
	}
	return tip, len(tip.files) > 0 || len(tip.dirs) > 0
}

// splitCheckoutStale compares installed state with the tip, never with an
// intermediate revision. Failed reads retain the existing conservative verdict.
func (g *Group) splitCheckoutStale(repoRoot, dir string, tip skillOriginTip, entries []libskillinstall.DriftEntry) (stale, rest []libskillinstall.DriftEntry) {
	if tip.files == nil {
		return nil, entries
	}
	matches := make(map[string]bool)
	var paths, keys []string
	for _, entry := range entries {
		match, path := tip.matchInstalledEntry(dir, entry)
		matches[entry.Path] = match
		if path != "" {
			paths = append(paths, path)
			keys = append(keys, entry.Path)
		}
	}
	if len(paths) > 0 {
		blobs, err := g.skillDriftHistoryGit(repoRoot, append([]string{"hash-object", "--"}, paths...)...)
		hashes := strings.Fields(blobs)
		if err != nil || len(hashes) != len(paths) {
			// No file state, including an absent file, is accepted on a failed batch.
			for _, entry := range entries {
				if !isTopLevelSkillKey(entry.Path) && !strings.HasSuffix(entry.Path, "/") {
					delete(matches, entry.Path)
				}
			}
		} else {
			for i, key := range keys {
				matches[key] = hashes[i] == tip.files[key].blob
			}
		}
	}
	for _, entry := range entries {
		if matches[entry.Path] {
			stale = append(stale, entry)
		} else {
			rest = append(rest, entry)
		}
	}
	return stale, rest
}

// matchInstalledEntry returns an immediate state match or a regular file to hash.
func (tip skillOriginTip) matchInstalledEntry(dir string, entry libskillinstall.DriftEntry) (bool, string) {
	rel := entry.Path
	if entry.Kind == libskillinstall.DriftBrokenSymlink {
		return false, ""
	}
	if isTopLevelSkillKey(rel) {
		target, err := os.Readlink(filepath.Join(dir, rel))
		return err == nil && target == filepath.Join(libskillinstall.Namespace, rel) && tip.dirs[rel], ""
	}
	if strings.HasSuffix(rel, "/") {
		return tip.dirs[strings.TrimSuffix(rel, "/")], ""
	}
	path := filepath.Join(dir, libskillinstall.Namespace, filepath.FromSlash(rel))
	info, err := os.Lstat(path)
	file, exists := tip.files[rel]
	if os.IsNotExist(err) {
		return !exists, ""
	}
	if err != nil || !info.Mode().IsRegular() || !exists || (info.Mode()&0111 != 0) != file.executable {
		return false, ""
	}
	return false, path
}

func formatCheckoutStaleLine(agent string, n, behind int, root string) string {
	return fmt.Sprintf("boss skills gate: %s %d checkout-stale path(s); checkout is %d commit(s) behind origin/HEAD — installed copies match origin/HEAD; nothing to reinstall · inspect: git -C %s diff --stat HEAD origin/HEAD -- %s/skills", agent, n, behind, shellQuote(root), libskillinstall.SourceRelPath)
}

func (g *Group) renderSkillGateTree(out io.Writer, agent, dir, repoRoot string, payload selectedSkillPayload, report libskillinstall.DriftReport, tip skillOriginTip, behindHeaderDone *bool) error {
	if len(report.Entries) == 0 {
		_, _ = fmt.Fprintf(out, "boss skills gate: %s %s — compared %d file(s) · skills dir: %s · payload source: %s\n", agent, currentStale(!report.Installed, "clean", "no skills installed"), report.Compared, dir, payload.srcRoot)
		return nil
	}
	selfEdited, unexplained, fallback := classifySkillGateDrift(repoRoot, report.Entries)
	if len(selfEdited) > 0 {
		_, _ = fmt.Fprintf(out, "boss skills gate: %s self-edited drift: %s%s\n", agent, strings.Join(selfEdited, ", "), skillGateFallbackNote(fallback))
	}
	if !fallback {
		stale, rest := g.splitCheckoutStale(repoRoot, dir, tip, unexplained)
		unexplained = rest
		if len(stale) > 0 {
			_, _ = fmt.Fprintln(out, formatCheckoutStaleLine(agent, len(stale), tip.behind, repoRoot))
		}
	}
	if len(unexplained) == 0 {
		return nil
	}
	if !*behindHeaderDone {
		*behindHeaderDone = true
		if n := skillCheckoutBehindCount(repoRoot); n > 0 {
			_, _ = fmt.Fprintf(out, "boss skills gate: this checkout is %d commit(s) behind origin/HEAD — directions are judged against HEAD\n", n)
		}
	}
	_, _ = fmt.Fprintf(out, "boss skills gate: %s skill drift detected — %d path(s) in this agent tree%s\n", agent, len(unexplained), skillGateFallbackNote(fallback))
	entries := g.skillDriftEntriesWithDirection(repoRoot, dir, unexplained)
	for _, entry := range entries {
		_, _ = fmt.Fprintf(out, "  - %s\n", entry.label())
	}
	advice := composeSkillInstallRemedy(payload, entries, g.skillCheckoutHasAncestry(repoRoot), true, agent)
	for _, line := range advice.lines("  ") {
		_, _ = fmt.Fprintln(out, line)
	}
	return errors.New("skill drift detected")
}

func skillGateFallbackNote(fallback bool) string {
	if fallback {
		return " (origin/HEAD unavailable; used git status fallback)"
	}
	return ""
}
