package session

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"

	"github.com/spf13/cobra"

	"github.com/recurser/boss/api/cli/cliutil"
	"github.com/recurser/boss/internal/domain/rpcerr"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/githubcallback"
	"github.com/recurser/bossalib/kit/vcs"
)

// repoContextResolver finds the registered repo a working directory belongs
// to.
type repoContextResolver interface {
	ResolveContext(ctx context.Context, workingDir string) (*pb.ResolveContextResponse, error)
}

// prStateClient is what `boss pr state` needs: the working-directory repo
// lookup and the daemon's cached PR read.
type prStateClient interface {
	repoContextResolver
	GetPRState(ctx context.Context, req *pb.GetPRStateRequest) (*pb.GetPRStateResponse, error)
}

type prStateOptions struct {
	repo           string
	fresh          bool
	reviewComments bool
	workflowRuns   bool
	asJSON         bool
}

func (g *Group) prStateCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "state <pr>",
		Short: "Read a PR's state through the daemon's GitHub read cache",
		Long: "State reads one pull request through the local daemon's GitHub read cache, which each\n" +
			"PR webhook clears, so repeated status and check reads within the cache window cost no\n" +
			"GitHub call. A green verdict adds one uncached workflow-runs read. It prints the head\n" +
			"SHA, PR state, draft flag, mergeability, review decision, checks (commit statuses\n" +
			"included), the check verdict with counts, the verify phase, whether verify passed the head,\n" +
			"the build receipt and, for a green verdict, whether every head workflow run has settled\n" +
			"and the head's workflow runs.\n" +
			"--with-workflow-runs reads the workflow runs whatever the verdict.\n\n" +
			"<pr> is a number, #n or a PR URL. The repo comes from --repo, else the PR URL, else the\n" +
			"registered repo the working directory belongs to. Only repos registered on the daemon\n" +
			"are read; any other answers NOT_FOUND.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			opts := prStateOptions{}
			opts.repo, _ = cmd.Flags().GetString("repo")
			opts.fresh, _ = cmd.Flags().GetBool("fresh")
			opts.reviewComments, _ = cmd.Flags().GetBool("with-review-comments")
			opts.workflowRuns, _ = cmd.Flags().GetBool("with-workflow-runs")
			opts.asJSON, _ = cmd.Flags().GetBool(cliutil.JSONFlagName)
			ref, needsRepo, err := parsePRStateRef(args[0], opts.repo)
			if err != nil {
				return cliutil.EmitJSONFailure(cmd, opts.asJSON, err)
			}
			c, err := g.newClient(cmd)
			if err != nil {
				return cliutil.EmitJSONFailure(cmd, opts.asJSON, err)
			}
			// Both daemon clients implement the read; asserting it keeps the
			// shared Client interface within its width budget.
			pc, ok := c.(prStateClient)
			if !ok {
				return cliutil.EmitJSONFailure(cmd, opts.asJSON, rpcerr.New(rpcerr.Unimplemented, errors.New("this client cannot read PR state")))
			}
			return runPRState(cmd, pc, ref, needsRepo, opts)
		},
	}
	cmd.Flags().String("repo", "", "Repository as owner/name (default: the PR URL's, else the working directory's)")
	cmd.Flags().Bool("fresh", false, "Bypass the daemon's read cache and read GitHub directly")
	cmd.Flags().Bool("with-review-comments", false, "Also read the PR's review comments (the expensive read)")
	cmd.Flags().Bool("with-workflow-runs", false, "Read the head's workflow runs whatever the check verdict")
	cmd.Flags().Bool(cliutil.JSONFlagName, false, "Emit a stable JSON envelope instead of human-readable text")
	return cmd
}

// parsePRStateRef parses <pr> against --repo. needsRepo reports a bare number
// with no --repo, whose repo comes from the working directory.
func parsePRStateRef(arg, repoFlag string) (githubcallback.PRRef, bool, error) {
	ref := strings.TrimPrefix(strings.TrimSpace(arg), "#")
	var owner, name string
	if strings.TrimSpace(repoFlag) != "" {
		var err error
		if owner, name, err = githubcallback.SplitRepo(repoFlag); err != nil {
			return githubcallback.PRRef{}, false, cliutil.CodedError(cliutil.CodeInvalidArgument, fmt.Errorf("--repo: %w", err))
		}
	} else if n, err := strconv.ParseInt(ref, 10, 32); err == nil && n > 0 {
		return githubcallback.PRRef{PRNumber: int32(n)}, true, nil
	}
	parsed, err := githubcallback.ParsePRRef(ref, owner, name)
	if err != nil {
		return githubcallback.PRRef{}, false, cliutil.CodedError(cliutil.CodeInvalidArgument, err)
	}
	return parsed, false, nil
}

// resolvePRRef fills in a bare PR number's repo from the working directory.
func resolvePRRef(ctx context.Context, c repoContextResolver, ref githubcallback.PRRef, needsRepo bool) (githubcallback.PRRef, error) {
	if !needsRepo {
		return ref, nil
	}
	owner, name, err := workingDirRepo(ctx, c, ref.PRNumber)
	if err != nil {
		return ref, err
	}
	ref.Owner, ref.Repo = owner, name
	return ref, nil
}

