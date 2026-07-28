package sessionports

import (
	"reflect"
	"testing"
)

func TestDeriveCandidate(t *testing.T) {
	tests := []struct {
		name string
		in   Listener
		want probeCandidate
		ok   bool
	}{
		{
			name: "loopback exact ipv4",
			in:   Listener{PID: 1, Address: "127.0.0.1", Port: 8080, Family: FamilyIPv4},
			want: probeCandidate{port: 8080, family: FamilyIPv4, host: "127.0.0.1"},
			ok:   true,
		},
		{
			name: "loopback exact ipv6",
			in:   Listener{PID: 1, Address: "::1", Port: 8443, Family: FamilyIPv6},
			want: probeCandidate{port: 8443, family: FamilyIPv6, host: "::1"},
			ok:   true,
		},
		{
			name: "loopback non-canonical ipv4 preserved",
			in:   Listener{PID: 1, Address: "127.5.5.5", Port: 3000, Family: FamilyIPv4},
			want: probeCandidate{port: 3000, family: FamilyIPv4, host: "127.5.5.5"},
			ok:   true,
		},
		{
			name: "ipv4 unspecified wildcard maps to 127.0.0.1",
			in:   Listener{PID: 1, Address: "0.0.0.0", Port: 8080, Family: FamilyIPv4},
			want: probeCandidate{port: 8080, family: FamilyIPv4, host: "127.0.0.1"},
			ok:   true,
		},
		{
			name: "star wildcard ipv4 maps to 127.0.0.1",
			in:   Listener{PID: 1, Address: "*", Port: 8080, Family: FamilyIPv4},
			want: probeCandidate{port: 8080, family: FamilyIPv4, host: "127.0.0.1"},
			ok:   true,
		},
		{
			name: "ipv6 unspecified wildcard maps to ::1",
			in:   Listener{PID: 1, Address: "::", Port: 9000, Family: FamilyIPv6},
			want: probeCandidate{port: 9000, family: FamilyIPv6, host: "::1"},
			ok:   true,
		},
		{
			name: "star wildcard ipv6 maps to ::1",
			in:   Listener{PID: 1, Address: "*", Port: 9000, Family: FamilyIPv6},
			want: probeCandidate{port: 9000, family: FamilyIPv6, host: "::1"},
			ok:   true,
		},
		{
			name: "lan-only ipv4 excluded",
			in:   Listener{PID: 1, Address: "192.168.1.10", Port: 8080, Family: FamilyIPv4},
			ok:   false,
		},
		{
			name: "lan-only ipv6 excluded",
			in:   Listener{PID: 1, Address: "2001:db8::1", Port: 8080, Family: FamilyIPv6},
			ok:   false,
		},
		{
			name: "star wildcard unknown family excluded",
			in:   Listener{PID: 1, Address: "*", Port: 8080, Family: FamilyUnknown},
			ok:   false,
		},
		{
			name: "invalid port excluded",
			in:   Listener{PID: 1, Address: "127.0.0.1", Port: 0, Family: FamilyIPv4},
			ok:   false,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := deriveCandidate(tc.in)
			if ok != tc.ok {
				t.Fatalf("ok = %v, want %v", ok, tc.ok)
			}
			if ok && got != tc.want {
				t.Fatalf("candidate = %+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestCanonicalizePrefersIPv4OverIPv6(t *testing.T) {
	// Same port verified over both families: IPv4 wins, one URL, scheme kept.
	verified := []verifiedEndpoint{
		{port: 8080, family: FamilyIPv6, host: "::1", scheme: "http"},
		{port: 8080, family: FamilyIPv4, host: "127.0.0.1", scheme: "http"},
	}
	got := canonicalize(verified)
	want := []Endpoint{{Port: 8080, URL: "http://127.0.0.1:8080"}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("canonicalize = %+v, want %+v", got, want)
	}
}

func TestCanonicalizeKeepsFirstEndpointForEqualFamily(t *testing.T) {
	verified := []verifiedEndpoint{
		{port: 8080, family: FamilyIPv4, host: "127.0.0.1", scheme: "http"},
		{port: 8080, family: FamilyIPv4, host: "127.0.0.2", scheme: "https"},
	}
	got := canonicalize(verified)
	want := []Endpoint{{Port: 8080, URL: "http://127.0.0.1:8080"}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("canonicalize = %+v, want %+v", got, want)
	}
}

func TestCanonicalizePreservesSchemeAndSortsByPort(t *testing.T) {
	verified := []verifiedEndpoint{
		{port: 9000, family: FamilyIPv6, host: "::1", scheme: "https"},
		{port: 8443, family: FamilyIPv4, host: "127.0.0.1", scheme: "https"},
		{port: 8080, family: FamilyIPv4, host: "127.0.0.1", scheme: "http"},
	}
	got := canonicalize(verified)
	want := []Endpoint{
		{Port: 8080, URL: "http://127.0.0.1:8080"},
		{Port: 8443, URL: "https://127.0.0.1:8443"},
		{Port: 9000, URL: "https://[::1]:9000"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("canonicalize = %+v, want %+v", got, want)
	}
}

func TestCanonicalizeEmpty(t *testing.T) {
	if got := canonicalize(nil); len(got) != 0 {
		t.Fatalf("expected empty, got %+v", got)
	}
}
