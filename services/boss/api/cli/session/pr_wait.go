package session

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/recurser/boss/api/cli/cliutil"
	"github.com/recurser/boss/internal/domain/rpcerr"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/githubcallback"
	"github.com/recurser/bossalib/kit/config"
)

// prWaitClient is what `boss pr wait` needs: the working-directory repo lookup
// and the daemon's webhook-woken PR wait.
type prWaitClient interface {
	repoContextResolver
	WaitPRState(ctx context.Context, req *pb.WaitPRStateRequest) (*pb.WaitPRStateResponse, error)
}

// prWaitConditions are the --until values.
var prWaitConditions = map[string]pb.PRWaitCondition{
	"checks-settled": pb.PRWaitCondition_PR_WAIT_CONDITION_CHECKS_SETTLED,
	"mergeable":      pb.PRWaitCondition_PR_WAIT_CONDITION_MERGEABLE,
	"head-changed":   pb.PRWaitCondition_PR_WAIT_CONDITION_HEAD_CHANGED,
}

type prWaitOptions struct {
	repo         string
	until        string
	sinceHead    string
	timeout      time.Duration
	fresh        bool
	acceptNoGate bool
	asJSON       bool
}

// prWaitJSON is the stable `boss pr wait --json` envelope. outcome is "met",
// "unreachable" or "timed_out"; snapshot is the `boss pr state --json`
// envelope of the last successful read.
type prWaitJSON struct {
	Outcome        string      `json:"outcome"`
	Condition      string      `json:"condition"`
	TimeoutSeconds int32       `json:"timeout_seconds"`
	Snapshot       prStateJSON `json:"snapshot"`
}

func (g *Group) prWaitCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "wait <pr>",
		Short: "Wait for a PR condition, woken by the daemon's webhooks instead of polling",
		Long: "Wait blocks until a pull request meets a condition, the condition becomes unreachable,\n" +
			"or the timeout passes. The local daemon re-reads the PR through its GitHub read cache\n" +
			"only when a webhook announces a change, and on a slow timer only while the repo's\n" +
			"webhook delivery is unhealthy, so a wait costs reads per event, not per poll tick. A\n" +
			"condition that already holds answers at once.\n\n" +
			"--until checks-settled: the check verdict is failing, or green with every head workflow\n" +
			"run settled. A green head where no gate ran keeps waiting unless --accept-no-gate.\n" +
			"--until mergeable: GitHub reports the PR mergeable; a conflict is unreachable.\n" +
			"--until head-changed: the head differs from --since-head (default: the first read's head).\n" +
			"A closed or merged PR is unreachable for every condition.\n\n" +
			"The outcome is met, unreachable or timed_out, and every answered outcome exits 0. A\n" +
			"non-zero exit means the wait could not be served. <pr> and the repo resolve as for\n" +
			"`boss pr state`. --timeout defaults to " + config.PRWaitDefault.String() + " and is capped at " + config.PRWaitCeiling.String() + ".",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			opts := prWaitOptions{}
			opts.repo, _ = cmd.Flags().GetString("repo")
			opts.until, _ = cmd.Flags().GetString("until")
			opts.sinceHead, _ = cmd.Flags().GetString("since-head")
			opts.timeout, _ = cmd.Flags().GetDuration("timeout")
			opts.fresh, _ = cmd.Flags().GetBool("fresh")
			opts.acceptNoGate, _ = cmd.Flags().GetBool("accept-no-gate")
			opts.asJSON, _ = cmd.Flags().GetBool(cliutil.JSONFlagName)
			req, err := newPRWaitRequest(opts)
			if err != nil {
				return cliutil.EmitJSONFailure(cmd, opts.asJSON, err)
			}
			ref, needsRepo, err := parsePRStateRef(args[0], opts.repo)
			if err != nil {
				return cliutil.EmitJSONFailure(cmd, opts.asJSON, err)
			}
			c, err := g.newClient(cmd)
			if err != nil {
				return cliutil.EmitJSONFailure(cmd, opts.asJSON, err)
			}
			wc, ok := c.(prWaitClient)
			if !ok {
				return cliutil.EmitJSONFailure(cmd, opts.asJSON, rpcerr.New(rpcerr.Unimplemented, errors.New("this client cannot wait on PR state")))
			}
			return runPRWait(cmd, wc, ref, needsRepo, req, opts)
		},
	}
	cmd.Flags().String("repo", "", "Repository as owner/name (default: the PR URL's, else the working directory's)")
	cmd.Flags().String("until", "", "Condition to wait for: "+strings.Join(prWaitConditionNames(), ", ")+" (required)")
	cmd.Flags().String("since-head", "", "Head SHA a head-changed wait compares against (default: the first read's head)")
	cmd.Flags().Duration("timeout", config.PRWaitDefault, "How long to wait, at most "+config.PRWaitCeiling.String())
	cmd.Flags().Bool("fresh", false, "Bypass the daemon's read cache and read GitHub directly")
	cmd.Flags().Bool("accept-no-gate", false, "Let checks-settled meet on a green head where every check skipped")
	cmd.Flags().Bool(cliutil.JSONFlagName, false, "Emit a stable JSON envelope instead of human-readable text")
	return cmd
}

