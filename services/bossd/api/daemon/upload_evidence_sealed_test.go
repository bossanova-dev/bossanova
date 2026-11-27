package server

import (
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"hash/crc32"
	"image"
	"image/png"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"connectrpc.com/connect"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossd/internal/proofseal"
	"github.com/rs/zerolog"
)

const proofMarker = "BOS2786-PRIVATE-REPO-MARKER"

func pngWithText(t *testing.T, text string) []byte {
	t.Helper()
	var encoded bytes.Buffer
	if err := png.Encode(&encoded, image.NewGray(image.Rect(0, 0, 1, 1))); err != nil {
		t.Fatal(err)
	}
	data := append([]byte("Comment\x00"), text...)
	chunk := binary.BigEndian.AppendUint32(nil, uint32(len(data)))
	typed := append([]byte("tEXt"), data...)
	chunk = append(chunk, typed...)
	chunk = binary.BigEndian.AppendUint32(chunk, crc32.ChecksumIEEE(typed))
	const afterIHDR = 8 + 25 // signature, then IHDR's length, type, 13 bytes and CRC
	raw := encoded.Bytes()
	out := slices.Concat(raw[:afterIHDR], chunk, raw[afterIHDR:])
	if _, err := png.Decode(bytes.NewReader(out)); err != nil {
		t.Fatalf("planted PNG does not decode: %v", err)
	}
	return out
}

type sealedPage struct {
	fake    *fakeHostedOpener
	pageURL string
	key     []byte
	log     string
}

func publishSealedPage(t *testing.T, contents map[string]string, order ...string) sealedPage {
	t.Helper()
	s, dir := evidenceServer(t, "http://unused.invalid")
	s.sealHostedProof = true
	var log bytes.Buffer
	s.logger = zerolog.New(&log)
	fake := &fakeHostedOpener{}
	s.evidenceBucketOpener = fake
	objects := hostedPage(t, dir, contents, order...)
	got, rpcErr := callEvidence(t, s, &pb.UploadEvidenceRequest{EvidenceDir: dir, PrNumber: 42, HeadSha: evidenceTestSHA, Objects: objects, Destination: pb.EvidenceDestination_EVIDENCE_DESTINATION_HOSTED})
	if rpcErr != nil {
		t.Fatal(rpcErr)
	}
	if fake.opened != 0 || got.Destination != pb.EvidenceDestination_EVIDENCE_DESTINATION_HOSTED || got.KeyPrefix != "" || got.PublicBaseUrl != "" {
		t.Fatalf("hosted publish opened the local bucket %d times or answered %v", fake.opened, got.Destination)
	}
	u, err := url.Parse(got.PageUrl)
	if err != nil {
		t.Fatal(err)
	}
	encoded, ok := strings.CutPrefix(u.Fragment, "k=")
	key, decodeErr := base64.RawURLEncoding.DecodeString(encoded)
	if !ok || decodeErr != nil || len(key) != proofseal.KeySize {
		t.Fatalf("page URL fragment %q carries no 32-byte #k= key", u.Fragment)
	}
	u.Fragment = ""
	if want := "https://app.example/review/acme/widgets/42?head=" + evidenceTestSHA; u.String() != want || strings.Contains(u.String(), encoded) {
		t.Fatalf("page URL without its fragment = %q, want %q", u.String(), want)
	}
	if !slices.Equal(got.ObjectKeys, manifestNames(fake.starts[0].Manifest)) {
		t.Fatalf("object keys %v differ from the started manifest", got.ObjectKeys)
	}
	return sealedPage{fake: fake, pageURL: got.PageUrl, key: key, log: log.String()}
}

func manifestNames(manifest []HostedProofEntry) []string {
	names := make([]string, 0, len(manifest))
	for _, entry := range manifest {
		names = append(names, entry.Name)
	}
	return names
}

func plaintextSource(stored string) string {
	switch stored {
	case proofseal.PageName:
		return evidencePageName
	case proofseal.LoaderName, proofseal.ShimName, evidencePlayerName:
		return ""
	}
	return strings.TrimSuffix(stored, proofseal.SealedSuffix)
}

