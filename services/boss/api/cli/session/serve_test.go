package session

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
	"testing/synctest"

	"github.com/recurser/boss/internal/domain/rpc"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/spf13/cobra"
)

type serveCommandFixture struct {
	Client
	operation, id string
	reads         int
	linksAfter    int
	sessions      []*pb.Session
}

func (f *serveCommandFixture) ListSessions(context.Context, *pb.ListSessionsRequest, rpc.SessionReadOptions) ([]*pb.Session, error) {
	return f.sessions, nil
}

func (f *serveCommandFixture) state() *pb.SessionServe {
	s := &pb.SessionServe{IsRunning: true, LastError: "prior error"}
	if f.reads >= f.linksAfter {
		s.ServiceLinks = []*pb.ServiceLink{{Name: "api", Url: "https://example.com"}}
	}
	return s
}
func (f *serveCommandFixture) StartSessionServe(_ context.Context, id string) (*pb.SessionServe, error) {
	f.operation = "start"
	f.id = id
	return f.state(), nil
}
func (f *serveCommandFixture) StopSessionServe(_ context.Context, id string) (*pb.SessionServe, error) {
	f.operation = "stop"
	f.id = id
	s := f.state()
	s.IsRunning = false
	return s, nil
}
func (f *serveCommandFixture) GetSessionServe(_ context.Context, id string) (*pb.SessionServe, error) {
	f.reads++
	if f.operation == "" {
		f.operation = "status"
	}
	f.id = id
	return f.state(), nil
}
func serveTestCommand(f *serveCommandFixture, env string, args ...string) (string, error) {
	cmd := New(func(*cobra.Command) (Client, error) { return f, nil }, Options{Getenv: func(string) string { return env }}).SessionCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SilenceUsage = true
	cmd.SilenceErrors = true
	cmd.SetArgs(append([]string{"serve"}, args...))
	err := cmd.Execute()
	return out.String(), err
}
func TestServeCommands_Sessions_AC250801(t *testing.T) {
	for _, op := range []string{"start", "stop", "status"} {
		for _, explicit := range []bool{true, false} {
			t.Run(op+map[bool]string{true: "-explicit", false: "-environment"}[explicit], func(t *testing.T) {
				f := &serveCommandFixture{}
				args := []string{op}
				if explicit {
					args = append(args, "abcdef0123456789abcdef0123456789")
				}
				out, err := serveTestCommand(f, "environment", args...)
				if err != nil {
					t.Fatal(err)
				}
				wantID := "environment"
				if explicit {
					wantID = "abcdef0123456789abcdef0123456789"
				}
				if f.id != wantID || f.operation != op {
					t.Fatalf("request %s %s", f.operation, f.id)
				}
				if !strings.Contains(out, "api  https://example.com") || !strings.Contains(out, "last error: prior error") {
					t.Fatal(out)
				}
				if op == "stop" && !strings.HasPrefix(out, "stopped\n") {
					t.Fatal(out)
				}
			})
		}
	}
	f := &serveCommandFixture{}
	out, err := serveTestCommand(f, "environment", "status", "--json")
	if err != nil {
		t.Fatal(err)
	}
	var envelope struct {
		SessionID    string                       `json:"session_id"`
		IsRunning    bool                         `json:"is_running"`
		ServiceLinks []struct{ Name, URL string } `json:"service_links"`
		LastError    string                       `json:"last_error"`
	}
	if err = json.Unmarshal([]byte(out), &envelope); err != nil {
		t.Fatal(err)
	}
	if envelope.SessionID != "environment" || !envelope.IsRunning || len(envelope.ServiceLinks) != 1 || envelope.ServiceLinks[0].URL != "https://example.com" || envelope.LastError != "prior error" {
		t.Fatal(out)
	}
	_, err = serveTestCommand(&serveCommandFixture{}, "", "status")
	if err == nil {
		t.Fatal("missing session accepted")
	}
}
func TestServeWait_Sessions_AC250802(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := &serveCommandFixture{linksAfter: 2}
		out, err := serveTestCommand(f, "session", "start", "--wait", "1s")
		if err != nil || f.reads < 2 || !strings.Contains(out, "https://example.com") {
			t.Fatalf("out=%s err=%v reads=%d", out, err, f.reads)
		}
	})
	synctest.Test(t, func(t *testing.T) {
		f := &serveCommandFixture{linksAfter: 100}
		_, err := serveTestCommand(f, "session", "start", "--wait", "10ms")
		if err == nil {
			t.Fatal("wait succeeded without links")
		}
	})
}

func TestServeSessionPrefixes(t *testing.T) {
	const fullID = "0123456789abcdef0123456789abcdef"
	for _, operation := range []string{"start", "stop", "status"} {
		t.Run(operation, func(t *testing.T) {
			f := &serveCommandFixture{sessions: []*pb.Session{{Id: fullID}}}
			out, err := serveTestCommand(f, "unused", operation, "01234567", "--json")
			if err != nil || f.id != fullID || !strings.Contains(out, fullID) {
				t.Fatalf("id=%q out=%s err=%v", f.id, out, err)
			}
		})
	}
	f := &serveCommandFixture{sessions: []*pb.Session{{Id: fullID}, {Id: "01234567abcdef0123456789abcdef0123"}}}
	out, err := serveTestCommand(f, "unused", "start", "01234567", "--json")
	if err == nil || !strings.Contains(out, "AMBIGUOUS_PREFIX") || f.operation != "" {
		t.Fatalf("operation=%q out=%s err=%v", f.operation, out, err)
	}
}
