package session

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/recurser/boss/internal/domain/rpc"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/spf13/cobra"
)

// createFrameClient provides only the create command's transport surface.
type createFrameClient struct {
	Client
	frames      []*pb.CreateSessionResponse
	beforeFrame func(int)
}

func (c *createFrameClient) ListRepos(context.Context) ([]*pb.Repo, error) {
	return []*pb.Repo{{Id: "repo-1", DisplayName: "app"}}, nil
}
func (c *createFrameClient) CreateSession(context.Context, *pb.CreateSessionRequest) (rpc.CreateSessionStream, error) {
	return &createFrameStream{frames: c.frames, beforeFrame: c.beforeFrame}, nil
}

type createFrameStream struct {
	frames      []*pb.CreateSessionResponse
	beforeFrame func(int)
	index       int
}

func (s *createFrameStream) Receive() bool {
	if s.index == len(s.frames) {
		return false
	}
	if s.beforeFrame != nil {
		s.beforeFrame(s.index)
	}
	s.index++
	return true
}
func (s *createFrameStream) Msg() *pb.CreateSessionResponse { return s.frames[s.index-1] }
func (s *createFrameStream) Err() error                     { return nil }
func (s *createFrameStream) Close() error                   { return nil }
func createdFrame(chatID string) *pb.CreateSessionResponse {
	sess := &pb.Session{Id: "session-created"}
	if chatID != "" {
		sess.AgentSessionId = &chatID
	}
	return &pb.CreateSessionResponse{Event: &pb.CreateSessionResponse_SessionCreated{SessionCreated: &pb.SessionCreated{Session: sess}}}
}
func setupFrame(text string) *pb.CreateSessionResponse {
	return &pb.CreateSessionResponse{Event: &pb.CreateSessionResponse_SetupOutput{SetupOutput: &pb.SetupScriptOutput{Text: text}}}
}

func TestCreateStreamPrintsIDsBeforeSetupCompletes(t *testing.T) {
	var stdout, stderr bytes.Buffer
	c := &createFrameClient{frames: []*pb.CreateSessionResponse{createdFrame("chat-created"), setupFrame("still setting up")}}
	c.beforeFrame = func(i int) {
		if i == 1 && stdout.String() != "session-id: session-created\nchat-id:    chat-created\n" {
			t.Fatalf("IDs unavailable before next setup frame: %q", stdout.String())
		}
	}
	g := New(func(*cobra.Command) (Client, error) { return c, nil }, Options{})
	cmd := g.NewCmd()
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	cmd.SetArgs([]string{"--repo", "repo-1", "--prompt", "add a thing"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
}

func TestCreateStreamPrintsSessionIDBeforeSettledChatID(t *testing.T) {
	var stdout, stderr bytes.Buffer
	c := &createFrameClient{frames: []*pb.CreateSessionResponse{createdFrame(""), setupFrame("setup"), createdFrame("settled-chat")}}
	c.beforeFrame = func(i int) {
		if i == 1 && stdout.String() != "session-id: session-created\n" {
			t.Fatalf("early output = %q", stdout.String())
		}
	}
	g := New(func(*cobra.Command) (Client, error) { return c, nil }, Options{})
	cmd := g.NewCmd()
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	cmd.SetArgs([]string{"--repo", "repo-1", "--prompt", "add a thing"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	if stdout.String() != "session-id: session-created\nchat-id:    settled-chat\n" {
		t.Fatalf("output = %q", stdout.String())
	}
}

func TestCreateStreamSetupFramesAreLineDelimited(t *testing.T) {
	for _, asJSON := range []bool{false, true} {
		t.Run(map[bool]string{false: "human", true: "json"}[asJSON], func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			c := &createFrameClient{frames: []*pb.CreateSessionResponse{setupFrame("first"), setupFrame("second\n"), createdFrame("chat-created")}}
			g := New(func(*cobra.Command) (Client, error) { return c, nil }, Options{})
			cmd := g.NewCmd()
			cmd.SetOut(&stdout)
			cmd.SetErr(&stderr)
			args := []string{"--repo", "repo-1", "--prompt", "add a thing"}
			if asJSON {
				args = append(args, "--json")
			}
			cmd.SetArgs(args)
			if err := cmd.Execute(); err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(stderr.String(), "first\nsecond\n") {
				t.Fatalf("setup output = %q", stderr.String())
			}
			if asJSON {
				var env map[string]any
				if err := json.Unmarshal(stdout.Bytes(), &env); err != nil {
					t.Fatal(err)
				}
			}
		})
	}
}