func TestUploadEvidenceHostedSealsEveryContentFile_Sessions_AC278601(t *testing.T) {
	contents := map[string]string{
		"index.html":         `<!doctype html><p>` + proofMarker + `</p><img src="screenshots/a.png">`,
		"screenshots/a.png":  string(pngWithText(t, proofMarker)),
		"media/clip.webm":    "webm " + proofMarker,
		"manifest.json":      `{"note":"` + proofMarker + `"}`,
		"request-log.ndjson": `{"url":"https://example.test/` + proofMarker + `"}`,
		"player.js":          "play()",
	}
	order := []string{"index.html", "screenshots/a.png", "media/clip.webm", "manifest.json", "request-log.ndjson", "player.js"}
	table := readProofExtensionTable(t)
	pages := []sealedPage{publishSealedPage(t, contents, order...), publishSealedPage(t, contents, order...)}
	for i, page := range pages {
		start := page.fake.starts[0]
		secrets := []string{proofMarker, proofseal.FragmentKey(page.key), string(page.key), page.pageURL}
		requests := fmt.Sprintf("%+v", start)
		for _, put := range page.fake.puts {
			requests += put.UploadID + put.RepoOriginURL + put.Name
		}
		for _, secret := range secrets {
			if strings.Contains(requests, secret) || strings.Contains(page.log, secret) {
				t.Fatalf("page %d: a marker, the key or the page URL reached a request or the log", i)
			}
		}
		sizes := map[string]int64{}
		for _, entry := range start.Manifest {
			sizes[entry.Name] = entry.Size
		}
		var names []string
		for _, put := range page.fake.puts {
			names = append(names, put.Name)
			if int64(len(put.Body)) != sizes[put.Name] {
				t.Errorf("page %d: %s is %d bytes, declared %d", i, put.Name, len(put.Body), sizes[put.Name])
			}
			if _, ok := table.ContentTypes[strings.TrimPrefix(path.Ext(put.Name), ".")]; !ok {
				t.Errorf("page %d: %s has an extension outside the shared table", i, put.Name)
			}
			for _, secret := range secrets {
				if bytes.Contains(put.Body, []byte(secret)) {
					t.Fatalf("page %d: stored object %s holds a marker or the key in the clear", i, put.Name)
				}
			}
			source := plaintextSource(put.Name)
			if source == "" {
				continue
			}
			plain, err := proofseal.Open(page.key, put.Name, put.Body)
			if err != nil || string(plain) != contents[source] {
				t.Errorf("page %d: %s does not open to %s under the link's key: %v", i, put.Name, source, err)
			}
		}
		want := []string{"page.html.enc", "screenshots/a.png.enc", "media/clip.webm.enc", "manifest.json.enc", "request-log.ndjson.enc", "player.js", "proof-shim.js", "index.html"}
		if !slices.Equal(names, want) {
			t.Fatalf("page %d: puts = %v, want %v", i, names, want)
		}
	}
	plaintext := func(page sealedPage, name string) []byte {
		i := slices.IndexFunc(page.fake.puts, func(p HostedProofPut) bool { return p.Name == name })
		return page.fake.puts[i].Body
	}
	for name, embedded := range map[string][]byte{proofseal.LoaderName: proofseal.Loader(), proofseal.ShimName: proofseal.Shim()} {
		if a, b := plaintext(pages[0], name), plaintext(pages[1], name); !bytes.Equal(a, embedded) || !bytes.Equal(b, embedded) {
			t.Errorf("%s differs from the embedded bytes or between pages", name)
		}
	}
	if string(plaintext(pages[0], evidencePlayerName)) != contents[evidencePlayerName] {
		t.Error("player.js was not uploaded as plaintext")
	}
	if bytes.Equal(pages[0].key, pages[1].key) {
		t.Fatal("two pages share a page key")
	}
	if _, err := proofseal.Open(pages[1].key, proofseal.PageName, plaintext(pages[0], proofseal.PageName)); err == nil {
		t.Fatal("one page's content opened under another page's key")
	}
}

