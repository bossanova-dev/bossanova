package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"

	"google.golang.org/protobuf/types/known/timestamppb"

	bossanovav1 "github.com/recurser/bossalib/gen/bossanova/v1"
)

// mcpToolNameLimit bounds how many tool names one server contributes to a
// response. The count and the prefix are what callers actually reason about;
// the names are a convenience, and an unbounded list would let a 500-tool
// server dominate a diagnostic payload. A server past the cap reports
// is_truncated so a reader never mistakes a clipped list for the whole
// inventory.
const mcpToolNameLimit = 200

// DescribeMCPSurface reports the MCP servers this codex runtime actually
// resolves for a worktree, reusing the very registry PreflightHeadlessRun
// gates on (codexAppServerOperationRegistry) so the two can never disagree
// about what codex loaded.
//
// It never returns a gRPC error for a probe that ran and failed: that outcome
// is reported as probe_error with servers empty, because a caller must be able
// to tell "codex reports no servers" from "we could not ask codex".
//
//nolint:unparam // the error is always nil today and that IS the contract: a failed probe is reported in-band as probe_error. The signature is fixed by agentRunnerServiceHandler.
func (s *Server) DescribeMCPSurface(
	ctx context.Context,
	req *bossanovav1.DescribeMCPSurfaceRequest,
) (*bossanovav1.DescribeMCPSurfaceResponse, error) {
	// probe_env is secret-bearing: it is forwarded into the probed process's
	// environment and is deliberately never logged here and never copied into
	// the response.
	target := s.runtimeTarget(req.GetModel(), "", req.GetWorkDir(), req.GetProbeEnv())

	registry := s.operationRegistry
	if registry == nil {
		registry = codexAppServerOperationRegistry{binary: "codex", loginShell: s.runner.loginShell}
	}

	resp := &bossanovav1.DescribeMCPSurfaceResponse{
		SourceLabel: codexOperationRegistrySource,
		Servers:     []*bossanovav1.MCPServerReport{},
		ProbedAt:    timestamppb.Now(),
	}

	surface, err := registry.Operations(ctx, target)
	if err != nil {
		resp.ProbeError = err.Error()
		return resp, nil
	}
	if surface.Source != "" {
		resp.SourceLabel = surface.Source
	}

	attribution := newCodexSourceAttribution(req.GetWorkDir(), req.GetProbeEnv())
	for _, server := range surface.Servers {
		resp.Servers = append(resp.Servers, codexServerReport(server, attribution))
	}
	return resp, nil
}

// codexServerReport converts one runtime row into its wire report.
//
// The tool-name prefix is RECONSTRUCTED from the server key rather than read
// off a tool name, because codex reports BARE tool names — there is nothing to
// read a prefix from. Reconstructing it from the key is also the point: codex
// builds its callable tool names as mcp__<key>__<tool>, so a config key that
// does not match the name a skill expects becomes visible here without
// invoking a single tool.
func codexServerReport(server runtimeMCPServer, attribution codexSourceAttribution) *bossanovav1.MCPServerReport {
	names := server.Operations
	truncated := false
	if len(names) > mcpToolNameLimit {
		names = names[:mcpToolNameLimit]
		truncated = true
	}
	source, detail := attribution.attribute(server.Name)
	return &bossanovav1.MCPServerReport{
		Name:           server.Name,
		IsDeclared:     server.IsDeclared,
		ToolCount:      int32(len(server.Operations)),
		ToolNamePrefix: mcpToolNamePrefix(server.Name),
		ToolNames:      append([]string{}, names...),
		IsTruncated:    truncated,
		AuthStatus:     server.AuthStatus,
		Source:         source,
		SourceDetail:   detail,
	}
}

// mcpToolNamePrefix builds the literal prefix a skill must call for a server.
func mcpToolNamePrefix(serverName string) string {
	if serverName == "" {
		return ""
	}
	return "mcp__" + serverName + "__"
}

