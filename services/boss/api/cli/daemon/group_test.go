package daemon

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/recurser/boss/internal/termreset"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/spf13/cobra"
)

func newTestGroup() *Group {
	deps := New(nil)
	deps.daemonAuthStateProbe = func(context.Context) (*pb.GetAuthStateResponse, error) {
		return nil, errors.New("daemon auth probe is stubbed out in this test package")
	}
	deps.daemonLocalSupervisionObservation = func() daemonSupervisionObservation {
		return daemonSupervisionObservation{Stop: daemonSupervisionStopNoStateRecord}
	}
	return deps
}
func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "boss-daemon-settings-*")
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

// This root exercises the same exported warning hooks that cmd wires before commands.
func (deps *Group) warningRoot() *cobra.Command {
	root := &cobra.Command{Use: "boss", PersistentPreRunE: func(cmd *cobra.Command, args []string) error {
		deps.warnIfDaemonBinaryStale(cmd)
		deps.warnIfBossBinaryBehindCheckout(cmd)
		deps.warnIfDaemonUnsupervised(cmd)
		return nil
	}}
	root.AddCommand(&cobra.Command{Use: "fix-terminal", RunE: func(cmd *cobra.Command, args []string) error {
		_, err := cmd.OutOrStdout().Write([]byte(termreset.AbnormalExitReset))
		return err
	}})
	root.AddCommand(deps.daemonCmd())
	return root
}

// The command is passed at execution time and constructing a second group cannot replace its factory.
func TestGroupClientFactoriesAreIndependent(t *testing.T) {
	firstCalls, secondCalls := 0, 0
	cmd := &cobra.Command{Use: "doctor"}
	first := New(func(got *cobra.Command) (Client, error) {
		if got != cmd {
			t.Fatal("factory did not receive the executing command")
		}
		firstCalls++
		return authStateFake{}, nil
	})
	second := New(func(got *cobra.Command) (Client, error) {
		if got != cmd {
			t.Fatal("factory did not receive the executing command")
		}
		secondCalls++
		return authStateFake{}, nil
	})
	if firstCalls != 0 || secondCalls != 0 {
		t.Fatal("construction called a client factory")
	}
	if _, err := first.authState(context.Background(), cmd); err != nil {
		t.Fatal(err)
	}
	if firstCalls != 1 || secondCalls != 0 {
		t.Fatalf("factory calls = %d, %d", firstCalls, secondCalls)
	}
	if _, err := second.authState(context.Background(), cmd); err != nil {
		t.Fatal(err)
	}
	if firstCalls != 1 || secondCalls != 1 {
		t.Fatalf("factory calls = %d, %d", firstCalls, secondCalls)
	}
}

type authStateFake struct{}

func (authStateFake) GetAuthState(context.Context) (*pb.GetAuthStateResponse, error) {
	return &pb.GetAuthStateResponse{}, nil
}

func TestSupervisionExposesOnlyAnEstablishedUnsupervisedVerdict(t *testing.T) {
	for _, tc := range []struct {
		name        string
		observation daemonSupervisionObservation
		want        bool
	}{
		{"unsupervised", daemonSupervisionObservation{Stop: daemonSupervisionStopNone, Verdict: daemonSupervisionUnsupervised, RecordedPID: 4242}, true},
		{"supervised", daemonSupervisionObservation{Stop: daemonSupervisionStopNone, Verdict: daemonSupervisionSupervised, RecordedPID: 4242}, false},
		{"unknown", daemonSupervisionObservation{Stop: daemonSupervisionStopNone, Verdict: daemonSupervisionUnknown, RecordedPID: 4242}, false},
		{"stopped", daemonSupervisionObservation{Stop: daemonSupervisionStopNotRunning, Verdict: daemonSupervisionUnsupervised, RecordedPID: 4242}, false},
		{"disabled", daemonSupervisionObservation{Stop: daemonSupervisionStopProbingDisabled}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			deps := newTestGroup()
			deps.daemonLocalSupervisionObservation = func() daemonSupervisionObservation { return tc.observation }
			got, pid := deps.Supervision()
			if got != tc.want || pid != tc.observation.RecordedPID {
				t.Fatalf("supervision = %v, %d; want %v, %d", got, pid, tc.want, tc.observation.RecordedPID)
			}
		})
	}
}
