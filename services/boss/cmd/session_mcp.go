package main

import (
	"context"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/spf13/cobra"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

// sessionMCPTimeout is generous because the probe spawns a real agent process
// in the chat's worktree — the 10s `session checks` uses is a database read's
// budget, not a cold-starting CLI's.
const sessionMCPTimeout = 60 * time.Second

// runSessionMCP answers the question nothing in boss could answer before:
// which MCP servers does THIS chat's agent actually have, what are their tool
// counts, and where were they declared. Asking the agent itself is not an
// option (a session with 60 MCP tools loaded has been observed answering
// "NONE") and `<agent> mcp list` reports the environment IT inherits rather
// than the chat's, so this routes to the owning agent plugin instead.
func runSessionMCP(cmd *cobra.Command, chatID string) error {
	asJSON, _ := cmd.Flags().GetBool(jsonFlagName)
	withTools, _ := cmd.Flags().GetBool("tools")

	c, err := newClient(cmd)
	if err != nil {
		return emitJSONFailure(cmd, asJSON, err)
	}
	ctx, cancel := context.WithTimeout(cmd.Context(), sessionMCPTimeout)
	defer cancel()

	resp, err := c.DescribeChatMCP(ctx, chatID)
	if err != nil {
		return emitJSONFailure(cmd, asJSON, fmt.Errorf("DescribeChatMCP: %w", err))
	}
	if asJSON {
		return emitJSON(cmd, newChatMCPJSON(resp, withTools))
	}
	return printChatMCP(cmd, resp, withTools)
}

// chatMCPJSON is the `boss session mcp --json` envelope. Servers is always a
// non-nil slice so "none" marshals as `[]` rather than `null` — the same rule
// checkSnapshotListJSON documents.
type chatMCPJSON struct {
	AgentName    string `json:"agent_name"`
	WorktreePath string `json:"worktree_path"`
	// IsSupported is false when the agent has no way to answer. It is NOT an
	// error and NOT "no servers": a caller that conflates the two learns
	// nothing from an empty list.
	IsSupported       bool   `json:"is_supported"`
	UnsupportedReason string `json:"unsupported_reason"`
	// Source is the provenance line, so an operator can re-run the probe.
	Source   string `json:"source"`
	ProbedAt string `json:"probed_at"`
	// ProbeError is set when the probe RAN and FAILED. Servers is then empty
	// and must not be read as "no servers configured".
	ProbeError string          `json:"probe_error"`
	Servers    []mcpServerJSON `json:"servers"`
}

type mcpServerJSON struct {
	Name string `json:"name"`
	// IsDeclared is true whenever the harness listed the server, even with
	// zero tools. A server that was never declared is absent from Servers
	// entirely — that asymmetry is the whole signal.
	IsDeclared     bool   `json:"is_declared"`
	ToolCount      int32  `json:"tool_count"`
	ToolNamePrefix string `json:"tool_name_prefix"`
	AuthStatus     string `json:"auth_status"`
	Source         string `json:"source"`
	SourceDetail   string `json:"source_detail"`
	IsTruncated    bool   `json:"is_truncated"`
	// ToolNames is included only under --tools: a 60-tool server would drown
	// the envelope a skill parses, and tool_count + tool_name_prefix answer
	// the question on their own.
	//
	// The pointer is what makes "not requested" and "requested, none" different
	// wire shapes. `omitempty` on a plain slice collapsed them: a
	// declared-but-empty server — the exact row this feature exists to surface —
	// dropped the key entirely under --tools, so `jq '.servers[].tool_names |
	// length'` errored on it. Nil omits the key; a non-nil empty slice marshals
	// as [], honouring the envelope's own "slices are never null or absent" rule.
	ToolNames *[]string `json:"tool_names,omitempty"`
}

func newChatMCPJSON(resp *pb.DescribeChatMCPResponse, withTools bool) chatMCPJSON {
	out := chatMCPJSON{
		AgentName:         resp.GetAgentName(),
		WorktreePath:      resp.GetWorktreePath(),
		IsSupported:       resp.GetIsSupported(),
		UnsupportedReason: resp.GetUnsupportedReason(),
		Source:            resp.GetSourceLabel(),
		ProbedAt:          rfc3339OrEmpty(resp.GetProbedAt()),
		ProbeError:        resp.GetProbeError(),
		Servers:           make([]mcpServerJSON, 0, len(resp.GetServers())),
	}
	for _, server := range resp.GetServers() {
		row := mcpServerJSON{
			Name:           server.GetName(),
			IsDeclared:     server.GetIsDeclared(),
			ToolCount:      server.GetToolCount(),
			ToolNamePrefix: server.GetToolNamePrefix(),
			AuthStatus:     server.GetAuthStatus(),
			Source:         mcpSourceName(server.GetSource()),
			SourceDetail:   server.GetSourceDetail(),
			IsTruncated:    server.GetIsTruncated(),
		}
		if withTools {
			// Always non-nil, so a declared-but-empty server marshals
			// "tool_names": [] rather than dropping the key.
			names := append([]string{}, server.GetToolNames()...)
			row.ToolNames = &names
		}
		out.Servers = append(out.Servers, row)
	}
	return out
}

// mcpSourceName turns the proto enum into the lowercase label the JSON
// envelope documents (repo_file / user_config / injected / unknown).
func mcpSourceName(source pb.MCPServerSource) string {
	name := strings.TrimPrefix(source.String(), "MCP_SERVER_SOURCE_")
	name = strings.ToLower(name)
	if name == "unspecified" {
		// A plugin that left source unset is saying the same thing an explicit
		// UNKNOWN says; collapsing them keeps the vocabulary the criterion
		// names (repo_file / user_config / injected / unknown) closed.
		return "unknown"
	}
	return name
}

// printChatMCP renders the human view. The provenance line comes FIRST, so an
// operator can reproduce the probe by hand before reading anything derived
// from it.
//
// Write errors are returned rather than discarded, matching emitJSON: a
// diagnostic truncated by a closed pipe must not read as a clean answer.
func printChatMCP(cmd *cobra.Command, resp *pb.DescribeChatMCPResponse, withTools bool) error {
	w := &firstErrWriter{out: cmd.OutOrStdout()}
	w.printf("agent:    %s\n", orNone(resp.GetAgentName()))
	w.printf("worktree: %s\n", orNone(resp.GetWorktreePath()))

	if !resp.GetIsSupported() {
		// Exit 0: "this agent cannot answer" is a real answer, and a non-zero
		// exit would make a driver retry something that will never succeed.
		w.printf("\nThis agent cannot report its MCP surface: %s\n", orNone(resp.GetUnsupportedReason()))
		return w.err
	}

	w.printf("source:   %s\n", orNone(resp.GetSourceLabel()))
	if probed := rfc3339OrEmpty(resp.GetProbedAt()); probed != "" {
		w.printf("probed:   %s\n", probed)
	}

	if probeErr := resp.GetProbeError(); probeErr != "" {
		// Deliberately NOT "no MCP servers": the probe never got an answer, and
		// saying otherwise is the confident-and-wrong report this command
		// exists to replace.
		w.printf("\nThe probe ran and FAILED, so this session's MCP surface is unknown\n(this is NOT the same as \"no MCP servers\"): %s\n", probeErr)
		return w.err
	}
	if len(resp.GetServers()) == 0 {
		w.printf("\nThe agent resolved no MCP servers for this worktree.\n")
		return w.err
	}

	w.printf("\n")
	for _, server := range resp.GetServers() {
		w.printf("%s\n", server.GetName())
		w.printf("  tools:  %s\n", toolCountPhrase(server))
		w.printf("  prefix: %s\n", orNone(server.GetToolNamePrefix()))
		w.printf("  auth:   %s\n", orNone(server.GetAuthStatus()))
		w.printf("  source: %s\n", sourcePhrase(server))
		if withTools && len(server.GetToolNames()) > 0 {
			w.printf("  names:  %s\n", strings.Join(server.GetToolNames(), ", "))
			if server.GetIsTruncated() {
				w.printf("          (list truncated — tool count above is exact)\n")
			}
		}
	}
	return w.err
}

// firstErrWriter keeps the rendering above readable while still propagating a
// write failure: it records the FIRST error and skips every later write, so the
// caller sees the original cause rather than a cascade.
type firstErrWriter struct {
	out io.Writer
	err error
}

func (w *firstErrWriter) printf(format string, args ...any) {
	if w.err != nil {
		return
	}
	if _, err := fmt.Fprintf(w.out, format, args...); err != nil {
		w.err = fmt.Errorf("write MCP surface output: %w", err)
	}
}

// toolCountPhrase renders the declared-but-empty state AS A STATE. A bare "0"
// reads as unremarkable; "declared, 0 tools" is the signal that separates a
// server which failed to connect from one that was never declared at all.
func toolCountPhrase(server *pb.MCPServerReport) string {
	if server.GetToolCount() == 0 && server.GetIsDeclared() {
		return "declared, 0 tools — the server is declared but exposed nothing"
	}
	return fmt.Sprintf("%d", server.GetToolCount())
}

func sourcePhrase(server *pb.MCPServerReport) string {
	name := mcpSourceName(server.GetSource())
	if detail := server.GetSourceDetail(); detail != "" {
		return name + " (" + detail + ")"
	}
	return name
}

func orNone(v string) string {
	if strings.TrimSpace(v) == "" {
		return "(none)"
	}
	return v
}
