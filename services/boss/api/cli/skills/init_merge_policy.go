package skills

import (
	"encoding/json"
	"os"
	"strings"
)

const startSlowGlob = "**"
const startSlowLabel = "start slow, every PR waits until you enable auto-merge"

type mergePolicy string

const (
	mergePolicyDefault mergePolicy = "default"
	mergePolicyBrake   mergePolicy = "brake"
	mergePolicyRules   mergePolicy = "rules"
)

func (policy mergePolicy) text() string {
	switch policy {
	case mergePolicyDefault:
		return `verify merges nothing by default: it parks every passing PR for boss pr approve or the TUI's [m]erge. To let verify auto-merge, add rules to verifyDefaults.merge.auto (for example {"level": "high"}) and merge that change to the base branch.`
	case mergePolicyBrake:
		return `the config keeps its "**" verifyDefaults.alwaysHumanPaths entry, which parks every passing PR for boss pr approve or the TUI's [m]erge and which no verifyDefaults.merge.auto rule can cover. To let verify auto-merge, remove that entry, add verifyDefaults.merge.auto rules (for example {"level": "high"}) and merge that change to the base branch.`
	default:
		return ""
	}
}

func writtenMergePolicy(existing []byte) mergePolicy {
	if hasStartSlowEntry(existing) {
		return mergePolicyBrake
	}
	if mergeAutoRules(existing) {
		return mergePolicyRules
	}
	return mergePolicyDefault
}

func mergeAutoRules(existing []byte) bool {
	var cfg struct {
		VerifyDefaults struct {
			Merge struct {
				Auto []json.RawMessage `json:"auto"`
			} `json:"merge"`
		} `json:"verifyDefaults"`
	}
	return json.Unmarshal(existing, &cfg) == nil && len(cfg.VerifyDefaults.Merge.Auto) > 0
}

func detectedMergePolicy(cfg *verifyDefaultsBlock) mergePolicy {
	if cfg != nil {
		return mergePolicyBrake
	}
	return mergePolicyDefault
}

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

func initCarriedBrake(target string, info os.FileInfo, force bool) *verifyDefaultsBlock {
	if force && carryStartSlow(target, info) {
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

func mergePolicyReport(policy mergePolicy) string {
	text := policy.text()
	if text == "" {
		return ""
	}
	var b strings.Builder
	b.WriteString("\nMerge policy:\n")
	for _, line := range wrapText(text) {
		b.WriteString("  " + line + "\n")
	}
	return b.String()
}

type adaptersBlock struct {
	Tracker string `json:"tracker"`
}
type githubTrackerBlock struct {
	Repository string `json:"repository"`
}
