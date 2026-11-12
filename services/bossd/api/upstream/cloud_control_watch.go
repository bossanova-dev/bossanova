package upstream

import (
	"context"
	"maps"
	"time"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

// defaultCloudControlPollInterval bounds how long bosso can hold a stale
// cloud_control mode after a local settings edit. The snapshot that follows a
// change advertises has_cloud_control_updates, which is what lets bosso refuse
// control commands without forwarding them.
const defaultCloudControlPollInterval = time.Second

func (c *StreamClient) cloudControlPollInterval() time.Duration {
	if c.cloudControlPoll > 0 {
		return c.cloudControlPoll
	}
	return defaultCloudControlPollInterval
}

// watchCloudControl re-sends the daemon snapshot whenever the locally resolved
// cloud_control mode differs from the last one this stream advertised. The
// operator edits settings.json from the TUI, the CLI or an editor, none of
// which reach this process, so the watcher reads the same policy the command
// dispatcher reads. advertised is the mode the connect snapshot carried.
//
// A failed build keeps the old advertised mode, so the next tick retries.
//
// It ticks on wall time, not c.clock: tests that drive the reconnect backoff
// on the injected fake clock wait for a timer of at least that backoff, and a
// poll timer on the same clock would satisfy that wait early.
func (c *StreamClient) watchCloudControl(ctx context.Context, advertised pb.CloudControl, outbound chan<- *pb.DaemonEvent, initial ...map[string]pb.CloudControl) {
	var previous map[string]pb.CloudControl
	if len(initial) > 0 {
		previous = initial[0]
	} else {
		previous, _ = c.repoCloudControl(ctx)
	}
	ticker := time.NewTicker(c.cloudControlPollInterval())
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		current, err := c.repoCloudControl(ctx)
		if err != nil {
			continue
		}
		if cloudControlProto(c.cloudControl) == advertised && maps.Equal(previous, current) {
			continue
		}
		// The delta orders the per-repo change; the snapshot that follows also
		// refreshes Session.cloud_control and the content redaction.
		if !c.sendRepoCloudControlDelta(ctx, outbound, previous, current) {
			return
		}
		snap, err := c.buildSnapshot(ctx)
		if err != nil {
			c.logger.Warn().Err(err).Msg("cloud control changed; snapshot rebuild failed, retrying")
			continue
		}
		select {
		case outbound <- &pb.DaemonEvent{Event: &pb.DaemonEvent_Snapshot{Snapshot: snap}}:
			c.logger.Info().Str("cloud_control", snap.GetCloudControl().String()).Msg("cloud control changed; snapshot re-sent")
			advertised = snap.GetCloudControl()
			previous = current
		case <-ctx.Done():
			return
		}
	}
}

func (c *StreamClient) repoCloudControl(ctx context.Context) (map[string]pb.CloudControl, error) {
	ids, err := c.cloudControlRepoIDs(ctx)
	if err != nil {
		return nil, err
	}
	modes := make(map[string]pb.CloudControl, len(ids))
	redactor := c.newCloudRedactor()
	for _, id := range ids {
		modes[id] = redactor.mode(ctx, id)
	}
	return modes, nil
}

// cloudControlRepoIDs lists the registered repositories by database id, the
// key grants, sessions and bosso's command targets use. The snapshot's RepoIds
// are origin URLs for webhook routing, so they cannot key cloud control.
func (c *StreamClient) cloudControlRepoIDs(ctx context.Context) ([]string, error) {
	if c.targetObjects == nil {
		if c.stores.Repos == nil {
			return nil, nil
		}
		return c.stores.Repos.SnapshotRepoIDs(ctx)
	}
	repos, err := c.targetObjects.ListTargetRepos(ctx)
	if err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(repos))
	for _, repo := range repos {
		ids = append(ids, repo.RepoID)
	}
	return ids, nil
}