// workingDirRepo resolves the registered repo the working directory belongs to.
func workingDirRepo(ctx context.Context, c repoContextResolver, pr int32) (owner, name string, err error) {
	missing := cliutil.CodedError(cliutil.CodeInvalidArgument, fmt.Errorf(
		"PR number %d needs repository context: pass --repo owner/name, run inside a registered repository, or use a PR URL", pr))
	wd, err := os.Getwd()
	if err != nil {
		return "", "", missing
	}
	resolved, err := c.ResolveContext(ctx, wd)
	if err != nil {
		return "", "", missing
	}
	owner, name, err = githubcallback.SplitRepo(vcs.RepoSlug(resolved.GetRepo().GetOriginUrl()))
	if err != nil {
		return "", "", missing
	}
	return owner, name, nil
}

func runPRState(cmd *cobra.Command, c prStateClient, ref githubcallback.PRRef, needsRepo bool, opts prStateOptions) error {
	ctx := cmd.Context()
	ref, err := resolvePRRef(ctx, c, ref, needsRepo)
	if err != nil {
		return cliutil.EmitJSONFailure(cmd, opts.asJSON, err)
	}
	resp, err := c.GetPRState(ctx, &pb.GetPRStateRequest{
		Repo:                        ref.Owner + "/" + ref.Repo,
		PrNumber:                    ref.PRNumber,
		ShouldBypassCache:           opts.fresh,
		ShouldIncludeReviewComments: opts.reviewComments,
		ShouldIncludeWorkflowRuns:   opts.workflowRuns,
	})
	if err != nil {
		return cliutil.EmitJSONFailure(cmd, opts.asJSON, fmt.Errorf("read PR state %s/%s#%d: %w", ref.Owner, ref.Repo, ref.PRNumber, err))
	}
	if opts.asJSON {
		return cliutil.EmitJSON(cmd, newPRStateJSON(resp))
	}
	return writePRStateSummary(cmd.OutOrStdout(), resp)
}

// writePRStateSummary prints the short human form of a PR state.
func writePRStateSummary(w io.Writer, r *pb.GetPRStateResponse) error {
	env := newPRStateJSON(r)
	draft := ""
	if env.IsDraft {
		draft = " (draft)"
	}
	mergeable := "unknown"
	if env.IsMergeable != nil {
		mergeable = map[bool]string{true: "yes", false: "no"}[*env.IsMergeable]
	}
	v := env.CheckVerdict
	verify := "passed"
	if !env.IsVerifyPassed {
		verify = phaseText(env.VerifyPhase)
	}
	lines := []string{
		fmt.Sprintf("PR %s#%d: %s%s", env.Repo, env.PRNumber, env.State, draft),
		fmt.Sprintf("head: %s (%s -> %s)", env.HeadSHA, env.HeadBranch, env.BaseBranch),
		fmt.Sprintf("mergeable: %s, merge state: %s, review: %s", mergeable, orNone(env.MergeStateStatus), orNone(env.ReviewDecision)),
		fmt.Sprintf("checks: %s (%s), %d passed, %d failed, %d pending, %d skipped, %d unclassified",
			v.State, v.Reason, v.PassedCount, v.FailedCount, v.PendingCount, v.SkippedCount, v.UnclassifiedCount),
		fmt.Sprintf("verify: %s", verify),
	}
	if env.HasSettledWorkflowRuns != nil {
		lines = append(lines, fmt.Sprintf("workflow runs settled: %t (%d held)", *env.HasSettledWorkflowRuns, len(env.HeldWorkflowRuns)))
	}
	if names := checkNames(env.Checks, isFailingCheck); len(names) > 0 {
		lines = append(lines, "failing: "+strings.Join(names, ", "))
	}
	if names := checkNames(env.Checks, func(c prCheckJSON) bool { return c.Status != "completed" }); len(names) > 0 {
		lines = append(lines, "pending: "+strings.Join(names, ", "))
	}
	_, err := fmt.Fprintln(w, strings.Join(lines, "\n"))
	return err
}

func phaseText(p prVerifyPhaseJSON) string {
	switch {
	case p.Reason != "" && p.Label != "":
		return fmt.Sprintf("%s (%s · %s)", p.Kind, p.Reason, p.Label)
	case p.Reason != "":
		return fmt.Sprintf("%s (%s)", p.Kind, p.Reason)
	}
	return p.Kind
}

func isFailingCheck(c prCheckJSON) bool {
	return c.Conclusion == "failure" || c.Conclusion == "cancelled" || c.Conclusion == "timed_out"
}

func checkNames(checks []prCheckJSON, keep func(prCheckJSON) bool) []string {
	var names []string
	for _, c := range checks {
		if keep(c) {
			names = append(names, c.Name)
		}
	}
	return names
}
