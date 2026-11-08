package config

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestResolveCoordinatorApprovalGrant(t *testing.T) {
	cases := []struct {
		raw     string
		want    CoordinatorApprovalGrant
		discard bool
	}{
		{"", CoordinatorApprovalGrantOn, false},
		{"  ", CoordinatorApprovalGrantOn, false},
		{"on", CoordinatorApprovalGrantOn, false},
		{"off", CoordinatorApprovalGrantOff, false},
		{" Off ", CoordinatorApprovalGrantOff, false},
		{"disabled", CoordinatorApprovalGrantOff, true},
	}
	for _, tc := range cases {
		var s Settings
		if err := json.Unmarshal([]byte(`{"coordinator_approval_grant":`+jsonString(tc.raw)+`}`), &s); err != nil {
			t.Fatal(err)
		}
		got, reason := ResolveCoordinatorApprovalGrant(&s)
		if got != tc.want || (reason != "") != tc.discard {
			t.Fatalf("%q: got %q reason %q", tc.raw, got, reason)
		}
		if tc.discard && !strings.Contains(reason, tc.raw) {
			t.Fatalf("%q: reason does not name the value: %q", tc.raw, reason)
		}
	}
	if got, reason := ResolveCoordinatorApprovalGrant(nil); got != CoordinatorApprovalGrantOff || reason == "" {
		t.Fatalf("unreadable settings: %q %q", got, reason)
	}
}

func TestCoordinatorApprovalGrantRoundTrips(t *testing.T) {
	var s Settings
	if err := json.Unmarshal([]byte(`{"coordinator_approval_grant":"Disabled"}`), &s); err != nil {
		t.Fatal(err)
	}
	out, err := json.Marshal(&s)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), `"coordinator_approval_grant":"Disabled"`) {
		t.Fatalf("value rewritten: %s", out)
	}
}

func jsonString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}
