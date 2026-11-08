package sshhost

import (
	"context"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/recurser/bossalib/contract/daemon/callerorigin"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"

	daemonrpc "github.com/recurser/boss/internal/adapter/daemonrpc"
)

// originDescribeTimeout bounds the local DescribeCaller call, so a wedged local
// daemon delays `--host` startup by at most this much.
const originDescribeTimeout = 3 * time.Second

// describeLocalCaller asks this host's daemon who this process is. A var so
// tests need no daemon.
var describeLocalCaller = func(ctx context.Context) (*pb.DescribeCallerResponse, error) {
	socket, err := daemonrpc.DefaultSocketPath()
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, originDescribeTimeout)
	defer cancel()
	return daemonrpc.NewLocal(socket).DescribeCaller(ctx)
}

// localOrigin is what a `--host` call asserts to the remote daemon about the
// caller this process runs for. The remote daemon sees only sshd, so this is
// the only fact it has; it uses it solely to lower authority.
func localOrigin(ctx context.Context) callerorigin.Assertion {
	host, _ := os.Hostname()
	resp, err := describeLocalCaller(ctx)
	return assertOrigin(resp, err, os.Getenv, host)
}

// assertOrigin combines this host's daemon attestation (kernel ancestry, as
// for a local call) with the managed-chat environment, taking whichever is
// lower: the environment alone can lower an operator attestation to an agent
// chat, never raise anything. With no attestation (no local daemon, or one
// that predates DescribeCaller) the environment decides; a terminal with no
// chat environment asserts the operator, as it would be locally.
func assertOrigin(resp *pb.DescribeCallerResponse, describeErr error, getenv func(string) string, host string) callerorigin.Assertion {
	a := callerorigin.Assertion{Kind: callerorigin.KindOperator, Host: sanitizeHost(host)}
	if describeErr == nil && resp != nil {
		switch resp.GetKind() {
		case pb.CallerKind_CALLER_KIND_AGENT_CHAT:
			a.Kind = callerorigin.KindAgentChat
			a.AgentSessionID, a.SessionID, a.Role = resp.GetAgentSessionId(), resp.GetSessionId(), chatRoleName(resp.GetRole())
			return a
		case pb.CallerKind_CALLER_KIND_DAEMON_CHILD:
			a.Kind = callerorigin.KindDaemonChild
			return a
		case pb.CallerKind_CALLER_KIND_UNSPECIFIED:
			a.Kind = callerorigin.KindUnattested
			return a
		case pb.CallerKind_CALLER_KIND_OPERATOR:
		}
	}
	if chat := strings.TrimSpace(getenv("BOSS_AGENT_SESSION_ID")); chat != "" {
		// The environment cannot attest a role, so the chat asserts none and
		// holds no role grant on the remote daemon.
		a.Kind = callerorigin.KindAgentChat
		a.AgentSessionID, a.SessionID = chat, strings.TrimSpace(getenv("BOSS_SESSION_ID"))
	}
	return a
}

func chatRoleName(role pb.ChatRole) string {
	if role == pb.ChatRole_CHAT_ROLE_UNSPECIFIED {
		return ""
	}
	return strings.ToLower(strings.TrimPrefix(role.String(), "CHAT_ROLE_"))
}

// sanitizeHost keeps a hostname the remote daemon will accept; anything else
// is omitted rather than failing the whole assertion closed.
func sanitizeHost(host string) string {
	if !assertableHost.MatchString(host) {
		return ""
	}
	return host
}

var assertableHost = regexp.MustCompile(`^[A-Za-z0-9._-]{1,255}$`)
