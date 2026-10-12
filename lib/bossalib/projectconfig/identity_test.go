package projectconfig

import (
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/recurser/bossalib/daemonstate"
)

func TestLocalIdentityFromState(t *testing.T) {
	const socket = "/tmp/boss-test.sock"

	t.Run("record with id and matching socket", func(t *testing.T) {
		id, warning := LocalIdentityFromState(daemonstate.Metadata{SocketPath: socket, DaemonID: "d-1", DisplayName: "laptop"}, nil, socket, "from-settings")
		if id != (DaemonIdentity{ID: "d-1", DisplayName: "laptop"}) || warning != "" {
			t.Fatalf("got %+v warning %q", id, warning)
		}
	})

	t.Run("record with id but no display name keeps the fallback name", func(t *testing.T) {
		id, warning := LocalIdentityFromState(daemonstate.Metadata{SocketPath: socket, DaemonID: "d-1", DisplayName: "  "}, nil, socket, "from-settings")
		if id != (DaemonIdentity{ID: "d-1", DisplayName: "from-settings"}) || warning != "" {
			t.Fatalf("got %+v warning %q", id, warning)
		}
	})

	t.Run("a record with no socket on either side is trusted", func(t *testing.T) {
		if id, _ := LocalIdentityFromState(daemonstate.Metadata{DaemonID: "d-1"}, nil, socket, "n"); id.ID != "d-1" {
			t.Fatalf("record without a socket: %+v", id)
		}
		if id, _ := LocalIdentityFromState(daemonstate.Metadata{DaemonID: "d-1", SocketPath: socket}, nil, "", "n"); id.ID != "d-1" {
			t.Fatalf("caller without a socket: %+v", id)
		}
	})

	fallbacks := map[string]struct {
		md  daemonstate.Metadata
		err error
	}{
		"record names another socket": {md: daemonstate.Metadata{SocketPath: "/tmp/other.sock", DaemonID: "d-1", DisplayName: "laptop"}},
		"read error":                  {err: os.ErrNotExist},
		"decode error":                {md: daemonstate.Metadata{DaemonID: "d-1", SocketPath: socket}, err: errors.New("decode daemon metadata: bad json")},
		"empty daemon id":             {md: daemonstate.Metadata{SocketPath: socket, DisplayName: "laptop"}},
	}
	for name, c := range fallbacks {
		t.Run(name, func(t *testing.T) {
			id, warning := LocalIdentityFromState(c.md, c.err, socket, "from-settings")
			if id != (DaemonIdentity{DisplayName: "from-settings"}) {
				t.Fatalf("want a name-only identity from the fallback, got %+v", id)
			}
			if !strings.Contains(warning, "routing id is unknown") || !strings.Contains(warning, `"from-settings"`) {
				t.Fatalf("warning %q", warning)
			}
		})
	}
}
