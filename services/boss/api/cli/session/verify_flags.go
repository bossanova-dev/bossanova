package session

import (
	"fmt"
	"strings"

	"github.com/recurser/boss/api/cli/cliutil"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

// validateVerifyFlags refuses the verify-only flag combinations the daemon
// would reject, before any RPC: --verify-only needs --pr, and --policy-ref
// (trial verify policy) exists only on a verify-only session. An empty
// --policy-ref is refused rather than dropped: dropped, the trial the
// operator asked for would become an ordinary session that can merge.
func validateVerifyFlags(verifyOnly bool, prNumber *int32, policyRefSet bool, policyRef string) error {
	switch {
	case verifyOnly && prNumber == nil:
		return cliutil.CodedError(cliutil.CodeInvalidArgument, fmt.Errorf("--verify-only requires --pr"))
	case policyRefSet && strings.TrimSpace(policyRef) == "":
		return cliutil.CodedError(cliutil.CodeInvalidArgument, fmt.Errorf("--policy-ref needs a branch name"))
	case policyRefSet && !verifyOnly:
		return cliutil.CodedError(cliutil.CodeInvalidArgument, fmt.Errorf("--policy-ref requires --verify-only"))
	}
	return nil
}

// createdSessionError refuses a create stream that returned no session, or a
// trial verify session that came back without the policy ref it asked for: a
// bossd that predates trial policy refs drops the field and creates an
// ordinary verify-only session, which judges under the base policy and may
// merge the PR.
func createdSessionError(session *pb.Session, policyRef string) error {
	if session == nil {
		return fmt.Errorf("daemon did not return a session")
	}
	if policyRef != "" && session.GetPolicyRef() != policyRef {
		return fmt.Errorf("daemon created session %s without --policy-ref %q: it predates trial policy refs, "+
			"so the session would verify under the base policy and may merge the PR; archive it with "+
			"`boss archive %s`, then upgrade bossd and retry", session.GetId(), policyRef, session.GetId())
	}
	return nil
}
