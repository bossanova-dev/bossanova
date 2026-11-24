package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	bossanovav1 "github.com/recurser/bossalib/gen/bossanova/v1"
)

// agyMCPSurfaceSource is the provenance line the human output prints, so an
// operator can reproduce the probe by hand.
const agyMCPSurfaceSource = "agy mcp list"

// agyMCPSurfaceProbeTimeout bounds `agy mcp list` strictly below bossd's 30s
// defaultPluginRPCTimeout with at least 10s of margin so a slow or hung probe
// returns an in-band probe_error rather than a host DeadlineExceeded.
const agyMCPSurfaceProbeTimeout = 15 * time.Second

// mcpToolNameLimit bounds how many cached tool names one server contributes to
// a response. Past the cap, is_truncated is set so callers never mistake a
// clipped list for the full inventory.
const mcpToolNameLimit = 200

type agyMCPServerRow struct {
	name      string
	transport string
	status    string
	command   string
}

// DescribeMCPSurface reports the MCP servers an Antigravity session in work_dir
// resolves under the chat's own environment (including its materialized HOME
// and BOSS_REPO_ID).
//
// agy's stream-json init event emits only its generic MCP dispatcher tools
// (`call_mcp_tool`, `list_resources`, `read_resource`), never the server list,
// and `/mcp` is unavailable in print mode. `agy mcp list` reads the active
// configuration under the chat's HOME non-interactively, and cached lazy tool
// schemas live under `$HOME/.gemini/antigravity-cli/mcp/<server>/*.json`.
//
// A probe that ran and failed is returned in-band as probe_error with servers
// empty, never as a gRPC error and never as "no servers configured".
func (s *Server) DescribeMCPSurface(
	ctx context.Context,
	req *bossanovav1.DescribeMCPSurfaceRequest,
) (*bossanovav1.DescribeMCPSurfaceResponse, error) {
	resp := &bossanovav1.DescribeMCPSurfaceResponse{
		SourceLabel: agyMCPSurfaceSource,
		Servers:     []*bossanovav1.MCPServerReport{},
		ProbedAt:    timestamppb.Now(),
	}

	if err := requireTrustedProject(req.GetProjectTrust()); err != nil {
		resp.ProbeError = err.Error()
		return resp, nil
	}

	probeCtx, cancel := context.WithTimeout(ctx, agyMCPSurfaceProbeTimeout)
	defer cancel()

	workDir := strings.TrimSpace(req.GetWorkDir())
	if workDir == "" {
		workDir = os.TempDir()
	}

	// probe_env is secret-bearing: it reaches the probed process's environment
	// and is deliberately never logged here and never copied into the response.
	out, err := s.runner.runAgy(probeCtx, workDir, []string{"mcp", "list"}, "", req.GetProbeEnv())
	if err != nil {
		resp.ProbeError = err.Error()
		return resp, nil
	}

	rows, err := parseAgyMCPList(out)
	if err != nil {
		resp.ProbeError = err.Error()
		return resp, nil
	}

	repoID := strings.TrimSpace(req.GetProbeEnv()["BOSS_REPO_ID"])
	home := probeHomeDir(req.GetProbeEnv())
	attribution := newAgySourceAttribution(req.GetWorkDir(), home)

	for _, row := range rows {
		// Disabled entries in mcp_config.json are inactive for this session;
		// omitting them matches Claude/Codex and prevents callers such as
		// trackerMcpPreflight from mistaking a disabled server for one that is
		// declared-and-enabled but unreachable.
		if strings.EqualFold(row.status, "disabled") {
			continue
		}
		if repoID != "" {
			if scopedIDs, scoped := scopedRepoIDsFromCommand(row.command); scoped && !slices.Contains(scopedIDs, repoID) {
				continue
			}
		}
		resp.Servers = append(resp.Servers, agyServerReport(row, home, attribution))
	}
	return resp, nil
}

func agyServerReport(row agyMCPServerRow, home string, attribution agySourceAttribution) *bossanovav1.MCPServerReport {
	var tools []string
	var total int
	var truncated bool
	if strings.EqualFold(row.status, "enabled") {
		tools, total, truncated = readCachedMCPTools(home, row.name)
	}
	source, detail := attribution.attribute(row.name)
	return &bossanovav1.MCPServerReport{
		Name:           row.name,
		IsDeclared:     true,
		ToolCount:      int32(total),
		ToolNamePrefix: "",
		ToolNames:      tools,
		IsTruncated:    truncated,
		AuthStatus:     row.status,
		Source:         source,
		SourceDetail:   detail,
	}
}

