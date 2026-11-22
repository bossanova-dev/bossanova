package server

import (
	"context"
	"testing"

	"connectrpc.com/connect"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	dbpkg "github.com/recurser/bossd/internal/db"
)

// TestUpdateRepo_FlagsReachTheWebRead sets each web-editable repo flag both
// ways through UpdateRepo and reads it back through GetRepoSettings, the
// read behind get_repo. A flag the request leaves out stays as it was.
func TestUpdateRepo_FlagsReachTheWebRead(t *testing.T) {
	flags := []struct {
		name string
		set  func(*pb.UpdateRepoRequest, *bool)
		get  func(*pb.RepoSettings) bool
	}{
		{"should_keep_branches_current", func(r *pb.UpdateRepoRequest, v *bool) { r.ShouldKeepBranchesCurrent = v }, (*pb.RepoSettings).GetShouldKeepBranchesCurrent},
		{"can_auto_delete_branches", func(r *pb.UpdateRepoRequest, v *bool) { r.CanAutoDeleteBranches = v }, (*pb.RepoSettings).GetCanAutoDeleteBranches},
		{"can_auto_rotate", func(r *pb.UpdateRepoRequest, v *bool) { r.CanAutoRotate = v }, (*pb.RepoSettings).GetCanAutoRotate},
	}
	ctx := context.Background()
	store := dbpkg.NewRepoStore(setupServerTestDB(t))
	s := New(Config{Repos: store})
	id := newRepoWithLinear(t, store, "")
	read := func() *pb.RepoSettings {
		resp, err := s.GetRepoSettings(ctx, connect.NewRequest(&pb.GetRepoSettingsRequest{Id: id}))
		if err != nil {
			t.Fatalf("GetRepoSettings: %v", err)
		}
		return resp.Msg.GetSettings()
	}
	for _, flag := range flags {
		for _, want := range []bool{true, false} {
			others := map[string]bool{}
			for _, other := range flags {
				others[other.name] = other.get(read())
			}
			req := &pb.UpdateRepoRequest{Id: id}
			flag.set(req, &want)
			if _, err := s.UpdateRepo(ctx, connect.NewRequest(req)); err != nil {
				t.Fatalf("UpdateRepo(%s=%v): %v", flag.name, want, err)
			}
			got := read()
			if flag.get(got) != want {
				t.Fatalf("%s = %v after setting %v", flag.name, flag.get(got), want)
			}
			for _, other := range flags {
				if other.name != flag.name && other.get(got) != others[other.name] {
					t.Fatalf("setting %s changed %s", flag.name, other.name)
				}
			}
		}
	}

	trusted := true
	if _, err := store.Update(ctx, id, dbpkg.UpdateRepoParams{IsTrusted: &trusted}); err != nil {
		t.Fatal(err)
	}
	if !read().GetIsTrusted() {
		t.Fatal("GetRepoSettings did not report the repo's local trust")
	}
}
