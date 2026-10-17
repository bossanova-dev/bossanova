package server

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/models"
	"github.com/recurser/bossd/internal/db"
	"github.com/recurser/bossd/internal/verifybridge"
)

type verifyRepoStoreFake struct {
	db.RepoStore
	repo *models.Repo
}

func (f *verifyRepoStoreFake) Get(context.Context, string) (*models.Repo, error) {
	if f.repo == nil {
		return nil, errors.New("no repo")
	}
	return f.repo, nil
}

// verifyVerbsFake records what the handler ran and with which environment.
// calls, when set, is a log shared with the merge provider so a test can assert
// the order of approve, merge and mark-merged.
type verifyVerbsFake struct {
	env        []string
	targets    []verifybridge.Target
	forMerge   []*verifybridge.MergeApproval
	calls      *[]string
	markCtxErr error
	// markCtxDeadline is the deadline MarkMerged ran under.
	markCtxDeadline time.Time

	approve    verifybridge.ApproveResult
	markMerged verifybridge.MarkMergedResult
	markErr    error
	err        error
	builtCount int

	// onApprove runs inside Approve, before it returns; approveCtxDeadline
	// records the deadline Approve ran under. markBlocks makes MarkMerged hang
	// until its context ends, the way a stuck tracker call would.
	onApprove          func()
	approveCtxDeadline time.Time
	markBlocks         bool
}

func (f *verifyVerbsFake) log(call string) {
	if f.calls != nil {
		*f.calls = append(*f.calls, call)
	}
}

func (f *verifyVerbsFake) Approve(ctx context.Context, t verifybridge.Target, forMerge *verifybridge.MergeApproval) (verifybridge.ApproveResult, error) {
	f.log("approve")
	f.targets = append(f.targets, t)
	f.forMerge = append(f.forMerge, forMerge)
	f.approveCtxDeadline, _ = ctx.Deadline()
	if f.onApprove != nil {
		f.onApprove()
	}
	return f.approve, f.err
}

func (f *verifyVerbsFake) MarkMerged(ctx context.Context, t verifybridge.Target) (verifybridge.MarkMergedResult, error) {
	f.log("mark-merged")
	f.targets = append(f.targets, t)
	f.markCtxErr = ctx.Err()
	f.markCtxDeadline, _ = ctx.Deadline()
	if f.markBlocks {
		select {
		case <-ctx.Done():
			return verifybridge.MarkMergedResult{}, fmt.Errorf("%w: %v", verifybridge.ErrNodeTimeout, ctx.Err())
		case <-time.After(10 * time.Second):
		}
	}
	return f.markMerged, f.markErr
}

func newVerifyTestServer(sess *models.Session, repo *models.Repo, verbs *verifyVerbsFake) *Server {
	return &Server{
		sessions: &sessionStoreFake{sess: sess},
		repos:    &verifyRepoStoreFake{repo: repo},
		newVerifyVerbs: func(env []string) verifyVerbs {
			verbs.env = env
			verbs.builtCount++
			return verbs
		},
	}
}

// envValue reads key from env the way exec does: the last entry wins.
func envValue(env []string, key string) (string, bool) {
	value, found := "", false
	for _, kv := range env {
		if k, v, ok := strings.Cut(kv, "="); ok && k == key {
			value, found = v, true
		}
	}
	return value, found
}

func verifyTestSession(t *testing.T) *models.Session {
	pr := 7
	return &models.Session{ID: "s1", RepoID: "r1", PRNumber: &pr, WorktreePath: t.TempDir()}
}

func TestApproveSessionPR_RunsTheVerbWithTheRepoCredentials(t *testing.T) {
	t.Setenv("LINEAR_API_KEY", "ambient-daemon-key")
	sess := verifyTestSession(t)
	repo := &models.Repo{ID: "r1", OriginURL: "git@github.com:Acme/App.git", LinearAPIKey: "lin_repo_key"}
	verbs := &verifyVerbsFake{approve: verifybridge.ApproveResult{
		Approved: true, Waives: "ledger-open", HeadSHA: "abc", TrackerWrites: "ok", ApprovalURL: "https://x/9", State: "approved",
	}}
	srv := newVerifyTestServer(sess, repo, verbs)

	resp, err := srv.ApproveSessionPR(context.Background(), connect.NewRequest(&pb.ApproveSessionPRRequest{Id: "s1", IsDryRun: true}))
	if err != nil {
		t.Fatalf("ApproveSessionPR: %v", err)
	}
	want := verifybridge.Target{PR: 7, Repo: "acme/app", Dir: sess.WorktreePath, DryRun: true}
	if len(verbs.targets) != 1 || verbs.targets[0] != want || verbs.forMerge[0] != nil {
		t.Fatalf("targets = %+v (for merge %+v), want %+v and a plain approve", verbs.targets, verbs.forMerge, want)
	}
	if got, _ := envValue(verbs.env, "LINEAR_API_KEY"); got != "lin_repo_key" {
		t.Fatalf("LINEAR_API_KEY = %q, want the repo's stored key over the daemon's", got)
	}
	if _, ok := envValue(verbs.env, "PATH"); !ok {
		t.Fatal("the verb's env dropped the daemon's PATH, so node and gh cannot be found")
	}
	r := resp.Msg.GetResult()
	if !r.GetIsApproved() || r.GetWaives() != "ledger-open" || r.GetHeadSha() != "abc" || r.GetTrackerWrites() != "ok" || r.GetApprovalUrl() != "https://x/9" || r.GetState() != "approved" {
		t.Fatalf("result = %+v", r)
	}
}

