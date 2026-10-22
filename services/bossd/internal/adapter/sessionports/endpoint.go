package sessionports

import (
	"net"
	"sort"
	"strconv"
)

// Endpoint is a single canonical, verified local web endpoint for a session:
// the listening Port and the reachable loopback URL (scheme + loopback host +
// port), e.g. "http://127.0.0.1:8080" or "https://[::1]:8443". It is the only
// value BOS-473 surfaces to session reads.
type Endpoint struct {
	Port int
	URL  string
}

// SessionSnapshot is the caller's per-session input to the Tracker: the session
// ID, its canonical worktree path (the identity anchor that lets stale in-flight
// work be rejected after deletion, resurrection, or a path change), and the
// session tmux name plus every live chat tmux name whose pane roots the process
// tree to inspect.
type SessionSnapshot struct {
	ID           string
	WorktreePath string
	TmuxNames    []string
}

// probeCandidate is a loopback target derived from a discovered Listener: the
// port, the address family, and the loopback host to dial and probe.
type probeCandidate struct {
	port   int
	family Family
	host   string
}

// target renders the candidate as a dial address, bracketing IPv6 hosts.
func (c probeCandidate) target() string {
	return net.JoinHostPort(c.host, strconv.Itoa(c.port))
}

// verifiedEndpoint is a probeCandidate whose HTTP(S) scheme has been confirmed
// by a successful probe.
type verifiedEndpoint struct {
	port   int
	family Family
	host   string
	scheme string
}

// deriveCandidate maps a discovered Listener to the loopback target that should
// be probed, or reports ok=false when the listener is not locally reachable.
//
// Only wildcard and loopback TCP listeners yield a candidate; a LAN-only bind (a
// parseable IP that is neither loopback nor unspecified) is excluded because it
// is not reachable as a canonical local endpoint. Wildcard binds are narrowed to
// the loopback address of their family: an IPv4 wildcard ("0.0.0.0" or "*" with
// FamilyIPv4) becomes 127.0.0.1, an IPv6 wildcard ("::" or "*" with FamilyIPv6)
// becomes ::1. An explicit loopback bind keeps its exact address.
func deriveCandidate(l Listener) (probeCandidate, bool) {
	if l.Port <= 0 {
		return probeCandidate{}, false
	}
	// "*" never parses as an IP, so the wildcard case branches on Family.
	if l.Address == "*" {
		switch l.Family {
		case FamilyIPv4:
			return probeCandidate{port: l.Port, family: FamilyIPv4, host: "127.0.0.1"}, true
		case FamilyIPv6:
			return probeCandidate{port: l.Port, family: FamilyIPv6, host: "::1"}, true
		default:
			return probeCandidate{}, false
		}
	}
	ip := net.ParseIP(l.Address)
	if ip == nil {
		return probeCandidate{}, false
	}
	switch {
	case ip.IsLoopback():
		return probeCandidate{port: l.Port, family: l.Family, host: l.Address}, true
	case ip.IsUnspecified():
		if ip.To4() != nil {
			return probeCandidate{port: l.Port, family: FamilyIPv4, host: "127.0.0.1"}, true
		}
		return probeCandidate{port: l.Port, family: FamilyIPv6, host: "::1"}, true
	default:
		// LAN-reachable bind: not a canonical local endpoint.
		return probeCandidate{}, false
	}
}

// canonicalize collapses verified targets to at most one Endpoint per port.
// Within a port a verified IPv4 target is preferred over IPv6 (Family ordering:
// FamilyIPv4 < FamilyIPv6), the winning target's scheme is preserved, and the
// URL brackets IPv6 hosts via net.JoinHostPort. The result is sorted ascending
// by port for deterministic output.
func canonicalize(verified []verifiedEndpoint) []Endpoint {
	best := make(map[int]verifiedEndpoint, len(verified))
	for _, v := range verified {
		cur, ok := best[v.port]
		if !ok || v.family < cur.family {
			best[v.port] = v
		}
	}
	out := make([]Endpoint, 0, len(best))
	for _, v := range best {
		url := v.scheme + "://" + net.JoinHostPort(v.host, strconv.Itoa(v.port))
		out = append(out, Endpoint{Port: v.port, URL: url})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Port < out[j].Port })
	return out
}
