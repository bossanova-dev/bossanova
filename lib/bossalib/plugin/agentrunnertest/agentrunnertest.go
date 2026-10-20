// Package agentrunnertest checks the shared AgentRunnerService contract over gRPC.
package agentrunnertest

import (
	"context"
	"net"
	"path/filepath"
	"testing"

	v1 "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/safego"
	"github.com/rs/zerolog"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"
)

// Runner describes one implementation, including its deliberately unsupported RPCs.
type Runner struct {
	Name        string
	Client      v1.AgentRunnerServiceClient
	Unsupported []string
	// StartRun overrides the lifecycle request; nil uses the isolated Env paths.
	StartRun func(Env) *v1.StartAgentRunRequest
}

// Env contains isolated paths and an ID that has never run.
type Env struct{ WorkDir, LogPath, UnknownSessionID string }

// Violation describes an observable contract failure without credential material.
type Violation struct{ Method, Problem string }

// Serve registers the production gRPC service on an in-memory connection.
func Serve(t testing.TB, srv v1.AgentRunnerServiceServer) v1.AgentRunnerServiceClient {
	t.Helper()
	listener := bufconn.Listen(1024 * 1024)
	server := grpc.NewServer()
	v1.RegisterAgentRunnerServiceServer(server, srv)
	done := safego.Go(zerolog.Nop(), func() { _ = server.Serve(listener) })
	conn, err := grpc.NewClient("passthrough:///agentrunner", grpc.WithTransportCredentials(insecure.NewCredentials()), grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return listener.DialContext(ctx) }))
	if err != nil {
		server.Stop()
		_ = listener.Close()
		<-done
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close(); server.Stop(); _ = listener.Close(); <-done })
	return v1.NewAgentRunnerServiceClient(conn)
}

// Check runs every RPC contract and returns all violations.
func Check(ctx context.Context, r Runner, env Env) []Violation {
	violations := make([]Violation, 0, len(contracts))
	violations = append(violations, unsupportedNames(r)...)
	for _, row := range contracts {
		violations = append(violations, checkMethod(ctx, r, env, row)...)
	}
	return violations
}

// Run reports one sequential subtest per RPC. It never skips under -short.
func Run(t *testing.T, r Runner) {
	t.Helper()
	env := Env{WorkDir: t.TempDir(), LogPath: filepath.Join(t.TempDir(), "agent.log"), UnknownSessionID: "00000000-0000-4000-8000-000000000001"}
	for _, v := range unsupportedNames(r) {
		t.Errorf("%s: %s", v.Method, v.Problem)
	}
	for _, row := range contracts {
		t.Run(row.name, func(t *testing.T) {
			for _, v := range checkMethod(t.Context(), r, env, row) {
				t.Errorf("%s: %s", v.Method, v.Problem)
			}
		})
	}
}
