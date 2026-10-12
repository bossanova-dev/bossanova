package main

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/recurser/boss/internal/client"
	"github.com/recurser/boss/internal/tuitest"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

// runCronAgainst drives the real `boss cron` command tree through the real
// LocalClient against an in-process mock daemon.
func runCronAgainst(t *testing.T, daemon *tuitest.MockDaemon, args ...string) string {
	t.Helper()
	t.Setenv("BOSS_SOCKET", daemon.SocketPath())
	cmd := cronCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs(args)
	if err := cmd.Execute(); err != nil {
		t.Fatalf("boss cron %v: %v\n%s", args, err, out.String())
	}
	return out.String()
}

// TestCronShowManagedBy pins how the config-ownership marker reaches an
// operator: `boss cron show` prints a `Managed by:` line and `--json` carries
// managed_by only for a marked job, an unmarked job's output is unchanged, and
// the mock daemon round-trips the marker through create and a field-less
// update.
func TestCronShowManagedBy(t *testing.T) {
	const marker = "boss-project:bossanova"
	d := tuitest.NewMockDaemon(t)
	c := client.NewLocal(d.SocketPath())
	ctx := context.Background()

	marked, err := c.CreateCronJob(ctx, &pb.CreateCronJobRequest{
		RepoId: "repo-1", Name: "Managed", Prompt: "do it", Schedule: "@daily", ManagedBy: marker,
	})
	if err != nil {
		t.Fatalf("CreateCronJob(marked): %v", err)
	}
	if marked.GetManagedBy() != marker {
		t.Fatalf("mock create echoed ManagedBy = %q, want %q", marked.GetManagedBy(), marker)
	}
	unmarked, err := c.CreateCronJob(ctx, &pb.CreateCronJobRequest{
		RepoId: "repo-1", Name: "Hand-made", Prompt: "do it", Schedule: "@daily",
	})
	if err != nil {
		t.Fatalf("CreateCronJob(unmarked): %v", err)
	}

	t.Run("marked text", func(t *testing.T) {
		out := runCronAgainst(t, d, "show", marked.GetId())
		if !strings.Contains(out, "Managed by:          "+marker+"\n") {
			t.Fatalf("show output missing Managed by line:\n%s", out)
		}
	})

	t.Run("marked json", func(t *testing.T) {
		out := runCronAgainst(t, d, "show", marked.GetId(), "--json")
		var got map[string]any
		if err := json.Unmarshal([]byte(out), &got); err != nil {
			t.Fatalf("show --json is not JSON: %v\n%s", err, out)
		}
		if got["managed_by"] != marker {
			t.Fatalf("managed_by = %v, want %q", got["managed_by"], marker)
		}
	})

	t.Run("unmarked omits the marker", func(t *testing.T) {
		out := runCronAgainst(t, d, "show", unmarked.GetId())
		if strings.Contains(out, "Managed by") {
			t.Fatalf("unmarked show printed a Managed by line:\n%s", out)
		}
		out = runCronAgainst(t, d, "show", unmarked.GetId(), "--json")
		var got map[string]any
		if err := json.Unmarshal([]byte(out), &got); err != nil {
			t.Fatalf("show --json is not JSON: %v\n%s", err, out)
		}
		if _, ok := got["managed_by"]; ok {
			t.Fatalf("unmarked show --json carries managed_by: %v", got["managed_by"])
		}
	})

	t.Run("ls json", func(t *testing.T) {
		out := runCronAgainst(t, d, "ls", "--json")
		var got []map[string]any
		if err := json.Unmarshal([]byte(out), &got); err != nil {
			t.Fatalf("ls --json is not JSON: %v\n%s", err, out)
		}
		for _, j := range got {
			v, ok := j["managed_by"]
			switch j["id"] {
			case marked.GetId():
				if v != marker {
					t.Errorf("ls marked managed_by = %v, want %q", v, marker)
				}
			case unmarked.GetId():
				if ok {
					t.Errorf("ls unmarked carries managed_by: %v", v)
				}
			}
		}
	})

	t.Run("update without the field keeps the marker", func(t *testing.T) {
		prompt := "do it again"
		updated, err := c.UpdateCronJob(ctx, &pb.UpdateCronJobRequest{Id: marked.GetId(), Prompt: &prompt})
		if err != nil {
			t.Fatalf("UpdateCronJob: %v", err)
		}
		if updated.GetManagedBy() != marker {
			t.Fatalf("after field-less update ManagedBy = %q, want %q", updated.GetManagedBy(), marker)
		}
		got, err := c.GetCronJob(ctx, marked.GetId())
		if err != nil {
			t.Fatalf("GetCronJob: %v", err)
		}
		if got.GetManagedBy() != marker {
			t.Fatalf("get after field-less update ManagedBy = %q, want %q", got.GetManagedBy(), marker)
		}
	})
}
