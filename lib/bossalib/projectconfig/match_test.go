package projectconfig

import "testing"

func TestMatchDaemon(t *testing.T) {
	pinned := func(name, id string) Daemon { return Daemon{Name: name, DaemonID: &id} }
	unpinned := func(name string) Daemon { return Daemon{Name: name} }

	cases := []struct {
		name      string
		entries   []Daemon
		id        DaemonIdentity
		wantIndex int
		want      MatchVerdict
	}{
		{
			name:      "pinned id wins over a name match on another entry",
			entries:   []Daemon{unpinned("laptop"), pinned("server", "d-1")},
			id:        DaemonIdentity{ID: "d-1", DisplayName: "laptop"},
			wantIndex: 1, want: MatchByID,
		},
		{
			name:      "pinned id matches whatever the display name",
			entries:   []Daemon{pinned("server", "d-1")},
			id:        DaemonIdentity{ID: "d-1", DisplayName: "renamed"},
			wantIndex: 0, want: MatchByID,
		},
		{
			name:      "unique display name among unpinned entries",
			entries:   []Daemon{unpinned("server"), unpinned("laptop")},
			id:        DaemonIdentity{ID: "d-9", DisplayName: "laptop"},
			wantIndex: 1, want: MatchByName,
		},
		{
			name:      "name match without a live id",
			entries:   []Daemon{unpinned("laptop")},
			id:        DaemonIdentity{DisplayName: "laptop"},
			wantIndex: 0, want: MatchByName,
		},
		{
			name:      "pinned entry with the same name but another id is not a name candidate",
			entries:   []Daemon{pinned("laptop", "d-other"), unpinned("laptop")},
			id:        DaemonIdentity{ID: "d-1", DisplayName: "laptop"},
			wantIndex: 1, want: MatchByName,
		},
		{
			name:      "pinned entry with the same name only is unmatched",
			entries:   []Daemon{pinned("laptop", "d-other")},
			id:        DaemonIdentity{ID: "d-1", DisplayName: "laptop"},
			wantIndex: -1, want: MatchUnmatched,
		},
		{
			name:      "no match",
			entries:   []Daemon{unpinned("server"), pinned("ci", "d-ci")},
			id:        DaemonIdentity{ID: "d-1", DisplayName: "laptop"},
			wantIndex: -1, want: MatchUnmatched,
		},
		{
			name:      "empty identity matches nothing",
			entries:   []Daemon{unpinned(""), pinned("x", "")},
			id:        DaemonIdentity{},
			wantIndex: -1, want: MatchUnmatched,
		},
		{
			name:      "no entries",
			id:        DaemonIdentity{ID: "d-1", DisplayName: "laptop"},
			wantIndex: -1, want: MatchUnmatched,
		},
		{
			name:      "two unpinned entries sharing the display name",
			entries:   []Daemon{unpinned("laptop"), unpinned("server"), unpinned("laptop")},
			id:        DaemonIdentity{ID: "d-1", DisplayName: "laptop"},
			wantIndex: -1, want: MatchAmbiguous,
		},
		{
			name:      "an id match resolves a name ambiguity",
			entries:   []Daemon{unpinned("laptop"), unpinned("laptop"), pinned("laptop", "d-1")},
			id:        DaemonIdentity{ID: "d-1", DisplayName: "laptop"},
			wantIndex: 2, want: MatchByID,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			index, verdict := MatchDaemon(tc.entries, tc.id)
			if index != tc.wantIndex || verdict != tc.want {
				t.Fatalf("MatchDaemon = (%d, %s), want (%d, %s)", index, verdict, tc.wantIndex, tc.want)
			}
		})
	}
}
