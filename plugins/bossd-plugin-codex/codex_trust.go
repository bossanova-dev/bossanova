package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

// Codex project trust levels. Codex reads the worktree's own .codex/config.toml
// (MCP server commands, model provider) only for a trusted project.
const (
	codexTrustedProjectLevel   = "trusted"
	codexUntrustedProjectLevel = "untrusted"
)

// trustCodexWorktree records worktreePath as a trusted Codex project in every
// Codex config the launch can read trust from (codexTrustDirs): the plugin's
// own and the one the launch runs with (configHomeEnv). applyCodexProjectTrust
// calls it only when bossd sends PROJECT_TRUST_DECISION_GRANT (the repo is
// trusted locally), on every RPC that starts codex; never call it for an
// untrusted repo.
//
// It writes the same set of homes untrustCodexWorktree does, so a grant after
// a revoke restores trust in an account-bound launch's own CODEX_HOME rather
// than only in the plugin's home, which that launch never reads. Missing
// config directories and files are created.
func trustCodexWorktree(worktreePath string, configHomeEnv map[string]string) error {
	return editCodexWorktreeTrust(worktreePath, configHomeEnv, true, codexTrustedProjectLevel)
}

// untrustCodexWorktree records worktreePath as an explicitly UNTRUSTED Codex
// project (BOS-1562). applyCodexProjectTrust calls it only when bossd sends
// PROJECT_TRUST_DECISION_REVOKE, on every RPC that starts codex.
//
// Deleting a trust entry is not enough: for a linked git worktree with no
// entry of its own, Codex falls back to the trust entry of the repository's
// main checkout, so a user who trusts their own checkout of the repo would
// leave every bossd worktree of it trusted. An explicit "untrusted" entry wins
// over that fallback, and Codex then neither loads the worktree's project
// config nor prompts for trust.
//
// It edits both the plugin's own Codex config and the one the launch runs
// with (configHomeEnv — an account-bound session's CODEX_HOME/HOME, or a
// headless run's extra_env, of which only those two keys are read). An absent
// config is a no-op that creates nothing: a home with no config trusts
// nothing. A config whose shape cannot be edited safely is an error, so the
// caller refuses the launch rather than run it trusted.
func untrustCodexWorktree(worktreePath string, configHomeEnv map[string]string) error {
	return editCodexWorktreeTrust(worktreePath, configHomeEnv, false, codexUntrustedProjectLevel)
}

// codexUntrustedOverride is the per-process Codex config override that runs
// one launch in worktreePath untrusted without editing any config.toml: a `-c`
// value marking every spelling of the path (codexProjectPathSpellings)
// explicitly untrusted. applyCodexProjectTrust returns it for
// PROJECT_TRUST_DECISION_DENY — an untrusted repo's launch in the user's own
// checkout, whose persisted trust (and, through Codex's main-checkout
// fallback, that of their linked worktrees) must not be rewritten — and for
// PROJECT_TRUST_DECISION_REVOKE on top of its persisted edits, because codex
// may read a home (a CODEX_HOME from the user's shell rc or tmux env) that
// REVOKE never edited.
//
// Codex merges the override over every config layer, so it beats a stale
// "trusted" entry and the main-checkout fallback, and an explicit "untrusted"
// also keeps the TUI from prompting (and persisting) trust. It is one inline
// `projects` table rather than `projects."<path>".trust_level=...` because
// codex splits a `-c` key on every '.' and keeps quotes literally, so a path
// can only be named inside the value (measured on codex-cli 0.162.1).
func codexUntrustedOverride(worktreePath string) (string, error) {
	if strings.TrimSpace(worktreePath) == "" {
		return "", nil
	}
	spellings, err := codexProjectPathSpellings(worktreePath)
	if err != nil {
		return "", err
	}
	entries := make([]string, 0, len(spellings))
	for _, path := range spellings {
		entries = append(entries, `"`+tomlBasicStringEscape(path)+`"={trust_level="`+codexUntrustedProjectLevel+`"}`)
	}
	return "projects={" + strings.Join(entries, ",") + "}", nil
}

// appendConfigOverride appends override to a codex argv as a `-c` value; an
// empty override appends nothing.
func appendConfigOverride(args []string, override string) []string {
	if override == "" {
		return args
	}
	return append(args, "-c", override)
}

// editCodexWorktreeTrust sets worktreePath's trust_level to level in each of
// codexTrustDirs, for every spelling of the path Codex may look up
// (codexProjectPathSpellings), joining any per-directory failures.
func editCodexWorktreeTrust(worktreePath string, configHomeEnv map[string]string, create bool, level string) error {
	if strings.TrimSpace(worktreePath) == "" {
		return nil
	}

	spellings, err := codexProjectPathSpellings(worktreePath)
	if err != nil {
		return err
	}

	dirs, err := codexTrustDirs(configHomeEnv, spellings[0])
	if err != nil {
		return err
	}
	var errs []error
	for _, dir := range dirs {
		if err := editCodexConfig(dir, create, func(config string) (string, error) {
			for _, path := range spellings {
				var err error
				if config, err = setCodexProjectTrustLevel(config, path, level); err != nil {
					return "", err
				}
			}
			return config, nil
		}); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", filepath.Join(dir, "config.toml"), err))
		}
	}
	return errors.Join(errs...)
}

