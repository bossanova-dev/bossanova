package sessionports

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
)

// procfsSocketSource is the Linux SocketSource. It reads the LISTEN rows from
// /proc/net/tcp and /proc/net/tcp6 (portable-parsed by parseProcNetTCP), maps
// each socket inode to owning PIDs through /proc/<pid>/fd, and scopes per-PID
// read failures (permission denied, a PID that vanished) to the affected owner.
//
// The type carries no OS-specific symbols — os.ReadFile/ReadDir/Readlink are
// portable — so it is unit-tested against a synthetic procfs tree on any host.
// Only its selection as the live SocketSource is gated to Linux (see
// socketsource_linux.go); the plumbing itself compiles everywhere.
type procfsSocketSource struct {
	// root is the procfs mount, overridable in tests. Empty means "/proc".
	root string
}

func (s *procfsSocketSource) procRoot() string {
	if s.root == "" {
		return "/proc"
	}
	return s.root
}

// Listeners builds the inode -> listener index from the net tables, then walks
// each PID's fd directory to attribute listeners. If neither net table can be
// read the whole scan is globally incomplete; a per-PID fd failure marks only
// that PID incomplete.
func (s *procfsSocketSource) Listeners(ctx context.Context, pids []int) (SocketScan, error) {
	if err := ctx.Err(); err != nil {
		return SocketScan{GlobalIncomplete: true}, nil
	}
	byInode := map[uint64]procNetListener{}
	readAny := false
	for _, tbl := range []struct {
		path   string
		family Family
	}{
		{filepath.Join(s.procRoot(), "net", "tcp"), FamilyIPv4},
		{filepath.Join(s.procRoot(), "net", "tcp6"), FamilyIPv6},
	} {
		data, err := os.ReadFile(tbl.path)
		if err != nil {
			if os.IsNotExist(err) {
				continue // table absent (e.g. IPv6 disabled): authoritatively no such sockets
			}
			// The table exists but could not be read (permission, transient):
			// a whole address family is invisible, so no session's evidence is
			// trustworthy. Degrade the batch rather than silently omit it.
			return SocketScan{GlobalIncomplete: true}, nil
		}
		readAny = true
		for _, row := range parseProcNetTCP(data, tbl.family) {
			byInode[row.Inode] = row
		}
	}
	if !readAny {
		return SocketScan{GlobalIncomplete: true}, nil
	}

	var scan SocketScan
	for _, pid := range pids {
		if err := ctx.Err(); err != nil {
			return SocketScan{GlobalIncomplete: true}, nil
		}
		fdDir := filepath.Join(s.procRoot(), fmt.Sprintf("%d", pid), "fd")
		entries, err := os.ReadDir(fdDir)
		if err != nil {
			if os.IsNotExist(err) {
				continue // PID gone: no listeners, not this source's incompleteness
			}
			scan.markIncomplete(pid) // permission denied or other transient read error
			continue
		}
		for _, entry := range entries {
			link, err := os.Readlink(filepath.Join(fdDir, entry.Name()))
			if err != nil {
				if os.IsNotExist(err) {
					continue // fd closed between ReadDir and Readlink: authoritative, skip
				}
				scan.markIncomplete(pid) // permission/other transient: don't-know for this owner
				continue
			}
			inode, ok := parseSocketInode(link)
			if !ok {
				continue
			}
			row, ok := byInode[inode]
			if !ok {
				continue
			}
			scan.Listeners = append(scan.Listeners, Listener{
				PID:     pid,
				Address: row.Host,
				Port:    row.Port,
				Family:  row.Family,
			})
		}
	}
	return scan, nil
}
