package projectconfig

import (
	"context"
	"errors"
	"fmt"
	"strings"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"google.golang.org/protobuf/proto"
)

// LocalWriter is the write subset of the daemon client ApplyLocal needs.
type LocalWriter interface {
	RegisterRepo(ctx context.Context, req *pb.RegisterRepoRequest) (*pb.Repo, error)
	UpdateRepo(ctx context.Context, req *pb.UpdateRepoRequest) (*pb.Repo, error)
	CronWriter
}

// CronWriter is the cron write surface the per-daemon cron rules write
// through: the local daemon's client, or a daemon-bound adapter over the
// orchestrator's cron proxies for the caller's other daemons.
type CronWriter interface {
	CreateCronJob(ctx context.Context, req *pb.CreateCronJobRequest) (*pb.CronJob, error)
	UpdateCronJob(ctx context.Context, req *pb.UpdateCronJobRequest) (*pb.CronJob, error)
	DeleteCronJob(ctx context.Context, id string) error
}

// LocalFailure is a change whose write failed, and why.
type LocalFailure struct {
	Change Change `json:"change"`
	Error  string `json:"error"`
}

// LocalResult is what ApplyLocal did: the changes it applied, the ones whose
// write failed (a marker-less echo is a failure carrying ReasonDaemonTooOld),
// and the ones it never reached because an earlier write of their class (or
// the repo registration they depend on) failed.
type LocalResult struct {
	Applied    []Change       `json:"applied"`
	Failed     []LocalFailure `json:"failed"`
	NotReached []Change       `json:"notReached"`
}

// Text renders the result: one line per failed and per unreached change, and
// a count of applied ones.
func (r LocalResult) Text() string {
	var b strings.Builder
	fmt.Fprintf(&b, "applied %d change(s)", len(r.Applied))
	if len(r.Failed) > 0 {
		fmt.Fprintf(&b, ", %d failed", len(r.Failed))
	}
	if len(r.NotReached) > 0 {
		fmt.Fprintf(&b, ", %d not reached", len(r.NotReached))
	}
	b.WriteByte('\n')
	for _, f := range r.Failed {
		fmt.Fprintf(&b, "failed: %s %s %q: %s\n", f.Change.Action, f.Change.Resource, f.Change.Identity, f.Error)
	}
	for _, c := range r.NotReached {
		fmt.Fprintf(&b, "not reached: %s %s %q\n", c.Action, c.Resource, c.Identity)
	}
	return b.String()
}

// opOutcome is what happened to one write.
type opOutcome int

const (
	opNotReached opOutcome = iota
	opDone
	opFailed
)

// ApplyLocal executes p's writes in order: repo, then secrets, then crons
// (creates, updates and adoptions, then disables, then deletes, each in name
// order). Each class stops at its first failed write and the next class still
// runs, except that a failed registration leaves every write needing the repo
// id unreached. A cron write whose echoed row lacks the project marker means
// this bossd drops the field: it fails with ReasonDaemonTooOld and stops the
// cron class. The error is non-nil when any write failed; it never carries a
// secret value.
func ApplyLocal(ctx context.Context, w LocalWriter, p LocalPlan) (LocalResult, error) {
	outcomes := make([]opOutcome, len(p.ops))
	failures := map[int]LocalFailure{} // by change index
	stopped := map[LocalClass]bool{}
	repoID, repoFailed := "", false

	for i, op := range p.ops {
		if stopped[op.class] || (op.needsRepo && (repoFailed || repoID == "")) {
			continue
		}
		change := p.Changes[op.change]
		err := applyOp(ctx, w, op, repoID, &repoID)
		if err == nil {
			outcomes[i] = opDone
			continue
		}
		outcomes[i] = opFailed
		stopped[op.class] = true
		if op.register != nil {
			repoFailed = true
		}
		msg := err.Error()
		if op.secret {
			msg = "setting the secret failed"
		}
		var old *tooOldError
		if errors.As(err, &old) {
			change.Action, change.Reason = ActionSkip, ReasonDaemonTooOld
		}
		failures[op.change] = LocalFailure{Change: change, Error: msg}
	}

	// A change is applied when every write it needs ran, failed when one
	// failed, and otherwise not reached.
	var res LocalResult
	seen := map[int]bool{}
	for _, op := range p.ops {
		if seen[op.change] {
			continue
		}
		seen[op.change] = true
		if f, ok := failures[op.change]; ok {
			res.Failed = append(res.Failed, f)
			continue
		}
		done := true
		for j, other := range p.ops {
			if other.change == op.change && outcomes[j] != opDone {
				done = false
			}
		}
		if done {
			res.Applied = append(res.Applied, p.Changes[op.change])
		} else {
			res.NotReached = append(res.NotReached, p.Changes[op.change])
		}
	}
	if len(res.Failed) > 0 {
		return res, fmt.Errorf("%d of %d change(s) failed to apply", len(res.Failed), len(seen))
	}
	return res, nil
}

// tooOldError marks a cron write the daemon accepted without its marker.
type tooOldError struct{ name string }

func (e *tooOldError) Error() string {
	return fmt.Sprintf("bossd did not store the managed_by marker on %q: it is too old for project config; upgrade bossd and re-run", e.name)
}

// applyOp runs one write. A registration stores the new repo id in *newRepoID.
func applyOp(ctx context.Context, w LocalWriter, op localOp, repoID string, newRepoID *string) error {
	switch {
	case op.register != nil:
		repo, err := w.RegisterRepo(ctx, op.register)
		if err != nil {
			return fmt.Errorf("register %s: %w", op.register.GetLocalPath(), err)
		}
		if repo.GetId() == "" {
			return errors.New("register: the daemon returned no repository id")
		}
		*newRepoID = repo.GetId()
		return nil
	case op.updateRepo != nil:
		req := op.updateRepo
		if op.needsRepo {
			req = proto.CloneOf(req)
			req.Id = repoID
		}
		if _, err := w.UpdateRepo(ctx, req); err != nil {
			return fmt.Errorf("update the repository: %w", err)
		}
		return nil
	}
	return applyCronOp(ctx, w, op, repoID)
}

// applyCronOp runs one cron write. A create or update whose echoed row lacks
// the op's marker is a tooOldError.
func applyCronOp(ctx context.Context, w CronWriter, op localOp, repoID string) error {
	switch {
	case op.createCron != nil:
		req := op.createCron
		if op.needsRepo {
			req = proto.CloneOf(req)
			req.RepoId = repoID
		}
		job, err := w.CreateCronJob(ctx, req)
		if err != nil {
			return fmt.Errorf("create cron job %s: %w", req.GetName(), err)
		}
		return checkMarker(op, job, req.GetName())
	case op.updateCron != nil:
		job, err := w.UpdateCronJob(ctx, op.updateCron)
		if err != nil {
			return fmt.Errorf("update cron job %s: %w", op.name, err)
		}
		return checkMarker(op, job, op.name)
	case op.deleteCron != "":
		if err := w.DeleteCronJob(ctx, op.deleteCron); err != nil {
			return fmt.Errorf("delete cron job %s: %w", op.name, err)
		}
		return nil
	}
	return errors.New("empty write")
}

func checkMarker(op localOp, job *pb.CronJob, name string) error {
	if op.marker != "" && job.GetManagedBy() != op.marker {
		return &tooOldError{name: name}
	}
	return nil
}
