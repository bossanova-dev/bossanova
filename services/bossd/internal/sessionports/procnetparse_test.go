package sessionports

import (
	"reflect"
	"strings"
	"testing"
)

func TestParseProcNetTCPv4(t *testing.T) {
	// Header + one LISTEN on 127.0.0.1:8080, one LISTEN on 0.0.0.0:80, and one
	// ESTABLISHED (st 01) that must be filtered out.
	const data = `  sl  local_address rem_address   st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode
   0: 0100007F:1F90 00000000:0000 0A 00000000:00000000 00:00000000 00000000  1000        0 100200 1 0000 100
   1: 00000000:0050 00000000:0000 0A 00000000:00000000 00:00000000 00000000     0        0 100201 1 0000 100
   2: 0100007F:9999 0100007F:1F90 01 00000000:00000000 00:00000000 00000000  1000        0 100202 1 0000 100
`
	got := parseProcNetTCP([]byte(data), FamilyIPv4)
	want := []procNetListener{
		{Host: "127.0.0.1", Port: 8080, Family: FamilyIPv4, Inode: 100200},
		{Host: "0.0.0.0", Port: 80, Family: FamilyIPv4, Inode: 100201},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("parseProcNetTCP v4 = %+v, want %+v", got, want)
	}
}

func TestParseProcNetTCPv6(t *testing.T) {
	// ::1 (loopback) on 5432 and :: (wildcard) on 8080.
	const data = `  sl  local_address                         remote_address                        st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode
   0: 00000000000000000000000001000000:1538 00000000000000000000000000000000:0000 0A 00000000:00000000 00:00000000 00000000  1000 0 200300 1 0000 100
   1: 00000000000000000000000000000000:1F90 00000000000000000000000000000000:0000 0A 00000000:00000000 00:00000000 00000000     0 0 200301 1 0000 100
`
	got := parseProcNetTCP([]byte(data), FamilyIPv6)
	want := []procNetListener{
		{Host: "::1", Port: 5432, Family: FamilyIPv6, Inode: 200300},
		{Host: "::", Port: 8080, Family: FamilyIPv6, Inode: 200301},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("parseProcNetTCP v6 = %+v, want %+v", got, want)
	}
}

func TestParseProcNetTCPMalformedSkipped(t *testing.T) {
	const data = `  sl  local_address rem_address   st ...
   0: not-an-address 00000000:0000 0A x x x x 0 0 999 1
   1: 0100007F:1F90 00000000:0000 0A 00000000:00000000 00:00000000 00000000  1000 0 100200 1
`
	got := parseProcNetTCP([]byte(data), FamilyIPv4)
	want := []procNetListener{{Host: "127.0.0.1", Port: 8080, Family: FamilyIPv4, Inode: 100200}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("parseProcNetTCP = %+v, want %+v", got, want)
	}
}

func TestDecodeProcNetAddrAcceptsMaximumPort(t *testing.T) {
	host, port, ok := decodeProcNetAddr("0100007F:FFFF", FamilyIPv4)
	if !ok || host != "127.0.0.1" || port != 65535 {
		t.Fatalf("decodeProcNetAddr(max port) = (%q, %d, %v), want (%q, %d, true)",
			host, port, ok, "127.0.0.1", 65535)
	}
}

func TestParseProcNetTCPAcceptsMinimumFields(t *testing.T) {
	const data = `0: 0100007F:1F90 00000000:0000 0A 00000000:00000000 00:00000000 00000000 1000 0 100200`

	got := parseProcNetTCP([]byte(data), FamilyIPv4)
	want := []procNetListener{{Host: "127.0.0.1", Port: 8080, Family: FamilyIPv4, Inode: 100200}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("parseProcNetTCP with ten fields = %+v, want %+v", got, want)
	}
}

func TestParseProcNetTCPAcceptsLargeProcLine(t *testing.T) {
	const prefix = `0: 0100007F:1F90 00000000:0000 0A 00000000:00000000 00:00000000 00000000 1000 0 100200 `
	data := prefix + strings.Repeat("x", 70*1024) + "\n"

	got := parseProcNetTCP([]byte(data), FamilyIPv4)
	want := []procNetListener{{Host: "127.0.0.1", Port: 8080, Family: FamilyIPv4, Inode: 100200}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("parseProcNetTCP with large line = %+v, want %+v", got, want)
	}
}

func TestParseSocketInode(t *testing.T) {
	tests := []struct {
		link  string
		want  uint64
		wantK bool
	}{
		{"socket:[100200]", 100200, true},
		{"socket:[0]", 0, true},
		{"/dev/pts/0", 0, false},
		{"anon_inode:[eventpoll]", 0, false},
		{"socket:[notnum]", 0, false},
		{"pipe:[123]", 0, false},
	}
	for _, tt := range tests {
		t.Run(tt.link, func(t *testing.T) {
			got, ok := parseSocketInode(tt.link)
			if ok != tt.wantK || got != tt.want {
				t.Fatalf("parseSocketInode(%q) = (%d,%v), want (%d,%v)", tt.link, got, ok, tt.want, tt.wantK)
			}
		})
	}
}
