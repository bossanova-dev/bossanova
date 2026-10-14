package main

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"connectrpc.com/connect"
	"github.com/recurser/boss/internal/client"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/spf13/cobra"
)

const trackerTestSessionID = "0123456789abcdef0123456789abcdef"

type setTrackerClient struct {
	client.BossClient
	calls                int
	sessionID, trackerID string
}

func (c *setTrackerClient) ListSessions(context.Context, *pb.ListSessionsRequest, client.SessionReadOptions) ([]*pb.Session, error) {
	return []*pb.Session{{Id: trackerTestSessionID}, {Id: "fedcba9876543210fedcba9876543210"}}, nil
}

func (c *setTrackerClient) SetSessionTracker(_ context.Context, id, trackerID string) (*pb.Session, error) {
	c.calls++
	c.sessionID, c.trackerID = id, trackerID
	url := "https://linear.app/acme/issue/" + trackerID + "/title"
	return &pb.Session{Id: id, TrackerId: &trackerID, TrackerUrl: &url}, nil
}

func runSetTrackerCmd(t *testing.T, factory func(*cobra.Command) (client.BossClient, error), args ...string) (string, error) {
	t.Helper()
	cmd := sessionSetTrackerCmd(factory)
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SilenceUsage = true
	cmd.SilenceErrors = true
	cmd.SetArgs(args)
	err := cmd.Execute()
	return out.String(), err
}

func TestSessionSetTracker_LinksByPrefixAndPrints(t *testing.T) {
	c := &setTrackerClient{}
	out, err := runSetTrackerCmd(t, func(*cobra.Command) (client.BossClient, error) { return c, nil }, "0123", "bos-1539")
	if err != nil {
		t.Fatalf("set-tracker: %v", err)
	}
	if c.calls != 1 || c.sessionID != trackerTestSessionID || c.trackerID != "BOS-1539" {
		t.Fatalf("call = %d (%q, %q)", c.calls, c.sessionID, c.trackerID)
	}
	want := "Session " + trackerTestSessionID + " linked to BOS-1539 (https://linear.app/acme/issue/BOS-1539/title).\n"
	if out != want {
		t.Fatalf("output = %q, want %q", out, want)
	}
}

func TestSessionSetTracker_RejectsBadIDBeforeDialing(t *testing.T) {
	dialed := 0
	_, err := runSetTrackerCmd(t, func(*cobra.Command) (client.BossClient, error) {
		dialed++
		return &setTrackerClient{}, nil
	}, "0123", "not an id")
	if err == nil || !strings.Contains(err.Error(), "not a tracker issue id") {
		t.Fatalf("err = %v, want a bad-id error", err)
	}
	if dialed != 0 {
		t.Fatalf("dialed the daemon %d times before validating the id", dialed)
	}
}

func TestSessionSetTracker_RemoteModeIsUnimplemented(t *testing.T) {
	remote := client.NewRemote("http://127.0.0.1:1", "token")
	_, err := remote.SetSessionTracker(context.Background(), trackerTestSessionID, "BOS-1539")
	if connect.CodeOf(err) != connect.CodeUnimplemented {
		t.Fatalf("code = %v (err %v), want Unimplemented", connect.CodeOf(err), err)
	}

	_, err = runSetTrackerCmd(t, func(*cobra.Command) (client.BossClient, error) { return remote, nil }, trackerTestSessionID, "BOS-1539")
	if err == nil || !strings.Contains(err.Error(), "only available against a local daemon") {
		t.Fatalf("err = %v, want the local-daemon-only message", err)
	}
}
