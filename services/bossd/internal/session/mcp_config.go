package session

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/recurser/bossalib/config"
	"github.com/recurser/bossalib/models"
)

// StrictMcpConfigForSession reports whether the agent should load ONLY the
// curated --mcp-config and ignore project .mcp.json / settings MCP servers
// (Claude Code's --strict-mcp-config). Every bossd spawn is strict — cron,
// interactive, wake, and boss-build alike — so the curated doc rendered by
// mcpConfigJSON is the WHOLE MCP surface for every agent, never merged with
// repo-root .mcp.json. The session parameter is kept (rather than deleting the
// function and inlining true at call sites) as the named, testable seam a
// future exception would live behind; it is intentionally unused today.
func StrictMcpConfigForSession(_ *models.Session) bool { return true }

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

// mcpConfigJSON renders the MCP-server config every agent spawn loads. Since
// StrictMcpConfigForSession is unconditional, this doc IS the whole MCP surface
// for every spawn (interactive, wake, cron, boss-build alike) — there is no
// merge with repo-root .mcp.json, so nothing may be curated-in only for some
// spawns. It always emits three servers, shape-matched to repo-root .mcp.json:
//
//   - "boss" — the stdio server: launches the trusted `mcp` binary, which
//     speaks Connect-RPC to bossd over the Unix socket, keyed "boss" so tools
//     surface as mcp__boss__*.
//   - "bossanova-linear" — HTTP; the Authorization header references
//     ${LINEAR_API_KEY} by name — the literal is written verbatim and the real
//     key is NEVER read or inlined here.
//   - "bossanova-sentry" — HTTP; unauthenticated, so it carries no headers key
//     at all.
func mcpConfigJSON(mcpBin, socket string) ([]byte, error) {
	var args []string
	if socket != "" {
		args = []string{"--socket", socket}
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
	doc := mcpConfigDoc{MCPServers: servers}
	return json.MarshalIndent(doc, "", "  ")
}

// WriteSessionMcpConfig writes a per-spawn MCP config for the chat identified by
// f.AgentSessionID into the boss app-data dir (NEVER the worktree) and returns
// its absolute path. It returns "" (no error) when no trusted mcp binary was
// resolved (f.McpBin == ""), so the chat launches without boss MCP wiring. The
// file is overwritten on every spawn (keyed by agent-session id).
func WriteSessionMcpConfig(f SessionFacts) (string, error) {
	if f.McpBin == "" {
		return "", nil
	}
	dir, err := mcpConfigDir()
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("create mcp-config dir: %w", err)
	}
	raw, err := mcpConfigJSON(f.McpBin, f.Socket)
	if err != nil {
		return "", fmt.Errorf("render mcp config: %w", err)
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
// per-chat file mapping stays stable across spawns.
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
