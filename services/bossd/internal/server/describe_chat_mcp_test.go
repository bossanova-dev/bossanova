package server

import (
	"bytes"
	"context"
	_ "embed"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/rs/zerolog"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/models"
	"github.com/recurser/bossd/internal/account"
	"github.com/recurser/bossd/internal/session"
)

// fakeMCPSurfaceProber records the request the daemon built and returns a
// canned answer, so the daemon leg is exercised with no agent binary present.
type fakeMCPSurfaceProber struct {
	gotAgentName string
	gotRequest   *pb.DescribeMCPSurfaceRequest
	resp         *pb.DescribeMCPSurfaceResponse
	err          error
}

func (f *fakeMCPSurfaceProber) DescribeMCPSurface(_ context.Context, agentName string, req *pb.DescribeMCPSurfaceRequest) (*pb.DescribeMCPSurfaceResponse, error) {
	f.gotAgentName = agentName
	f.gotRequest = req
	return f.resp, f.err
}

func newMCPTestServer(chat *models.AgentChat, sess *models.Session, prober mcpSurfaceProber) *Server {
	return &Server{
		agentChats:     &chatStoreFake{chat: chat},
		sessions:       &sessionStoreFake{sess: sess},
		mcpSurfaceHook: prober,
	}
}

func TestDescribeChatMCP_ReturnsPluginSurface(t *testing.T) {
	chat := &models.AgentChat{ID: "c1", SessionID: "s1", AgentSessionID: "agent-1", AgentName: "codex", Model: "gpt-5.6"}
	sess := &models.Session{ID: "s1", WorktreePath: "/work/tree"}
	prober := &fakeMCPSurfaceProber{resp: &pb.DescribeMCPSurfaceResponse{
		SourceLabel: "codex app-server mcpServerStatus/list",
		Servers: []*pb.MCPServerReport{{
			Name:           "bossanova-linear",
			IsDeclared:     true,
			ToolCount:      3,
			ToolNamePrefix: "mcp__bossanova-linear__",
			AuthStatus:     "oauth",
			Source:         pb.MCPServerSource_MCP_SERVER_SOURCE_REPO_FILE,
			SourceDetail:   ".codex/config.toml",
		}},
	}}
	srv := newMCPTestServer(chat, sess, prober)

	resp, err := srv.DescribeChatMCP(context.Background(), connect.NewRequest(&pb.DescribeChatMCPRequest{
		AgentSessionId: "agent-1",
	}))
	if err != nil {
		t.Fatalf("DescribeChatMCP: %v", err)
	}
	if !resp.Msg.GetIsSupported() {
		t.Fatalf("is_supported = false, reason %q", resp.Msg.GetUnsupportedReason())
	}
	if resp.Msg.GetAgentName() != "codex" || resp.Msg.GetWorktreePath() != "/work/tree" {
		t.Fatalf("agent/worktree = %q/%q", resp.Msg.GetAgentName(), resp.Msg.GetWorktreePath())
	}
	if len(resp.Msg.GetServers()) != 1 || resp.Msg.GetServers()[0].GetToolNamePrefix() != "mcp__bossanova-linear__" {
		t.Fatalf("servers = %+v", resp.Msg.GetServers())
	}
	// The probe must be routed to the CHAT's agent and scoped to the chat.
	if prober.gotAgentName != "codex" {
		t.Fatalf("routed to agent %q, want the chat's own %q", prober.gotAgentName, "codex")
	}
	if prober.gotRequest.GetWorkDir() != "/work/tree" || prober.gotRequest.GetAgentSessionId() != "agent-1" {
		t.Fatalf("probe request = %+v", prober.gotRequest)
	}
	if prober.gotRequest.GetModel() != "gpt-5.6" {
		t.Fatalf("probe model = %q, want the chat's own %q", prober.gotRequest.GetModel(), "gpt-5.6")
	}
}

