package lease

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/recurser/boss/api/cli/cliutil"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/spf13/cobra"
	"google.golang.org/protobuf/types/known/timestamppb"
)

type leaseCommandClient struct {
	acquired        *pb.AcquireTrackerIssueLeaseRequest
	released        *pb.ReleaseTrackerIssueLeaseRequest
	response        *pb.AcquireTrackerIssueLeaseResponse
	releaseResponse *pb.ReleaseTrackerIssueLeaseResponse
	err             error
}

func (f *leaseCommandClient) AcquireTrackerIssueLease(_ context.Context, r *pb.AcquireTrackerIssueLeaseRequest) (*pb.AcquireTrackerIssueLeaseResponse, error) {
	f.acquired = r
	return f.response, f.err
}
func (f *leaseCommandClient) ReleaseTrackerIssueLease(_ context.Context, r *pb.ReleaseTrackerIssueLeaseRequest) (*pb.ReleaseTrackerIssueLeaseResponse, error) {
	f.released = r
	return f.releaseResponse, f.err
}

func executeLease(t *testing.T, f *leaseCommandClient, args ...string) (string, string, int) {
	t.Helper()
	root := &cobra.Command{Use: "boss", SilenceErrors: true, SilenceUsage: true}
	root.AddCommand(New(func(*cobra.Command) (Client, error) { return f, nil }))
	var out, stderr bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&stderr)
	root.SetArgs(args)
	err := root.Execute()
	err = cliutil.EmitRootJSONFailure(root, args, err)
	if err != nil && err.Error() != "" {
		stderr.WriteString(err.Error())
	}
	return out.String(), stderr.String(), ExitStatus(err)
}
func leaseArgs(action string) []string {
	return []string{"lease", action, "--issue", "linear:BOS-1", "--purpose", "plan", "--holder", strings.Repeat("a", 32), "--repo-origin", "https://example.com/repo.git", "--json"}
}

