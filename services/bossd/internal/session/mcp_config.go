package session

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/recurser/bossalib/config"
	"github.com/recurser/bossalib/models"
)

// StrictManagedMcpConfigForSession reports whether the agent should load ONLY the
// curated --mcp-config and ignore project .mcp.json / settings MCP servers
// (Claude Code's --strict-mcp-config). Every bossd spawn is strict — cron,
// interactive, wake, and boss-build alike — so the curated doc rendered by
// mcpConfigJSON is the WHOLE MCP surface for every agent, never merged with
// repo-root .mcp.json. The session parameter is kept (rather than deleting the
// function and inlining true at call sites) as the named, testable seam a
// future exception would live behind; it is intentionally unused today.
func StrictManagedMcpConfigForSession(_ *models.Session) bool { return true }

// mcpServerSpec is one entry under "mcpServers" in the JSON config consumed by
// the agent (e.g. Claude Code's --mcp-config). The "boss" key chosen by
// WriteSessionMcpConfig drives the mcp__boss__* tool namespace the agent sees.
type mcpServerSpec struct {
	Command string            `json:"command,omitempty"`
	Args    []string          `json:"args,omitempty"`
	Type    string            `json:"type,omitempty"`
	URL     string            `json:"url,omitempty"`
	Headers map[string]string `json:"headers,omitempty"`
}

type mcpConfigDoc struct {
	MCPServers map[string]mcpServerSpec `json:"mcpServers"`
}

// maxConfigBaseBytes leaves room for the .json suffix and atomicWriteFile's
// temporary suffix on filesystems with a 255-byte filename limit.
const maxConfigBaseBytes = 200

// mcpConfigJSON renders the MCP-server config every agent spawn loads. Since
// StrictManagedMcpConfigForSession is unconditional, this doc IS the whole MCP surface
// for every spawn (interactive, wake, cron, boss-build alike) — there is no
// merge with repo-root .mcp.json, so nothing may be curated-in only for some
// spawns. It renders up to three servers, shape-matched to repo-root .mcp.json,
// minus everything in disabledServers — which since BOS-827 contains "boss" by
// default, so an ordinary spawn receives the two HTTP remotes and no
// mcp__boss__* namespace:
//
//   - "boss" — the stdio server: launches the trusted `mcp` binary, which
//     speaks Connect-RPC to bossd over the Unix socket, keyed "boss" so tools
//     surface as mcp__boss__*.
//   - "bossanova-linear" — HTTP; the Authorization header references
//     ${LINEAR_API_KEY} by name — the literal is written verbatim and the real
//     key is NEVER read or inlined here.
//   - "bossanova-sentry" — HTTP; unauthenticated, so it carries no headers key
//     at all.
func mcpConfigJSON(mcpBin, socket string, onlyTools, disabledServers []string) ([]byte, error) {
	var args []string
	if socket != "" {
		args = []string{"--socket", socket}
	}
	// managed_mcp_tools narrows the boss server's advertised tool surface for
	// every spawn. The boss server registers 55 tools, each carrying its full
	// JSON schema and description into the agent's context on every turn, and a
	// given run uses a fraction of them. Empty (the default) passes no --only and
	// keeps the whole surface, so behaviour is unchanged until an operator opts
	// in. Only the boss server is filterable: the other two are third-party HTTP
	// servers whose tool lists we do not control — scoping those means omitting
	// the server, not trimming it.
	if len(onlyTools) > 0 {
		args = append(args, "--only", strings.Join(onlyTools, ","))
	}
	servers := map[string]mcpServerSpec{
		"boss": {Command: mcpBin, Args: args},
		"bossanova-linear": {
			Type:    "http",
			URL:     "https://mcp.linear.app/mcp",
			Headers: map[string]string{"Authorization": "Bearer ${LINEAR_API_KEY}"},
		},
		"bossanova-sentry": {
			Type: "http",
			URL:  "https://mcp.sentry.dev/mcp",
		},
	}
	for _, name := range disabledServers {
		delete(servers, name)
	}
	if len(servers) == 0 {
		// Every server disabled. Returning an empty mcpServers doc would still
		// produce a config path, and the caller treats a non-empty path as "MCP
		// is wired" — including when deciding whether to advertise mcp__boss__*.
		// Signal "write nothing" instead so the spawn omits --mcp-config
		// entirely, which is the same shape as an unresolvable mcp binary.
		return nil, nil
	}
	doc := mcpConfigDoc{MCPServers: servers}
	return json.MarshalIndent(doc, "", "  ")
}

