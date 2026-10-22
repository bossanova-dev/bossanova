package sessionports

import (
	"context"
	"errors"
	"os/exec"
	"reflect"
	"strings"
	"testing"
)

// TestClassifyLsofResult pins the macOS completeness contract portably (it runs
// on every OS, unlike the lsof binary): a clean exit — even non-zero because
// some PIDs have no listeners — is authoritative; a timeout/cancel is
// GlobalIncomplete; only an inability to execute lsof is a source error.
func TestClassifyLsofResult(t *testing.T) {
	const twoListeners = "p652\ntIPv4\nn*:7000\np828\ntIPv6\nn[::1]:5432\n"

	t.Run("clean zero exit parses as complete", func(t *testing.T) {
		scan, err := classifyLsofResult([]byte(twoListeners), nil, nil)
		if err != nil || scan.GlobalIncomplete || len(scan.Listeners) != 2 {
			t.Fatalf("got scan=%+v err=%v", scan, err)
		}
	})
	t.Run("non-zero exit with no listeners is authoritative empty", func(t *testing.T) {
		// lsof exits 1 when the batch has no listening sockets: complete, empty.
		exitErr := runExitError(t)
		scan, err := classifyLsofResult(nil, exitErr, nil)
		if err != nil || scan.GlobalIncomplete || len(scan.Listeners) != 0 {
			t.Fatalf("no-listener exit should be complete empty, got scan=%+v err=%v", scan, err)
		}
	})
	t.Run("non-zero exit still parses any emitted rows", func(t *testing.T) {
		exitErr := runExitError(t)
		scan, err := classifyLsofResult([]byte(twoListeners), exitErr, nil)
		if err != nil || scan.GlobalIncomplete || len(scan.Listeners) != 2 {
			t.Fatalf("got scan=%+v err=%v", scan, err)
		}
	})
	t.Run("derived deadline is global incomplete", func(t *testing.T) {
		scan, err := classifyLsofResult([]byte(twoListeners), context.DeadlineExceeded, nil)
		if err != nil || !scan.GlobalIncomplete || len(scan.Listeners) != 0 {
			t.Fatalf("timeout should be global incomplete empty, got scan=%+v err=%v", scan, err)
		}
	})
	t.Run("parent cancellation is global incomplete", func(t *testing.T) {
		scan, err := classifyLsofResult([]byte(twoListeners), nil, context.Canceled)
		if err != nil || !scan.GlobalIncomplete {
			t.Fatalf("parent cancel should be global incomplete, got scan=%+v err=%v", scan, err)
		}
	})
	t.Run("exec failure is a source error", func(t *testing.T) {
		execErr := &exec.Error{Name: "lsof", Err: errors.New("executable file not found in $PATH")}
		scan, err := classifyLsofResult(nil, execErr, nil)
		if !errors.Is(err, execErr) || scan.GlobalIncomplete {
			t.Fatalf("exec failure should surface as error, got scan=%+v err=%v", scan, err)
		}
	})
}

// runExitError produces a genuine *exec.ExitError (non-zero clean exit) to stand
// in for lsof's exit-1 "no matching files" result.
func runExitError(t *testing.T) error {
	t.Helper()
	if _, err := exec.LookPath("false"); err != nil {
		t.Skip("false not available")
	}
	err := exec.Command("false").Run()
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		t.Fatalf("expected *exec.ExitError from `false`, got %v", err)
	}
	return err
}

func TestParseLsof(t *testing.T) {
	// Real macOS `lsof -nP -a -p <csv> -iTCP -sTCP:LISTEN -Fpnt` shape: one
	// process record (p) followed by file records, each with an fd (f), a type
	// (t), and a name (n). The type field is what disambiguates IPv4 vs IPv6
	// for wildcard binds that share identical "n" text.
	const output = `p652
f10
tIPv4
n*:55187
f11
tIPv6
n*:55187
p828
f7
tIPv6
n[::1]:5432
f8
tIPv4
n127.0.0.1:5432
`
	got := parseLsof([]byte(output))
	want := []Listener{
		{PID: 652, Address: "*", Port: 55187, Family: FamilyIPv4},
		{PID: 652, Address: "*", Port: 55187, Family: FamilyIPv6},
		{PID: 828, Address: "::1", Port: 5432, Family: FamilyIPv6},
		{PID: 828, Address: "127.0.0.1", Port: 5432, Family: FamilyIPv4},
	}
	if !reflect.DeepEqual(got.Listeners, want) {
		t.Fatalf("parseLsof listeners = %+v, want %+v", got.Listeners, want)
	}
	if len(got.IncompletePIDs) != 0 {
		t.Fatalf("expected no incomplete pids, got %v", got.IncompletePIDs)
	}
	if got.GlobalIncomplete {
		t.Fatalf("expected scan not globally incomplete")
	}
}