// TestDescribeChatMCP_ProbeEnvMatchesSharedSpawnHelper is the anti-misdiagnosis
// assertion this whole ticket turns on: the environment the daemon hands the
// probe must be byte-identical to the one the SPAWN path builds for the same
// chat, through the same named helper. A probe under a different environment is
// how a correct configuration came to look broken.
func TestDescribeChatMCP_ProbeEnvMatchesSharedSpawnHelper(t *testing.T) {
	chat := &models.AgentChat{ID: "c1", SessionID: "s1", AgentSessionID: "agent-2", AgentName: "claude"}
	sess := &models.Session{ID: "s1", WorktreePath: t.TempDir()}
	prober := &fakeMCPSurfaceProber{resp: &pb.DescribeMCPSurfaceResponse{}}
	srv := newMCPTestServer(chat, sess, prober)

	if _, err := srv.DescribeChatMCP(context.Background(), connect.NewRequest(&pb.DescribeChatMCPRequest{
		AgentSessionId: "agent-2",
	})); err != nil {
		t.Fatalf("DescribeChatMCP: %v", err)
	}

	// recordAccountUse deliberately: this recomputes the env exactly as the WAKE
	// path does, mode included. The probe ran under skipAccountUseRecord, so a
	// difference here would mean the account-use mode had leaked into the env
	// derivation — which is precisely what must never happen.
	want, _ := srv.chatSpawnEnv(context.Background(), sess, chat, srv.defaultAccountIDForChat(context.Background(), sess, chat), "wake chat", recordAccountUse)
	got := prober.gotRequest.GetProbeEnv()
	if !maps.Equal(got, want) {
		t.Fatalf("probe_env keys/values differ from the shared spawn helper's output\n got: %v\nwant: %v", got, want)
	}
	if len(want) == 0 {
		t.Fatal("the shared helper produced an empty env — the comparison above would be vacuous")
	}
}

// TestDescribeChatMCP_NeverEchoesProbeEnvValues pins the secrecy contract at
// the daemon boundary. It plants a distinctive secret in the worktree's own
// .env — the same route a repo's real LINEAR_API_KEY takes into a chat's
// environment — proves the probe request actually carried it, and then requires
// it to appear nowhere in the response. Asserting on a planted sentinel rather
// than on "no env value at all" keeps the test honest: agent_name legitimately
// equals a non-secret env value, and a blanket check would fail on that
// coincidence while proving nothing about secrets.
func TestDescribeChatMCP_NeverEchoesProbeEnvValues(t *testing.T) {
	const secret = "sentinel-secret-must-not-be-echoed"
	worktree := t.TempDir()
	if err := os.WriteFile(filepath.Join(worktree, ".env"), []byte("SENTINEL_TOKEN="+secret+"\n"), 0o600); err != nil {
		t.Fatalf("write worktree .env: %v", err)
	}

	chat := &models.AgentChat{ID: "c1", SessionID: "s1", AgentSessionID: "agent-3", AgentName: "claude"}
	sess := &models.Session{ID: "s1", WorktreePath: worktree}
	prober := &fakeMCPSurfaceProber{resp: &pb.DescribeMCPSurfaceResponse{
		SourceLabel: "claude -p --output-format stream-json (system/init event)",
		Servers:     []*pb.MCPServerReport{{Name: "bossanova-linear", IsDeclared: true}},
	}}
	srv := newMCPTestServer(chat, sess, prober)

	resp, err := srv.DescribeChatMCP(context.Background(), connect.NewRequest(&pb.DescribeChatMCPRequest{
		AgentSessionId: "agent-3",
	}))
	if err != nil {
		t.Fatalf("DescribeChatMCP: %v", err)
	}
	if prober.gotRequest.GetProbeEnv()["SENTINEL_TOKEN"] != secret {
		t.Fatal("the probe request never carried the planted secret — the assertion below would be vacuous")
	}
	if strings.Contains(resp.Msg.String(), secret) {
		t.Fatal("the response echoed a secret-bearing probe_env value")
	}
}

