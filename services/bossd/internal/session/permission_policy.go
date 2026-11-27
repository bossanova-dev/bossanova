package session

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/recurser/bossalib/models"
	"github.com/recurser/bossd/internal/domain/repotrust"
)

const maxSkillsConfigBytes = 1 << 20 // 1 MiB safety cap for reading .boss-skills.json

// UnattendedPermissionPolicy is the daemon-owned tool-permission policy for an
// unattended agent launch (BOS-2678). Call sites at the proto boundary project
// it onto bossanovav1.UnattendedPermissionPolicy.
type UnattendedPermissionPolicy struct {
	WorktreePath           string
	AllowedCommandPrefixes []string
	IsNetworkAllowed       bool
}

// PolicyInputs are the worktree and repository facts read from disk before
// BuildUnattendedPermissionPolicy computes the final command allowlist.
type PolicyInputs struct {
	WorktreePath   string
	Lockfiles      []string
	SkillsCommands map[string]string
	IsRepoTrusted  bool
}

type lockfileManager struct {
	filename string
	manager  string
}

var (
	baseUnattendedCommandPrefixes = []string{"git", "gh", "boss", "node", "make"}

	lockfileManagers = []lockfileManager{
		{filename: "pnpm-lock.yaml", manager: "pnpm"},
		{filename: "package-lock.json", manager: "npm"},
		{filename: "yarn.lock", manager: "yarn"},
		{filename: "bun.lock", manager: "bun"},
		{filename: "bun.lockb", manager: "bun"},
		{filename: "go.sum", manager: "go"},
		{filename: "Cargo.lock", manager: "cargo"},
		{filename: "uv.lock", manager: "uv"},
	}

	// disallowedCommandPrefixes lists interpreters and command wrappers that
	// must never be widened into an unattended allowlist from repo config,
	// because allowing `sh:*` or `env:*` collapses prefix scoping into arbitrary
	// shell execution.
	disallowedCommandPrefixes = map[string]struct{}{
		"sh":      {},
		"bash":    {},
		"zsh":     {},
		"fish":    {},
		"env":     {},
		"sudo":    {},
		"xargs":   {},
		"eval":    {},
		"exec":    {},
		"python":  {},
		"python3": {},
		"ruby":    {},
		"perl":    {},
		"npx":     {},
		"bunx":    {},
	}
)

// UnattendedPermissionPolicyFor resolves worktree and repo inputs and builds
// the scoped unattended tool-permission policy for a launch.
func UnattendedPermissionPolicyFor(worktree string, repo *models.Repo) UnattendedPermissionPolicy {
	return BuildUnattendedPermissionPolicy(resolvePolicyInputs(worktree, repo))
}

// resolvePolicyInputs reads the worktree root's lockfiles and — ONLY when repo
// is locally trusted — its committed .boss-skills.json commands map. Missing
// or malformed files degrade cleanly to empty inputs.
func resolvePolicyInputs(worktree string, repo *models.Repo) PolicyInputs {
	trusted := repotrust.Allowed(repo)
	cleanWorktree := normalizePolicyWorktree(worktree)
	inputs := PolicyInputs{
		WorktreePath:  cleanWorktree,
		IsRepoTrusted: trusted,
	}
	if cleanWorktree == "" {
		return inputs
	}

	for _, lm := range lockfileManagers {
		info, err := os.Stat(filepath.Join(cleanWorktree, lm.filename))
		if err == nil && !info.IsDir() {
			inputs.Lockfiles = append(inputs.Lockfiles, lm.filename)
		}
	}

	if trusted {
		inputs.SkillsCommands = readSkillsCommands(filepath.Join(cleanWorktree, ".boss-skills.json"))
	}
	return inputs
}

