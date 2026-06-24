package clidoc

import "testing"

func TestGroupOrderIsStableAndTitled(t *testing.T) {
	if len(GroupOrder) == 0 {
		t.Fatal("GroupOrder must define at least one group")
	}
	seen := map[string]bool{}
	for _, g := range GroupOrder {
		if g.ID == "" || g.Title == "" {
			t.Errorf("group spec has empty field: %+v", g)
		}
		if seen[g.ID] {
			t.Errorf("duplicate group id %q", g.ID)
		}
		seen[g.ID] = true
		title, ok := GroupTitle(g.ID)
		if !ok || title != g.Title {
			t.Errorf("GroupTitle(%q) = %q,%v; want %q,true", g.ID, title, ok, g.Title)
		}
	}
}

func TestGroupTitleUnknownID(t *testing.T) {
	if title, ok := GroupTitle("does-not-exist"); ok || title != "" {
		t.Errorf("GroupTitle(unknown) = %q,%v; want \"\",false", title, ok)
	}
}

func TestRegistryKeysAreCommandPaths(t *testing.T) {
	for path := range Registry {
		if len(path) < len("boss ") || path[:5] != "boss " {
			t.Errorf("registry key %q is not a 'boss ...' command path", path)
		}
	}
}
