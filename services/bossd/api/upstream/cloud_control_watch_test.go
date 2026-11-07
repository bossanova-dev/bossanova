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
