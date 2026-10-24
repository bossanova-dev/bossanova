package diagnostics

import (
	"bytes"
	"context"
	"reflect"

	"github.com/recurser/boss/internal/domain/rpc"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/spf13/cobra"

	"os"
	"path/filepath"
	"testing"
)

func newTestGroup() *Group {
	return New(nil, Options{CLICommandPaths: func() []string { return []string{"boss env", "boss ls"} }})
}
func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "boss-diagnostics-settings-*")
	if err != nil {
		panic(err)
	}
	if err := os.Setenv("BOSS_SETTINGS_PATH", filepath.Join(dir, "settings.json")); err != nil {
		panic(err)
	}
	code := m.Run()
	_ = os.RemoveAll(dir)
	os.Exit(code)
}

func TestGroupClientFactoriesAreIndependent(t *testing.T) {
	calls := []string{}
	first := New(func(cmd *cobra.Command) (Client, error) {
		calls = append(calls, "first:"+cmd.Name())
		return repairFake{detail: "first"}, nil
	}, Options{})
	second := New(func(cmd *cobra.Command) (Client, error) {
		calls = append(calls, "second:"+cmd.Name())
		return repairFake{detail: "second"}, nil
	}, Options{})
	if len(calls) != 0 {
		t.Fatal("construction called a client factory")
	}
	for _, tc := range []struct {
		group *Group
		want  string
	}{{first, "first"}, {second, "second"}, {first, "first"}} {
		cmd := tc.group.repairCmd()
		var out bytes.Buffer
		cmd.SetOut(&out)
		cmd.SetArgs([]string{"start"})
		if err := cmd.Execute(); err != nil {
			t.Fatal(err)
		}
		if want := "Auto-repair armed: " + tc.want + "\n"; out.String() != want {
			t.Fatalf("output = %q, want %q", out.String(), want)
		}
	}
	if !reflect.DeepEqual(calls, []string{"first:start", "second:start", "first:start"}) {
		t.Fatalf("factory calls = %v", calls)
	}
}

type repairFake struct{ detail string }

func (repairFake) ListSessions(context.Context, *pb.ListSessionsRequest, rpc.SessionReadOptions) ([]*pb.Session, error) {
	return nil, nil
}
func (repairFake) RepairDoctor(context.Context) (*pb.RepairDoctorResponse, error) {
	return &pb.RepairDoctorResponse{}, nil
}
func (f repairFake) StartRepairWorkflow(context.Context) (*pb.StartRepairWorkflowResponse, error) {
	return &pb.StartRepairWorkflowResponse{Detail: f.detail}, nil
}
