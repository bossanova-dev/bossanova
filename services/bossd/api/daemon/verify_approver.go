package server

import (
	"context"
	"strings"

	"github.com/rs/zerolog"

	"github.com/recurser/bossd/internal/domain"
)

// The approver environment the verify toolbox writes into an approval record's
// approver line. The caller gate attests the values; nothing in a request body
// can set them, and any inherited value is replaced.
const approverEnvPrefix = "BOSS_APPROVER_"

// approverOf is the principal the caller gate attributed to this approval. A
// handler reached without the gate (an in-process upstream command, which bosso
// authorized as the machine owner) acts for the operator.
func approverOf(ctx context.Context) domain.CallerPrincipal {
	if p, ok := callerFrom(ctx); ok {
		return p
	}
	return domain.CallerPrincipal{Kind: domain.CallerOperator}
}

// withApprover returns env with the approver variables for p, replacing any
// the daemon inherited.
func withApprover(env []string, p domain.CallerPrincipal) []string {
	out := make([]string, 0, len(env)+6)
	for _, kv := range env {
		if !strings.HasPrefix(kv, approverEnvPrefix) {
			out = append(out, kv)
		}
	}
	kind := "operator"
	if p.Kind == domain.CallerAgentChat {
		kind = "agent-chat"
		out = append(out,
			approverEnvPrefix+"ROLE="+string(p.Role),
			approverEnvPrefix+"CHAT="+p.AgentSessionID,
			approverEnvPrefix+"SESSION="+p.SessionID)
	}
	out = append(out, approverEnvPrefix+"KIND="+kind)
	if p.Forwarded {
		out = append(out, approverEnvPrefix+"FORWARDED_FROM="+forwardedFrom(p))
	}
	return out
}

func forwardedFrom(p domain.CallerPrincipal) string {
	if p.ForwardedFrom != "" {
		return p.ForwardedFrom
	}
	return "unknown"
}

// logApprover adds the principal an approval or merge ran for to ev, so the
// daemon log names a granted chat by its chat and session ids.
func logApprover(ev *zerolog.Event, p domain.CallerPrincipal) *zerolog.Event {
	ev = ev.Str("caller_kind", string(p.Kind))
	if p.Kind == domain.CallerAgentChat {
		ev = ev.Str("caller_role", string(p.Role)).
			Str("caller_agent_session_id", p.AgentSessionID).
			Str("caller_session_id", p.SessionID).
			Bool("role_grant", true)
	}
	if p.Forwarded {
		ev = ev.Str("forwarded_from", forwardedFrom(p))
	}
	return ev
}
