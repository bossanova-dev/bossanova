package callerorigin

import (
	"net/http"
	"testing"
)

func TestReadRoundTripsAssertions(t *testing.T) {
	for _, a := range []Assertion{
		{Kind: KindOperator, Host: "laptop.local"},
		{Kind: KindAgentChat, AgentSessionID: "0b5c-chat", SessionID: "sess-1", Role: "coordinator", Host: "laptop"},
		{Kind: KindDaemonChild},
		{Kind: KindUnattested},
	} {
		h := http.Header{}
		a.Set(h)
		got, present := Read(h)
		if !present || got != a {
			t.Fatalf("round trip %+v: %+v present=%v", a, got, present)
		}
	}
}

func TestReadWithoutAssertion(t *testing.T) {
	if got, present := Read(http.Header{"Authorization": {"Bearer x"}}); present || got != (Assertion{}) {
		t.Fatalf("absent assertion read as %+v", got)
	}
}

func TestReadFailsClosedOnMalformedAssertions(t *testing.T) {
	cases := map[string]http.Header{
		"unknown kind":        {HeaderKind: {"root"}},
		"no kind":             {HeaderHost: {"laptop"}},
		"repeated kind":       {HeaderKind: {"agent-chat", "operator"}},
		"chat without id":     {HeaderKind: {"agent-chat"}},
		"operator with chat":  {HeaderKind: {"operator"}, HeaderAgentSessionID: {"chat"}},
		"bad role":            {HeaderKind: {"agent-chat"}, HeaderAgentSessionID: {"chat"}, HeaderRole: {"Coordinator!"}},
		"bad id":              {HeaderKind: {"agent-chat"}, HeaderAgentSessionID: {"chat id"}},
		"newline in host":     {HeaderKind: {"operator"}, HeaderHost: {"a\nb"}},
		"repeated agent chat": {HeaderKind: {"agent-chat"}, HeaderAgentSessionID: {"a", "b"}},
	}
	for name, h := range cases {
		got, present := Read(h)
		if !present || got.Kind != KindUnattested || got.AgentSessionID != "" || got.Role != "" {
			t.Fatalf("%s: %+v present=%v", name, got, present)
		}
	}
}
