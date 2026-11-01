package plugin

import (
	"errors"

	"google.golang.org/grpc/codes"
	grpcstatus "google.golang.org/grpc/status"

	"github.com/recurser/bossalib/kit/vcs"
)

// githubReadErr maps a provider read refused for an exhausted GitHub quota to
// codes.ResourceExhausted, so a plugin can recognise it (vcs.IsRateLimited)
// and quiet its logging until the quota resets. Other errors pass through.
func githubReadErr(err error) error {
	if errors.Is(err, vcs.ErrRateLimited) {
		return grpcstatus.Error(codes.ResourceExhausted, err.Error())
	}
	return err
}