func TestApproveSessionPR_AnUnconfiguredRepoPinsTheKeyEmpty(t *testing.T) {
	t.Setenv("LINEAR_API_KEY", "ambient-daemon-key")
	verbs := &verifyVerbsFake{approve: verifybridge.ApproveResult{Reason: "no-standing-park"}}
	srv := newVerifyTestServer(verifyTestSession(t), &models.Repo{ID: "r1", OriginURL: "https://github.com/acme/app"}, verbs)
	if _, err := srv.ApproveSessionPR(context.Background(), connect.NewRequest(&pb.ApproveSessionPRRequest{Id: "s1"})); err != nil {
		t.Fatalf("ApproveSessionPR: %v", err)
	}
	if got, ok := envValue(verbs.env, "LINEAR_API_KEY"); !ok || got != "" {
		t.Fatalf("LINEAR_API_KEY = %q (set %v), want it pinned empty rather than the daemon's", got, ok)
	}
}

func TestApproveSessionPR_RefusesASessionItCannotTarget(t *testing.T) {
	noPR := &models.Session{ID: "s1", RepoID: "r1", WorktreePath: t.TempDir()}
	pr := 7
	noWorktree := &models.Session{ID: "s1", RepoID: "r1", PRNumber: &pr}
	for _, tc := range []struct {
		name string
		sess *models.Session
		id   string
		want connect.Code
	}{
		{"no session", nil, "s1", connect.CodeNotFound},
		{"no id", verifyTestSession(t), "", connect.CodeInvalidArgument},
		{"no pull request", noPR, "s1", connect.CodeFailedPrecondition},
		{"no worktree", noWorktree, "s1", connect.CodeFailedPrecondition},
	} {
		t.Run(tc.name, func(t *testing.T) {
			verbs := &verifyVerbsFake{}
			srv := newVerifyTestServer(tc.sess, &models.Repo{ID: "r1"}, verbs)
			_, err := srv.ApproveSessionPR(context.Background(), connect.NewRequest(&pb.ApproveSessionPRRequest{Id: tc.id}))
			if connect.CodeOf(err) != tc.want {
				t.Fatalf("err = %v, want %s", err, tc.want)
			}
			if verbs.builtCount != 0 {
				t.Fatal("the verb ran for a session it cannot target")
			}
		})
	}
}

func TestApproveSessionPR_MapsBridgeErrors(t *testing.T) {
	for _, tc := range []struct {
		err  error
		want connect.Code
	}{
		{fmt.Errorf("%w: slow", verifybridge.ErrNodeTimeout), connect.CodeDeadlineExceeded},
		{fmt.Errorf("%w: gone", verifybridge.ErrInvalidTarget), connect.CodeFailedPrecondition},
		{fmt.Errorf("%w: run `boss skills install`", verifybridge.ErrToolboxMissing), connect.CodeFailedPrecondition},
		{fmt.Errorf("%w: no node", verifybridge.ErrNodeMissing), connect.CodeFailedPrecondition},
		{fmt.Errorf("%w: exit 1", verifybridge.ErrNodeExit), connect.CodeInternal},
		{verifybridge.ErrShape, connect.CodeInternal},
	} {
		verbs := &verifyVerbsFake{err: tc.err}
		srv := newVerifyTestServer(verifyTestSession(t), &models.Repo{ID: "r1"}, verbs)
		_, err := srv.ApproveSessionPR(context.Background(), connect.NewRequest(&pb.ApproveSessionPRRequest{Id: "s1"}))
		if connect.CodeOf(err) != tc.want || !strings.Contains(err.Error(), tc.err.Error()) {
			t.Fatalf("%v: err = %v (code %s), want %s carrying the bridge text", tc.err, err, connect.CodeOf(err), tc.want)
		}
	}
}
