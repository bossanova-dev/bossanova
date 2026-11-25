package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"unicode"
	"unicode/utf8"
)

const (
	// trackerMCPConfigOptionKey carries the written tracker --mcp-config path
	// through Runner.StartWithOptions to buildArgv for untrusted launches.
	trackerMCPConfigOptionKey = "tracker_mcp_config"

	// injectedTrackerSourceDetail describes the boss-owned tracker server in
	// DescribeMCPSurface attribution for untrusted repositories.
	injectedTrackerSourceDetail = "Bossanova tracker config (untrusted repository)"

	defaultLinearTrackerServer = "linear"
	linearMCPURL               = "https://mcp.linear.app/mcp"
	linearMCPAuthHeader        = "Bearer ${LINEAR_API_KEY}"
	bossSkillsConfigFile       = ".boss-skills.json"
	maxSkillsConfigBytes       = 1 << 20
	maxTrackerServerNameBytes  = 128
)

// trackerMCPDefaultDir resolves the default cache directory for boss-owned
// tracker MCP config files. Tests override this in TestMain so no test reaches
// the user's real cache directory.
var trackerMCPDefaultDir = func() (string, error) {
	base, err := os.UserCacheDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(base, "bossanova", "claude-tracker-mcp"), nil
}

type trackerMCPServerConfig struct {
	Type    string            `json:"type"`
	URL     string            `json:"url"`
	Headers map[string]string `json:"headers"`
}

type trackerMCPConfigDoc struct {
	MCPServers map[string]trackerMCPServerConfig `json:"mcpServers"`
}