// codexProjectPathSpellings is every key Codex may look worktreePath's project
// trust up under: the absolute path as given, and its symlink-resolved form
// when that differs (macOS /var -> /private/var, a symlinked worktree root).
// Codex consults the resolved form first, so a decision keyed only on the
// spelling bossd passed would miss it. A path that cannot be resolved (it
// does not exist yet) has only its absolute spelling.
func codexProjectPathSpellings(worktreePath string) ([]string, error) {
	absPath, err := filepath.Abs(worktreePath)
	if err != nil {
		return nil, fmt.Errorf("resolve worktree path: %w", err)
	}
	spellings := []string{absPath}
	if resolved, err := filepath.EvalSymlinks(absPath); err == nil && resolved != absPath {
		spellings = append(spellings, resolved)
	}
	return spellings, nil
}

// codexTrustDirs is the deduplicated set of Codex config directories a trust
// decision edits: the plugin's own, then the launch's effective one
// (configHomeEnv resolved against the worktree the child runs in).
func codexTrustDirs(configHomeEnv map[string]string, worktreePath string) ([]string, error) {
	baseDir, err := codexConfigDir()
	if err != nil {
		return nil, err
	}
	effectiveDir, err := codexConfigDirForEnv(configHomeEnv, worktreePath)
	if err != nil {
		return nil, err
	}
	dirs := []string{filepath.Clean(baseDir)}
	if effective := filepath.Clean(effectiveDir); effective != dirs[0] {
		dirs = append(dirs, effective)
	}
	return dirs, nil
}

// editCodexConfig rewrites configDir/config.toml through edit while holding
// the config lock, keeping the file's mode. An edit error leaves the file
// untouched. With create, a missing directory
// and file are created; without it, an absent config is a no-op that creates
// nothing (not even the lock file).
func editCodexConfig(configDir string, create bool, edit func(string) (string, error)) error {
	configPath := filepath.Clean(filepath.Join(configDir, "config.toml"))
	if create {
		if err := os.MkdirAll(configDir, 0o700); err != nil {
			return fmt.Errorf("create codex config dir: %w", err)
		}
	} else if _, err := os.Stat(configPath); os.IsNotExist(err) {
		return nil
	}

	lockFile, err := lockCodexConfig(configDir)
	if err != nil {
		return err
	}
	defer unlockCodexConfig(lockFile)

	var mode os.FileMode = 0o600
	current, err := os.ReadFile(configPath)
	if err != nil {
		if !os.IsNotExist(err) {
			return fmt.Errorf("read codex config: %w", err)
		}
		if !create {
			return nil
		}
	} else if info, statErr := os.Stat(configPath); statErr == nil {
		mode = info.Mode().Perm()
	}

	next, err := edit(string(current))
	if err != nil {
		return err
	}
	if next == string(current) {
		return nil
	}
	if err := os.WriteFile(configPath, []byte(next), mode); err != nil {
		return fmt.Errorf("write codex config: %w", err)
	}
	return nil
}

func lockCodexConfig(configDir string) (*os.File, error) {
	lockPath := filepath.Clean(filepath.Join(configDir, ".config.toml.lock"))
	lockFile, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open codex config lock: %w", err)
	}
	if err := syscall.Flock(int(lockFile.Fd()), syscall.LOCK_EX); err != nil {
		_ = lockFile.Close()
		return nil, fmt.Errorf("lock codex config: %w", err)
	}
	return lockFile, nil
}

func unlockCodexConfig(lockFile *os.File) {
	_ = syscall.Flock(int(lockFile.Fd()), syscall.LOCK_UN)
	_ = lockFile.Close()
}

func codexConfigDir() (string, error) {
	return codexConfigDirForEnv(nil, "")
}

