package sessionfilter

import (
	"slices"
	"testing"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

func ids(sessions []*pb.Session) []string {
	out := []string{}
	for _, s := range sessions {
		out = append(out, s.GetId())
	}
	return out
}

func fixture() []*pb.Session {
	return []*pb.Session{
		{Id: "octo-main", PrAuthorLogin: "Octocat", PrTargetBranch: "main"},
		{Id: "octo-dev", PrAuthorLogin: "octocat", PrTargetBranch: "dev"},
		{Id: "hubot-main", PrAuthorLogin: "hubot", PrTargetBranch: "main"},
		{Id: "hubot-Main", PrAuthorLogin: "hubot", PrTargetBranch: "Main"},
		{Id: "no-pr"},
	}
}

func TestPRFilter(t *testing.T) {
	t.Parallel()
	all := []string{"octo-main", "octo-dev", "hubot-main", "hubot-Main", "no-pr"}
	for _, tc := range []struct {
		name             string
		authors, targets []string
		want             []string
	}{
		{"empty lists match everything", nil, nil, all},
		{"blank entries are ignored", []string{"", "  "}, []string{" "}, all},
		{"author matches any, ignoring case", []string{"OCTOCAT"}, nil, []string{"octo-main", "octo-dev"}},
		{"author entries are trimmed", []string{" hubot "}, nil, []string{"hubot-main", "hubot-Main"}},
		{"several authors match any", []string{"octocat", "hubot"}, nil, []string{"octo-main", "octo-dev", "hubot-main", "hubot-Main"}},
		{"target branch matches exactly", nil, []string{"main"}, []string{"octo-main", "hubot-main"}},
		{"target branch is case-sensitive", nil, []string{"Main"}, []string{"hubot-Main"}},
		{"both filters must match", []string{"octocat"}, []string{"main"}, []string{"octo-main"}},
		{"blank entries beside real ones still constrain", []string{"", "hubot"}, nil, []string{"hubot-main", "hubot-Main"}},
		{"no match is empty", []string{"nobody"}, nil, []string{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := ids(ByPR(tc.authors, tc.targets).Filter(fixture()))
			if !slices.Equal(got, tc.want) {
				t.Fatalf("Filter = %v; want %v", got, tc.want)
			}
		})
	}
}

func TestPRNeverMatchesSessionWithoutPR(t *testing.T) {
	t.Parallel()
	noPR := &pb.Session{Id: "no-pr"}
	for _, f := range []PR{ByPR([]string{"octocat"}, nil), ByPR(nil, []string{"main"})} {
		if f.Matches(noPR) {
			t.Fatalf("%+v matched a session with no PR", f)
		}
	}
	if !ByPR(nil, nil).Matches(noPR) {
		t.Fatal("an unconstrained filter rejected a session with no PR")
	}
}

// Filtering a shared list never writes to it, so a coalesced reader keeps
// the full list.
func TestPRFilterLeavesInputUntouched(t *testing.T) {
	t.Parallel()
	shared := fixture()
	before := ids(shared)
	got := ByPR([]string{"hubot"}, nil).Filter(shared)
	if !slices.Equal(ids(shared), before) {
		t.Fatalf("input became %v; want %v", ids(shared), before)
	}
	got = append(got, &pb.Session{Id: "appended"})
	if !slices.Equal(ids(shared), before) || len(got) != 3 {
		t.Fatalf("appending to the result changed the input to %v", ids(shared))
	}
}
