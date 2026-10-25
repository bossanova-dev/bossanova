package projectconfig

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"connectrpc.com/connect"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

// CloudWriter is the write surface of the orchestrator client ApplyCloud
// needs. *client.RemoteClient satisfies it.
type CloudWriter interface {
	RemoteCronWriter
	CreateTrigger(ctx context.Context, req *pb.CreateTriggerRequest) (*pb.CreateTriggerResponse, error)
	UpdateTrigger(ctx context.Context, req *pb.UpdateTriggerRequest) (*pb.Trigger, error)
	DeleteTrigger(ctx context.Context, id string) error
	RotateTriggerSecret(ctx context.Context, id string) (*pb.RotateTriggerSecretResponse, error)
	CreateSessionWebhook(ctx context.Context, req *pb.CreateSessionWebhookRequest) (*pb.CreateSessionWebhookResponse, error)
	UpdateSessionWebhook(ctx context.Context, req *pb.UpdateSessionWebhookRequest) (*pb.SessionWebhook, error)
	DeleteSessionWebhook(ctx context.Context, organizationID, id string) error
	RotateSessionWebhookSecret(ctx context.Context, req *pb.RotateSessionWebhookSecretRequest) (*pb.RotateSessionWebhookSecretResponse, error)
}

// SecretSink receives each secret bosso mints, as its response arrives and
// before the next write. label names the row (`trigger "deploy"`, or
// `webhook https://host #1a2b3c4d`: the host plus a digest of the URL, never
// its path). A minted secret goes nowhere else: never into the plan, the
// result, an error or a log.
type SecretSink interface {
	Secret(label, value string)
}

// CloudResult is what ApplyCloud did: the changes it applied, the ones whose
// write failed (bosso's refusals carry the conflict or skip they amount to),
// and the ones it never reached because an earlier write of their class
// failed. Warnings are post-write notes, such as a webhook URL bosso stored
// in a different form than the file's.
type CloudResult struct {
	Applied    []Change       `json:"applied"`
	Failed     []LocalFailure `json:"failed"`
	NotReached []Change       `json:"notReached"`
	Warnings   []string       `json:"warnings,omitempty"`
}

// Text renders the result like LocalResult.Text, then its warnings.
func (r CloudResult) Text() string {
	text := LocalResult{Applied: r.Applied, Failed: r.Failed, NotReached: r.NotReached}.Text()
	var b strings.Builder
	b.WriteString(strings.Replace(text, "applied ", "cloud: applied ", 1))
	for _, w := range r.Warnings {
		fmt.Fprintf(&b, "warning: %s\n", w)
	}
	return b.String()
}

// ApplyCloud executes p's writes class by class (triggers, then webhooks,
// then remote crons), each class in rank and name order and remote crons
// daemon by daemon in entry-name order. A class (for remote crons, one
// daemon) stops at its first failed write and the next one still runs. A
// remote cron write the proxy answers Unavailable is ReasonDaemonOffline, and
// one whose echoed row lacks the marker is ReasonDaemonTooOld. Every secret bosso mints reaches sink
// before the next write. A write whose echoed row lacks the project marker
// fails with ReasonServerTooOld; ALREADY_EXISTS on a trigger write or a
// webhook create is ReasonManagedByOtherMember; FAILED_PRECONDITION or INVALID_ARGUMENT on a
// webhook create, and FAILED_PRECONDITION on a webhook update or rotate, is a
// conflict whose detail is bosso's message. The error is
// non-nil when any write failed; it never carries a secret value.
func ApplyCloud(ctx context.Context, w CloudWriter, sink SecretSink, p CloudPlan) (CloudResult, error) {
	outcomes := make([]opOutcome, len(p.ops))
	failures := map[int]LocalFailure{}
	type stopDomain struct {
		class  CloudClass
		domain string
	}
	stopped := map[stopDomain]bool{}
	var res CloudResult

	for i, op := range p.ops {
		if stopped[stopDomain{op.class, op.domain}] {
			continue
		}
		change := p.Changes[op.change]
		warning, err := applyCloudOp(ctx, w, sink, p.org, op)
		if warning != "" {
			res.Warnings = append(res.Warnings, warning)
		}
		if err == nil {
			outcomes[i] = opDone
			continue
		}
		outcomes[i] = opFailed
		stopped[stopDomain{op.class, op.domain}] = true
		failures[op.change] = cloudFailure(change, op, err)
	}

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
		return res, fmt.Errorf("%d of %d cloud change(s) failed to apply", len(res.Failed), len(seen))
	}
	return res, nil
}

