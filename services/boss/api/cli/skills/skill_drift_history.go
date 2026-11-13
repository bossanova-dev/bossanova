package skills

import (
	"fmt"
	"path/filepath"
	"strings"

	libskillinstall "github.com/recurser/bossalib/contract/plugin/skillinstall"
)

// skillDriftHistoryLookback bounds commits touching any skill tree. Exhaustion
// without a match is undecided, so bounded work never permits destructive healing.
const skillDriftHistoryLookback = 5000

// skillDriftHistory is lazy and batch-owned: even failed walks are performed at
// most once per revision range, without leaking results across repositories.
type skillDriftHistory struct {
	head    *skillDriftHistoryIndex
	origin  *skillDriftHistoryIndex
	changed *skillDriftOriginChanges
}

type skillDriftHistoryIndex struct {
	blobs   map[string]struct{}
	commits int
	err     error
}

func (g *Group) resolveSkillDriftBlobAncestry(repoRoot, blob string, history *skillDriftHistory) skillDriftDirection {
	if history.head == nil {
		history.head = g.readSkillDriftHistory(repoRoot, "HEAD")
	}
	head := history.head
	if head.err != nil {
		return skillDriftUnknown
	}
	if _, found := head.blobs[blob]; found {
		return skillDriftBehind
	}
	if head.commits >= skillDriftHistoryLookback {
		return skillDriftUnknown
	}
	if history.origin == nil {
		history.origin = g.readSkillDriftHistory(repoRoot, "HEAD..origin/HEAD")
	}
	if _, found := history.origin.blobs[blob]; history.origin.err == nil && found {
		return skillDriftAhead
	}
	return skillDriftUnrecoverable
}

func (g *Group) readSkillDriftHistory(repoRoot, revRange string) *skillDriftHistoryIndex {
	index := &skillDriftHistoryIndex{blobs: make(map[string]struct{})}
	out, err := g.skillDriftHistoryGit(repoRoot, "log", "--format=%H", "--raw", "--no-renames",
		"--abbrev=40", fmt.Sprintf("-n%d", skillDriftHistoryLookback), revRange, "--",
		":(glob)**/skills/**", ":(glob)skills-toolbox/**")
	index.err = err
	if err != nil {
		return index
	}
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if !strings.HasPrefix(line, ":") {
			index.commits++
			continue
		}
		// :<oldmode> <newmode> <oldblob> <newblob> <status>\t<path>
		fields := strings.Fields(line)
		if len(fields) >= 4 {
			for _, blob := range fields[2:4] {
				index.blobs[blob] = struct{}{}
			}
		}
	}
	return index
}

type skillDriftOriginChanges struct {
	paths map[string]struct{}
	err   error
}

func (g *Group) resolveSkillDriftPathAncestry(repoRoot, rel, blob string, history *skillDriftHistory) skillDriftDirection {
	direction := g.resolveSkillDriftBlobAncestry(repoRoot, blob, history)
	if direction != skillDriftBehind {
		return direction
	}
	if history.changed == nil {
		history.changed = g.readSkillDriftOriginChanges(repoRoot)
	}
	if history.changed.err != nil {
		return skillDriftUnknown
	}
	if _, changed := history.changed.paths[rel]; changed {
		return skillDriftAhead
	}
	return direction
}

func (g *Group) readSkillDriftOriginChanges(repoRoot string) *skillDriftOriginChanges {
	index := &skillDriftOriginChanges{}
	origin, err := g.skillDriftHistoryGit(repoRoot, "rev-parse", "--verify", "--quiet", "origin/HEAD")
	if err != nil || strings.TrimSpace(origin) == "" {
		return index
	}
	prefix := filepath.ToSlash(filepath.Join(libskillinstall.SourceRelPath, "skills"))
	out, err := g.skillDriftHistoryGit(repoRoot, "diff", "--name-only", "-z", "--no-renames", "HEAD...origin/HEAD", "--", prefix)
	index.err = err
	if err != nil {
		return index
	}
	index.paths = make(map[string]struct{})
	for path := range strings.SplitSeq(out, "\x00") {
		if rel, ok := strings.CutPrefix(path, prefix+"/"); ok {
			index.paths[rel] = struct{}{}
		}
	}
	return index
}
