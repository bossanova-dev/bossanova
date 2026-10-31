package chatlabel

import (
	"testing"
	"time"
)

func TestLabels(t *testing.T) {
	// Twin table: services/web/src/features/chat/lib/chatLabel.test.ts.
	for _, tc := range []struct {
		name   string
		role   Role
		titles []string
		want   []string
	}{
		{"build", RoleBuild, []string{"build", "build"}, []string{"build", "build 2"}},
		{"verify", RoleVerify, []string{"verify", "verify 2"}, []string{"verify", "verify 2"}},
		{"rename counts", RoleVerify, []string{"my notes", "verify 2"}, []string{"my notes", "verify 2"}},
		{"repair reasons", RoleRepair, []string{"repair: conflict", "repair: checks", "repair 8: review", "repair: P2 waits +2"}, []string{"repair: conflict", "repair 2: checks", "repair 3: review", "repair 4: P2 waits +2"}},
		{"legacy repair", RoleRepair, []string{"Repair: long session"}, []string{"repair"}},
		{"legacy build", RoleBuild, []string{"Run \"long session\"", "long session", "long session (resumed 2)"}, []string{"build", "build 2", "build 3"}},
		{"default agent", RoleBuild, []string{"", "New chat", " BUILD 9 "}, []string{"build", "build 2", "build 3"}},
		{"operator", RoleOperator, []string{"notes", ""}, []string{"notes", "New chat"}},
		{"unknown", Role("other"), []string{"notes", ""}, []string{"notes", "New chat"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			chats := make([]Chat, len(tc.titles))
			for i, title := range tc.titles {
				chats[i] = Chat{ID: string(rune('a' + i)), Role: tc.role, Title: title}
			}
			got := Labels(chats, "long session")
			for i, want := range tc.want {
				if s := got[chats[i].ID]; s != want {
					t.Errorf("label %d = %q, want %q", i, s, want)
				}
			}
		})
	}
}

func TestLabelsChronologicalAndStable(t *testing.T) {
	now := time.Now()
	chats := []Chat{{ID: "new", Role: RoleBuild, Title: "build", CreatedAt: now}, {ID: "old-a", Role: RoleBuild, Title: "build", CreatedAt: now.Add(-time.Hour)}, {ID: "old-b", Role: RoleBuild, Title: "build", CreatedAt: now.Add(-time.Hour)}}
	got := Labels(chats, "")
	for id, want := range map[string]string{"new": "build 3", "old-a": "build", "old-b": "build 2"} {
		if got[id] != want {
			t.Errorf("%s = %q, want %q", id, got[id], want)
		}
	}
	if chats[0].ID != "new" {
		t.Fatal("Labels reordered input")
	}
}

func TestTitle(t *testing.T) {
	for _, tc := range []struct {
		role         Role
		reason, want string
	}{{RoleBuild, "", "build"}, {RoleVerify, "", "verify"}, {RoleRepair, "", "repair"}, {RoleRepair, "checks", "repair: checks"}} {
		if got := Title(tc.role, tc.reason); got != tc.want {
			t.Errorf("Title(%q,%q)=%q, want %q", tc.role, tc.reason, got, tc.want)
		}
	}
}
