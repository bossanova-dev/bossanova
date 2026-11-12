package upstream

import (
	"context"
	"maps"
	"slices"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

// repoCloudControlModes publishes each registered repository's effective mode,
// in repoIDs order, from the redactor's per-snapshot mode cache.
func (r *cloudRedactor) repoCloudControlModes(ctx context.Context, repoIDs []string) []*pb.RepoCloudControlMode {
	modes := make([]*pb.RepoCloudControlMode, 0, len(repoIDs))
	for _, id := range repoIDs {
		modes = append(modes, &pb.RepoCloudControlMode{RepoId: id, CloudControl: r.mode(ctx, id)})
	}
	return modes
}

// repoCloudControlDelta lists the repositories whose effective mode changed,
// was added or was removed between previous and current, sorted by id.
func repoCloudControlDelta(previous, current map[string]pb.CloudControl) *pb.RepoCloudControlsChanged {
	delta := &pb.RepoCloudControlsChanged{}
	for _, id := range slices.Sorted(maps.Keys(current)) {
		if mode, ok := previous[id]; !ok || mode != current[id] {
			delta.Upserted = append(delta.Upserted, &pb.RepoCloudControlMode{RepoId: id, CloudControl: current[id]})
		}
	}
	for _, id := range slices.Sorted(maps.Keys(previous)) {
		if _, ok := current[id]; !ok {
			delta.RemovedRepoIds = append(delta.RemovedRepoIds, id)
		}
	}
	return delta
}

// sendRepoCloudControlDelta publishes a newer revision of the per-repo modes
// when they changed. It reports false only when ctx ended before the send.
func (c *StreamClient) sendRepoCloudControlDelta(ctx context.Context, outbound chan<- *pb.DaemonEvent, previous, current map[string]pb.CloudControl) bool {
	if maps.Equal(previous, current) {
		return true
	}
	delta := repoCloudControlDelta(previous, current)
	delta.Revision = c.repoCloudControlRevision.Add(1)
	select {
	case outbound <- &pb.DaemonEvent{Event: &pb.DaemonEvent_RepoCloudControlsChanged{RepoCloudControlsChanged: delta}}:
		return true
	case <-ctx.Done():
		return false
	}
}
