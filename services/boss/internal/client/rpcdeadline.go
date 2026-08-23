package client

import (
	"context"
	"errors"
	"fmt"
	"time"

	"connectrpc.com/connect"
	"github.com/recurser/bossalib/config"
	"github.com/recurser/bossalib/gen/bossanova/v1/bossanovav1connect"
)

// init applies the e2e-only unary RPC deadline override. e2eRPCDeadlineOverride
// is defined twice — once per build tag — exactly like applyE2EHostAttachSeed in
// services/boss/cmd: the e2e-tagged variant (rpcdeadline_e2e.go) reads an
// environment variable, and the production variant (rpcdeadline_prod.go) reads
// nothing at all and always declines.
//
// The override exists because a proof scenario has to make a wedged daemon's
// bounded failure AND its self-recovery both land inside one step's timeout
// budget, and the production 30s bound alone puts the daemon-down screen past
// the scenario schema's 60000 ms per-step cap.
func init() {
	applyE2ERPCDeadlineOverride()
}

// applyE2ERPCDeadlineOverride is the whole body of init(), named so a test can
// call it directly. init() runs once, before any test can arrange an
// environment, so without this seam the wiring from e2eRPCDeadlineOverride to
// defaultRPCDeadline is only ever exercised by whatever the process happened to
// inherit — which in CI is nothing.
//
// It moves BOTH bounds: defaultRPCDeadline to the named duration and
// slowRPCDeadline to slowDeadlineRatio times it, preserving the production
// 120s:30s shape. Scaling only the default would leave every slowProcedures
// member at two minutes, and the wedged RPCs a scenario cares most about are on
// that list — RecordChat, the call the attach view blocks on, among them — so a
// default-only override cannot express a bounded attach failure inside a proof
// step's timeout budget at all.
//
// It reports whether it changed the bounds.
func applyE2ERPCDeadlineOverride() bool {
	d, ok := e2eRPCDeadlineOverride()
	if !ok {
		return false
	}
	defaultRPCDeadline = d
	slowRPCDeadline = slowDeadlineRatio * d
	return true
}

// defaultRPCDeadline bounds an ordinary unary daemon RPC. It is a var, not a
// const, purely so tests can shrink it; nothing in production reassigns it.
var defaultRPCDeadline = 30 * time.Second

// slowRPCDeadline bounds the unary RPCs that legitimately take a long time. It
// is aligned with the daemon's own http.Server WriteTimeout
// (services/bossd/internal/server/server.go): a non-streaming response the
// daemon has not written by then cannot arrive at all, so waiting longer buys
// nothing. Keep it equal to config.SwitchResultCeiling; the production test
// guards the local account-switch result boundary.
//
// Like defaultRPCDeadline it is a var, not a const, so tests can shrink it and
// so the e2e-only override above can scale it alongside the
// default. Both have to move together: an e2e build that shrank only the
// default would leave every table member at two minutes, well past any proof
// step's timeout budget.
var slowRPCDeadline = config.SwitchResultCeiling

// slowDeadlineRatio is slowRPCDeadline / defaultRPCDeadline (120s / 30s). The
// e2e override preserves it so a scaled-down build keeps the same shape — slow
// RPCs still get materially longer than ordinary ones — rather than collapsing
// both bounds into one value that could hide an ordering bug.
const slowDeadlineRatio = 4

