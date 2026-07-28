package sessionports

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"net"
	"strconv"
	"strings"
)

// tcpStateListen is the hex TCP state for LISTEN in /proc/net/tcp{,6}.
const tcpStateListen = "0A"

// procNetListener is one LISTEN row decoded from /proc/net/tcp or tcp6: the
// bound host/port and the socket inode used to attribute it to a PID via
// /proc/<pid>/fd.
type procNetListener struct {
	Host   string
	Port   int
	Family Family
	Inode  uint64
}

// parseProcNetTCP parses /proc/net/tcp (family FamilyIPv4) or /proc/net/tcp6
// (FamilyIPv6), returning only LISTEN rows. The local_address column is a
// hex-encoded address:port in kernel byte order; wildcard (0.0.0.0 / ::) and
// loopback (127.0.0.1 / ::1) decode to distinct canonical strings so the bind
// scope is preserved. Malformed rows are skipped, not fatal — the file is read
// live and a single unparseable line must not sink the rest.
func parseProcNetTCP(data []byte, family Family) []procNetListener {
	var out []procNetListener
	scanner := bufio.NewScanner(bytes.NewReader(data))
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		// Need at least sl, local, rem, st, ..., inode (index 9).
		if len(fields) < 10 {
			continue
		}
		if fields[3] != tcpStateListen {
			continue
		}
		host, port, ok := decodeProcNetAddr(fields[1], family)
		if !ok {
			continue
		}
		inode, err := strconv.ParseUint(fields[9], 10, 64)
		if err != nil {
			continue
		}
		out = append(out, procNetListener{Host: host, Port: port, Family: family, Inode: inode})
	}
	return out
}

// decodeProcNetAddr decodes a "HEXADDR:HEXPORT" local_address column.
func decodeProcNetAddr(s string, family Family) (host string, port int, ok bool) {
	addrHex, portHex, found := strings.Cut(s, ":")
	if !found {
		return "", 0, false
	}
	portVal, err := strconv.ParseUint(portHex, 16, 32)
	if err != nil || portVal > 65535 {
		return "", 0, false
	}
	ip, ok := decodeHexIP(addrHex, family)
	if !ok {
		return "", 0, false
	}
	return ip, int(portVal), true
}

// decodeHexIP decodes the kernel hex address form into a canonical IP string.
// The kernel prints each 32-bit word in host byte order; bossd's supported
// targets are all little-endian, so IPv4 is a single little-endian 32-bit word
// and IPv6 is four little-endian 32-bit words. (A big-endian Linux host would
// misdecode — an accepted constraint, not a supported target.) net.IP.String
// renders the canonical text, keeping wildcard and loopback distinguishable.
func decodeHexIP(addrHex string, family Family) (string, bool) {
	raw, err := hex.DecodeString(addrHex)
	if err != nil {
		return "", false
	}
	switch family {
	case FamilyIPv4:
		if len(raw) != 4 {
			return "", false
		}
		v := binary.LittleEndian.Uint32(raw)
		b := make([]byte, 4)
		binary.BigEndian.PutUint32(b, v)
		return net.IP(b).String(), true
	case FamilyIPv6:
		if len(raw) != 16 {
			return "", false
		}
		b := make([]byte, 16)
		for i := 0; i < 4; i++ {
			word := binary.LittleEndian.Uint32(raw[i*4 : i*4+4])
			binary.BigEndian.PutUint32(b[i*4:i*4+4], word)
		}
		return net.IP(b).String(), true
	default:
		return "", false
	}
}

// parseSocketInode extracts the inode from a /proc/<pid>/fd/<n> symlink target.
// Socket fds read as "socket:[<inode>]"; any other link shape (pipes, ttys,
// regular files, anon inodes) is not a socket and returns ok=false.
func parseSocketInode(link string) (uint64, bool) {
	const prefix = "socket:["
	if !strings.HasPrefix(link, prefix) || !strings.HasSuffix(link, "]") {
		return 0, false
	}
	inner := link[len(prefix) : len(link)-1]
	inode, err := strconv.ParseUint(inner, 10, 64)
	if err != nil {
		return 0, false
	}
	return inode, true
}