// parseAgyMCPList parses the tabular stdout of `agy mcp list`.
// Observed formats:
//   - "No MCP servers configured."
//   - Header "NAME   TYPE   STATUS   COMMAND/URL" followed by whitespace-column rows.
func parseAgyMCPList(out string) ([]agyMCPServerRow, error) {
	lines := strings.Split(strings.ReplaceAll(out, "\r\n", "\n"), "\n")
	nonEmpty := make([]string, 0, len(lines))
	for _, line := range lines {
		if trimmed := strings.TrimSpace(line); trimmed != "" {
			nonEmpty = append(nonEmpty, trimmed)
		}
	}
	if len(nonEmpty) == 0 {
		return nil, nil
	}
	if len(nonEmpty) == 1 && strings.EqualFold(nonEmpty[0], "No MCP servers configured.") {
		return nil, nil
	}

	headerIdx := -1
	for i, line := range nonEmpty {
		fields := strings.Fields(line)
		if len(fields) >= 3 &&
			strings.EqualFold(fields[0], "NAME") &&
			strings.EqualFold(fields[1], "TYPE") &&
			strings.EqualFold(fields[2], "STATUS") {
			headerIdx = i
			break
		}
	}
	if headerIdx < 0 {
		return nil, fmt.Errorf("agy mcp list output had no NAME/TYPE/STATUS header — format not recognized")
	}

	rows := make([]agyMCPServerRow, 0, len(nonEmpty)-headerIdx-1)
	for _, line := range nonEmpty[headerIdx+1:] {
		row, ok := parseAgyMCPTableRow(line)
		if !ok {
			continue
		}
		rows = append(rows, row)
	}
	return rows, nil
}

func parseAgyMCPTableRow(line string) (agyMCPServerRow, bool) {
	name, rest, ok := cutWhitespaceField(line)
	if !ok {
		return agyMCPServerRow{}, false
	}
	transport, rest, ok := cutWhitespaceField(rest)
	if !ok {
		return agyMCPServerRow{}, false
	}
	status, rest, ok := cutWhitespaceField(rest)
	if !ok {
		return agyMCPServerRow{}, false
	}
	return agyMCPServerRow{
		name:      name,
		transport: transport,
		status:    status,
		command:   strings.TrimSpace(rest),
	}, true
}

func cutWhitespaceField(s string) (field, rest string, ok bool) {
	s = strings.TrimLeft(s, " \t")
	if s == "" {
		return "", "", false
	}
	idx := strings.IndexAny(s, " \t")
	if idx < 0 {
		return s, "", true
	}
	return s[:idx], s[idx:], true
}

// scopedRepoIDsFromCommand extracts repo IDs from bossd's materialized repo
// scoping shell wrappers:
//   - current (#4621+): `case "$BOSS_REPO_ID" in 'id1'|'id2') ...;; *) exit 0;; esac`
//   - legacy (pre-#4621): `[ -z "$BOSS_REPO_ID" ] || [ "$BOSS_REPO_ID" = <id> ] || exit 0; ...`
//
// Returns (nil, false) for any non-bossd command so user-configured servers are
// never filtered out.
func scopedRepoIDsFromCommand(cmd string) ([]string, bool) {
	if ids, ok := scopedRepoIDsFromCaseWrapper(cmd); ok {
		return ids, true
	}
	return scopedRepoIDFromLegacyTestWrapper(cmd)
}

func scopedRepoIDsFromCaseWrapper(cmd string) ([]string, bool) {
	const prefix = `case "$BOSS_REPO_ID" in `
	const suffix = `*) exit 0;; esac`
	start := strings.Index(cmd, prefix)
	if start < 0 {
		return nil, false
	}
	body := cmd[start+len(prefix):]
	end := strings.Index(body, suffix)
	if end < 0 {
		return nil, false
	}
	armsSection := strings.TrimSpace(body[:end])
	if armsSection == "" {
		return nil, false
	}
	var ids []string
	for _, clause := range strings.Split(armsSection, ";;") {
		clause = strings.TrimSpace(clause)
		if clause == "" {
			continue
		}
		pattern, _, ok := strings.Cut(clause, ")")
		if !ok {
			continue
		}
		for _, part := range strings.Split(pattern, "|") {
			if id, ok := unquoteSingleShellWord(strings.TrimSpace(part)); ok && id != "" {
				ids = append(ids, id)
			}
		}
	}
	if len(ids) == 0 {
		return nil, false
	}
	return ids, true
}