// codexSourceAttribution answers "which scanned config file declared this
// server?" from a bounded scan of the two locations codex itself reads: the
// worktree's own .codex/config.toml and the config.toml under the resolved
// CODEX_HOME. Repo wins over user, matching codex's own precedence. A server
// that matches neither is UNKNOWN with an empty detail — the scan never
// guesses, and never infers provenance from tool names.
type codexSourceAttribution struct {
	repoKeys   map[string]struct{}
	repoDetail string
	userKeys   map[string]struct{}
	userDetail string
}

func newCodexSourceAttribution(workDir string, probeEnv map[string]string) codexSourceAttribution {
	attribution := codexSourceAttribution{}
	if strings.TrimSpace(workDir) != "" {
		repoPath := filepath.Join(workDir, ".codex", "config.toml")
		if keys, ok := readCodexMCPServerKeys(repoPath); ok {
			attribution.repoKeys = keys
			attribution.repoDetail = filepath.Join(".codex", "config.toml")
		}
	}
	if home, err := codexConfigDirForEnv(probeEnv); err == nil && strings.TrimSpace(home) != "" {
		userPath := filepath.Join(home, "config.toml")
		if keys, ok := readCodexMCPServerKeys(userPath); ok {
			attribution.userKeys = keys
			attribution.userDetail = userPath
		}
	}
	return attribution
}

func (a codexSourceAttribution) attribute(name string) (bossanovav1.MCPServerSource, string) {
	if _, ok := a.repoKeys[name]; ok {
		return bossanovav1.MCPServerSource_MCP_SERVER_SOURCE_REPO_FILE, a.repoDetail
	}
	if _, ok := a.userKeys[name]; ok {
		return bossanovav1.MCPServerSource_MCP_SERVER_SOURCE_USER_CONFIG, a.userDetail
	}
	return bossanovav1.MCPServerSource_MCP_SERVER_SOURCE_UNKNOWN, ""
}

// readCodexMCPServerKeys collects the keys of every [mcp_servers.<key>] table
// in a codex config.toml. It is a deliberately small header scan rather than a
// full TOML parse: the only thing being asked is which server keys a file
// declares, and adding a TOML dependency to a plugin binary to answer that
// would be a worse trade. The second return reports whether the file was
// readable at all, so an absent file stays UNKNOWN instead of being reported
// as "declared nowhere" with false confidence.
func readCodexMCPServerKeys(path string) (map[string]struct{}, bool) {
	raw, err := os.ReadFile(filepath.Clean(path))
	if err != nil {
		return nil, false
	}
	keys := map[string]struct{}{}
	for _, line := range strings.Split(string(raw), "\n") {
		trimmed := strings.TrimSpace(line)
		if !strings.HasPrefix(trimmed, "[") || !strings.HasSuffix(trimmed, "]") {
			continue
		}
		header := strings.TrimSuffix(strings.TrimPrefix(trimmed, "["), "]")
		// [[mcp_servers.x]] array-of-tables is not a shape codex uses; trimming
		// the extra bracket keeps such a line from being read as a key of "[x".
		header = strings.TrimSuffix(strings.TrimPrefix(header, "["), "]")
		key, ok := strings.CutPrefix(strings.TrimSpace(header), "mcp_servers.")
		if !ok {
			continue
		}
		if key = codexTOMLKeyName(key); key != "" {
			keys[key] = struct{}{}
		}
	}
	return keys, true
}

// codexTOMLKeyName unquotes a single dotted-header segment. A quoted key is
// taken verbatim (that is how a key containing a dot or a dash is written);
// an unquoted key stops at the first dot so [mcp_servers.foo.env] attributes
// to "foo" rather than to "foo.env".
func codexTOMLKeyName(key string) string {
	key = strings.TrimSpace(key)
	if len(key) >= 2 && strings.HasPrefix(key, `"`) {
		if end := strings.Index(key[1:], `"`); end >= 0 {
			return key[1 : 1+end]
		}
		return ""
	}
	if dot := strings.Index(key, "."); dot >= 0 {
		key = key[:dot]
	}
	return strings.TrimSpace(key)
}
