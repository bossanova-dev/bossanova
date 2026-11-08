package sshhost

import (
	"errors"
	"testing"

	"github.com/recurser/bossalib/contract/daemon/callerorigin"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

func envOf(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

func TestAssertOrigin(t *testing.T) {
	chatEnv := envOf(map[string]string{"BOSS_AGENT_SESSION_ID": "chat-env", "BOSS_SESSION_ID": "sess-env"})
	noEnv := envOf(nil)
	down := errors.New("dial unix: no such file")
	cases := []struct {
		name string
		resp *pb.DescribeCallerResponse
		err  error
		env  func(string) string
		want callerorigin.Assertion
	}{
		{"operator terminal, local daemon attests operator", &pb.DescribeCallerResponse{Kind: pb.CallerKind_CALLER_KIND_OPERATOR}, nil, noEnv,
			callerorigin.Assertion{Kind: callerorigin.KindOperator, Host: "laptop"}},
		{"operator terminal, no local daemon", nil, down, noEnv,
			callerorigin.Assertion{Kind: callerorigin.KindOperator, Host: "laptop"}},
		{"coordinator chat attested by the local daemon", &pb.DescribeCallerResponse{Kind: pb.CallerKind_CALLER_KIND_AGENT_CHAT, AgentSessionId: "chat-1", SessionId: "sess-1", Role: pb.ChatRole_CHAT_ROLE_COORDINATOR}, nil, chatEnv,
			callerorigin.Assertion{Kind: callerorigin.KindAgentChat, AgentSessionID: "chat-1", SessionID: "sess-1", Role: "coordinator", Host: "laptop"}},
		{"chat whose env was stripped is still attested", &pb.DescribeCallerResponse{Kind: pb.CallerKind_CALLER_KIND_AGENT_CHAT, AgentSessionId: "chat-1", Role: pb.ChatRole_CHAT_ROLE_BUILD}, nil, noEnv,
			callerorigin.Assertion{Kind: callerorigin.KindAgentChat, AgentSessionID: "chat-1", Role: "build", Host: "laptop"}},
		{"chat env lowers an operator attestation", &pb.DescribeCallerResponse{Kind: pb.CallerKind_CALLER_KIND_OPERATOR}, nil, chatEnv,
			callerorigin.Assertion{Kind: callerorigin.KindAgentChat, AgentSessionID: "chat-env", SessionID: "sess-env", Host: "laptop"}},
		{"chat env with no local daemon asserts a roleless chat", nil, down, chatEnv,
			callerorigin.Assertion{Kind: callerorigin.KindAgentChat, AgentSessionID: "chat-env", SessionID: "sess-env", Host: "laptop"}},
		{"unattested stays unattested", &pb.DescribeCallerResponse{}, nil, noEnv,
			callerorigin.Assertion{Kind: callerorigin.KindUnattested, Host: "laptop"}},
		{"daemon child", &pb.DescribeCallerResponse{Kind: pb.CallerKind_CALLER_KIND_DAEMON_CHILD}, nil, chatEnv,
			callerorigin.Assertion{Kind: callerorigin.KindDaemonChild, Host: "laptop"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := assertOrigin(tc.resp, tc.err, tc.env, "laptop"); got != tc.want {
				t.Fatalf("got %+v, want %+v", got, tc.want)
			}
		})
	}
	if got := assertOrigin(nil, down, noEnv, "bad host\n"); got.Host != "" {
		t.Fatalf("unsafe hostname asserted: %q", got.Host)
	}
}