func TestAcquireResult(t *testing.T) {
	expiry := time.Date(2026, 11, 5, 14, 5, 0, 0, time.UTC)
	for _, tc := range []struct {
		name      string
		acquired  bool
		scope     pb.TrackerIssueLeaseScope
		code      int
		scopeText string
	}{
		{"granted", true, pb.TrackerIssueLeaseScope_TRACKER_ISSUE_LEASE_SCOPE_ORGANIZATION, 0, "organization"},
		{"held", false, pb.TrackerIssueLeaseScope_TRACKER_ISSUE_LEASE_SCOPE_DAEMON, 3, "daemon"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := &leaseCommandClient{response: &pb.AcquireTrackerIssueLeaseResponse{IsAcquired: tc.acquired, Scope: tc.scope, ExpiresAt: timestamppb.New(expiry)}}
			out, stderr, code := executeLease(t, f, leaseArgs("acquire")...)
			if code != tc.code || stderr != "" {
				t.Fatalf("code=%d stderr=%q", code, stderr)
			}
			var got map[string]any
			if err := json.Unmarshal([]byte(out), &got); err != nil {
				t.Fatal(err)
			}
			if len(got) != 3 || got["is_acquired"] != tc.acquired || got["scope"] != tc.scopeText || got["expires_at"] != "2026-11-05T14:05:00Z" {
				t.Fatalf("output=%s", out)
			}
			if f.acquired.GetTtlSeconds() != 7200 || f.acquired.GetIssueKey() != "linear:BOS-1" || f.acquired.GetHolderId() != strings.Repeat("a", 32) || f.acquired.GetRepoOriginUrl() != "https://example.com/repo.git" || f.acquired.GetPurpose() != pb.TrackerIssueLeasePurpose_TRACKER_ISSUE_LEASE_PURPOSE_PLAN {
				t.Fatalf("request=%v", f.acquired)
			}
		})
	}
}
func TestLeaseErrors(t *testing.T) {
	for _, action := range []string{"acquire", "release"} {
		t.Run(action, func(t *testing.T) {
			f := &leaseCommandClient{err: errors.New("daemon unreachable")}
			out, stderr, code := executeLease(t, f, leaseArgs(action)...)
			if code != 2 || out != "" || !strings.Contains(stderr, "daemon unreachable") {
				t.Fatalf("code=%d stdout=%q stderr=%q", code, out, stderr)
			}
		})
	}
}
func TestReleaseIdempotent(t *testing.T) {
	for _, released := range []bool{true, false} {
		f := &leaseCommandClient{releaseResponse: &pb.ReleaseTrackerIssueLeaseResponse{IsReleased: released}}
		out, stderr, code := executeLease(t, f, leaseArgs("release")...)
		var got map[string]any
		if err := json.Unmarshal([]byte(out), &got); err != nil {
			t.Fatal(err)
		}
		if code != 0 || stderr != "" || len(got) != 1 || got["is_released"] != true {
			t.Fatalf("code=%d stdout=%q stderr=%q", code, out, stderr)
		}
		if f.released.GetIssueKey() != "linear:BOS-1" || f.released.GetHolderId() != strings.Repeat("a", 32) || f.released.GetRepoOriginUrl() != "https://example.com/repo.git" || f.released.GetPurpose() != pb.TrackerIssueLeasePurpose_TRACKER_ISSUE_LEASE_PURPOSE_PLAN {
			t.Fatalf("request=%v", f.released)
		}
	}
}
func TestLeaseInputRejections(t *testing.T) {
	for _, action := range []string{"acquire", "release"} {
		for _, flag := range []string{"--issue", "--purpose", "--holder"} {
			t.Run(action+flag, func(t *testing.T) {
				args := leaseArgs(action)
				for i, v := range args {
					if v == flag {
						args = append(args[:i], args[i+2:]...)
						break
					}
				}
				f := &leaseCommandClient{}
				_, _, code := executeLease(t, f, args...)
				if code == 0 || f.acquired != nil || f.released != nil {
					t.Fatalf("code=%d client called", code)
				}
			})
		}
		t.Run(action+"-purpose", func(t *testing.T) {
			args := append(leaseArgs(action), "--purpose", "unknown")
			f := &leaseCommandClient{}
			_, _, code := executeLease(t, f, args...)
			if code == 0 || f.acquired != nil || f.released != nil {
				t.Fatalf("code=%d client called", code)
			}
		})
	}
	for _, ttl := range []string{"0", "0s", "-1s", "1ms", "2562047h"} {
		t.Run(ttl, func(t *testing.T) {
			f := &leaseCommandClient{}
			_, _, code := executeLease(t, f, append(leaseArgs("acquire"), "--ttl", ttl)...)
			if code == 0 || f.acquired != nil {
				t.Fatalf("code=%d request=%v", code, f.acquired)
			}
		})
	}
}
func TestLeaseTTLAndBuildPurpose(t *testing.T) {
	f := &leaseCommandClient{response: &pb.AcquireTrackerIssueLeaseResponse{IsAcquired: true}}
	out, _, code := executeLease(t, f, append(leaseArgs("acquire"), "--ttl", "90m", "--purpose", "build")...)
	if code != 0 || f.acquired.GetTtlSeconds() != 5400 || f.acquired.GetPurpose() != pb.TrackerIssueLeasePurpose_TRACKER_ISSUE_LEASE_PURPOSE_BUILD {
		t.Fatalf("code=%d request=%v", code, f.acquired)
	}
	var got map[string]any
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatal(err)
	}
	if got["expires_at"] != "" {
		t.Fatalf("output=%s", out)
	}
}
func TestLeaseOrigin(t *testing.T) {
	for _, withRepo := range []bool{false, true} {
		t.Run(map[bool]string{true: "git", false: "non-repo"}[withRepo], func(t *testing.T) {
			dir := t.TempDir()
			t.Chdir(dir)
			const origin = "git@example.com:team/repo.git"
			if withRepo {
				for _, args := range [][]string{{"-c", "commit.gpgsign=false", "init"}, {"config", "commit.gpgsign", "false"}, {"remote", "add", "origin", origin}} {
					cmd := exec.CommandContext(t.Context(), "git", args...)
					cmd.WaitDelay = time.Second
					if out, err := cmd.CombinedOutput(); err != nil {
						t.Fatalf("git: %v: %s", err, out)
					}
				}
			}
			for _, action := range []string{"acquire", "release"} {
				args := leaseArgs(action)
				for i, v := range args {
					if v == "--repo-origin" {
						args = append(args[:i], args[i+2:]...)
						break
					}
				}
				f := &leaseCommandClient{response: &pb.AcquireTrackerIssueLeaseResponse{IsAcquired: true}}
				_, _, code := executeLease(t, f, args...)
				if code != 0 {
					t.Fatalf("code=%d", code)
				}
				got := f.acquired.GetRepoOriginUrl()
				if action == "release" {
					got = f.released.GetRepoOriginUrl()
				}
				want := ""
				if withRepo {
					want = origin
				}
				if got != want {
					t.Fatalf("origin=%q want=%q", got, want)
				}
			}
		})
	}
}

func TestLeaseHumanOutput(t *testing.T) {
	expiry := timestamppb.New(time.Date(2026, 11, 5, 14, 5, 0, 0, time.UTC))
	for _, tc := range []struct {
		action   string
		acquired bool
		want     string
	}{
		{"acquire", true, "acquired linear:BOS-1 (plan) until 14:05, organization scope\n"},
		{"acquire", false, "held by another run until 14:05\n"},
		{"release", false, "released linear:BOS-1 (plan)\n"},
	} {
		args := leaseArgs(tc.action)
		args = args[:len(args)-1]
		f := &leaseCommandClient{response: &pb.AcquireTrackerIssueLeaseResponse{IsAcquired: tc.acquired, ExpiresAt: expiry, Scope: pb.TrackerIssueLeaseScope_TRACKER_ISSUE_LEASE_SCOPE_ORGANIZATION}}
		out, stderr, _ := executeLease(t, f, args...)
		if out != tc.want || stderr != "" {
			t.Fatalf("stdout=%q stderr=%q", out, stderr)
		}
	}
}
