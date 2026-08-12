package main

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"
	"google.golang.org/protobuf/types/known/timestamppb"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

func mcpTestCommand() (*cobra.Command, *bytes.Buffer) {
	out := &bytes.Buffer{}
	cmd := &cobra.Command{Use: "mcp"}
	cmd.SetOut(out)
	cmd.Flags().Bool(jsonFlagName, false, "")
	cmd.Flags().Bool("tools", false, "")
	return cmd, out
}

// mixedSurfaceResponse carries the two states the ticket exists to separate: a
// healthy server with tools, and a server the harness declared that exposed
// nothing. A server that was never declared is represented by its ABSENCE.
func mixedSurfaceResponse() *pb.DescribeChatMCPResponse {
	return &pb.DescribeChatMCPResponse{
		AgentName:    "agent-under-test",
		WorktreePath: "/work/tree",
		IsSupported:  true,
		SourceLabel:  "the agent's own runtime registry",
		ProbedAt:     timestamppb.New(time.Date(2026, 8, 12, 10, 0, 0, 0, time.UTC)),
		Servers: []*pb.MCPServerReport{
			{
				Name:           "bossanova-linear",
				IsDeclared:     true,
				ToolCount:      3,
				ToolNamePrefix: "mcp__bossanova-linear__",
				ToolNames:      []string{"get_issue", "list_issues", "save_issue"},
				AuthStatus:     "oauth",
				Source:         pb.MCPServerSource_MCP_SERVER_SOURCE_REPO_FILE,
				SourceDetail:   ".mcp.json",
			},
			{
				Name:           "bossanova-sentry",
				IsDeclared:     true,
				ToolCount:      0,
				ToolNamePrefix: "mcp__bossanova-sentry__",
				AuthStatus:     "bearerToken",
				Source:         pb.MCPServerSource_MCP_SERVER_SOURCE_UNKNOWN,
			},
		},
	}
}

// TestSessionMCPJSONEnvelopeIsStable pins every field a skill parses, and the
// non-nil-slice rule that keeps "none" marshalling as [] rather than null.
func TestSessionMCPJSONEnvelopeIsStable(t *testing.T) {
	raw, err := json.Marshal(newChatMCPJSON(mixedSurfaceResponse(), false))
	if err != nil {
		t.Fatalf("marshal envelope: %v", err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("decode envelope: %v", err)
	}
	for _, key := range []string{
		"agent_name", "worktree_path", "is_supported", "unsupported_reason",
		"source", "probed_at", "probe_error", "servers",
	} {
		if _, ok := decoded[key]; !ok {
			t.Fatalf("envelope is missing %q: %s", key, raw)
		}
	}
	servers, ok := decoded["servers"].([]any)
	if !ok || len(servers) != 2 {
		t.Fatalf("servers = %v, want 2 rows", decoded["servers"])
	}
	first, _ := servers[0].(map[string]any)
	for _, key := range []string{
		"name", "is_declared", "tool_count", "tool_name_prefix",
		"auth_status", "source", "source_detail", "is_truncated",
	} {
		if _, ok := first[key]; !ok {
			t.Fatalf("server row is missing %q: %v", key, first)
		}
	}
	if first["tool_name_prefix"] != "mcp__bossanova-linear__" {
		t.Fatalf("tool_name_prefix = %v", first["tool_name_prefix"])
	}
	if first["source"] != "repo_file" {
		t.Fatalf("source = %v, want the lowercase label", first["source"])
	}

	// An empty surface must still marshal `servers` as [].
	empty, err := json.Marshal(newChatMCPJSON(&pb.DescribeChatMCPResponse{IsSupported: true}, false))
	if err != nil {
		t.Fatalf("marshal empty envelope: %v", err)
	}
	if !strings.Contains(string(empty), `"servers":[]`) {
		t.Fatalf("empty envelope = %s, want servers:[]", empty)
	}
}

// TestSessionMCPToolsFlagGatesToolNames pins that tool_names is opt-in: a
// 60-tool server would otherwise drown the envelope a skill parses.
func TestSessionMCPToolsFlagGatesToolNames(t *testing.T) {
	without, err := json.Marshal(newChatMCPJSON(mixedSurfaceResponse(), false))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(without), "tool_names") {
		t.Fatalf("default --json must omit tool_names: %s", without)
	}
	with, err := json.Marshal(newChatMCPJSON(mixedSurfaceResponse(), true))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(with), `"tool_names":["get_issue","list_issues","save_issue"]`) {
		t.Fatalf("--tools must include tool_names: %s", with)
	}

	// The declared-but-empty server is the row this whole feature exists to
	// surface, and under --tools it must carry an EMPTY ARRAY rather than drop
	// the key: the envelope's rule is that a slice is never null or absent, and
	// a missing key makes `jq '.servers[].tool_names | length'` error on exactly
	// the rows a caller is hunting for.
	var decoded struct {
		Servers []struct {
			Name      string    `json:"name"`
			ToolNames *[]string `json:"tool_names"`
		} `json:"servers"`
	}
	if err := json.Unmarshal(with, &decoded); err != nil {
		t.Fatalf("decode --tools envelope: %v", err)
	}
	for _, server := range decoded.Servers {
		if server.ToolNames == nil {
			t.Fatalf("server %q omitted tool_names under --tools: %s", server.Name, with)
		}
	}
	if !strings.Contains(string(with), `"tool_names":[]`) {
		t.Fatalf("a declared-but-empty server must marshal tool_names as [] under --tools: %s", with)
	}
}

