package status

import (
	"slices"
	"testing"

	"github.com/recurser/bossalib/kit/vcs"
)

func TestSetServiceLinks(t *testing.T) {
	tr := NewDisplayTracker()
	rc := &countingRecomputer{}
	tr.SetRecomputer(rc)
	published := 0
	tr.SetOnAttentionChange(func(string) { published++ })
	links := []ServiceLink{{Name: "web", URL: "https://web.example.test"}, {Name: "api", URL: "https://api.example.test"}}
	want := []ServiceLink{links[1], links[0]}
	tr.SetServiceLinks("s", links)
	if got := tr.Get("s"); got == nil || !slices.Equal(got.ServiceLinks, want) {
		t.Fatalf("links = %+v", got)
	}
	if rc.count() != 1 || published != 1 {
		t.Fatalf("recomputes=%d publications=%d", rc.count(), published)
	}
	links[0].URL = "changed"
	got := tr.Get("s")
	got.ServiceLinks[0].URL = "changed"
	batch := tr.GetBatch([]string{"s"})
	batch["s"].ServiceLinks[0].URL = "changed"
	if !slices.Equal(tr.Get("s").ServiceLinks, want) {
		t.Fatal("caller mutated tracker links")
	}
	tr.SetServiceLinks("s", []ServiceLink{want[1], want[0]})
	if rc.count() != 1 || published != 1 {
		t.Fatal("identical set republished")
	}
	tr.Set("s", vcs.DisplayInfo{Status: vcs.DisplayStatusPassing})
	if !slices.Equal(tr.Get("s").ServiceLinks, want) {
		t.Fatal("poll lost links")
	}
	tr.SetServiceLinks("s", nil)
	if got := tr.Get("s"); got == nil || got.Status != vcs.DisplayStatusPassing || len(got.ServiceLinks) != 0 {
		t.Fatalf("clearing changed PR state: %+v", got)
	}
	tr.SetServiceLinks("only", want)
	tr.SetSettingUp("only", true)
	tr.SetSettingUp("only", false)
	if tr.Get("only") == nil {
		t.Fatal("clearing another flag lost links")
	}
	tr.SetServiceLinks("only", nil)
	if tr.Get("only") != nil {
		t.Fatal("links-only entry survived clearing")
	}
	before := rc.count()
	tr.SetServiceLinks("absent", nil)
	if tr.Get("absent") != nil || rc.count() != before {
		t.Fatal("empty absent set created state")
	}
}