// TestDescribeChatMCP_LogsKeyNamesNotValues pins the other half of the secrecy
// contract. The response is not the only place a credential can escape: the
// daemon logs this RPC, and logging the env map wholesale would put every
// chat's credentials in the daemon log. The debug line must carry KEY NAMES
// only.
func TestDescribeChatMCP_LogsKeyNamesNotValues(t *testing.T) {
	const secret = "sentinel-secret-must-not-be-logged"
	worktree := t.TempDir()
	if err := os.WriteFile(filepath.Join(worktree, ".env"), []byte("SENTINEL_TOKEN="+secret+"\n"), 0o600); err != nil {
		t.Fatalf("write worktree .env: %v", err)
	}

	var logs bytes.Buffer
	chat := &models.AgentChat{ID: "c1", SessionID: "s1", AgentSessionID: "agent-log", AgentName: "claude"}
	sess := &models.Session{ID: "s1", WorktreePath: worktree}
	prober := &fakeMCPSurfaceProber{resp: &pb.DescribeMCPSurfaceResponse{}}
	srv := newMCPTestServer(chat, sess, prober)
	srv.logger = zerolog.New(&logs).Level(zerolog.DebugLevel)

	if _, err := srv.DescribeChatMCP(context.Background(), connect.NewRequest(&pb.DescribeChatMCPRequest{
		AgentSessionId: "agent-log",
	})); err != nil {
		t.Fatalf("DescribeChatMCP: %v", err)
	}

	logged := logs.String()
	if !strings.Contains(logged, "SENTINEL_TOKEN") {
		t.Fatalf("the key name is absent, so the value check below would be vacuous. logs: %s", logged)
	}
	if strings.Contains(logged, secret) {
		t.Fatalf("a probe_env VALUE reached the daemon log. logs: %s", logged)
	}
}

// TestDescribeChatMCP_UnsupportedAgentIsNotAnError pins the degrade contract:
// an agent that cannot answer yields is_supported=false with a human reason and
// a NIL error, so a CLI can exit 0 and explain itself.
//
// The WRAPPED case is the one that matters for regressions. Every site that
// returns the marker today returns it unwrapped, so a bare type assertion is
// correct today and only today: the first future edit that adds context to one
// of those returns would silently flip this outcome to CodeInternal — an
// unsupported agent reported as a daemon fault — with nothing to catch it.
// Recognition must survive wrapping, so it is asserted here rather than assumed.
func TestDescribeChatMCP_UnsupportedAgentIsNotAnError(t *testing.T) {
	marker := errAgentCannotDescribeMCP{reason: `the "stub-runner" agent cannot report its MCP surface`}
	for _, tc := range []struct {
		name string
		err  error
	}{
		{name: "returned unwrapped", err: marker},
		{name: "wrapped with context", err: fmt.Errorf("dispatch to stub-runner: %w", marker)},
		{name: "wrapped twice", err: fmt.Errorf("outer: %w", fmt.Errorf("inner: %w", marker))},
	} {
		t.Run(tc.name, func(t *testing.T) {
			chat := &models.AgentChat{ID: "c1", SessionID: "s1", AgentSessionID: "agent-4", AgentName: "stub-runner"}
			sess := &models.Session{ID: "s1", WorktreePath: "/work/tree"}
			srv := newMCPTestServer(chat, sess, &fakeMCPSurfaceProber{err: tc.err})

			resp, err := srv.DescribeChatMCP(context.Background(), connect.NewRequest(&pb.DescribeChatMCPRequest{
				AgentSessionId: "agent-4",
			}))
			if err != nil {
				t.Fatalf("an unsupported agent must not be a gRPC error: %v", err)
			}
			if resp.Msg.GetIsSupported() {
				t.Fatal("is_supported = true, want false for an agent with no answer")
			}
			if resp.Msg.GetUnsupportedReason() != marker.reason {
				t.Fatalf("unsupported_reason = %q, want the marker's own sentence %q", resp.Msg.GetUnsupportedReason(), marker.reason)
			}
			if resp.Msg.GetServers() == nil {
				t.Fatal("servers must be non-nil so it marshals as [] rather than null")
			}
		})
	}
}

