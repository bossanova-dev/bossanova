package sessionports

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"os/exec"
	"strconv"
)

// classifyLsofResult maps a raw lsof invocation outcome to a SocketScan,
// applying the macOS completeness contract in a platform-independent (hence
// unit-testable) form:
//
//   - ctxErr set, or a derived deadline/cancellation surfaced as runErr: the
//     captured output is a truncated prefix, so the whole batch is
//     untrustworthy → GlobalIncomplete.
//   - a non-*exec.ExitError runErr (lsof could not be executed at all — missing
//     binary, exec failure): a genuine source error, not evidence.
//   - otherwise (runErr nil, or a clean non-zero exit because some/all PIDs
//     simply have no listeners): the field output is authoritative complete
//     evidence, parsed by parseLsof (which scopes any malformed row to its
//     owner).
func classifyLsofResult(out []byte, runErr, ctxErr error) (SocketScan, error) {
	if ctxErr != nil || errors.Is(runErr, context.DeadlineExceeded) || errors.Is(runErr, context.Canceled) {
		return SocketScan{GlobalIncomplete: true}, nil
	}
	var exitErr *exec.ExitError
	if runErr != nil && !errors.As(runErr, &exitErr) {
		return SocketScan{}, runErr
	}
	return parseLsof(out), nil
}

// parseLsof parses the field output of
//
//	lsof -nP -a -p <csv> -iTCP -sTCP:LISTEN -Fpnt
//
// into a SocketScan. lsof emits one line per field: "p<pid>" opens a process
// record, then each file appears as an "f<fd>" line optionally followed by a
// type line "t<IPv4|IPv6>" and a name line "n<host:port>". Because -iTCP
// -sTCP:LISTEN already restricts the set to listening TCP sockets, every name
// under a process is a listener; the only classification we still need is the
// address family, which the "n" text cannot supply for wildcard binds
// ("*:8080" is byte-identical for IPv4 and IPv6). The "t" field is therefore
// requested and load-bearing here — the plan's original "-Fpn" cannot satisfy
// the IPv4/IPv6 acceptance criterion, so this parser expects "-Fpnt".
//
// A name that cannot be parsed, or a name with no preceding recognizable type,
// is dropped and its owning PID marked incomplete: partial evidence is scoped
// to the affected owner, never guessed and never allowed to sink valid rows
// from other processes.
func parseLsof(output []byte) SocketScan {
	var scan SocketScan
	var curPID int
	var havePID bool
	var curFamily Family

	scanner := bufio.NewScanner(bytes.NewReader(output))
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		field := line[0]
		value := string(line[1:])
		switch field {
		case 'p':
			pid, err := strconv.Atoi(value)
			if err != nil {
				havePID = false
				continue
			}
			curPID = pid
			havePID = true
			curFamily = FamilyUnknown
		case 'f':
			// A new file record: forget the previous record's type so a stale
			// family cannot bleed onto the next socket.
			curFamily = FamilyUnknown
		case 't':
			switch value {
			case "IPv4":
				curFamily = FamilyIPv4
			case "IPv6":
				curFamily = FamilyIPv6
			default:
				curFamily = FamilyUnknown
			}
		case 'n':
			if !havePID {
				continue
			}
			if curFamily == FamilyUnknown {
				scan.markIncomplete(curPID)
				continue
			}
			host, port, ok := splitHostPort(value)
			if !ok {
				scan.markIncomplete(curPID)
				continue
			}
			scan.Listeners = append(scan.Listeners, Listener{
				PID:     curPID,
				Address: host,
				Port:    port,
				Family:  curFamily,
			})
			// Consume the type: the next name needs its own t field.
			curFamily = FamilyUnknown
		default:
			// Other field types (P for protocol, etc.) are ignored.
		}
	}
	if scanner.Err() != nil {
		// A read error (e.g. an overlong line) truncated the field stream, so
		// remaining PIDs' sockets are unseen: the whole batch is untrustworthy.
		scan.GlobalIncomplete = true
	}
	return scan
}
