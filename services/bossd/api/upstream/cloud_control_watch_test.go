package upstream

import (
	"context"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/kit/config"
	"github.com/rs/zerolog"
)

type switchableCloudControl struct{ mode atomic.Value }

func (p *switchableCloudControl) Mode() config.CloudControlMode {
	return p.mode.Load().(config.CloudControlMode)
}

func pendingEvent(out <-chan *pb.DaemonEvent) *pb.DaemonEvent {
	select {
	case ev := <-out:
		return ev
	default:
		return nil
	}
}

// bosso may refuse from the advertised mode only because the daemon re-sends
// its snapshot within one poll of a local settings change, in both directions.
func TestWatchCloudControlResnapshotsOnLocalChange(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		policy := &switchableCloudControl{}
		policy.mode.Store(config.CloudControlReadOnly)
		client := NewStreamClient(StreamClientConfig{Logger: zerolog.Nop(), CloudControl: policy, Hostname: "mac-mini"})
		connect, err := client.buildSnapshot(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		if !connect.GetHasCloudControlUpdates() || connect.GetCloudControl() != pb.CloudControl_CLOUD_CONTROL_READ_ONLY {
			t.Fatalf("connect snapshot = %v", connect)
		}
		ctx, cancel := context.WithCancel(t.Context())
		out := make(chan *pb.DaemonEvent, 4)
		done := make(chan struct{})
		go func() {
			defer close(done)
			client.watchCloudControl(ctx, connect.GetCloudControl(), out)
		}()

		time.Sleep(3 * defaultCloudControlPollInterval)
		synctest.Wait()
		if ev := pendingEvent(out); ev != nil {
			t.Fatalf("unchanged mode re-sent %v", ev)
		}
		for _, step := range []struct {
			mode config.CloudControlMode
			want pb.CloudControl
		}{
			{config.CloudControlFull, pb.CloudControl_CLOUD_CONTROL_FULL},
			{config.CloudControlReadOnly, pb.CloudControl_CLOUD_CONTROL_READ_ONLY},
		} {
			policy.mode.Store(step.mode)
			time.Sleep(defaultCloudControlPollInterval)
			synctest.Wait()
			snap := pendingEvent(out).GetSnapshot()
			if snap.GetCloudControl() != step.want || !snap.GetHasCloudControlUpdates() || snap.GetHostname() != "mac-mini" {
				t.Fatalf("after %s: snapshot = %v", step.mode, snap)
			}
			time.Sleep(3 * defaultCloudControlPollInterval)
			synctest.Wait()
			if ev := pendingEvent(out); ev != nil {
				t.Fatalf("after %s: repeated %v", step.mode, ev)
			}
		}
		cancel()
		<-done
	})
}

func (p *switchableCloudControl) RepoMode(context.Context, string) (config.CloudControlMode, config.CloudControlSource) {
	return p.Mode(), config.CloudControlSourceMachine
}

func TestWatchCloudControlRepoDowngrade(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		policy := &switchableCloudControl{}
		policy.mode.Store(config.CloudControlFull)
		client, _, targets := targetClient()
		client.cloudControl = &repoSwitchPolicy{repo: policy}
		client.targetObjects = targets
		client.stores = StreamStores{Sessions: &fakeSessionReader{sessions: []*pb.Session{{Id: "sa", RepoId: "a", Plan: "secret"}}}}
		initial, err := client.repoCloudControl(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		out := make(chan *pb.DaemonEvent, 4)
		done := make(chan struct{})
		go func() {
			defer close(done)
			client.watchCloudControl(ctx, pb.CloudControl_CLOUD_CONTROL_FULL, out, initial)
		}()
		synctest.Wait()
		// Keep the advertised machine value fixed while only the repo authority changes.
		policy.mode.Store(config.CloudControlPrivate)
		time.Sleep(defaultCloudControlPollInterval)
		synctest.Wait()
		snap := pendingEvent(out).GetSnapshot()
		if snap == nil || snap.CloudControl != pb.CloudControl_CLOUD_CONTROL_FULL || len(snap.Sessions) != 1 || snap.Sessions[0].Plan != "" {
			t.Fatalf("downgrade did not replace content: %v", snap)
		}
		cancel()
		<-done
	})
}

type repoSwitchPolicy struct{ repo *switchableCloudControl }

func (p *repoSwitchPolicy) Mode() config.CloudControlMode { return config.CloudControlFull }
func (p *repoSwitchPolicy) RepoMode(ctx context.Context, id string) (config.CloudControlMode, config.CloudControlSource) {
	return p.repo.RepoMode(ctx, id)
}