// cloudFailure maps a failed write to the change it amounts to.
func cloudFailure(change Change, op cloudOp, err error) LocalFailure {
	var old *serverTooOldError
	var daemonOld *tooOldError
	switch {
	case op.cron != nil && errors.As(err, &daemonOld):
		change.Action, change.Reason = ActionSkip, ReasonDaemonTooOld
	case op.cron != nil && connect.CodeOf(err) == connect.CodeUnavailable:
		change.Action, change.Reason = ActionSkip, ReasonDaemonOffline
		change.Detail = "the daemon was unreachable; nothing more was sent to it"
	case errors.As(err, &old):
		change.Action, change.Reason = ActionSkip, ReasonServerTooOld
	case op.class == CloudClassTriggers && connect.CodeOf(err) == connect.CodeAlreadyExists:
		change.Action, change.Reason = ActionConflict, ReasonManagedByOtherMember
		change.Detail = "another member of the organization already has a trigger with this name managed by this project"
	case op.createWebhook != nil && connect.CodeOf(err) == connect.CodeAlreadyExists:
		// The plan creates only when it saw no row with this URL and marker, so
		// the row that conflicts is one bosso did not list: another owner's,
		// hidden while the organization does not share sessions (BOS-1593).
		change.Action, change.Reason = ActionConflict, ReasonManagedByOtherMember
		change.Detail = "another member of the organization already has a webhook for this URL managed by this project"
	case op.createWebhook != nil && (connect.CodeOf(err) == connect.CodeFailedPrecondition || connect.CodeOf(err) == connect.CodeInvalidArgument):
		change.Action, change.Reason = ActionConflict, ReasonRefused
		change.Detail = connectMessage(err)
	case (op.updateWebhook != nil || op.rotateWebhook != nil) && connect.CodeOf(err) == connect.CodeFailedPrecondition:
		// An endpoint whose creator is no longer an owner is listed to every
		// owner but may only be disabled or deleted (BOS-1593).
		change.Action, change.Reason = ActionConflict, ReasonRefused
		change.Detail = connectMessage(err)
	}
	return LocalFailure{Change: change, Error: err.Error()}
}

// connectMessage is a connect error's message without its code prefix.
func connectMessage(err error) string {
	var ce *connect.Error
	if errors.As(err, &ce) {
		return ce.Message()
	}
	return err.Error()
}

// serverTooOldError marks a write bosso accepted without its marker.
type serverTooOldError struct{ name string }

func (e *serverTooOldError) Error() string {
	return fmt.Sprintf("bosso did not store the managed_by marker on %s: it is too old for project config; nothing else in this class was written", e.name)
}

func checkCloudMarker(op cloudOp, echoed string) error {
	if op.marker != "" && echoed != op.marker {
		return &serverTooOldError{name: op.name}
	}
	return nil
}

// applyCloudOp runs one write. A minted secret goes to sink at once; a
// warning describes a write that succeeded with a caveat.
func applyCloudOp(ctx context.Context, w CloudWriter, sink SecretSink, org string, op cloudOp) (string, error) {
	mint := func(secret string) {
		if secret != "" && op.label != "" && sink != nil {
			sink.Secret(op.label, secret)
		}
	}
	switch {
	case op.cron != nil:
		return "", applyCronOp(ctx, daemonCronWriter{w: w, daemonID: op.daemonID}, *op.cron, "")
	case op.createTrigger != nil:
		resp, err := w.CreateTrigger(ctx, op.createTrigger)
		if err != nil {
			return "", fmt.Errorf("create trigger %q: %w", op.name, err)
		}
		mint(resp.GetSecret())
		return "", checkCloudMarker(op, resp.GetTrigger().GetManagedBy())
	case op.updateTrigger != nil:
		t, err := w.UpdateTrigger(ctx, op.updateTrigger)
		if err != nil {
			return "", fmt.Errorf("update trigger %q: %w", op.name, err)
		}
		return "", checkCloudMarker(op, t.GetManagedBy())
	case op.deleteTrigger != "":
		if err := w.DeleteTrigger(ctx, op.deleteTrigger); err != nil {
			return "", fmt.Errorf("delete trigger %q: %w", op.name, err)
		}
		return "", nil
	case op.rotateTrigger != "":
		resp, err := w.RotateTriggerSecret(ctx, op.rotateTrigger)
		if err != nil {
			return "", fmt.Errorf("rotate the secret of trigger %q: %w", op.name, err)
		}
		mint(resp.GetSecret())
		return "", nil
	case op.createWebhook != nil:
		resp, err := w.CreateSessionWebhook(ctx, op.createWebhook)
		if err != nil {
			return "", fmt.Errorf("create webhook %s: %w", webhookHost(op.name), err)
		}
		mint(resp.GetSecret())
		var warning string
		if stored := resp.GetWebhook().GetUrl(); stored != "" && stored != op.wantURL {
			warning = fmt.Sprintf("webhook %s: bosso normalized the stored URL; write the form bosso stores in the file so it keeps matching", webhookHost(stored))
		}
		return warning, checkCloudMarker(op, resp.GetWebhook().GetManagedBy())
	case op.updateWebhook != nil:
		hook, err := w.UpdateSessionWebhook(ctx, op.updateWebhook)
		if err != nil {
			return "", fmt.Errorf("update webhook %s: %w", webhookHost(op.name), err)
		}
		return "", checkCloudMarker(op, hook.GetManagedBy())
	case op.deleteWebhook != "":
		if err := w.DeleteSessionWebhook(ctx, org, op.deleteWebhook); err != nil {
			return "", fmt.Errorf("delete webhook %s: %w", webhookHost(op.name), err)
		}
		return "", nil
	case op.rotateWebhook != nil:
		resp, err := w.RotateSessionWebhookSecret(ctx, op.rotateWebhook)
		if err != nil {
			return "", fmt.Errorf("rotate the secret of webhook %s: %w", webhookHost(op.name), err)
		}
		mint(resp.GetSecret())
		return "", nil
	}
	return "", errors.New("empty write")
}