// TestDescribeChatMCP_RealProbeFailurePropagates keeps the degrade narrow: a
// fault that is NOT "this agent has no answer" must surface as an error rather
// than be flattened into an unsupported answer.
func TestDescribeChatMCP_RealProbeFailurePropagates(t *testing.T) {
	chat := &models.AgentChat{ID: "c1", SessionID: "s1", AgentSessionID: "agent-5", AgentName: "claude"}
	sess := &models.Session{ID: "s1", WorktreePath: "/work/tree"}
	srv := newMCPTestServer(chat, sess, &fakeMCPSurfaceProber{err: errors.New("plugin connection reset")})

	if _, err := srv.DescribeChatMCP(context.Background(), connect.NewRequest(&pb.DescribeChatMCPRequest{
		AgentSessionId: "agent-5",
	})); err == nil {
		t.Fatal("a transport fault must not be reported as an unsupported agent")
	}
}

// TestDescribeChatMCP_ProbeErrorIsNotNoServers pins that the plugin's
// probe_error survives the daemon leg intact, so a CLI can say "the probe
// failed" rather than "no MCP servers".
func TestDescribeChatMCP_ProbeErrorIsNotNoServers(t *testing.T) {
	chat := &models.AgentChat{ID: "c1", SessionID: "s1", AgentSessionID: "agent-6", AgentName: "codex"}
	sess := &models.Session{ID: "s1", WorktreePath: "/work/tree"}
	srv := newMCPTestServer(chat, sess, &fakeMCPSurfaceProber{resp: &pb.DescribeMCPSurfaceResponse{
		SourceLabel: "codex app-server mcpServerStatus/list",
		ProbeError:  "start app-server: not found",
	}})

	resp, err := srv.DescribeChatMCP(context.Background(), connect.NewRequest(&pb.DescribeChatMCPRequest{
		AgentSessionId: "agent-6",
	}))
	if err != nil {
		t.Fatalf("DescribeChatMCP: %v", err)
	}
	if !resp.Msg.GetIsSupported() {
		t.Fatal("a probe that ran and failed is still a supported agent")
	}
	if resp.Msg.GetProbeError() == "" {
		t.Fatal("probe_error must survive to the caller")
	}
	if len(resp.Msg.GetServers()) != 0 {
		t.Fatalf("servers = %+v, want empty alongside probe_error", resp.Msg.GetServers())
	}
}

func TestDescribeChatMCP_ArgumentAndLookupErrors(t *testing.T) {
	tests := []struct {
		name     string
		chat     *models.AgentChat
		id       string
		wantCode connect.Code
	}{
		{
			name:     "empty agent_session_id",
			chat:     &models.AgentChat{ID: "c1", SessionID: "s1", AgentSessionID: "agent-7"},
			id:       "",
			wantCode: connect.CodeInvalidArgument,
		},
		{
			name:     "unknown chat",
			chat:     nil,
			id:       "missing",
			wantCode: connect.CodeNotFound,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			srv := newMCPTestServer(tc.chat, &models.Session{ID: "s1", WorktreePath: "/w"}, &fakeMCPSurfaceProber{
				resp: &pb.DescribeMCPSurfaceResponse{},
			})
			_, err := srv.DescribeChatMCP(context.Background(), connect.NewRequest(&pb.DescribeChatMCPRequest{
				AgentSessionId: tc.id,
			}))
			if err == nil {
				t.Fatal("expected an error")
			}
			if got := connect.CodeOf(err); got != tc.wantCode {
				t.Fatalf("code = %v, want %v", got, tc.wantCode)
			}
		})
	}
}

// describeChatMCPSource is the daemon leg's own source text, embedded so the
// assertion below holds under both `go test` and Bazel (where the package's
// sources are not otherwise readable at run time).
//
//go:embed describe_chat_mcp.go
var describeChatMCPSource string

// TestDescribeChatMCPIsHarnessAgnostic runs BOS-867's own acceptance grep in
// CI: the daemon leg must name no harness at all, because every per-harness
// detail belongs behind AgentRunnerService.DescribeMCPSurface. Asserting on the
// absence of the NAMES (not merely of an `if`) is deliberately stricter — a
// harness-shaped special case cannot creep in as a map lookup or a switch
// without tripping this.
func TestDescribeChatMCPIsHarnessAgnostic(t *testing.T) {
	for _, harness := range []string{"claude", "codex"} {
		if strings.Contains(describeChatMCPSource, harness) {
			t.Fatalf("describe_chat_mcp.go names the %q harness; route it through the plugin RPC instead", harness)
		}
	}
	// Guard the guard: if the embed ever resolved to nothing, the loop above
	// would pass vacuously.
	if !strings.Contains(describeChatMCPSource, "func (s *Server) DescribeChatMCP(") {
		t.Fatal("the embedded source is not describe_chat_mcp.go — the assertion above would be vacuous")
	}
}