// BuildUnattendedPermissionPolicy computes the deterministic, deduplicated
// command-prefix policy from already-resolved worktree and repo inputs.
func BuildUnattendedPermissionPolicy(inputs PolicyInputs) UnattendedPermissionPolicy {
	worktree := ""
	if inputs.WorktreePath != "" {
		worktree = filepath.Clean(inputs.WorktreePath)
	}

	prefixes := make([]string, 0, len(baseUnattendedCommandPrefixes)+len(inputs.Lockfiles)+len(inputs.SkillsCommands))
	seen := make(map[string]struct{}, len(baseUnattendedCommandPrefixes)+len(inputs.Lockfiles)+len(inputs.SkillsCommands))
	add := func(prefix string) {
		if prefix == "" {
			return
		}
		if _, ok := seen[prefix]; ok {
			return
		}
		seen[prefix] = struct{}{}
		prefixes = append(prefixes, prefix)
	}

	for _, p := range baseUnattendedCommandPrefixes {
		add(p)
	}

	if len(inputs.Lockfiles) > 0 {
		present := make(map[string]struct{}, len(inputs.Lockfiles))
		for _, name := range inputs.Lockfiles {
			present[name] = struct{}{}
		}
		for _, lm := range lockfileManagers {
			if _, ok := present[lm.filename]; ok {
				add(lm.manager)
			}
		}
	}

	if inputs.IsRepoTrusted && len(inputs.SkillsCommands) > 0 {
		keys := make([]string, 0, len(inputs.SkillsCommands))
		for k := range inputs.SkillsCommands {
			keys = append(keys, k)
		}
		slices.Sort(keys)
		for _, k := range keys {
			if word := firstCommandWord(inputs.SkillsCommands[k]); isSafeCommandPrefix(word) {
				add(word)
			}
		}
	}

	return UnattendedPermissionPolicy{
		WorktreePath:           worktree,
		AllowedCommandPrefixes: prefixes,
		IsNetworkAllowed:       true,
	}
}

func normalizePolicyWorktree(worktree string) string {
	trimmed := strings.TrimSpace(worktree)
	if trimmed == "" {
		return ""
	}
	if !filepath.IsAbs(trimmed) {
		if abs, err := filepath.Abs(trimmed); err == nil {
			return filepath.Clean(abs)
		}
	}
	return filepath.Clean(trimmed)
}

func readSkillsCommands(path string) map[string]string {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer func() { _ = f.Close() }()

	data, err := io.ReadAll(io.LimitReader(f, maxSkillsConfigBytes+1))
	if err != nil || len(data) > maxSkillsConfigBytes {
		return nil
	}

	var cfg struct {
		Commands map[string]json.RawMessage `json:"commands"`
	}
	if err := json.Unmarshal(data, &cfg); err != nil || len(cfg.Commands) == 0 {
		return nil
	}

	out := make(map[string]string, len(cfg.Commands))
	for k, raw := range cfg.Commands {
		var cmd string
		if json.Unmarshal(raw, &cmd) == nil {
			out[k] = cmd
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// firstCommandWord splits command on ASCII space and tab separators and returns
// the first non-assignment word, skipping leading NAME=value words whose values
// carry no shell control metacharacters. Non-space whitespace (such as '\n') is
// intentionally left inside the token so isSafeCommandPrefix rejects it.
func firstCommandWord(command string) string {
	words := strings.FieldsFunc(command, func(r rune) bool {
		return r == ' ' || r == '\t'
	})
	for _, word := range words {
		if isSafeEnvAssignment(word) {
			continue
		}
		return word
	}
	return ""
}

func isSafeEnvAssignment(word string) bool {
	eq := strings.IndexByte(word, '=')
	if eq <= 0 {
		return false
	}
	name := word[:eq]
	for i := 0; i < len(name); i++ {
		c := name[i]
		isAlpha := (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || c == '_'
		isDigit := c >= '0' && c <= '9'
		if i == 0 && !isAlpha {
			return false
		}
		if !isAlpha && !isDigit {
			return false
		}
	}
	val := word[eq+1:]
	return !strings.ContainsAny(val, ";|&$`()<>'\"\\\n\r")
}

// isSafeCommandPrefix validates a repo-derived command word. Only plain ASCII
// executable names starting with [A-Za-z0-9_] and continuing with
// [A-Za-z0-9._+-] are accepted, excluding all whitespace, path separators,
// shell/rule metacharacters, and known interpreters/wrappers.
func isSafeCommandPrefix(word string) bool {
	if word == "" {
		return false
	}
	if _, blocked := disallowedCommandPrefixes[strings.ToLower(word)]; blocked {
		return false
	}
	for i := 0; i < len(word); i++ {
		c := word[i]
		isAlphaNum := (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '_'
		if i == 0 {
			if !isAlphaNum {
				return false
			}
			continue
		}
		if !isAlphaNum && c != '.' && c != '-' && c != '+' {
			return false
		}
	}
	return true
}
