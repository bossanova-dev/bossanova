package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"connectrpc.com/connect"
	"github.com/recurser/boss/internal/adapter/daemontest"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/gen/bossanova/v1/bossanovav1connect"
)

type leaseWiringDaemon struct {
	bossanovav1connect.UnimplementedDaemonServiceHandler
	isHeld   bool
	err      error
	acquired *pb.AcquireTrackerIssueLeaseRequest
	released *pb.ReleaseTrackerIssueLeaseRequest
}

func (d *leaseWiringDaemon) AcquireTrackerIssueLease(_ context.Context, req *connect.Request[pb.AcquireTrackerIssueLeaseRequest]) (*connect.Response[pb.AcquireTrackerIssueLeaseResponse], error) {
	d.acquired = req.Msg
	if d.err != nil {
		return nil, d.err
	}
	return connect.NewResponse(&pb.AcquireTrackerIssueLeaseResponse{IsAcquired: !d.isHeld, Scope: pb.TrackerIssueLeaseScope_TRACKER_ISSUE_LEASE_SCOPE_DAEMON}), nil
}

func (d *leaseWiringDaemon) ReleaseTrackerIssueLease(_ context.Context, req *connect.Request[pb.ReleaseTrackerIssueLeaseRequest]) (*connect.Response[pb.ReleaseTrackerIssueLeaseResponse], error) {
	d.released = req.Msg
	if d.err != nil {
		return nil, d.err
	}
	return connect.NewResponse(&pb.ReleaseTrackerIssueLeaseResponse{}), nil
}

// executeBoss runs the real boss command tree the way main does and returns
// stdout and the process exit status main would use.
func executeBoss(t *testing.T, socket string, args ...string) (string, int) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(home, ".local", "state"))
	t.Setenv("BOSS_SETTINGS_PATH", filepath.Join(home, "settings.json"))
	t.Setenv("BOSS_SKIP_SKILLS", "1")
	t.Setenv("BOSS_SOCKET", socket)
	root := rootCmdWithGroups(newSkillsGroup(), &fakeDaemonCommands{})
	var stdout, stderr bytes.Buffer
	root.SetOut(&stdout)
	root.SetErr(&stderr)
	root.SetArgs(args)
	err := root.Execute()
	if err == nil {
		return stdout.String(), 0
	}
	return stdout.String(), exitStatus(err)
}

func leaseCommand(action string) []string {
	return []string{"lease", action, "--issue", "linear:BOS-1", "--purpose", "build", "--holder", strings.Repeat("a", 32), "--repo-origin", "https://example.com/repo.git", "--json"}
}

func TestBossLeaseExitStatuses_ScheduledJobs_AC227301(t *testing.T) {
	daemon := &leaseWiringDaemon{}
	socket := daemontest.Serve(t, daemon)

	stdout, status := executeBoss(t, socket, leaseCommand("acquire")...)
	var acquired map[string]any
	if status != 0 || json.Unmarshal([]byte(stdout), &acquired) != nil || acquired["is_acquired"] != true || acquired["scope"] != "daemon" {
		t.Fatalf("granted: status=%d stdout=%q", status, stdout)
	}
	if daemon.acquired.GetIssueKey() != "linear:BOS-1" || daemon.acquired.GetPurpose() != pb.TrackerIssueLeasePurpose_TRACKER_ISSUE_LEASE_PURPOSE_BUILD || daemon.acquired.GetTtlSeconds() != 7200 {
		t.Fatalf("acquire request = %v", daemon.acquired)
	}

	daemon.isHeld = true
	stdout, status = executeBoss(t, socket, leaseCommand("acquire")...)
	var held map[string]any
	if status != 3 || json.Unmarshal([]byte(stdout), &held) != nil || held["is_acquired"] != false {
		t.Fatalf("held: status=%d stdout=%q", status, stdout)
	}

	for range 2 {
		stdout, status = executeBoss(t, socket, leaseCommand("release")...)
		var released map[string]any
		if status != 0 || json.Unmarshal([]byte(stdout), &released) != nil || released["is_released"] != true {
			t.Fatalf("release: status=%d stdout=%q", status, stdout)
		}
	}

	daemon.err = connect.NewError(connect.CodeInvalidArgument, errors.New("holder must be 32 hexadecimal characters"))
	stdout, status = executeBoss(t, socket, leaseCommand("acquire")...)
	if status != 2 || stdout != "" {
		t.Fatalf("error: status=%d stdout=%q", status, stdout)
	}

	stdout, status = executeBoss(t, filepath.Join(t.TempDir(), "absent.sock"), leaseCommand("release")...)
	if status != 2 || stdout != "" {
		t.Fatalf("unreachable daemon: status=%d stdout=%q", status, stdout)
	}

	daemon.err = nil
	daemon.acquired = nil
	stdout, status = executeBoss(t, socket, append([]string{"--remote", "https://example.invalid"}, leaseCommand("acquire")...)...)
	if status != 2 || stdout != "" || daemon.acquired != nil {
		t.Fatalf("--remote: status=%d stdout=%q acquired=%v", status, stdout, daemon.acquired)
	}
}