// lastUsedSpyRegistry implements account.Registry and records every
// TouchLastUsed, so a server-level test can see whether a code path advanced
// the account LRU key.
type lastUsedSpyRegistry struct {
	accounts   []account.AccountMeta
	touchCalls int
	touchedID  string
}

func (r *lastUsedSpyRegistry) List(context.Context) ([]account.AccountMeta, error) {
	return r.accounts, nil
}

func (r *lastUsedSpyRegistry) Get(_ context.Context, id string) (account.AccountMeta, bool, error) {
	for _, a := range r.accounts {
		if a.ID == id {
			return a, true, nil
		}
	}
	return account.AccountMeta{}, false, nil
}

func (r *lastUsedSpyRegistry) TouchLastUsed(_ context.Context, id string, _ time.Time) error {
	r.touchCalls++
	r.touchedID = id
	return nil
}

// TestDescribeChatMCP_DoesNotRecordAccountUse proves the read-only claim end to
// end, at the level the RPC is actually wired: DescribeChatMCP must not advance
// the bound account's last-used timestamp, because that timestamp is the LRU key
// account selection reads — a diagnostic that bumped it could change which
// account the NEXT session is handed.
//
// The wake path's mode is asserted in the SAME test against the SAME spy: a
// resolver or registry that recorded nothing at all would satisfy the probe half
// alone while being entirely broken.
func TestDescribeChatMCP_DoesNotRecordAccountUse(t *testing.T) {
	accountID := "acct-1"
	chat := &models.AgentChat{ID: "c1", SessionID: "s1", AgentSessionID: "agent-lru", AgentName: "claude", AccountID: &accountID}
	sess := &models.Session{ID: "s1", WorktreePath: t.TempDir()}
	prober := &fakeMCPSurfaceProber{resp: &pb.DescribeMCPSurfaceResponse{}}
	srv := newMCPTestServer(chat, sess, prober)
	reg := &lastUsedSpyRegistry{accounts: []account.AccountMeta{
		{ID: accountID, Provider: "claude", Status: "active", Health: "ok"},
	}}
	srv.resolver = account.NewResolver(
		reg,
		&fakeMaterializer{supports: true, env: map[string]string{"ANTHROPIC_API_KEY": "sk-probe"}},
		zerolog.Nop(),
	)
	srv.lifecycle = &session.Lifecycle{}

	if _, err := srv.DescribeChatMCP(context.Background(), connect.NewRequest(&pb.DescribeChatMCPRequest{
		AgentSessionId: "agent-lru",
	})); err != nil {
		t.Fatalf("DescribeChatMCP: %v", err)
	}
	// Non-vacuity: the probe must actually have resolved the bound account's
	// credentials, or "it did not touch last-used" would be true of a no-op.
	if got := prober.gotRequest.GetProbeEnv()["ANTHROPIC_API_KEY"]; got != "sk-probe" {
		t.Fatalf("probe env ANTHROPIC_API_KEY = %q, want the materialized account credential — without it this test asserts nothing", got)
	}
	if reg.touchCalls != 0 {
		t.Fatalf("DescribeChatMCP advanced last-used %d times (account %q), want 0: a read-only diagnostic must not change the account LRU key", reg.touchCalls, reg.touchedID)
	}

	// The spawn/wake mode, through the same helper and the same spy, DOES record.
	_, _ = srv.chatSpawnEnv(context.Background(), sess, chat, "", "wake chat", recordAccountUse)
	if reg.touchCalls != 1 || reg.touchedID != accountID {
		t.Fatalf("wake-mode chatSpawnEnv last-used = (calls=%d id=%q), want exactly one touch of %q — this half is what makes the probe assertion above non-vacuous", reg.touchCalls, reg.touchedID, accountID)
	}
}
