package skills

import (
	"encoding/json"
	"os"
	"strings"
)

const startSlowGlob = "**"
const startSlowLabel = "start slow, every PR waits until you enable auto-merge"
const startSlowPolicyText = `verify parks every passing PR for boss pr approve or the TUI's [m]erge. To enable auto-merge, remove the "**" entry from verifyDefaults.alwaysHumanPaths and merge that change to the base branch.`
const startSlowBeforeBuild = "commit .boss-skills.json and merge it to the base branch before the first build: verify reads its park-every-PR policy from there, and until then it merges passing PRs"

// detectedConfig carries detected commands and the policy resolved in Go. DisallowUnknownFields is used when decoding into it, so a
// detector that grows a new block fails loudly here instead of having that
// block silently dropped from every config this command writes.
type detectedConfig struct {
	Commands map[string]string `json:"commands,omitempty"`
	Adapters *adaptersBlock    `json:"adapters,omitempty"`
	// TrackerConfig is filled in Go after the detect decode (never decoded from
	// node), and only when a Linear team was resolved.
	TrackerConfig  *trackerBlock        `json:"trackerConfig,omitempty"`
	VerifyDefaults *verifyDefaultsBlock `json:"verifyDefaults,omitempty"`
}

// trackerBlock is the emitted trackerConfig. Only the team is written:
// mcpServer, states and labels come from the skill module's defaults.
type trackerBlock struct {
	Linear *linearTrackerBlock `json:"linear,omitempty"`
	GitHub *githubTrackerBlock `json:"github,omitempty"`
}

type linearTrackerBlock struct {
	Team    string `json:"team"`
	TeamKey string `json:"teamKey,omitempty"`
}

type verifyDefaultsBlock struct {
	AlwaysHumanPaths []alwaysHumanPathEntry `json:"alwaysHumanPaths"`
}

type alwaysHumanPathEntry struct {
	Glob  string `json:"glob"`
	Label string `json:"label"`
}

func newStartSlowPolicy() *verifyDefaultsBlock {
	return &verifyDefaultsBlock{AlwaysHumanPaths: []alwaysHumanPathEntry{{Glob: startSlowGlob, Label: startSlowLabel}}}
}

func initStartSlowPolicy(target string, info os.FileInfo, force bool) *verifyDefaultsBlock {
	if info == nil || (force && carryStartSlow(target, info)) {
		return newStartSlowPolicy()
	}
	return nil
}

// Only inspect regular files: --force must never follow a symlink to decide policy.
func carryStartSlow(target string, info os.FileInfo) bool {
	if info == nil || !info.Mode().IsRegular() {
		return false
	}
	existing, err := os.ReadFile(target)
	return err == nil && hasStartSlowEntry(existing)
}

func hasStartSlowEntry(existing []byte) bool {
	var cfg struct {
		VerifyDefaults struct {
			AlwaysHumanPaths []json.RawMessage `json:"alwaysHumanPaths"`
		} `json:"verifyDefaults"`
	}
	if json.Unmarshal(existing, &cfg) != nil {
		return false
	}
	for _, raw := range cfg.VerifyDefaults.AlwaysHumanPaths {
		var glob string
		if json.Unmarshal(raw, &glob) == nil && glob == startSlowGlob {
			return true
		}
		var entry struct {
			Glob string `json:"glob"`
		}
		if json.Unmarshal(raw, &entry) == nil && entry.Glob == startSlowGlob {
			return true
		}
	}
	return false
}

func startSlowReport(enabled bool) string {
	if !enabled {
		return ""
	}
	var b strings.Builder
	b.WriteString("\nMerge policy:\n")
	for _, text := range []string{startSlowPolicyText, startSlowBeforeBuild} {
		for _, line := range wrapText(text) {
			b.WriteString("  " + line + "\n")
		}
	}
	return b.String()
}

type adaptersBlock struct {
	Tracker string `json:"tracker"`
}
type githubTrackerBlock struct {
	Repository string `json:"repository"`
}
