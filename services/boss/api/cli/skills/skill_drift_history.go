package skills

import (
	"fmt"
	"strings"
)

// skillDriftHistoryLookback bounds commits touching any skill tree. Exhaustion
// without a match is undecided, so bounded work never permits destructive healing.
const skillDriftHistoryLookback = 5000

// skillDriftHistory is lazy and batch-owned: even failed walks are performed at
// most once per revision range, without leaking results across repositories.
type skillDriftHistory struct {
	head   *skillDriftHistoryIndex
	origin *skillDriftHistoryIndex
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
