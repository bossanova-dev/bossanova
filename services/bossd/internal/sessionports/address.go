package sessionports

import (
	"strconv"
	"strings"
)

// splitHostPort splits an "host:port" socket name into its host and numeric
// port, tolerating IPv6 bracket notation ("[::1]:5432") and wildcard hosts
// ("*:8080"). It preserves the host text verbatim (minus IPv6 brackets) so
// wildcard and loopback binds stay distinguishable downstream. An empty host,
// a missing colon, or a non-numeric port is a parse failure the caller scopes
// to the owning PID as incomplete evidence.
func splitHostPort(name string) (host string, port int, ok bool) {
	idx := strings.LastIndex(name, ":")
	if idx < 0 || idx == len(name)-1 {
		return "", 0, false
	}
	host = name[:idx]
	portStr := name[idx+1:]
	port, err := strconv.Atoi(portStr)
	if err != nil || port < 0 || port > 65535 {
		return "", 0, false
	}
	// Strip IPv6 brackets but keep the address itself (e.g. "::1", "::").
	if len(host) >= 2 && host[0] == '[' && host[len(host)-1] == ']' {
		host = host[1 : len(host)-1]
	}
	if host == "" {
		return "", 0, false
	}
	return host, port, true
}
