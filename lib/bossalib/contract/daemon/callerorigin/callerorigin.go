// Package callerorigin carries a forwarding client's caller-origin assertion
// to a remote daemon.
//
// A `boss --host` call reaches the remote daemon through an SSH socket
// forward, so the remote daemon's kernel ancestry sees only sshd and would
// read the caller as the operator. The boss CLI asks its own host's daemon
// who it is and sends that answer in these headers. The receiving daemon uses
// it only to lower an operator attribution; it never grants anything.
package callerorigin

import (
	"context"
	"net/http"
	"regexp"

	"connectrpc.com/connect"
)

// Kind is the asserted provenance of the process a forwarded call runs for.
type Kind string

const (
	KindOperator    Kind = "operator"
	KindAgentChat   Kind = "agent-chat"
	KindDaemonChild Kind = "daemon-child"
	KindUnattested  Kind = "unattested"
)

// Header names. Every one is optional except HeaderKind; the presence of any
// of them makes the request carry an assertion.
const (
	HeaderKind           = "Boss-Caller-Origin-Kind"
	HeaderAgentSessionID = "Boss-Caller-Origin-Agent-Session-Id"
	HeaderSessionID      = "Boss-Caller-Origin-Session-Id"
	HeaderRole           = "Boss-Caller-Origin-Role"
	HeaderHost           = "Boss-Caller-Origin-Host"
)

var headers = []string{HeaderKind, HeaderAgentSessionID, HeaderSessionID, HeaderRole, HeaderHost}

var (
	idPattern   = regexp.MustCompile(`^[A-Za-z0-9._:-]{1,128}$`)
	rolePattern = regexp.MustCompile(`^[a-z]{1,32}$`)
	hostPattern = regexp.MustCompile(`^[A-Za-z0-9._-]{1,255}$`)
)

// Assertion is what the forwarding client says about its own caller. Role is
// the chat role's lower-case name (for example "coordinator").
type Assertion struct {
	Kind           Kind
	AgentSessionID string
	SessionID      string
	Role           string
	Host           string
}

// Set writes the assertion into h, omitting empty fields.
func (a Assertion) Set(h http.Header) {
	for name, value := range map[string]string{
		HeaderKind: string(a.Kind), HeaderAgentSessionID: a.AgentSessionID,
		HeaderSessionID: a.SessionID, HeaderRole: a.Role, HeaderHost: a.Host,
	} {
		if value != "" {
			h.Set(name, value)
		}
	}
}

// Read returns the assertion h carries; present is false when h carries none.
// A present assertion that is malformed in any way (repeated header, unknown
// kind, a field that fails its pattern, or chat identity on a non-chat kind)
// reads as KindUnattested, so the receiving daemon fails closed.
func Read(h http.Header) (a Assertion, present bool) {
	for _, name := range headers {
		if len(h.Values(name)) > 0 {
			present = true
			break
		}
	}
	if !present {
		return Assertion{}, false
	}
	unattested := Assertion{Kind: KindUnattested}
	values := make(map[string]string, len(headers))
	for _, name := range headers {
		got := h.Values(name)
		if len(got) > 1 {
			return unattested, true
		}
		if len(got) == 1 {
			values[name] = got[0]
		}
	}
	a = Assertion{
		Kind: Kind(values[HeaderKind]), AgentSessionID: values[HeaderAgentSessionID],
		SessionID: values[HeaderSessionID], Role: values[HeaderRole], Host: values[HeaderHost],
	}
	if !a.valid() {
		return Assertion{Kind: KindUnattested, Host: hostOrEmpty(a.Host)}, true
	}
	return a, true
}

func (a Assertion) valid() bool {
	if a.Host != "" && !hostPattern.MatchString(a.Host) {
		return false
	}
	switch a.Kind {
	case KindAgentChat:
		return idPattern.MatchString(a.AgentSessionID) &&
			(a.SessionID == "" || idPattern.MatchString(a.SessionID)) &&
			(a.Role == "" || rolePattern.MatchString(a.Role))
	case KindOperator, KindDaemonChild, KindUnattested:
		return a.AgentSessionID == "" && a.SessionID == "" && a.Role == ""
	}
	return false
}

func hostOrEmpty(host string) string {
	if hostPattern.MatchString(host) {
		return host
	}
	return ""
}

// NewClientInterceptor attaches a to every request on the client.
func NewClientInterceptor(a Assertion) ClientInterceptor { return ClientInterceptor{a} }

// ClientInterceptor is the connect.Interceptor NewClientInterceptor returns.
type ClientInterceptor struct{ assertion Assertion }

func (c ClientInterceptor) WrapUnary(next connect.UnaryFunc) connect.UnaryFunc {
	return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
		c.assertion.Set(req.Header())
		return next(ctx, req)
	}
}

func (c ClientInterceptor) WrapStreamingClient(next connect.StreamingClientFunc) connect.StreamingClientFunc {
	return func(ctx context.Context, spec connect.Spec) connect.StreamingClientConn {
		conn := next(ctx, spec)
		c.assertion.Set(conn.RequestHeader())
		return conn
	}
}

func (c ClientInterceptor) WrapStreamingHandler(next connect.StreamingHandlerFunc) connect.StreamingHandlerFunc {
	return next // client interceptor: no server side
}
