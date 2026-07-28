package sessionports

// Family distinguishes the address family of a listening socket. It is kept
// explicit (rather than inferred from the address text) because a wildcard
// bind such as "*:8080" is reported identically by the OS for IPv4 and IPv6,
// yet the two are distinct sockets that BOS-472 must be able to tell apart.
type Family int

const (
	// FamilyUnknown is the zero value used when a source could not determine
	// the address family. It never appears in a published Listener.
	FamilyUnknown Family = iota
	// FamilyIPv4 is an AF_INET listening socket.
	FamilyIPv4
	// FamilyIPv6 is an AF_INET6 listening socket.
	FamilyIPv6
)

// String renders the family for diagnostics and stable test output.
func (f Family) String() string {
	switch f {
	case FamilyIPv4:
		return "ipv4"
	case FamilyIPv6:
		return "ipv6"
	default:
		return "unknown"
	}
}

// Identity is the OS-level birth identity of a process: its start time as
// reported by the kernel (sysctl KERN_PROC_PID on macOS, /proc/<pid>/stat on
// Linux). Two processes that reuse the same PID at different times have
// different start times, so comparing a captured Identity against a freshly
// read one detects PID reuse — the core race this package guards against.
//
// StartTime is an opaque, per-platform monotonic-ish token; only equality is
// meaningful, never ordering across platforms. Present reports whether the
// process existed at capture time. A zero Identity (Present=false) means
// "no such process".
type Identity struct {
	StartTime int64
	Present   bool
}

// Equal reports whether two identities describe the same live process
// incarnation. Two non-present identities are never equal: absence is not a
// stable identity to attribute sockets to.
func (id Identity) Equal(other Identity) bool {
	return id.Present && other.Present && id.StartTime == other.StartTime
}

// Root is a session's tmux-pane process root together with the birth identity
// captured by the caller (BOS-472) at the same instant it read the pane PID.
// The Detector revalidates PID against Identity so a recycled root PID cannot
// leak the replacement process's descendant listeners.
type Root struct {
	PID      int
	Identity Identity
}

// Listener is a single discovered listening TCP socket owned by a process in a
// validated session tree. Address preserves the exact bind host as reported by
// the OS ("*"/"0.0.0.0"/"::" for wildcard, "127.0.0.1"/"::1" for loopback) so
// downstream consumers can distinguish externally reachable from
// loopback-only ports.
type Listener struct {
	PID     int
	Address string
	Port    int
	Family  Family
}

// SessionResult holds the listeners attributed to one session plus whether the
// evidence gathered this scan is complete. Complete=false means the scan hit a
// transient obstacle (a process momentarily unreadable, a partial socket read,
// cancellation) and the negative/partial evidence must NOT be used to remove
// ports a previous complete scan published — it is "don't know", not "gone".
// Complete=true with no Listeners is authoritative: the session owns no
// listening ports right now.
type SessionResult struct {
	Listeners []Listener
	Complete  bool
}