func scopedRepoIDFromLegacyTestWrapper(cmd string) ([]string, bool) {
	const marker = `[ "$BOSS_REPO_ID" = `
	start := strings.Index(cmd, marker)
	if start < 0 {
		return nil, false
	}
	rest := cmd[start+len(marker):]
	rawID, after, ok := strings.Cut(rest, " ]")
	if !ok {
		return nil, false
	}
	if !strings.HasPrefix(strings.TrimSpace(after), "|| exit 0") {
		return nil, false
	}
	id := unquoteShellWord(strings.TrimSpace(rawID))
	if id == "" || strings.ContainsAny(id, " \t\n\r") {
		return nil, false
	}
	return []string{id}, true
}

func unquoteShellWord(s string) string {
	if id, ok := unquoteSingleShellWord(s); ok {
		return id
	}
	if len(s) >= 2 && s[0] == '"' && s[len(s)-1] == '"' {
		return s[1 : len(s)-1]
	}
	return s
}

func unquoteSingleShellWord(s string) (string, bool) {
	if len(s) < 2 || s[0] != '\'' || s[len(s)-1] != '\'' {
		return "", false
	}
	inner := s[1 : len(s)-1]
	return strings.ReplaceAll(inner, `'\''`, `'`), true
}

func probeHomeDir(probeEnv map[string]string) string {
	if home := strings.TrimSpace(probeEnv["HOME"]); home != "" {
		return home
	}
	home, _ := os.UserHomeDir()
	return strings.TrimSpace(home)
}

func isSafeServerDirSegment(name string) bool {
	if name == "" || name == "." || name == ".." {
		return false
	}
	return !strings.ContainsAny(name, `/\`)
}

// readCachedMCPTools reads the lazy tool schema files agy writes under
// `$HOME/.gemini/antigravity-cli/mcp/<server>/*.json`. Non-JSON files (such as
// instructions.md) are ignored. Missing directories return empty without error.
func readCachedMCPTools(home, serverName string) (tools []string, total int, truncated bool) {
	if home == "" || !isSafeServerDirSegment(serverName) {
		return nil, 0, false
	}
	dir := filepath.Join(home, ".gemini", "antigravity-cli", "mcp", serverName)
	entries, err := os.ReadDir(filepath.Clean(dir))
	if err != nil {
		return nil, 0, false
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		base := entry.Name()
		if !strings.HasSuffix(base, ".json") {
			continue
		}
		tool := strings.TrimSuffix(base, ".json")
		if tool != "" {
			names = append(names, tool)
		}
	}
	sort.Strings(names)
	total = len(names)
	if total > mcpToolNameLimit {
		names = names[:mcpToolNameLimit]
		truncated = true
	}
	return names, total, truncated
}

type agySourceAttribution struct {
	repoKeys   map[string]struct{}
	userKeys   map[string]struct{}
	userDetail string
}

func newAgySourceAttribution(workDir, home string) agySourceAttribution {
	var attr agySourceAttribution
	if strings.TrimSpace(workDir) != "" {
		attr.repoKeys = readMCPServersJSONKeys(filepath.Join(workDir, ".mcp.json"))
	}
	if home != "" {
		userPath := filepath.Join(home, ".gemini", "config", "mcp_config.json")
		if keys := readMCPServersJSONKeys(userPath); len(keys) > 0 {
			attr.userKeys = keys
			attr.userDetail = userPath
		}
	}
	return attr
}

func (a agySourceAttribution) attribute(name string) (bossanovav1.MCPServerSource, string) {
	if _, ok := a.repoKeys[name]; ok {
		return bossanovav1.MCPServerSource_MCP_SERVER_SOURCE_REPO_FILE, ".mcp.json"
	}
	if _, ok := a.userKeys[name]; ok {
		return bossanovav1.MCPServerSource_MCP_SERVER_SOURCE_USER_CONFIG, a.userDetail
	}
	return bossanovav1.MCPServerSource_MCP_SERVER_SOURCE_UNKNOWN, ""
}

func readMCPServersJSONKeys(path string) map[string]struct{} {
	var doc struct {
		MCPServers map[string]json.RawMessage `json:"mcpServers"`
	}
	if !readJSON(path, &doc) || len(doc.MCPServers) == 0 {
		return nil
	}
	keys := make(map[string]struct{}, len(doc.MCPServers))
	for k := range doc.MCPServers {
		keys[k] = struct{}{}
	}
	return keys
}
