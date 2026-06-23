package serve

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/recurser/bossalib/bossmcp"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

// stubBackend is a minimal bossmcp.Backend that only answers ListSessions; the
// remaining methods panic if called, which keeps the test honest about which
// path it exercises.
type stubBackend struct {
	bossmcp.Backend // nil embed: any unimplemented method panics if invoked
	sessions        []*pb.Session
}

func (s *stubBackend) ListSessions(context.Context, *pb.ListSessionsRequest) ([]*pb.Session, error) {
	return s.sessions, nil
}

// freeAddr asks the OS for an ephemeral port, then returns it as host:port.
func freeAddr(t *testing.T) string {
	t.Helper()
	return "127.0.0.1:0"
}

func startHTTP(t *testing.T, backend bossmcp.Backend) (baseURL string, stop func()) {
	t.Helper()

	ctx, cancel := context.WithCancel(context.Background())

	// Bind first so we know the real port before connecting.
	addrCh := make(chan string, 1)
	errCh := make(chan error, 1)
	go func() {
		errCh <- HTTPWithListenerHook(ctx, freeAddr(t), backend, bossmcp.Options{}, func(actual string) {
			addrCh <- actual
		})
	}()

	select {
	case actual := <-addrCh:
		baseURL = "http://" + actual
	case err := <-errCh:
		t.Fatalf("server exited early: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for server to bind")
	}

	return baseURL, cancel
}

func TestHTTPListSessions(t *testing.T) {
	t.Parallel()

	backend := &stubBackend{sessions: []*pb.Session{{Id: "s-1", Title: "alpha"}}}
	baseURL, stop := startHTTP(t, backend)
	defer stop()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	client := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "0.0.0"}, nil)
	transport := &mcp.StreamableClientTransport{Endpoint: baseURL + "/mcp"}
	session, err := client.Connect(ctx, transport, nil)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer func() { _ = session.Close() }()

	res, err := session.CallTool(ctx, &mcp.CallToolParams{
		Name:      "list_sessions",
		Arguments: map[string]any{},
	})
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if res.IsError {
		t.Fatalf("tool returned error: %+v", res.Content)
	}

	// The tool serialises sessions as a JSON text block; assert our stub session
	// round-tripped through the MCP transport.
	text := textOf(t, res)
	if !strings.Contains(text, "s-1") || !strings.Contains(text, "alpha") {
		t.Fatalf("list_sessions result missing stub session: %s", text)
	}
}

func TestHealthz(t *testing.T) {
	t.Parallel()

	backend := &stubBackend{}
	baseURL, stop := startHTTP(t, backend)
	defer stop()

	resp, err := http.Get(baseURL + "/healthz") //nolint:noctx // test
	if err != nil {
		t.Fatalf("GET /healthz: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("/healthz status = %d, want 200", resp.StatusCode)
	}
}

func textOf(t *testing.T, res *mcp.CallToolResult) string {
	t.Helper()
	var sb strings.Builder
	for _, c := range res.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			sb.WriteString(tc.Text)
		}
	}
	return sb.String()
}
