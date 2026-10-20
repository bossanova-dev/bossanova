package serve

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
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

// testHTTPToken is the bearer token the test servers are started with.
const testHTTPToken = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

// bearerTransport attaches a fixed Authorization header to every request.
type bearerTransport struct{ token string }

func (b bearerTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+b.token)
	return http.DefaultTransport.RoundTrip(r)
}

func startHTTP(t *testing.T, backend bossmcp.Backend) (baseURL string, stop func()) {
	t.Helper()

	ctx, cancel := context.WithCancel(context.Background())

	// Bind first so we know the real port before connecting.
	addrCh := make(chan string, 1)
	errCh := make(chan error, 1)
	go func() {
		errCh <- HTTPWithListenerHook(ctx, freeAddr(t), backend, bossmcp.Options{}, HTTPOptions{Token: testHTTPToken}, func(actual string) {
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

// TestHTTPAcceptsBearer proves an MCP client carrying the configured bearer
// token reaches the tool surface end to end.
func TestHTTPAcceptsBearer(t *testing.T) {
	t.Parallel()

	backend := &stubBackend{sessions: []*pb.Session{{Id: "s-1", Title: "alpha"}}}
	baseURL, stop := startHTTP(t, backend)
	defer stop()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	client := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "0.0.0"}, nil)
	transport := &mcp.StreamableClientTransport{
		Endpoint:   baseURL + "/mcp",
		HTTPClient: &http.Client{Transport: bearerTransport{token: testHTTPToken}},
	}
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

// postMCP sends a raw MCP initialize request to /mcp with the given
// Authorization header (omitted when empty) and returns the status code and
// response headers; the body is drained and closed.
func postMCP(t *testing.T, baseURL, authHeader string) (int, http.Header) {
	t.Helper()
	body := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"t","version":"0"}}}`
	req, err := http.NewRequest(http.MethodPost, baseURL+"/mcp", strings.NewReader(body)) //nolint:noctx // test
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	if authHeader != "" {
		req.Header.Set("Authorization", authHeader)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST /mcp: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, resp.Body)
	return resp.StatusCode, resp.Header
}

func TestHTTPRejectsMissingBearer(t *testing.T) {
	t.Parallel()

	backend := &stubBackend{}
	baseURL, stop := startHTTP(t, backend)
	defer stop()

	status, header := postMCP(t, baseURL, "")
	if status != http.StatusUnauthorized {
		t.Fatalf("POST /mcp without bearer: status = %d, want 401", status)
	}
	if got := header.Get("WWW-Authenticate"); !strings.HasPrefix(got, "Bearer") {
		t.Fatalf("WWW-Authenticate = %q, want Bearer challenge", got)
	}
}

func TestHTTPRejectsWrongBearer(t *testing.T) {
	t.Parallel()

	baseURL, stop := startHTTP(t, &stubBackend{})
	defer stop()

	for _, h := range []string{
		"Bearer " + strings.Repeat("f", 64),
		"Basic " + testHTTPToken,
		testHTTPToken,
		"Bearer ",
	} {
		if status, _ := postMCP(t, baseURL, h); status != http.StatusUnauthorized {
			t.Fatalf("POST /mcp with %q: status = %d, want 401", h, status)
		}
	}
	// Control: the same raw request with the right token is not rejected.
	if status, _ := postMCP(t, baseURL, "Bearer "+testHTTPToken); status == http.StatusUnauthorized {
		t.Fatal("POST /mcp with the correct bearer was rejected")
	}
}

// TestHTTPEmptyTokenFailsClosed proves a server started without a token
// rejects every /mcp request instead of serving it unauthenticated.
func TestHTTPEmptyTokenFailsClosed(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	addrCh := make(chan string, 1)
	go func() {
		_ = HTTPWithListenerHook(ctx, "127.0.0.1:0", &stubBackend{}, bossmcp.Options{}, HTTPOptions{}, func(a string) { addrCh <- a })
	}()
	var baseURL string
	select {
	case a := <-addrCh:
		baseURL = "http://" + a
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for server to bind")
	}
	for _, h := range []string{"", "Bearer ", "Bearer " + testHTTPToken} {
		if status, _ := postMCP(t, baseURL, h); status != http.StatusUnauthorized {
			t.Fatalf("empty-token server with %q: status = %d, want 401", h, status)
		}
	}
}

func TestHTTPRefusesNonLoopbackBind(t *testing.T) {
	t.Parallel()

	for _, addr := range []string{"0.0.0.0:0", ":0", "[::]:0", "192.0.2.10:0", "example.com:0"} {
		listened := false
		err := HTTPWithListenerHook(context.Background(), addr, &stubBackend{}, bossmcp.Options{}, HTTPOptions{Token: testHTTPToken}, func(string) { listened = true })
		if err == nil {
			t.Fatalf("addr %q: want error refusing non-loopback bind", addr)
		}
		if listened {
			t.Fatalf("addr %q: server listened before refusing", addr)
		}
		if !strings.Contains(err.Error(), addr) {
			t.Fatalf("addr %q: error %q does not name the address", addr, err)
		}
	}
}

func TestCheckLoopbackAddr(t *testing.T) {
	t.Parallel()

	for _, ok := range []string{"127.0.0.1:8765", "127.0.0.2:0", "[::1]:8765", "localhost:8765", "LOCALHOST:1"} {
		if err := checkLoopbackAddr(ok); err != nil {
			t.Fatalf("checkLoopbackAddr(%q) = %v, want nil", ok, err)
		}
	}
	for _, bad := range []string{"0.0.0.0:8765", ":8765", "10.0.0.1:1", "localhost.evil.com:1", "nonsense"} {
		if err := checkLoopbackAddr(bad); err == nil {
			t.Fatalf("checkLoopbackAddr(%q) = nil, want error", bad)
		}
	}
}

func TestHTTPHealthzAndBuildinfoStayOpen(t *testing.T) {
	t.Parallel()

	baseURL, stop := startHTTP(t, &stubBackend{})
	defer stop()

	for _, path := range []string{"/healthz", "/buildinfo"} {
		resp, err := http.Get(baseURL + path) //nolint:noctx // test
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("GET %s without a token: status = %d, want 200", path, resp.StatusCode)
		}
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

// TestStdioDriftEndToEnd is the direct reproduction of the incident: a live
// server whose on-disk binary is replaced surfaces the staleness notice on the
// next tools/list, over a real MCP transport, without the transport dropping.
func TestStdioDriftEndToEnd(t *testing.T) {
	t.Parallel()

	p := writeExe(t)
	detector := newDriftDetector(p, 0) // stat every tools/list for determinism
	server := newServer(&stubBackend{}, bossmcp.Options{}, detector)

	client := mcp.NewClient(&mcp.Implementation{Name: "drift-test", Version: "0.0.0"}, nil)
	st, ct := mcp.NewInMemoryTransports()
	ctx := context.Background()
	if _, err := server.Connect(ctx, st, nil); err != nil {
		t.Fatalf("server.Connect: %v", err)
	}
	cs, err := client.Connect(ctx, ct, nil)
	if err != nil {
		t.Fatalf("client.Connect: %v", err)
	}
	defer func() { _ = cs.Close() }()

	// Before the rebuild: no tool description carries the notice.
	before, err := cs.ListTools(ctx, nil)
	if err != nil {
		t.Fatalf("ListTools (before): %v", err)
	}
	for _, tool := range before.Tools {
		if strings.Contains(tool.Description, DriftNotice) {
			t.Fatalf("fresh server tool %q already carries drift notice", tool.Name)
		}
	}

	// Replace the binary underneath the running server (a `make build`).
	future := time.Now().Add(2 * time.Minute)
	if err := os.Chtimes(p, future, future); err != nil {
		t.Fatalf("chtimes: %v", err)
	}

	// After the rebuild: the same live session's next tools/list carries the
	// notice — the transport is still up (the call succeeded).
	after, err := cs.ListTools(ctx, nil)
	if err != nil {
		t.Fatalf("ListTools (after): transport dropped: %v", err)
	}
	var found bool
	for _, tool := range after.Tools {
		if strings.Contains(tool.Description, DriftNotice) {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("after rebuild, tools/list should carry the staleness notice")
	}
}

// TestBuildInfoEndpoint asserts the HTTP flavor serves its compiled-in build
// metadata as JSON, which `boss mcp status` reads to name drift.
func TestBuildInfoEndpoint(t *testing.T) {
	t.Parallel()

	baseURL, stop := startHTTP(t, &stubBackend{})
	defer stop()

	resp, err := http.Get(baseURL + "/buildinfo") //nolint:noctx // test
	if err != nil {
		t.Fatalf("GET /buildinfo: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("/buildinfo status = %d, want 200", resp.StatusCode)
	}
	var bi BuildInfo
	if err := json.NewDecoder(resp.Body).Decode(&bi); err != nil {
		t.Fatalf("decode /buildinfo: %v", err)
	}
	// buildinfo defaults are "dev"/"unknown" in tests; the Summary must be non-empty.
	if bi.Version == "" || bi.Summary == "" {
		t.Fatalf("/buildinfo returned empty fields: %+v", bi)
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
