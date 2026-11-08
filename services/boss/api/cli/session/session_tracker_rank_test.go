package session

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/recurser/boss/api/cli/cliutil"
	"github.com/recurser/boss/internal/domain/rpc"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/spf13/cobra"
	"google.golang.org/protobuf/types/known/timestamppb"
)

type rankCommandClient struct {
	setTrackerClient
	rank *pb.TrackerRank
}

type rankReadClient struct {
	Client
	session *pb.Session
}

func (c *rankReadClient) ListSessions(context.Context, *pb.ListSessionsRequest, rpc.SessionReadOptions) ([]*pb.Session, error) {
	return []*pb.Session{c.session}, nil
}

func (c *rankReadClient) GetSession(context.Context, string, rpc.SessionReadOptions) (*pb.Session, error) {
	return c.session, nil
}

func TestSessionRankJSONReadSurfaces_Sessions_AC238801(t *testing.T) {
	deadline := "2026-11-30"
	for _, kind := range []string{"dated", "undated", "absent"} {
		for _, surface := range []string{"ls", "show"} {
			t.Run(kind+"/"+surface, func(t *testing.T) {
				rank := &pb.TrackerRank{Tier: 1, TierSource: "own", DeadlineOn: &deadline, DeadlineSource: "project", IsScheduled: true, UnblockedTicketCount: 3, RankedAt: timestamppb.New(time.Date(2026, 11, 7, 1, 2, 3, 0, time.UTC))}
				want := map[string]any{"tier": float64(1), "tierSource": "own", "deadlineOn": deadline, "deadlineSource": "project", "isScheduled": true, "unblockedTicketCount": float64(3), "rankedAt": "2026-11-07T01:02:03Z"}
				switch kind {
				case "undated":
					rank.DeadlineOn, rank.DeadlineSource, rank.IsScheduled, rank.UnblockedTicketCount = nil, "", false, 0
					delete(want, "deadlineOn")
					want["deadlineSource"], want["isScheduled"], want["unblockedTicketCount"] = "", false, float64(0)
				case "absent":
					rank, want = nil, nil
				}
				client := &rankReadClient{session: &pb.Session{Id: trackerTestSessionID, TrackerRank: rank}}
				g := New(func(*cobra.Command) (Client, error) { return client, nil }, Options{})
				cmd, args := g.LSCmd(), []string{"--json"}
				if surface == "show" {
					cmd, args = g.ShowCmd(), []string{trackerTestSessionID, "--json"}
				}
				var out bytes.Buffer
				cmd.SetOut(&out)
				cmd.SetArgs(args)
				if err := cmd.Execute(); err != nil {
					t.Fatal(err)
				}
				var envelope struct {
					Sessions []map[string]json.RawMessage `json:"sessions"`
					Session  map[string]json.RawMessage   `json:"session"`
				}
				if err := json.Unmarshal(out.Bytes(), &envelope); err != nil {
					t.Fatal(err)
				}
				row := envelope.Session
				if surface == "ls" {
					if len(envelope.Sessions) != 1 {
						t.Fatalf("sessions=%s", out.String())
					}
					row = envelope.Sessions[0]
				}
				value, present := row["trackerRank"]
				if present != (want != nil) {
					t.Fatalf("rank presence=%v: %s", present, out.String())
				}
				if !present {
					return
				}
				var got map[string]any
				if err := json.Unmarshal(value, &got); err != nil {
					t.Fatal(err)
				}
				if !reflect.DeepEqual(got, want) {
					t.Fatalf("rank=%v want=%v", got, want)
				}
			})
		}
	}
}

func (c *rankCommandClient) SetSessionTrackerRank(_ context.Context, id, ticket string, rank *pb.TrackerRank) (*pb.Session, error) {
	c.rank = rank
	c.sessionID = id
	c.trackerID = ticket
	return &pb.Session{Id: id, TrackerId: &ticket, TrackerRank: rank}, nil
}

func TestSessionSetRank_Sessions_AC238801(t *testing.T) {
	for _, stdin := range []bool{false, true} {
		t.Run(map[bool]string{false: "argument", true: "stdin"}[stdin], func(t *testing.T) {
			client := &rankCommandClient{}
			cmd := New(func(*cobra.Command) (Client, error) { return client, nil }, Options{}).sessionSetRankCmd()
			payload := `{"tier":1,"tierFrom":"blocks project O'Brien's tools","deadline":"2026-11-30","deadlineFrom":"project","scheduled":true,"leverage":3}`
			arg := payload
			if stdin {
				arg = "-"
				cmd.SetIn(strings.NewReader(payload))
			}
			var out bytes.Buffer
			cmd.SetOut(&out)
			cmd.SetArgs([]string{"0123", "bos-1", "--rank-json", arg})
			if err := cmd.Execute(); err != nil {
				t.Fatal(err)
			}
			r := client.rank
			if client.sessionID != trackerTestSessionID || client.trackerID != "BOS-1" || r.Tier != 1 || r.TierSource != "blocks project O'Brien's tools" || r.GetDeadlineOn() != "2026-11-30" || r.DeadlineSource != "project" || !r.IsScheduled || r.UnblockedTicketCount != 3 || r.RankedAt != nil {
				t.Fatalf("sent rank %+v to %s %s", r, client.sessionID, client.trackerID)
			}
			if out.String() != "Session "+trackerTestSessionID+" rank stored for BOS-1 (tier 1, 2026-11-30, unblocks 3).\n" {
				t.Fatal(out.String())
			}
		})
	}
}