// codexConfigDirForEnv resolves Codex's config directory after applying the
// supplied process-environment overlay. A missing overlay key inherits the
// daemon environment; an empty key deliberately falls through to Codex's
// ~/.codex default, matching the environment passed to a spawned process.
//
// workDir is the directory the Codex child will run in. A relative CODEX_HOME
// or HOME is resolved by that child against its own cwd, not the plugin's, so
// it is resolved against workDir here too — otherwise a trust edit lands in a
// directory the child never reads. Not every bossd env builder pre-resolves
// these (resume, orphan resume and rotation do not), which is why the plugin
// does it at the point of use. An empty workDir leaves a relative value as is.
func codexConfigDirForEnv(extraEnv map[string]string, workDir string) (string, error) {
	codexHome, projected := extraEnv["CODEX_HOME"]
	if !projected {
		codexHome = os.Getenv("CODEX_HOME")
	}
	if codexHome = strings.TrimSpace(codexHome); codexHome != "" {
		return resolveAgainstWorkDir(codexHome, workDir), nil
	}

	home, projected := extraEnv["HOME"]
	if !projected {
		var err error
		home, err = os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("resolve home dir: %w", err)
		}
	}
	if home = strings.TrimSpace(home); home == "" {
		return "", fmt.Errorf("resolve home dir: empty")
	}
	return filepath.Join(resolveAgainstWorkDir(home, workDir), ".codex"), nil
}

// resolveAgainstWorkDir interprets a relative path the way a child process
// started in workDir would. An absolute path, or an empty workDir, is returned
// unchanged.
func resolveAgainstWorkDir(path, workDir string) string {
	if workDir == "" || filepath.IsAbs(path) {
		return path
	}
	return filepath.Join(workDir, path)
}

// setCodexProjectTrustLevel sets trust_level = level for worktreePath's
// project in config, recognising every equivalent TOML spelling of the table
// and key: bare, "basic" or 'literal' keys, whitespace around dots, a
// [projects."<path>"] table or a dotted projects."<path>".trust_level key.
//
// An existing trust_level value is rewritten in place; otherwise the line is
// added under an existing [projects."<path>"] header, or a new table is
// appended. A config it cannot edit without guessing — one it cannot parse,
// a projects table written inline or as an array, a project table defined
// only by other dotted keys — is an error rather than a duplicate table (a
// TOML error that would break the whole Codex config) or a trust left in
// place.
func setCodexProjectTrustLevel(config, worktreePath, level string) (string, error) {
	stmts, err := parseTOMLStatements(config)
	if err != nil {
		return "", fmt.Errorf("cannot edit codex project trust: %w", err)
	}
	table := []string{"projects", worktreePath}
	trustKey := []string{"projects", worktreePath, "trust_level"}
	refuse := func(why string) (string, error) {
		return "", fmt.Errorf("cannot edit codex project trust for %q: %s; edit config.toml by hand", worktreePath, why)
	}

	var trustValues []tomlStatement
	header := -1
	dottedTable := false
	for i, st := range stmts {
		switch {
		case st.kind == tomlStmtKeyValue && tomlPathEqual(st.path, trustKey):
			trustValues = append(trustValues, st)
		case tomlPathHasPrefix(st.path, trustKey):
			return refuse("trust_level is written as a table")
		case st.kind == tomlStmtArrayTable && tomlPathHasPrefix(table, st.path):
			return refuse("the projects table is an array of tables")
		case st.kind == tomlStmtKeyValue && tomlPathHasPrefix(table, st.path):
			return refuse("the projects table is written as an inline table")
		case st.kind == tomlStmtTable && tomlPathEqual(st.path, table):
			if header < 0 {
				header = i
			}
		case st.kind == tomlStmtKeyValue && tomlPathHasPrefix(st.path, table):
			dottedTable = true
		}
	}

	quoted := `"` + level + `"`
	if len(trustValues) > 0 {
		for i := len(trustValues) - 1; i >= 0; i-- {
			st := trustValues[i]
			config = config[:st.valueStart] + quoted + config[st.valueEnd:]
		}
		return config, nil
	}

	trustLine := "trust_level = " + quoted
	if header >= 0 {
		at := stmts[header].lineEnd
		insert := trustLine + "\n"
		if at > 0 && config[at-1] != '\n' {
			insert = "\n" + insert
		}
		return config[:at] + insert + config[at:], nil
	}
	if dottedTable {
		return refuse("the project table is defined by dotted keys")
	}

	headerLine := `[projects."` + tomlBasicStringEscape(worktreePath) + `"]`
	if strings.TrimSpace(config) == "" {
		return headerLine + "\n" + trustLine + "\n", nil
	}
	if !strings.HasSuffix(config, "\n") {
		config += "\n"
	}
	return config + "\n" + headerLine + "\n" + trustLine + "\n", nil
}

func tomlBasicStringEscape(value string) string {
	var b strings.Builder
	for _, r := range value {
		switch r {
		case '\\':
			b.WriteString(`\\`)
		case '"':
			b.WriteString(`\"`)
		case '\b':
			b.WriteString(`\b`)
		case '\t':
			b.WriteString(`\t`)
		case '\n':
			b.WriteString(`\n`)
		case '\f':
			b.WriteString(`\f`)
		case '\r':
			b.WriteString(`\r`)
		default:
			if r < 0x20 {
				_, _ = fmt.Fprintf(&b, `\u%04X`, r)
			} else {
				b.WriteRune(r)
			}
		}
	}
	return b.String()
}