// WriteSessionMcpConfig writes a per-spawn MCP config for the chat identified by
// f.AgentSessionID into the boss app-data dir (NEVER the worktree) and returns
// its absolute path. It returns "" (no error) when no trusted mcp binary was
// resolved (f.McpBin == ""), the agent cannot consume an MCP config, or every
// managed server is disabled — in each case the chat launches without MCP
// wiring. The file is overwritten on every supported spawn (keyed by
// agent-session id).
func WriteSessionMcpConfig(f SessionFacts) (string, error) {
	// OpenCode has no MCP-config launch option. Withholding the config also
	// prevents BuildAppendSystemPrompt from advertising mcp__boss__* tools that
	// the launched chat cannot use.
	if f.McpBin == "" || f.Agent == "opencode" {
		return "", nil
	}
	raw, err := mcpConfigJSON(f.McpBin, f.Socket, managedMcpTools(), effectiveDisabledManagedMcpServers())
	if err != nil {
		return "", fmt.Errorf("render mcp config: %w", err)
	}
	if raw == nil {
		// Every managed server is disabled — write no file, so the spawn omits
		// --mcp-config and the prompt does not advertise mcp__boss__*.
		return "", nil
	}
	dir, err := mcpConfigDir()
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("create mcp-config dir: %w", err)
	}
	path := filepath.Join(dir, safeConfigBase(f.AgentSessionID)+".json")
	if err := atomicWriteFile(path, raw, 0o600); err != nil {
		return "", fmt.Errorf("write mcp config: %w", err)
	}
	return path, nil
}

// safeConfigBase derives a single, path-safe filename component from an
// untrusted agent-session id. Path separators and any other byte outside
// [A-Za-z0-9._-] are replaced with '_', so a hostile id (for example one
// containing ".." or "/" recorded via RecordChat, which only rejects empty
// ids) cannot make filepath.Join escape the mcp-configs dir and overwrite, say,
// <app_data_dir>/settings.json. When sanitizing leaves nothing usable (empty,
// "." or ".."), a deterministic sha256-derived name is used instead so the
// per-chat file mapping stays stable across spawns. When sanitization changes
// an otherwise usable ID, append a digest so distinct IDs such as "a/b" and
// "a?b" cannot share a credential-bearing managed config home.
func safeConfigBase(id string) string {
	var b strings.Builder
	for _, r := range id {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9',
			r == '-', r == '_', r == '.':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	cleaned := b.String()
	if cleaned == "" || cleaned == "." || cleaned == ".." {
		sum := sha256.Sum256([]byte(id))
		return hex.EncodeToString(sum[:])
	}
	if cleaned != id || len(cleaned) > maxConfigBaseBytes {
		sum := sha256.Sum256([]byte(id))
		suffix := "-" + hex.EncodeToString(sum[:8])
		if len(cleaned) > maxConfigBaseBytes-len(suffix) {
			cleaned = cleaned[:maxConfigBaseBytes-len(suffix)]
		}
		return cleaned + suffix
	}
	return cleaned
}

// atomicWriteFile writes data to a temp file in the same directory and renames
// it over path, so a concurrent reader (e.g. an agent starting up and reading
// its --mcp-config) never observes a half-written, invalid-JSON file. The temp
// file is cleaned up on any error before the rename.
func atomicWriteFile(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, filepath.Base(path)+".*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	cleanup := func() { _ = os.Remove(tmpName) }
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		cleanup()
		return err
	}
	if err := tmp.Chmod(perm); err != nil {
		_ = tmp.Close()
		cleanup()
		return err
	}
	if err := tmp.Close(); err != nil {
		cleanup()
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		cleanup()
		return err
	}
	return nil
}

