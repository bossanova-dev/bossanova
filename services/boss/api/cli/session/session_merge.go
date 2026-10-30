package session

import (
	"context"
	"errors"
	"fmt"

	"github.com/recurser/boss/api/cli/cliutil"
	"github.com/recurser/boss/internal/domain/prverdict"
	"github.com/recurser/boss/internal/domain/rpc"
	"github.com/recurser/boss/internal/domain/rpcerr"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/kit/vcs"
	"github.com/spf13/cobra"
)

// runMerge is a thin transport over the daemon's MergeSession RPC. The daemon
// owns the merge gate, the per-repo merge serialization, the already-merged
// short-circuit, and the merge-strategy resolution — none of that is mirrored
// here. The client pins the displayed head, and the daemon evaluates its verdict.
// Merges wait on the repo merge lock and
// the provider, so the context is unbounded like runArchive's (the procedure is
// already in the client's no-short-deadline set).
func (g *Group) runMerge(cmd *cobra.Command, sessionID string) error {
	asJSON, _ := cmd.Flags().GetBool("json")

	// --match-head pins the merge to the head the caller verified (BOS-1381).
	// Validated before any RPC, including session-id resolution, so a bad pin
	// never reaches the daemon and never degrades to an unpinned merge.
	matchHead, _ := cmd.Flags().GetString("match-head")
	if matchHead != "" {
		normalized, ok := vcs.NormalizeHeadSHA(matchHead)
		if !ok {
			return cliutil.EmitJSONFailure(cmd, asJSON, cliutil.CodedError(cliutil.CodeInvalidArgument,
				fmt.Errorf("merge: --match-head %q is not a 40-character hex commit SHA", matchHead)))
		}
		matchHead = normalized
	}

	c, err := g.newClient(cmd)
	if err != nil {
		return cliutil.EmitJSONFailure(cmd, asJSON, err)
	}
	ctx := context.Background()
	sessionID, err = cliutil.ResolveSessionID(c, ctx, sessionID)
	if err != nil {
		return cliutil.EmitJSONFailure(cmd, asJSON, err)
	}

	yes, _ := cmd.Flags().GetBool("yes")
	approve, _ := cmd.Flags().GetBool("approve")
	if asJSON && !yes {
		// --json is a machine contract, and the confirmation prompt is not part
		// of it. Prompting here would either block on a stdin nobody attached,
		// or read EOF and print "Cancelled." with exit 0 — a merge that reads
		// as declined though it was never offered. Refusing loudly is the safe
		// direction, and it travels as an envelope like every other failure.
		return cliutil.EmitJSONFailure(cmd, asJSON, cliutil.CodedError(cliutil.CodeConfirmationRequired,
			errors.New("merge: --json requires --yes (refusing to prompt for confirmation on a machine-readable invocation)")))
	}

	var displayed *pb.Session
	if !yes || approve || matchHead == "" {
		displayed, err = c.GetSession(ctx, sessionID, rpc.SessionReadOptions{})
		if err != nil {
			return cliutil.EmitJSONFailure(cmd, asJSON, fmt.Errorf("get session: %w", err))
		}
	}
	if matchHead == "" && displayed.GetPrNumber() != 0 {
		matchHead = displayed.GetPrDisplayHeadSha()
		if matchHead == "" {
			return cliutil.EmitJSONFailure(cmd, asJSON, rpcerr.New(rpcerr.FailedPrecondition, fmt.Errorf("couldn't merge PR #%d: its latest commit is not known yet", displayed.GetPrNumber())))
		}
	}
	if !yes {
		target := mergeTargetDescription(displayed)
		if matchHead != "" {
			target += " at " + matchHead[:min(7, len(matchHead))]
		}
		fmt.Printf("Merge session %s — %s? [y/N] ", sessionID, target)
		var answer string
		if _, err := fmt.Scanln(&answer); err != nil || (answer != "y" && answer != "Y") {
			fmt.Println("Cancelled.")
			return nil
		}
	}
	sess, detail, err := mergeSession(ctx, c, sessionID, matchHead, approve, displayed)

	if err != nil {
		return cliutil.EmitJSONFailure(cmd, asJSON, fmt.Errorf("merge session: %w", err))
	}
	if asJSON {
		// Re-read the session before reporting its state. The daemon's handler
		// reads the session BEFORE its own deferred display refresh applies the
		// Merged transition, so the session it returns from a genuine merge can
		// still carry the PRE-merge state — a field that cannot mean what its
		// name implies. One extra RPC per merge, on the --json path only.
		settled, readErr := c.GetSession(ctx, sessionID, rpc.SessionReadOptions{})
		if readErr != nil || settled == nil {
			settled = nil
		}
		return cliutil.EmitJSON(cmd, newMergeJSON(sess, settled, detail))
	}
	fmt.Printf("Session %s merged (%s).\n", sess.GetId(), sess.GetTitle())
	if detail != "" {
		fmt.Printf("Note: %s\n", detail)
	}
	return nil
}

// mergeSession preserves the plain merge envelope for the operator merge path.
func mergeSession(ctx context.Context, c Client, id, head string, approve bool, displayed *pb.Session) (*pb.Session, string, error) {
	if !approve {
		return c.MergeSession(ctx, id, head)
	}
	result, err := c.OperatorMergeSession(ctx, id, head, true, displayed.GetAttention().GetParkCodes())
	if err != nil {
		return nil, "", err
	}
	if !result.GetIsMerged() {
		return nil, "", rpcerr.New(rpcerr.FailedPrecondition, errors.New(prverdict.MergeRefusal(int(displayed.GetPrNumber()), result.GetApproval())))
	}
	return result.GetSession(), result.GetMergeDetail(), nil
}
