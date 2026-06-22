package trackerprompt

import (
	"testing"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

func TestFormat_LinearFull(t *testing.T) {
	got := Format(&pb.TrackerIssue{
		ExternalId:  "FRE-1176",
		Title:       "Fix login",
		Description: "Users can't log in",
		Url:         "https://linear.app/x/FRE-1176",
	}, "linear")
	want := "Linear issue:\n\n[FRE-1176] Fix login\n\nUsers can't log in\n\nhttps://linear.app/x/FRE-1176\n"
	if got != want {
		t.Fatalf("got %q want %q", got, want)
	}
}

func TestFormat_SentryLabel(t *testing.T) {
	got := Format(&pb.TrackerIssue{ExternalId: "X-1", Title: "boom"}, "sentry")
	want := "Sentry issue:\n\n[X-1] boom\n"
	if got != want {
		t.Fatalf("got %q want %q", got, want)
	}
}

func TestFormat_Nil(t *testing.T) {
	if Format(nil, "linear") != "" {
		t.Fatal("nil issue should format to empty string")
	}
}

func TestFormat_OmitsMissingFields(t *testing.T) {
	got := Format(&pb.TrackerIssue{ExternalId: "A-2"}, "linear")
	want := "Linear issue:\n\n[A-2]\n"
	if got != want {
		t.Fatalf("got %q want %q", got, want)
	}
}