// TestSessionMCPDeclaredButEmptyIsDistinguishable is the BOS-804 signal, checked
// in BOTH renderings: the declared-but-empty server is present and marked, while
// a server that was never declared is absent entirely.
func TestSessionMCPDeclaredButEmptyIsDistinguishable(t *testing.T) {
	resp := mixedSurfaceResponse()

	envelope := newChatMCPJSON(resp, false)
	var empty *mcpServerJSON
	for i := range envelope.Servers {
		if envelope.Servers[i].Name == "bossanova-sentry" {
			empty = &envelope.Servers[i]
		}
		if envelope.Servers[i].Name == "never-declared" {
			t.Fatal("an undeclared server must be absent from the envelope")
		}
	}
	if empty == nil {
		t.Fatal("the declared-but-empty server is missing from the envelope")
	}
	if !empty.IsDeclared || empty.ToolCount != 0 {
		t.Fatalf("declared/count = %v/%d, want true/0", empty.IsDeclared, empty.ToolCount)
	}

	cmd, out := mcpTestCommand()
	if err := printChatMCP(cmd, resp, false); err != nil {
		t.Fatalf("printChatMCP: %v", err)
	}
	text := out.String()
	if !strings.Contains(text, "declared, 0 tools") {
		t.Fatalf("the text rendering must show the declared-but-empty STATE, not a bare 0:\n%s", text)
	}
	if strings.Contains(text, "never-declared") {
		t.Fatalf("an undeclared server leaked into the text rendering:\n%s", text)
	}
	// The provenance line comes first so an operator can re-run the probe.
	if !strings.Contains(text, "source:   the agent's own runtime registry") {
		t.Fatalf("the provenance line is missing:\n%s", text)
	}
}

// TestSessionMCPProbeFailureIsNotNoServers pins that a probe which ran and
// failed says so in both renderings, rather than reporting "no MCP servers".
func TestSessionMCPProbeFailureIsNotNoServers(t *testing.T) {
	resp := &pb.DescribeChatMCPResponse{
		AgentName:    "agent-under-test",
		WorktreePath: "/work/tree",
		IsSupported:  true,
		SourceLabel:  "the agent's own runtime registry",
		ProbeError:   "start app-server: executable not found",
	}

	cmd, out := mcpTestCommand()
	if err := printChatMCP(cmd, resp, false); err != nil {
		t.Fatalf("printChatMCP: %v", err)
	}
	text := out.String()
	if !strings.Contains(text, "FAILED") || !strings.Contains(text, "executable not found") {
		t.Fatalf("the failure must be stated explicitly:\n%s", text)
	}
	if strings.Contains(text, "resolved no MCP servers") {
		t.Fatalf("a failed probe must never render as \"no MCP servers\":\n%s", text)
	}

	envelope := newChatMCPJSON(resp, false)
	if envelope.ProbeError == "" || len(envelope.Servers) != 0 {
		t.Fatalf("envelope = %+v, want probe_error set and servers empty", envelope)
	}
}

// TestSessionMCPUnsupportedAgentExitsZero pins the degrade at the CLI edge: an
// agent that cannot answer prints an explanatory line and returns no error, so
// the command exits 0 rather than making a driver retry forever.
func TestSessionMCPUnsupportedAgentExitsZero(t *testing.T) {
	resp := &pb.DescribeChatMCPResponse{
		AgentName:         "stub-runner",
		WorktreePath:      "/work/tree",
		IsSupported:       false,
		UnsupportedReason: `the "stub-runner" agent cannot report its MCP surface`,
	}
	cmd, out := mcpTestCommand()
	if err := printChatMCP(cmd, resp, false); err != nil {
		t.Fatalf("an unsupported agent must not be an error: %v", err)
	}
	text := out.String()
	if !strings.Contains(text, "cannot report its MCP surface") {
		t.Fatalf("the reason must be printed:\n%s", text)
	}
	if strings.Contains(text, "resolved no MCP servers") {
		t.Fatalf("unsupported must never render as \"no MCP servers\":\n%s", text)
	}
}

func TestMCPSourceNameVocabularyIsClosed(t *testing.T) {
	tests := []struct {
		source pb.MCPServerSource
		want   string
	}{
		{pb.MCPServerSource_MCP_SERVER_SOURCE_UNSPECIFIED, "unknown"},
		{pb.MCPServerSource_MCP_SERVER_SOURCE_UNKNOWN, "unknown"},
		{pb.MCPServerSource_MCP_SERVER_SOURCE_REPO_FILE, "repo_file"},
		{pb.MCPServerSource_MCP_SERVER_SOURCE_USER_CONFIG, "user_config"},
		{pb.MCPServerSource_MCP_SERVER_SOURCE_INJECTED, "injected"},
	}
	for _, tc := range tests {
		if got := mcpSourceName(tc.source); got != tc.want {
			t.Fatalf("mcpSourceName(%v) = %q, want %q", tc.source, got, tc.want)
		}
	}
}

// sessionMCPSource is the command's own source text, embedded so the assertion
// below holds under both `go test` and Bazel.
//
//go:embed session_mcp.go
var sessionMCPSource string

// TestSessionMCPIsHarnessAgnostic runs BOS-867's own acceptance grep in CI: the
// CLI must name no harness, because every per-harness detail belongs behind
// AgentRunnerService.DescribeMCPSurface.
func TestSessionMCPIsHarnessAgnostic(t *testing.T) {
	for _, harness := range []string{"claude", "codex"} {
		if strings.Contains(sessionMCPSource, harness) {
			t.Fatalf("session_mcp.go names the %q harness; it must stay agent-agnostic", harness)
		}
	}
	if !strings.Contains(sessionMCPSource, "func runSessionMCP(") {
		t.Fatal("the embedded source is not session_mcp.go — the assertion above would be vacuous")
	}
}