func TestUploadEvidenceHostedPublishesSealed_Sessions_AC260202(t *testing.T) {
	page := publishSealedPage(t, hostedContents, "index.html", "media/a.png", "player.js", "manifest.json")
	start := page.fake.starts[0]
	sealed := func(n int) int64 { return proofseal.SealedSize(int64(n)) }
	loader, shim := int64(len(proofseal.Loader())), int64(len(proofseal.Shim()))
	wantManifest := []HostedProofEntry{{"page.html.enc", sealed(15)}, {"media/a.png.enc", sealed(3)}, {"player.js", 6}, {"manifest.json.enc", sealed(2)}, {"proof-shim.js", shim}, {"index.html", loader}}
	if start.RepoOriginURL != "git@github.com:acme/widgets.git" || start.PR != 42 || start.HeadSHA != evidenceTestSHA || !slices.Equal(start.Manifest, wantManifest) {
		t.Fatalf("start = %+v", start)
	}
	var order []string
	for _, put := range page.fake.puts {
		order = append(order, put.Name)
	}
	if want := []string{"page.html.enc", "media/a.png.enc", "player.js", "manifest.json.enc", "proof-shim.js", "index.html"}; !slices.Equal(order, want) {
		t.Fatalf("put order = %v, want %v", order, want)
	}
	if want := "https://app.example/review/acme/widgets/42?head=" + evidenceTestSHA + "#k=" + proofseal.FragmentKey(page.key); page.pageURL != want {
		t.Fatal("page URL is not the review route with the page key as its fragment")
	}
	t.Logf("sealed hosted page put order %v", order)
}

func TestUploadEvidenceHostedSealsByDefault_Sessions_AC278601(t *testing.T) {
	fixture, dir := evidenceServer(t, "http://unused.invalid")
	s := New(Config{Logger: zerolog.Nop()})
	s.repos, s.sessions = fixture.repos, fixture.sessions
	fake := &fakeHostedOpener{}
	s.evidenceBucketOpener = fake
	objects := hostedPage(t, dir, hostedContents, "index.html", "media/a.png", "player.js", "manifest.json")
	got, rpcErr := callEvidence(t, s, &pb.UploadEvidenceRequest{EvidenceDir: dir, PrNumber: 42, HeadSha: evidenceTestSHA, Objects: objects, Destination: pb.EvidenceDestination_EVIDENCE_DESTINATION_HOSTED})
	if rpcErr != nil {
		t.Fatal(rpcErr)
	}
	if !strings.Contains(got.PageUrl, "#k=") {
		t.Fatalf("page URL %q carries no key: a default daemon published an unsealed page", got.PageUrl)
	}
	for _, put := range fake.puts {
		if put.Name == proofseal.LoaderName && !bytes.Equal(put.Body, proofseal.Loader()) {
			t.Fatal("index.html is the plaintext page, not the loader")
		}
		if put.Name == "media/a.png" || put.Name == "manifest.json" {
			t.Fatalf("%s was put in the clear", put.Name)
		}
	}
}

func TestUploadEvidenceHostedSealedRefusals(t *testing.T) {
	s, dir := evidenceServer(t, "http://unused.invalid")
	s.sealHostedProof = true
	fake := &fakeHostedOpener{}
	s.evidenceBucketOpener = fake
	objects := hostedPage(t, dir, hostedContents, "manifest.json", "player.js")
	_, rpcErr := callEvidence(t, s, &pb.UploadEvidenceRequest{EvidenceDir: dir, PrNumber: 42, HeadSha: evidenceTestSHA, Objects: objects, Destination: pb.EvidenceDestination_EVIDENCE_DESTINATION_HOSTED})
	if rpcErr == nil || rpcErr.Meta().Get(EvidenceLimitHeader) != "file-count" || len(fake.starts) != 0 {
		t.Fatalf("error = %v, starts %d; want a file-count refusal before any start", rpcErr, len(fake.starts))
	}

	fake.afterStart = func() {
		if err := os.Truncate(filepath.Join(dir, "index.html"), 1); err != nil {
			t.Fatal(err)
		}
	}
	objects = hostedPage(t, dir, hostedContents, "index.html", "manifest.json")
	_, rpcErr = callEvidence(t, s, &pb.UploadEvidenceRequest{EvidenceDir: dir, PrNumber: 42, HeadSha: evidenceTestSHA, Objects: objects, Destination: pb.EvidenceDestination_EVIDENCE_DESTINATION_HOSTED})
	if rpcErr == nil || rpcErr.Code() != connect.CodeUnavailable || rpcErr.Meta().Get(EvidenceReasonHeader) != "object-changed" {
		t.Fatalf("error = %v, want Unavailable object-changed", rpcErr)
	}
	if slices.ContainsFunc(fake.puts, func(p HostedProofPut) bool { return p.Name == proofseal.PageName || p.Name == proofseal.LoaderName }) || len(fake.aborts) != 1 {
		t.Fatalf("puts %d, aborts %d; want the changed page never put and one abort", len(fake.puts), len(fake.aborts))
	}
}