func TestSessionSetRankValidationBeforeDial_Sessions_AC238802(t *testing.T) {
	good := `{"tier":1,"tierFrom":"own","deadline":null,"deadlineFrom":null,"scheduled":false,"leverage":0}`
	for _, payload := range []string{`{`, `null`, `[]`, `1`, `{}`, good + ` {}`, strings.Replace(good, `"tier":1`, `"tier":7`, 1), strings.Replace(good, `"tier":1`, `"tier":1.5`, 1), strings.Replace(good, `"tier":1`, `"tier":null`, 1), strings.Replace(good, `"leverage":0`, `"leverage":-1`, 1), strings.Replace(good, `"leverage":0`, `"leverage":0.5`, 1), strings.Replace(good, `"deadline":null`, `"deadline":"2026-13-01"`, 1), strings.Replace(good, `"deadline":null`, `"deadline":true`, 1), strings.Replace(good, `"tier":1`, `"extra":1,"tier":1`, 1)} {
		t.Run(payload, func(t *testing.T) {
			dialed := false
			cmd := New(func(*cobra.Command) (Client, error) { dialed = true; return &rankCommandClient{}, nil }, Options{}).sessionSetRankCmd()
			cmd.SetArgs([]string{"0123", "BOS-1", "--rank-json", payload})
			err := cmd.Execute()
			if err == nil || dialed {
				t.Fatalf("err=%v dialed=%v", err, dialed)
			}
			if cliutil.ErrorCodeFor(err) != cliutil.CodeInvalidArgument {
				t.Fatalf("code=%v err=%v", cliutil.ErrorCodeFor(err), err)
			}
		})
	}
	cmd := New(func(*cobra.Command) (Client, error) { t.Fatal("dialed"); return nil, nil }, Options{}).sessionSetRankCmd()
	cmd.SetArgs([]string{"0123", "bad!", "--rank-json", good})
	if err := cmd.Execute(); err == nil {
		t.Fatal("accepted bad id")
	}
	client := &rankCommandClient{}
	cmd = New(func(*cobra.Command) (Client, error) { return client, nil }, Options{}).sessionSetRankCmd()
	cmd.SetArgs([]string{"0123", "BOS-1", "--rank-json", good})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	if client.rank.DeadlineOn != nil || client.rank.DeadlineSource != "" {
		t.Fatal(client.rank)
	}
}

// Exercise the rank producer against the same historical list-planned fixture
// as rank-work.test.mjs, then pass its object through the real command boundary.
func TestSessionSetRankListPlannedFixture_Sessions_AC238803(t *testing.T) {
	payloadBytes, err := os.ReadFile("../../../../../skills-toolbox/testdata/rank-work/list-planned-failing-run.json")
	if err != nil {
		t.Fatal(err)
	}
	var candidates []struct {
		Rank json.RawMessage `json:"rank"`
	}
	if err := json.Unmarshal(payloadBytes, &candidates); err != nil {
		t.Fatal(err)
	}
	payload := candidates[0].Rank
	var expected struct {
		Tier         int32   `json:"tier"`
		TierFrom     string  `json:"tierFrom"`
		Deadline     *string `json:"deadline"`
		DeadlineFrom *string `json:"deadlineFrom"`
		Scheduled    bool    `json:"scheduled"`
		Leverage     int32   `json:"leverage"`
	}
	if err := json.Unmarshal(payload, &expected); err != nil {
		t.Fatal(err)
	}
	client := &rankCommandClient{}
	cmd := New(func(*cobra.Command) (Client, error) { return client, nil }, Options{}).sessionSetRankCmd()
	cmd.SetArgs([]string{"0123", "BOS-1", "--rank-json", string(payload)})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	got := client.rank
	if got.Tier != expected.Tier || got.TierSource != expected.TierFrom || !reflect.DeepEqual(got.DeadlineOn, expected.Deadline) || got.IsScheduled != expected.Scheduled || got.UnblockedTicketCount != expected.Leverage {
		t.Fatalf("rank changed: input=%s sent=%+v", payload, got)
	}
	source := ""
	if expected.DeadlineFrom != nil {
		source = *expected.DeadlineFrom
	}
	if got.DeadlineSource != source {
		t.Fatalf("deadline source=%q want %q", got.DeadlineSource, source)
	}
}
