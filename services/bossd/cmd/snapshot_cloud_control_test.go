package main

import (
	"context"
	"path/filepath"
	"testing"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/kit/config"
	"github.com/recurser/bossd/api/upstream"
	"github.com/rs/zerolog"
)

func TestSnapshotForPublishCloudControl(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")
	t.Setenv("BOSS_SETTINGS_PATH", path)
	policy := upstream.NewCloudControlPolicy(zerolog.Nop())
	for _, raw := range []string{"", "full", "read-only", "private"} {
		if err := config.SaveTo(path, config.Settings{CloudControl: raw}); err != nil {
			t.Fatal(err)
		}
		snap, err := buildSnapshotForPublish(context.Background(), upstream.StreamStores{}, "daemon", "host", policy, nil)
		if err != nil {
			t.Fatal(err)
		}
		want := pb.CloudControl_CLOUD_CONTROL_READ_ONLY
		if raw == "" || raw == "full" {
			want = pb.CloudControl_CLOUD_CONTROL_FULL
		}
		if raw == "private" {
			want = pb.CloudControl_CLOUD_CONTROL_PRIVATE
		}
		if snap.GetCloudControl() != want {
			t.Fatalf("raw=%q snapshot=%v", raw, snap.GetCloudControl())
		}
	}
	snap, err := buildSnapshotForPublish(context.Background(), upstream.StreamStores{}, "daemon", "host", nil, nil)
	if err != nil || snap.GetCloudControl() != pb.CloudControl_CLOUD_CONTROL_READ_ONLY {
		t.Fatalf("nil policy snapshot=%v err=%v", snap, err)
	}
}
