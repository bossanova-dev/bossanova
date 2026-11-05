package ui

import (
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

func TestShortSessionTitle(t *testing.T) {
	cases := []struct{ name, title, id, want string }{
		{"untracked", "A short title", "", "A short title"},
		{"token start", "[BOS-1] A short title", "BOS-1", "A short title"},
		{"token middle", "A  [BOS-1]\tshort title", "BOS-1", "A short title"},
		{"token end", "A short title [BOS-1]", "BOS-1", "A short title"},
		{"token absent", "A short title", "BOS-1", "A short title"},
		{"first token only", "[BOS-1] A [BOS-1] title", "BOS-1", "A [BOS-1] title"},
		{"exact limit", strings.Repeat("a", 40), "", strings.Repeat("a", 40)},
		{"word boundary", "Improve session titles for the home board and web tailword", "", "Improve session titles for the home…"},
		{"long word", strings.Repeat("a", 41), "", strings.Repeat("a", 39) + "…"},
		{"unicode", strings.Repeat("😀", 41), "", strings.Repeat("😀", 39) + "…"},
		{"unicode space", strings.Repeat("界", 35) + "\u3000more words", "", strings.Repeat("界", 35) + "…"},
		{"token only", " [BOS-1] ", "BOS-1", ""},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			if got := ShortSessionTitle(tt.title, tt.id); got != tt.want {
				t.Errorf("got %q, want %q", got, tt.want)
			}
		})
	}
}

func TestSessionListTitle(t *testing.T) {
	id := "BOS-1"
	for _, tt := range []struct {
		name  string
		sess  *pb.Session
		width int
		want  string
	}{
		{"tracked", &pb.Session{Title: "[BOS-1] short", TrackerId: &id}, 5, "BOS-1  short"},
		{"renamed", &pb.Session{Title: "short", TrackerId: &id}, 5, "BOS-1  short"},
		{"padded to widest id", &pb.Session{Title: "short", TrackerId: &id}, 8, "BOS-1     short"},
		{"only id", &pb.Session{Title: "[BOS-1]", TrackerId: &id}, 8, "BOS-1"},
		{"branch fallback", &pb.Session{BranchName: "branch", TrackerId: &id}, 5, "BOS-1  branch"},
		{"untracked branch", &pb.Session{BranchName: "branch"}, 8, "branch"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := SessionListTitle(tt.sess, tt.width); got != tt.want {
				t.Errorf("got %q, want %q", got, tt.want)
			}
		})
	}
}

func TestSessionListTrackerLinks_Sessions_AC151(t *testing.T) {
	id, url := "BOS-1", "https://linear.app/example/issue/BOS-1"
	for name, render := range map[string]func(*pb.Session, string) string{"normal": RenderTrackerLink, "muted": RenderMutedTrackerLink, "selected": RenderSelectedTrackerLink} {
		t.Run(name, func(t *testing.T) {
			for _, title := range []string{"BOS-1  short", "BOS-1     short", "BOS-1", "[BOS-1] original title"} {
				got := render(&pb.Session{TrackerId: &id, TrackerUrl: &url}, title)
				if ansi.Strip(got) != title || !strings.Contains(got, "\x1b]8;;"+url) {
					t.Errorf("title %q not linked intact: %q", title, got)
				}
				if plain := render(&pb.Session{TrackerId: &id}, title); strings.Contains(plain, "\x1b]8;;") {
					t.Errorf("URL-less title linked: %q", plain)
				}
			}
			for _, title := range []string{"prefix BOS-1 title", "BOS-10  title", "BOS-1 unrelated"} {
				if got := render(&pb.Session{TrackerId: &id, TrackerUrl: &url}, title); strings.Contains(got, "\x1b]8;;") {
					t.Errorf("unrelated title linked: %q", got)
				}
			}
		})
	}
}

func TestSessionListIDWidth(t *testing.T) {
	short, long := "GIG-629", "FRE-1966"
	sessions := []*pb.Session{{TrackerId: &short}, {}, {TrackerId: &long}}
	if got := SessionListIDWidth(sessions); got != len(long) {
		t.Fatalf("got %d, want %d", got, len(long))
	}
	a, b := SessionListTitle(&pb.Session{Title: "a", TrackerId: &short}, 8), SessionListTitle(&pb.Session{Title: "b", TrackerId: &long}, 8)
	if strings.Index(a, "a") != strings.Index(b, "b") {
		t.Fatalf("titles not aligned: %q vs %q", a, b)
	}
}