func prWaitConditionNames() []string {
	names := make([]string, 0, len(prWaitConditions))
	for name := range prWaitConditions {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// newPRWaitRequest validates the flags before any RPC: a missing or unknown
// condition, or a timeout outside (0, PRWaitCeiling], is INVALID_ARGUMENT, so
// --json callers get the failure envelope rather than a usage error.
func newPRWaitRequest(opts prWaitOptions) (*pb.WaitPRStateRequest, error) {
	cond, ok := prWaitConditions[opts.until]
	if !ok {
		return nil, cliutil.CodedError(cliutil.CodeInvalidArgument, fmt.Errorf("--until must be one of %s, got %q", strings.Join(prWaitConditionNames(), ", "), opts.until))
	}
	if opts.timeout <= 0 || opts.timeout > config.PRWaitCeiling {
		return nil, cliutil.CodedError(cliutil.CodeInvalidArgument, fmt.Errorf("--timeout must be above 0 and at most %s, got %s", config.PRWaitCeiling, opts.timeout))
	}
	return &pb.WaitPRStateRequest{
		Condition:             cond,
		SinceHeadSha:          strings.TrimSpace(opts.sinceHead),
		TimeoutSeconds:        int32((opts.timeout + time.Second - 1) / time.Second),
		ShouldBypassCache:     opts.fresh,
		ShouldAcceptNoGateRan: opts.acceptNoGate,
	}, nil
}

func runPRWait(cmd *cobra.Command, c prWaitClient, ref githubcallback.PRRef, needsRepo bool, req *pb.WaitPRStateRequest, opts prWaitOptions) error {
	ctx := cmd.Context()
	ref, err := resolvePRRef(ctx, c, ref, needsRepo)
	if err != nil {
		return cliutil.EmitJSONFailure(cmd, opts.asJSON, err)
	}
	req.Repo, req.PrNumber = ref.Owner+"/"+ref.Repo, ref.PRNumber
	resp, err := c.WaitPRState(ctx, req)
	if err != nil {
		return cliutil.EmitJSONFailure(cmd, opts.asJSON, fmt.Errorf("wait on PR %s/%s#%d: %w", ref.Owner, ref.Repo, ref.PRNumber, err))
	}
	env := prWaitJSON{
		Outcome:        enumName(resp.GetOutcome(), "PR_WAIT_OUTCOME_"),
		Condition:      opts.until,
		TimeoutSeconds: resp.GetTimeoutSeconds(),
		Snapshot:       newPRStateJSON(resp.GetSnapshot()),
	}
	if opts.asJSON {
		return cliutil.EmitJSON(cmd, env)
	}
	return writePRWaitSummary(cmd.OutOrStdout(), env, resp.GetSnapshot())
}

func writePRWaitSummary(w io.Writer, env prWaitJSON, snap *pb.GetPRStateResponse) error {
	if _, err := fmt.Fprintf(w, "%s: %s (timeout %ds)\n", env.Condition, env.Outcome, env.TimeoutSeconds); err != nil {
		return err
	}
	return writePRStateSummary(w, snap)
}