// slowProcedures is the SET of unary RPCs that get slowRPCDeadline instead
// of defaultRPCDeadline, because each can block on work that is not bounded by
// the daemon's own database access: a subprocess, an external network call, a
// plugin gRPC round trip, or the OS keychain.
//
// The membership test is "can this handler routinely take more than a few
// seconds on a healthy daemon?", not "does it touch the network":
//
//   - git and worktree work — CloneAndRegisterRepo runs a synchronous git clone;
//     ResurrectSession does `git worktree add` and then runs the repo's setup
//     script (pnpm install / bazel) before starting the agent; ArchiveSession
//     does `git worktree remove --force` over a tree that may carry node_modules
//     or a Bazel output base (this repo's own proof harness models it with a 4s
//     SetArchiveDelay); EmptyTrash repeats that branch-reaping work once per
//     repo across the whole trash, and RemoveSession does the same per session
//     after joining an in-flight background draft-PR goroutine for up to 10s —
//     it is what the trash view loops over INSTEAD of EmptyTrash, so leaving it
//     out would charge the identical batch 120s in one RPC and 30s per item
//     one at a time.
//   - tmux/plugin spawn — RecordChat, WakeChat and SendChatMessage all reach
//     spawnChatTmux, which dispatches BuildInteractive over plugin gRPC,
//     materialises account credentials from the keyring (which can prompt) and
//     spawns tmux. RecordChat is on the first-attach path this very ticket is
//     about, so capping it at the default would report a merely-cold daemon as
//     wedged on the one screen the fix exists to improve.
//   - external network / provider work — MergeSession blocks on the merge gate
//     plus the provider merge and its verification; RefreshSessionPR,
//     ListTrackerIssues and ListRepoPRs call out to Linear/Sentry/GitHub; ListAccounts with refresh
//     fans out one live provider usage probe per account and waits for all of
//     them; RepairDoctor, TestAccount, RunCronJobNow and SwitchSessionAccount
//     do subprocess or provider work.
//   - keychain work — AddAccount and RefreshAccount loop over keyring loads and
//     saves, which block on the OS keychain rather than on the network.
//
// Keying on the generated bossanovav1connect constants makes a *rename* a build
// failure. It does NOT catch an *omission*: a newly added slow unary RPC that
// nobody adds here silently gets the 30s default. Over-granting is the safe
// direction — every member is still ceilinged by the daemon's own 120s
// http.Server WriteTimeout — so when in doubt, add the RPC.
//
// Deliberately NOT a member: DaemonServiceGetAuthState (BOS-944). It reads
// in-memory daemon state under a mutex — no network, no subprocess, no
// keychain — so the 30s default is already orders of magnitude more than it
// needs. Granting it the slow bound would be strictly worse than harmless
// here: `boss daemon doctor` calls it precisely when the daemon is suspected
// of being wedged, and the whole value of the check is that a non-answering
// daemon fails fast enough to be reported as a finding rather than hanging the
// doctor. The exclusion is recorded here because an omission from this map is
// otherwise silent.
//
// This is a SET rather than a procedure→duration map on purpose: a map of
// durations is populated once at package-var initialisation, so a later
// reassignment of slowRPCDeadline (the e2e override, or a test) would leave
// stale copies behind and silently not apply. deadlineFor reads the var live.
var slowProcedures = map[string]struct{}{
	bossanovav1connect.DaemonServiceCloneAndRegisterRepoProcedure: {},
	bossanovav1connect.DaemonServiceMergeSessionProcedure:         {},
	bossanovav1connect.DaemonServiceRefreshSessionPRProcedure:     {},
	bossanovav1connect.DaemonServiceListTrackerIssuesProcedure:    {},
	bossanovav1connect.DaemonServiceListRepoPRsProcedure:          {},
	bossanovav1connect.DaemonServiceListAccountsProcedure:         {},
	bossanovav1connect.DaemonServiceRepairDoctorProcedure:         {},
	bossanovav1connect.DaemonServiceTestAccountProcedure:          {},
	bossanovav1connect.DaemonServiceAddAccountProcedure:           {},
	bossanovav1connect.DaemonServiceRefreshAccountProcedure:       {},
	bossanovav1connect.DaemonServiceRunCronJobNowProcedure:        {},
	bossanovav1connect.DaemonServiceSendChatMessageProcedure:      {},
	bossanovav1connect.DaemonServiceSwitchSessionAccountProcedure: {},
	bossanovav1connect.DaemonServiceRecordChatProcedure:           {},
	bossanovav1connect.DaemonServiceWakeChatProcedure:             {},
	bossanovav1connect.DaemonServiceResurrectSessionProcedure:     {},
	bossanovav1connect.DaemonServiceArchiveSessionProcedure:       {},
	bossanovav1connect.DaemonServiceEmptyTrashProcedure:           {},
	bossanovav1connect.DaemonServiceRemoveSessionProcedure:        {},
}

// deadlineFor returns the bound for one unary procedure. Both bounds are read
// from their vars on every call, so an override applied after initialisation
// (this file's e2e hook, or a test) takes effect immediately.
func deadlineFor(procedure string) time.Duration {
	if _, ok := slowProcedures[procedure]; ok {
		return slowRPCDeadline
	}
	return defaultRPCDeadline
}

// deadlineInterceptor bounds every unary daemon RPC so a wedged daemon — one
// that accepts the socket connection but never writes a response — cannot hang
// boss forever. It is unary-only by design: the streaming wrappers are
// pass-throughs, mirroring errMapInterceptor, because CreateSession and
// AttachSession are long-lived by nature and must not be bounded.
//
// It carries socketPath because it also words the failure. Only this
// interceptor can tell a bound IT imposed from one the caller asked for, or
// from a CodeDeadlineExceeded the daemon raised on its own inner deadline —
// and only the first means "the daemon is not answering". Telling a user to
// restart a daemon that answered fine, just not inside their own 30s budget
// (`boss repair doctor`) or 2s budget (the MCP commands), would be actively
// misleading, so the wording is built here rather than in mapClientErr, which
// cannot make that distinction.
type deadlineInterceptor struct {
	socketPath string
}

func (i deadlineInterceptor) WrapUnary(next connect.UnaryFunc) connect.UnaryFunc {
	return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
		// A caller-supplied deadline always wins: never extended, never
		// shortened. Only an unbounded call gets the default bound.
		if _, ok := ctx.Deadline(); ok {
			return next(ctx, req)
		}
		// Cancelling on return is safe for unary: the response body is fully
		// read before next returns.
		ctx, cancel := context.WithTimeout(ctx, deadlineFor(req.Spec().Procedure))
		defer cancel()
		resp, err := next(ctx, req)
		// Decide "this expiry is mine" on the context we created, not on the
		// returned error's code. The parent had no deadline of its own
		// (checked above), so ctx expiring can only be the bound we just
		// imposed — whereas a CodeDeadlineExceeded in the error could equally
		// have come from the daemon's own inner deadline, which is no evidence
		// that the daemon has stopped answering. %w keeps errors.Is/As and
		// connect.CodeOf working through the wrapper.
		if err != nil && errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return resp, fmt.Errorf("daemon at %s accepted the connection but did not answer in time; check it with `boss daemon status` and recover it with `boss daemon restart`: %w", i.socketPath, err)
		}
		return resp, err
	}
}

func (deadlineInterceptor) WrapStreamingClient(next connect.StreamingClientFunc) connect.StreamingClientFunc {
	return next
}

func (deadlineInterceptor) WrapStreamingHandler(next connect.StreamingHandlerFunc) connect.StreamingHandlerFunc {
	return next
}