// untrustedTrackerServer determines whether an untrusted worktree should
// receive the boss-owned Linear MCP server and what server name to register it
// under. Absent, non-regular, oversized, or unparseable .boss-skills.json files
// fall back to the default Linear server name.
func untrustedTrackerServer(workDir string) (name string, ok bool) {
	if strings.TrimSpace(workDir) == "" {
		return defaultLinearTrackerServer, true
	}
	raw, ok := readBoundedRegularFile(filepath.Join(workDir, bossSkillsConfigFile), maxSkillsConfigBytes)
	if !ok {
		return defaultLinearTrackerServer, true
	}

	var doc struct {
		Adapters      json.RawMessage `json:"adapters"`
		TrackerConfig json.RawMessage `json:"trackerConfig"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return defaultLinearTrackerServer, true
	}

	if tracker, hasTracker := decodeAdapterTracker(doc.Adapters); hasTracker && tracker != defaultLinearTrackerServer {
		return "", false
	}

	if server := decodeLinearMCPServer(doc.TrackerConfig); validTrackerServerName(server) {
		return server, true
	}
	return defaultLinearTrackerServer, true
}

func readBoundedRegularFile(path string, maxBytes int64) ([]byte, bool) {
	linfo, err := os.Lstat(path)
	if err != nil || !linfo.Mode().IsRegular() {
		return nil, false
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, false
	}
	defer func() { _ = f.Close() }()

	finfo, err := f.Stat()
	if err != nil || !finfo.Mode().IsRegular() || !os.SameFile(linfo, finfo) {
		return nil, false
	}
	raw, err := io.ReadAll(io.LimitReader(f, maxBytes+1))
	if err != nil || int64(len(raw)) > maxBytes {
		return nil, false
	}
	return raw, true
}

func decodeAdapterTracker(raw json.RawMessage) (string, bool) {
	if len(raw) == 0 {
		return "", false
	}
	var adapters struct {
		Tracker json.RawMessage `json:"tracker"`
	}
	if err := json.Unmarshal(raw, &adapters); err != nil || len(adapters.Tracker) == 0 {
		return "", false
	}
	var tracker string
	if err := json.Unmarshal(adapters.Tracker, &tracker); err != nil || tracker == "" {
		return "", false
	}
	return tracker, true
}

func decodeLinearMCPServer(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var tc struct {
		Linear json.RawMessage `json:"linear"`
	}
	if err := json.Unmarshal(raw, &tc); err != nil || len(tc.Linear) == 0 {
		return ""
	}
	var linear struct {
		MCPServer json.RawMessage `json:"mcpServer"`
	}
	if err := json.Unmarshal(tc.Linear, &linear); err != nil || len(linear.MCPServer) == 0 {
		return ""
	}
	var server string
	if err := json.Unmarshal(linear.MCPServer, &server); err != nil {
		return ""
	}
	return server
}

func validTrackerServerName(name string) bool {
	if name == "" || len(name) > maxTrackerServerNameBytes || !utf8.ValidString(name) {
		return false
	}
	for _, r := range name {
		if unicode.IsControl(r) {
			return false
		}
	}
	return true
}

// trackerMCPConfigJSON renders the single-server Linear MCP config document
// using encoding/json. It contains only the constant Linear HTTP endpoint and
// the literal ${LINEAR_API_KEY} placeholder.
func trackerMCPConfigJSON(name string) []byte {
	doc := trackerMCPConfigDoc{
		MCPServers: map[string]trackerMCPServerConfig{
			name: {
				Type: "http",
				URL:  linearMCPURL,
				Headers: map[string]string{
					"Authorization": linearMCPAuthHeader,
				},
			},
		},
	}
	raw, err := json.Marshal(doc)
	if err != nil {
		panic(fmt.Sprintf("marshal tracker MCP config: %v", err))
	}
	return raw
}

// writeTrackerMCPConfig writes doc to a content-addressed 0600 file inside dir
// (created with mode 0700), reusing an existing regular 0600 file with matching
// bytes and otherwise replacing atomically via a temp file rename.
func writeTrackerMCPConfig(dir string, doc []byte) (string, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	sum := sha256.Sum256(doc)
	target := filepath.Join(dir, hex.EncodeToString(sum[:8])+".json")

	if info, err := os.Lstat(target); err == nil && info.Mode().IsRegular() && info.Mode().Perm() == 0o600 && info.Size() == int64(len(doc)) {
		if existing, readErr := os.ReadFile(target); readErr == nil && bytes.Equal(existing, doc) {
			return target, nil
		}
	}

	tmp, err := os.CreateTemp(dir, ".tracker-mcp-*.tmp")
	if err != nil {
		return "", err
	}
	tmpPath := tmp.Name()
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.Remove(tmpPath)
		}
	}()

	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return "", err
	}
	if _, err := tmp.Write(doc); err != nil {
		_ = tmp.Close()
		return "", err
	}
	if err := tmp.Close(); err != nil {
		return "", err
	}
	if err := os.Rename(tmpPath, target); err != nil {
		return "", err
	}
	cleanup = false
	return target, nil
}

// resolveTrackerMCP resolves the tracker MCP server for a launch, writes its
// content-addressed config file when needed, and returns the CLI flags and
// server name. Trusted launches and non-Linear repos return nil, "", nil.
func resolveTrackerMCP(dir, workDir string, trusted bool) (args []string, serverName string, err error) {
	if trusted {
		return nil, "", nil
	}
	name, ok := untrustedTrackerServer(workDir)
	if !ok {
		return nil, "", nil
	}
	if dir == "" {
		dir, err = trackerMCPDefaultDir()
		if err != nil {
			return nil, "", err
		}
	}
	path, err := writeTrackerMCPConfig(dir, trackerMCPConfigJSON(name))
	if err != nil {
		return nil, "", err
	}
	return []string{"--mcp-config", path}, name, nil
}

// trackerMCPArgs returns the --mcp-config argv pair for an untrusted Linear
// launch, or nil when no tracker config should be injected.
func trackerMCPArgs(dir, workDir string, trusted bool) ([]string, error) {
	args, _, err := resolveTrackerMCP(dir, workDir, trusted)
	return args, err
}