func TestParseLsofWildcardIPv6(t *testing.T) {
	const output = `p900
f3
tIPv6
n[::]:8080
`
	got := parseLsof([]byte(output))
	want := []Listener{{PID: 900, Address: "::", Port: 8080, Family: FamilyIPv6}}
	if !reflect.DeepEqual(got.Listeners, want) {
		t.Fatalf("parseLsof = %+v, want %+v", got.Listeners, want)
	}
}

func TestSplitHostPortAcceptsBoundaryPorts(t *testing.T) {
	tests := []struct {
		name string
		in   string
		port int
	}{
		{name: "zero", in: "127.0.0.1:0", port: 0},
		{name: "maximum", in: "127.0.0.1:65535", port: 65535},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			host, port, ok := splitHostPort(tt.in)
			if !ok || host != "127.0.0.1" || port != tt.port {
				t.Fatalf("splitHostPort(%q) = (%q, %d, %v), want (%q, %d, true)",
					tt.in, host, port, ok, "127.0.0.1", tt.port)
			}
		})
	}
}

func TestSplitHostPortRejectsEmptyBracketedHost(t *testing.T) {
	host, port, ok := splitHostPort("[]:8080")
	if ok {
		t.Fatalf("splitHostPort(empty bracketed host) = (%q, %d, true), want parse failure", host, port)
	}
}

func TestParseLsofAcceptsFieldLargerThanScannerDefault(t *testing.T) {
	// lsof can emit long command/name metadata fields. A field larger than
	// bufio.Scanner's 64 KiB default must not truncate later listener records.
	output := "x" + strings.Repeat("a", 64*1024) + "\n" +
		"p900\ntIPv4\nn127.0.0.1:8080\n"

	got := parseLsof([]byte(output))
	want := []Listener{{PID: 900, Address: "127.0.0.1", Port: 8080, Family: FamilyIPv4}}
	if !reflect.DeepEqual(got.Listeners, want) {
		t.Fatalf("parseLsof listeners = %+v, want %+v", got.Listeners, want)
	}
	if got.GlobalIncomplete {
		t.Fatal("large valid field must not make scan globally incomplete")
	}
}

func TestParseLsofMalformedRowScopedToOwner(t *testing.T) {
	// pid 828 has one good row and one malformed name (no port). The good row
	// is retained; only pid 828 is marked incomplete. pid 652 is untouched.
	const output = `p652
tIPv4
n*:7000
p828
tIPv4
n127.0.0.1:5432
tIPv4
ngarbage-no-port
`
	got := parseLsof([]byte(output))
	want := []Listener{
		{PID: 652, Address: "*", Port: 7000, Family: FamilyIPv4},
		{PID: 828, Address: "127.0.0.1", Port: 5432, Family: FamilyIPv4},
	}
	if !reflect.DeepEqual(got.Listeners, want) {
		t.Fatalf("parseLsof = %+v, want %+v", got.Listeners, want)
	}
	if !got.IncompletePIDs[828] {
		t.Fatalf("expected pid 828 incomplete, got %v", got.IncompletePIDs)
	}
	if got.IncompletePIDs[652] {
		t.Fatalf("pid 652 must not be incomplete, got %v", got.IncompletePIDs)
	}
}

func TestParseLsofNameWithoutTypeIsIncomplete(t *testing.T) {
	// A name with no preceding recognizable type field cannot be classified as
	// IPv4/IPv6, so it is dropped and its owner marked incomplete rather than
	// guessed.
	const output = `p700
n127.0.0.1:9000
`
	got := parseLsof([]byte(output))
	if len(got.Listeners) != 0 {
		t.Fatalf("expected no listeners, got %+v", got.Listeners)
	}
	if !got.IncompletePIDs[700] {
		t.Fatalf("expected pid 700 incomplete, got %v", got.IncompletePIDs)
	}
}

func TestParseLsofEmpty(t *testing.T) {
	got := parseLsof([]byte(""))
	if len(got.Listeners) != 0 || got.GlobalIncomplete || len(got.IncompletePIDs) != 0 {
		t.Fatalf("empty lsof output should be an empty complete scan, got %+v", got)
	}
}