// managedMcpTools returns the operator-configured boss tool allowlist, or nil
// for "advertise every tool". A settings file that fails to load yields nil, so
// a config problem degrades to the full surface rather than to a spawn whose
// boss server advertises nothing.
func managedMcpTools() []string {
	s, err := config.Load()
	if err != nil {
		return nil
	}
	return s.ManagedMcpTools
}

// defaultDisabledManagedMcpServers is what bossd omits from a spawn when the
// operator has expressed no preference — i.e. when disabled_managed_mcp_servers
// is absent from settings.json, which is every install that has not opted out.
//
// The boss server alone costs ~20k tokens of mcp__boss__* tool schemas re-sent
// on EVERY turn, and since BOS-825 the boss skills prefer the boss CLI for the
// same operations, so those tokens buy a redundant transport. BOS-827 therefore
// flipped it off by default, for every spawn class — interactive, wake, cron,
// tmux_unattended, --detach and quick-chat alike.
//
// The two third-party HTTP servers stay wired: bossd does not control their
// tool lists and boss-build genuinely uses Linear, so scoping those is separate
// work.
//
// It is a function rather than a package var so a caller cannot mutate the
// default out from under the next spawn.
func defaultDisabledManagedMcpServers() []string { return []string{"boss"} }

// effectiveDisabledManagedMcpServers resolves the set of managed MCP servers a
// spawn omits. It is the ONE place that answer is derived: both mcpConfigJSON
// (which writes the config) and IsManagedMcpServerDisabled (which the
// system-prompt builder reads) go through it, so the prompt cannot advertise a
// namespace the config withholds.
//
// It distinguishes four cases, and the first two are easy to conflate:
//
//   - settings failed to load — fall back to the HISTORICAL FULL SURFACE (omit
//     nothing, boss included). A config problem must not silently strip an
//     agent's tools; that fail-open predates BOS-827 and is deliberately
//     preserved rather than folded into the new default.
//   - key absent (nil) — apply defaultDisabledManagedMcpServers.
//   - explicitly empty ([]) — omit nothing: the operator's settings-only
//     rollback to the fully-wired surface.
//   - populated — omit exactly those names, verbatim. The default does not
//     merge in, so an operator naming only "bossanova-sentry" gets boss wired.
func effectiveDisabledManagedMcpServers() []string {
	s, err := config.Load()
	if err != nil {
		return nil
	}
	if s.DisabledManagedMcpServers == nil {
		return defaultDisabledManagedMcpServers()
	}
	return *s.DisabledManagedMcpServers
}

// IsManagedMcpServerDisabled reports whether name is omitted from the managed
// MCP config bossd writes for every spawn.
//
// It is exported because the system-prompt builder must agree with the config
// writer about whether the boss server is present: the prompt advertises
// mcp__boss__* only when the agent will actually receive those tools, and
// before this setting existed "a config was written" was a sufficient proxy for
// that. It no longer is — a config can now carry Linear while omitting boss —
// so both sides read this one function rather than re-deriving the answer.
//
// Since BOS-827 that agreement also covers the DEFAULT, not just an explicit
// setting: with the key absent this reports true for "boss", so an ordinary
// spawn is told nothing about a tool namespace it will not receive.
func IsManagedMcpServerDisabled(name string) bool {
	return slices.Contains(effectiveDisabledManagedMcpServers(), name)
}

// mcpConfigDir resolves <appDataDir>/mcp-configs, honoring a configured
// app_data_dir override and falling back to the platform default. It never
// returns a worktree path.
func mcpConfigDir() (string, error) {
	if s, err := config.Load(); err == nil {
		if dir, ok, derr := config.ConfiguredAppDataDir(s); derr == nil && ok {
			return filepath.Join(dir, "mcp-configs"), nil
		}
	}
	dir, err := config.DefaultAppDataDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "mcp-configs"), nil
}
