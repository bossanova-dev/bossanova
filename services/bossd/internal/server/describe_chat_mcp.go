package server

import (
	"context"
	"errors"
	"fmt"

	"connectrpc.com/connect"
	"google.golang.org/grpc/codes"
	grpcstatus "google.golang.org/grpc/status"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossd/internal/agent"
)

// mcpSurfaceProber is the optional-capability seam for the DescribeMCPSurface
// RPC. It is deliberately NOT part of agent.AgentRunnerClient: a runner binary
// built before the RPC existed still satisfies that interface, and widening it
// would break every existing client and fake for a capability the daemon must
// degrade on anyway. The live implementation type-asserts instead, mirroring
// the headlessCapabilityProfilePreflightClient / progressLivenessProber
// precedents. Tests inject a fake so no real agent binary is needed.
type mcpSurfaceProber interface {
	DescribeMCPSurface(ctx context.Context, agentName string, req *pb.DescribeMCPSurfaceRequest) (*pb.DescribeMCPSurfaceResponse, error)
}

// errAgentCannotDescribeMCP marks the "this agent has no answer" outcome so
// DescribeChatMCP can report it as a typed is_supported=false rather than as an
// error. It is deliberately distinct from a probe that ran and failed.
type errAgentCannotDescribeMCP struct{ reason string }

func (e errAgentCannotDescribeMCP) Error() string { return e.reason }

// liveMCPSurfaceProber dispatches DescribeMCPSurface to the AgentRunner plugin
// matching agentName, mirroring liveTranscriptOracle's registry lookup and its
// empty-name fallback to defaultLegacyAgent for chat rows persisted before
// AgentName was tracked.
type liveMCPSurfaceProber struct {
	clients map[string]agent.AgentRunnerClient
}

func (p liveMCPSurfaceProber) DescribeMCPSurface(ctx context.Context, agentName string, req *pb.DescribeMCPSurfaceRequest) (*pb.DescribeMCPSurfaceResponse, error) {
	name := agentName
	if name == "" {
		name = defaultLegacyAgent
	}
	client, ok := p.clients[name]
	if !ok {
		return nil, errAgentCannotDescribeMCP{
			reason: fmt.Sprintf("no agent plugin named %q is loaded, so its MCP surface cannot be read", name),
		}
	}
	prober, ok := client.(interface {
		DescribeMCPSurface(context.Context, *pb.DescribeMCPSurfaceRequest) (*pb.DescribeMCPSurfaceResponse, error)
	})
	if !ok {
		return nil, errAgentCannotDescribeMCP{
			reason: fmt.Sprintf("the %q agent plugin predates DescribeMCPSurface — rebuild the plugin binaries to read its MCP surface", name),
		}
	}
	resp, err := prober.DescribeMCPSurface(ctx, req)
	if err != nil {
		if grpcstatus.Code(err) == codes.Unimplemented {
			return nil, errAgentCannotDescribeMCP{
				reason: fmt.Sprintf("the %q agent cannot report its MCP surface", name),
			}
		}
		return nil, err
	}
	return resp, nil
}

// DescribeChatMCP reports which MCP servers a chat's agent actually resolved,
// by routing to the owning plugin's DescribeMCPSurface under the chat's OWN
// re-derived environment. It answers the question nothing in boss could answer
// before: "which MCP servers does this session actually have, and where did
// they come from?"
//
// Read-only. It starts no coding turn, writes no configuration, and rebinds no
// account. It is also deliberately harness-agnostic: every per-agent detail
// lives behind the plugin RPC, so this file contains no branch on which agent
// the chat runs.
func (s *Server) DescribeChatMCP(ctx context.Context, req *connect.Request[pb.DescribeChatMCPRequest]) (*connect.Response[pb.DescribeChatMCPResponse], error) {
	agentSessionID := req.Msg.GetAgentSessionId()
	if agentSessionID == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("agent_session_id is required"))
	}

	chat, err := s.agentChats.GetByAgentSessionID(ctx, agentSessionID)
	if err != nil || chat == nil {
		return nil, connect.NewError(connect.CodeNotFound, fmt.Errorf("chat not found for agent_session_id %q", agentSessionID))
	}
	sess, err := s.sessions.Get(ctx, chat.SessionID)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("get session: %w", err))
	}

	// The chat's own environment, through the SAME named chain the spawn path
	// uses. Probing under anything else — the daemon's ambient environment, a
	// freshly resolved login shell — is what made a correct configuration look
	// broken and cost hours of misdiagnosis.
	// skipAccountUseRecord: this is a read-only diagnostic. The env is derived
	// by the same shared body a spawn uses (including materializing the bound
	// account's credentials, which is what produces it), but the account's
	// last-used timestamp is left alone — that timestamp is the LRU key account
	// selection reads, so probing must not change which account the NEXT session
	// is handed.
	probeEnv := s.chatSpawnEnv(ctx, sess, chat, s.defaultAccountIDForChat(ctx, sess, chat), "describe chat mcp", skipAccountUseRecord)

	// Key names only, never values: probe_env carries the chat's credentials.
	// The response below carries no environment at all.
	envKeys := make([]string, 0, len(probeEnv))
	for key := range probeEnv {
		envKeys = append(envKeys, key)
	}
	s.logger.Debug().
		Str("agent_session_id", agentSessionID).
		Str("agent", chat.AgentName).
		Strs("probe_env_keys", envKeys).
		Msg("describe chat mcp: probing agent MCP surface")

	resp := &pb.DescribeChatMCPResponse{
		AgentName:    chat.AgentName,
		WorktreePath: sess.WorktreePath,
		IsSupported:  true,
		Servers:      []*pb.MCPServerReport{},
	}

	var prober mcpSurfaceProber = liveMCPSurfaceProber{clients: s.agentClients}
	if s.mcpSurfaceHook != nil {
		prober = s.mcpSurfaceHook
	}

	surface, err := prober.DescribeMCPSurface(ctx, chat.AgentName, &pb.DescribeMCPSurfaceRequest{
		WorkDir:        sess.WorktreePath,
		AgentSessionId: agentSessionID,
		Model:          chat.Model,
		ProbeEnv:       probeEnv,
	})
	if err != nil {
		// An agent that cannot answer is a first-class outcome, not a failure:
		// the caller gets a typed "this agent cannot report its MCP surface"
		// with a nil error, so a CLI can exit 0 and say so. Any OTHER error is
		// a real fault and propagates.
		var unsupported errAgentCannotDescribeMCP
		if !errors.As(err, &unsupported) {
			return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("describe mcp surface: %w", err))
		}
		resp.IsSupported = false
		resp.UnsupportedReason = unsupported.reason
		return connect.NewResponse(resp), nil
	}

	resp.SourceLabel = surface.GetSourceLabel()
	resp.ProbeError = surface.GetProbeError()
	resp.ProbedAt = surface.GetProbedAt()
	if servers := surface.GetServers(); len(servers) > 0 {
		resp.Servers = servers
	}
	return connect.NewResponse(resp), nil
}
