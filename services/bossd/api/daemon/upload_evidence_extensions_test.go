package server

import (
	_ "embed"
	"encoding/json"
	"slices"
	"testing"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

// proofExtensionTable is testdata/proof-extensions.json, a byte copy of
// bosso's table; scripts/release/user-uploads-worker.test.mjs compares the
// copies and serves every extension through the proof host Worker.
type proofExtensionTable struct {
	ContentTypes map[string]string `json:"contentTypes"`
	DaemonMedia  []string          `json:"daemonMedia"`
}

// The table is embedded because this package's bazel test runs from the
// repository root.
//
//go:embed testdata/proof-extensions.json
var proofExtensionTableJSON []byte

func readProofExtensionTable(t *testing.T) proofExtensionTable {
	t.Helper()
	var table proofExtensionTable
	if err := json.Unmarshal(proofExtensionTableJSON, &table); err != nil {
		t.Fatal(err)
	}
	return table
}

func TestEvidenceMediaExtensionsMatchTheSharedTable_Sessions_AC278201(t *testing.T) {
	table := readProofExtensionTable(t)
	got, want := slices.Sorted(slices.Values(evidenceMediaExtensions)), slices.Sorted(slices.Values(table.DaemonMedia))
	if !slices.Equal(got, want) {
		t.Fatalf("media extensions = %v, want the table's %v", got, want)
	}
	for _, extension := range table.DaemonMedia {
		name := "media/clip." + extension
		if got, want := evidenceContentType(name), table.ContentTypes[extension]; got == "" || got != want {
			t.Errorf("content type of %s = %q, want the table's %q", name, got, want)
		}
	}
	for _, name := range []string{"screenshots/a.png", "screenshots/a.jpg", "screenshots/a.jpeg", "screenshots/a.webp"} {
		extension := name[len("screenshots/a."):]
		if got, want := evidenceContentType(name), table.ContentTypes[extension]; got == "" || got != want {
			t.Errorf("content type of %s = %q, want the table's %q", name, got, want)
		}
	}
}

// A .jpeg screenshot and .webm media publish to the hosted proof host.
func TestUploadEvidenceHostedPublishesJPEGAndWebM_Sessions_AC278201(t *testing.T) {
	s, dir := evidenceServer(t, "http://unused.invalid")
	fake := &fakeHostedOpener{}
	s.evidenceBucketOpener = fake
	contents := map[string]string{"index.html": "<!doctype html>", "screenshots/a.jpeg": "jpeg", "media/clip.webm": "webm"}
	objects := hostedPage(t, dir, contents, "screenshots/a.jpeg", "media/clip.webm", "index.html")
	if _, rpcErr := callEvidence(t, s, &pb.UploadEvidenceRequest{EvidenceDir: dir, PrNumber: 42, HeadSha: evidenceTestSHA, Objects: objects, Destination: pb.EvidenceDestination_EVIDENCE_DESTINATION_HOSTED}); rpcErr != nil {
		t.Fatal(rpcErr)
	}
	var names []string
	for _, put := range fake.puts {
		names = append(names, put.Name)
	}
	if want := []string{"screenshots/a.jpeg", "media/clip.webm", "index.html"}; !slices.Equal(names, want) {
		t.Fatalf("puts = %v, want %v", names, want)
	}
}
