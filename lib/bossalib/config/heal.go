package config

import (
	"os"
	"path/filepath"
	"strings"
)

// HealPluginPaths repairs configured plugin entries whose stored Path no longer
// points at an executable binary — e.g. a Homebrew upgrade deleted the versioned
// Cellar directory the path was pinned to — by swapping in a same-name binary
// found by auto-discovery. Only Path changes; Enabled, Version and Config are
// preserved so user customizations survive. Repair is limited to entries whose
// current path is missing/non-executable: a path that exists but fails checksum is
// left for VerifyConfiguredPlugins to reject (fail closed), so a tampered binary is
// never silently swapped. The replacement is accepted only when the discovered
// binary is itself executable, so an entry is never marked healed to a path that
// cannot run. The discovered path is rewritten to its stable Homebrew opt form when
// possible so the healed config survives future upgrades. Input slices are not
// mutated. Returns the healed slice and the names whose paths were repaired.
func HealPluginPaths(configured, discovered []PluginConfig) ([]PluginConfig, []string) {
	byName := make(map[string]string, len(discovered))
	for _, d := range discovered {
		byName[d.Name] = d.Path
	}
	// Shallow copy: each element's Config map header is shared with the caller's
	// slice. This is safe because heal only reassigns the Path value field and
	// never writes into Config; do not mutate Config here without deep-copying it
	// first, or the caller's input would change too.
	out := append([]PluginConfig(nil), configured...)
	var healed []string
	for i := range out {
		if isExecutableFile(out[i].Path) {
			continue
		}
		newPath, ok := byName[out[i].Name]
		if !ok {
			continue
		}
		// Only heal to a binary that actually exists and is executable, so an entry
		// is never persisted as "repaired" while still pointing at a non-runnable
		// path. bossd's discovered list is already vetted, but this keeps the
		// function self-consistent for any caller.
		if !isExecutableFile(newPath) {
			continue
		}
		newPath = stableHomebrewPluginPath(newPath)
		if newPath == out[i].Path {
			continue
		}
		out[i].Path = newPath
		healed = append(healed, out[i].Name)
	}
	return out, healed
}

// stableHomebrewPluginPath maps a resolved Homebrew Cellar plugin path
// (.../Cellar/<formula>/<version>/libexec/plugins/<bin>) to its version-independent
// opt-symlink equivalent (.../opt/<formula>/libexec/plugins/<bin>), which Homebrew
// re-points on every upgrade. It only rewrites when the opt path exists and
// resolves to the same file; otherwise it returns path unchanged.
func stableHomebrewPluginPath(path string) string {
	parts := strings.Split(filepath.ToSlash(filepath.Clean(path)), "/")
	for i := 0; i+2 < len(parts); i++ {
		if parts[i] == "Cellar" {
			formula := parts[i+1]
			prefix := strings.Join(parts[:i], "/")
			rest := strings.Join(parts[i+3:], "/") // drop Cellar/<formula>/<version>
			opt := filepath.FromSlash(strings.Join([]string{prefix, "opt", formula, rest}, "/"))
			if sameFile(opt, path) {
				return opt
			}
			return path
		}
	}
	return path
}

func sameFile(a, b string) bool {
	ai, err := os.Stat(a)
	if err != nil {
		return false
	}
	bi, err := os.Stat(b)
	if err != nil {
		return false
	}
	return os.SameFile(ai, bi)
}
