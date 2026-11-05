package diagnostics

import (
	"bytes"
	"context"
	"errors"
	"os"
	"reflect"
	"testing"
	"time"

	"connectrpc.com/connect"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/spf13/cobra"
)

type readCredentialClientFake struct {
	response *pb.GetGitHubReadCredentialResponse
	err      error
	request  *pb.GetGitHubReadCredentialRequest
	deadline bool
}

func (f *readCredentialClientFake) GetGitHubReadCredential(ctx context.Context, req *pb.GetGitHubReadCredentialRequest) (*pb.GetGitHubReadCredentialResponse, error) {
	f.request = req
	d, ok := ctx.Deadline()
	f.deadline = ok && time.Until(d) > 0 && time.Until(d) <= 5*time.Second
	return f.response, f.err
}

type forbiddenCredentialStdin struct{ t *testing.T }

func (r forbiddenCredentialStdin) Read([]byte) (int, error) {
	r.t.Fatal("helper read stdin")
	return 0, errors.New("stdin forbidden")
}

func TestGHReadCredential(t *testing.T) {
	for _, tc := range []struct {
		name              string
		response          *pb.GetGitHubReadCredentialResponse
		rpcErr, errorDial error
		want              string
	}{
		{name: "token", response: &pb.GetGitHubReadCredentialResponse{InstallationId: 42, Token: "test-token"}, want: "42 test-token\n"},
		{name: "empty", response: &pb.GetGitHubReadCredentialResponse{}},
		{name: "nil response"},
		{name: "unavailable", rpcErr: connect.NewError(connect.CodeUnavailable, errors.New("offline"))},
		{name: "old daemon", rpcErr: connect.NewError(connect.CodeUnimplemented, errors.New("older daemon"))},
		{name: "deadline", rpcErr: context.DeadlineExceeded},
		{name: "dial", errorDial: errors.New("dial failed")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("GH_HOST", "github.com")
			t.Setenv("GH_REPO", "owner/repo")
			fake := &readCredentialClientFake{response: tc.response, err: tc.rpcErr}
			group := New(nil, Options{GitHubReadCredentialClient: func(*cobra.Command) (GitHubReadCredentialClient, error) { return fake, tc.errorDial }})
			root := &cobra.Command{Use: "boss", PersistentPreRunE: func(*cobra.Command, []string) error { t.Fatal("inherited startup hook ran"); return nil }}
			cmd := group.ghReadCredentialCmd()
			root.AddCommand(cmd)
			var out, stderr bytes.Buffer
			root.SetOut(&out)
			root.SetErr(&stderr)
			root.SetIn(forbiddenCredentialStdin{t})
			args := []string{"pr", "view", "42", "--json", "title"}
			root.SetArgs(append([]string{"__gh-read-credential", "--"}, args...))
			if err := root.Execute(); err != nil {
				t.Fatalf("Execute: %v", err)
			}
			if out.String() != tc.want || stderr.Len() != 0 {
				t.Fatalf("stdout=%q stderr=%q", out.String(), stderr.String())
			}
			if tc.errorDial == nil {
				wd, err := os.Getwd()
				if err != nil {
					t.Fatal(err)
				}
				if fake.request == nil || !reflect.DeepEqual(fake.request.GhArgs, args) || fake.request.WorkingDirectory != wd || fake.request.GhHost != "github.com" || fake.request.GhRepo != "owner/repo" || !fake.deadline {
					t.Fatalf("request=%v deadline=%v", fake.request, fake.deadline)
				}
			}
		})
	}
}

func TestGHReadCredentialWithoutLocalFactory(t *testing.T) {
	group := New(func(*cobra.Command) (Client, error) { t.Fatal("general client factory used"); return nil, nil }, Options{})
	cmd := group.ghReadCredentialCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"--", "api", "user"})
	if err := cmd.Execute(); err != nil || out.Len() != 0 {
		t.Fatalf("err=%v stdout=%q", err, out.String())
	}
}
