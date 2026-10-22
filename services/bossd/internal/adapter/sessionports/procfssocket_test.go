package sessionports

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"testing"
)

// writeFakeProc builds a synthetic procfs tree under a temp dir: net tables
// plus per-pid fd symlinks. fdLinks maps pid -> (fd name -> symlink target).
func writeFakeProc(t *testing.T, tcp, tcp6 string, fdLinks map[int]map[string]string) string {
	t.Helper()
	root := t.TempDir()
	netDir := filepath.Join(root, "net")
	if err := os.MkdirAll(netDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if tcp != "" {
		if err := os.WriteFile(filepath.Join(netDir, "tcp"), []byte(tcp), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if tcp6 != "" {
		if err := os.WriteFile(filepath.Join(netDir, "tcp6"), []byte(tcp6), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	for pid, links := range fdLinks {
		fdDir := filepath.Join(root, strconv.Itoa(pid), "fd")
		if err := os.MkdirAll(fdDir, 0o755); err != nil {
			t.Fatal(err)
		}
		for name, target := range links {
			if err := os.Symlink(target, filepath.Join(fdDir, name)); err != nil {
				t.Fatal(err)
			}
		}
	}
	return root
}

const fakeTCP = `  sl  local_address rem_address   st tx rx tr tm rtx uid to inode
   0: 0100007F:1F90 00000000:0000 0A 00000000:00000000 00:00000000 00000000 1000 0 100200 1 x
`

const fakeTCP6 = `  sl  local_address                         remote_address                        st tx rx tr tm rtx uid to inode
   0: 00000000000000000000000001000000:1538 00000000000000000000000000000000:0000 0A 00000000:00000000 00:00000000 00000000 1000 0 200300 1 x
`

func TestProcfsSocketSourceAttributesInodesToPIDs(t *testing.T) {
	root := writeFakeProc(t, fakeTCP, fakeTCP6, map[int]map[string]string{
		101: {
			"3": "socket:[100200]",    // -> 127.0.0.1:8080 v4
			"4": "socket:[200300]",    // -> ::1:5432 v6
			"5": "/dev/null",          // non-socket, ignored
			"6": "socket:[999999]",    // unknown inode, ignored
			"7": "anon_inode:[epoll]", // ignored
		},
	})
	src := &procfsSocketSource{root: root}
	scan, err := src.Listeners(context.Background(), []int{101, 202 /* missing pid */})
	if err != nil {
		t.Fatalf("Listeners error: %v", err)
	}
	if scan.GlobalIncomplete || len(scan.IncompletePIDs) != 0 {
		t.Fatalf("expected complete scan, got %+v", scan)
	}
	got := append([]Listener(nil), scan.Listeners...)
	sort.Slice(got, func(i, j int) bool { return got[i].Port < got[j].Port })
	want := []Listener{
		{PID: 101, Address: "127.0.0.1", Port: 8080, Family: FamilyIPv4},
		{PID: 101, Address: "::1", Port: 5432, Family: FamilyIPv6},
	}
	sort.Slice(want, func(i, j int) bool { return want[i].Port < want[j].Port })
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("listeners = %+v, want %+v", got, want)
	}
}

func TestProcfsSocketSourcePermissionFailureScopedToPID(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root bypasses directory permissions")
	}
	root := writeFakeProc(t, fakeTCP, "", map[int]map[string]string{
		101: {"3": "socket:[100200]"}, // -> 127.0.0.1:8080, readable
		202: {"3": "socket:[100200]"}, // fd dir will be made unreadable
	})
	fdDir := filepath.Join(root, "202", "fd")
	if err := os.Chmod(fdDir, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(fdDir, 0o755) }) // let TempDir cleanup recurse
	src := &procfsSocketSource{root: root}
	scan, err := src.Listeners(context.Background(), []int{101, 202})
	if err != nil {
		t.Fatalf("Listeners error: %v", err)
	}
	if scan.GlobalIncomplete {
		t.Fatalf("a per-PID permission failure must not be global, got %+v", scan)
	}
	if !scan.IncompletePIDs[202] {
		t.Fatalf("expected pid 202 marked incomplete, got %v", scan.IncompletePIDs)
	}
	if scan.IncompletePIDs[101] {
		t.Fatalf("pid 101 must stay complete, got %v", scan.IncompletePIDs)
	}
	if len(scan.Listeners) != 1 || scan.Listeners[0].PID != 101 {
		t.Fatalf("expected 101's listener retained, got %+v", scan.Listeners)
	}
}

func TestProcfsSocketSourceUnreadableNetTableIsGlobalIncomplete(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root bypasses file permissions")
	}
	// tcp is readable but tcp6 exists and cannot be read: a whole address
	// family is invisible, so the batch must degrade rather than silently omit.
	root := writeFakeProc(t, fakeTCP, fakeTCP6, map[int]map[string]string{
		101: {"3": "socket:[100200]"},
	})
	tcp6 := filepath.Join(root, "net", "tcp6")
	if err := os.Chmod(tcp6, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(tcp6, 0o644) })
	src := &procfsSocketSource{root: root}
	scan, err := src.Listeners(context.Background(), []int{101})
	if err != nil {
		t.Fatalf("Listeners error: %v", err)
	}
	if !scan.GlobalIncomplete {
		t.Fatalf("expected global incomplete when a net table is unreadable, got %+v", scan)
	}
}

func TestProcfsSocketSourceNoNetTablesIsGlobalIncomplete(t *testing.T) {
	root := writeFakeProc(t, "", "", nil) // no tcp/tcp6 files
	src := &procfsSocketSource{root: root}
	scan, err := src.Listeners(context.Background(), []int{1})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !scan.GlobalIncomplete {
		t.Fatalf("expected global incomplete when net tables unreadable, got %+v", scan)
	}
}

func TestProcfsSocketSourceCanceledContext(t *testing.T) {
	root := writeFakeProc(t, fakeTCP, "", nil)
	src := &procfsSocketSource{root: root}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	scan, err := src.Listeners(ctx, []int{1})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !scan.GlobalIncomplete {
		t.Fatalf("expected global incomplete on canceled context, got %+v", scan)
	}
}
