package domain

import "testing"

func TestProxyCompanionTarget(t *testing.T) {
	target := ProxyTargetForCompanion("session-1")
	if got, ok := ParseProxyCompanionTarget(target); !ok || got != "session-1" {
		t.Fatalf("companion target did not round-trip")
	}
	for _, target := range []string{"session-1", ProxyTargetForChat("chat-1", "account-1"), "companion\x00", "companion\x00session\x00extra"} {
		if _, ok := ParseProxyCompanionTarget(target); ok {
			t.Error("accepted a non-companion target")
		}
	}
}
