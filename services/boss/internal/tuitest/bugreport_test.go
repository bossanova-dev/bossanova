package tuitest_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"

	"github.com/recurser/boss/internal/tuitest"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/gen/bossanova/v1/bossanovav1connect"
)

// mockOrchestrator captures ReportBug requests for assertion. All other
// OrchestratorService methods return Unimplemented via the embedded struct.
type mockOrchestrator struct {
	bossanovav1connect.UnimplementedOrchestratorServiceHandler
	mu       sync.Mutex
	last     *pb.ReportBugRequest
	reportID string
}

func (m *mockOrchestrator) ReportBug(_ context.Context, req *connect.Request[pb.ReportBugRequest]) (*connect.Response[pb.ReportBugResponse], error) {
	m.mu.Lock()
	m.last = req.Msg
	id := m.reportID
	if id == "" {
		id = "rep-abc123def456"
	}
	m.mu.Unlock()
	return connect.NewResponse(&pb.ReportBugResponse{ReportId: id}), nil
}

func (m *mockOrchestrator) LastRequest() *pb.ReportBugRequest {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.last
}

func startMockBosso(t *testing.T, mock *mockOrchestrator) string {
	t.Helper()
	mux := http.NewServeMux()
	path, handler := bossanovav1connect.NewOrchestratorServiceHandler(mock)
	mux.Handle(path, handler)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv.URL
}

func waitForReportBug(t *testing.T, mock *mockOrchestrator, timeout time.Duration) *pb.ReportBugRequest {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if req := mock.LastRequest(); req != nil {
			return req
		}
		time.Sleep(50 * time.Millisecond)
	}
	return nil
}

func TestTUI_BugReport_SubmitFlow(t *testing.T) {
	mock := &mockOrchestrator{}
	url := startMockBosso(t, mock)
	t.Setenv("BOSS_REPORT_URL", url)

	h := tuitest.New(t,
		tuitest.WithRepos(testRepos()...),
		tuitest.WithSessions(testSessions()...),
	)

	if err := h.Driver.WaitForText(waitTimeout, "Add dark mode"); err != nil {
		t.Fatal(err)
	}

	// Ctrl+B opens the modal. 0x02 is the byte sent by ctrl+b in a PTY.
	if err := h.Driver.SendKey(0x02); err != nil {
		t.Fatal(err)
	}

	if err := h.Driver.WaitForText(waitTimeout, "What went wrong"); err != nil {
		t.Fatalf("bug report modal did not open; screen:\n%s", h.Driver.Screen())
	}

	if err := h.Driver.SendString("test comment"); err != nil {
		t.Fatal(err)
	}
	if err := h.Driver.SendEnter(); err != nil {
		t.Fatal(err)
	}

	req := waitForReportBug(t, mock, waitTimeout)
	if req == nil {
		t.Fatalf("ReportBug was never called; screen:\n%s", h.Driver.Screen())
	}

	if req.Comment != "test comment" {
		t.Errorf("comment = %q; want %q", req.Comment, "test comment")
	}
	if req.Context == nil {
		t.Fatal("context was nil")
	}
	if req.Context.Os == "" {
		t.Error("context.Os should be populated from runtime.GOOS")
	}
	if req.Context.Arch == "" {
		t.Error("context.Arch should be populated from runtime.GOARCH")
	}
	if len(req.Context.Sessions) == 0 {
		t.Error("context.Sessions should include the mock daemon's session list")
	}

	if err := h.Driver.WaitForText(waitTimeout, "Report submitted"); err != nil {
		t.Fatalf("success toast did not appear; screen:\n%s", h.Driver.Screen())
	}

	// Auto-dismiss after 3s restores the home view (session rows return).
	if err := h.Driver.WaitFor(6*time.Second, func(screen string) bool {
		return !strings.Contains(screen, "Report submitted") && strings.Contains(screen, "Add dark mode")
	}); err != nil {
		t.Fatalf("prior view was not restored after auto-dismiss; screen:\n%s", h.Driver.Screen())
	}
}

func TestTUI_BugReport_EscCancelsFromEditing(t *testing.T) {
	mock := &mockOrchestrator{}
	url := startMockBosso(t, mock)
	t.Setenv("BOSS_REPORT_URL", url)

	h := tuitest.New(t,
		tuitest.WithRepos(testRepos()...),
		tuitest.WithSessions(testSessions()...),
	)

	if err := h.Driver.WaitForText(waitTimeout, "Add dark mode"); err != nil {
		t.Fatal(err)
	}

	if err := h.Driver.SendKey(0x02); err != nil {
		t.Fatal(err)
	}
	if err := h.Driver.WaitForText(waitTimeout, "What went wrong"); err != nil {
		t.Fatal(err)
	}

	if err := h.Driver.SendEscape(); err != nil {
		t.Fatal(err)
	}
	if err := h.Driver.WaitForNoText(waitTimeout, "What went wrong"); err != nil {
		t.Fatalf("modal did not close on esc; screen:\n%s", h.Driver.Screen())
	}

	if !h.Driver.ScreenContains("Add dark mode") {
		t.Fatalf("home was not restored after cancel; screen:\n%s", h.Driver.Screen())
	}
	if got := mock.LastRequest(); got != nil {
		t.Fatalf("no report should have been sent after cancel; got %+v", got)
	}
}
