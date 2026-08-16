package pty

import (
	"bytes"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeTempImage creates a real file with the given name and returns its path.
func writeTempImage(t *testing.T, name string, size int) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, bytes.Repeat([]byte{0x89}, size), 0o600); err != nil {
		t.Fatalf("write temp file: %v", err)
	}
	return path
}

// recorder is a counting claim fake: it records every body it is offered and
// claims the ones imagePastePath accepts.
type recorder struct {
	bodies  []string
	claimed int
}

func (r *recorder) claim(body []byte) bool {
	r.bodies = append(r.bodies, string(body))
	if _, ok := imagePastePath(body); ok {
		r.claimed++
		return true
	}
	return false
}

func TestBracketedPaste(t *testing.T) {
	got := BracketedPaste("hello world")
	want := []byte("\x1b[200~hello world\x1b[201~")
	if !bytes.Equal(got, want) {
		t.Errorf("BracketedPaste = %q, want %q", got, want)
	}
	// A trailing newline would submit the turn before the user typed their
	// prompt — the one thing this helper exists to prevent.
	if bytes.HasSuffix(got, []byte("\n")) || bytes.HasSuffix(got, []byte("\r")) {
		t.Errorf("BracketedPaste = %q, want no trailing newline", got)
	}
	if empty := BracketedPaste(""); !bytes.Equal(empty, []byte("\x1b[200~\x1b[201~")) {
		t.Errorf("BracketedPaste(\"\") = %q", empty)
	}
}

// TestPasteScannerNilClaimIsIdentity pins the structural guarantee that local
// (non --host) mode is byte-for-byte unchanged: with no interceptor the scanner
// keeps no state and returns each chunk untouched — including a paste that a
// real interceptor WOULD claim.
func TestPasteScannerNilClaimIsIdentity(t *testing.T) {
	img := writeTempImage(t, "shot.png", 32)

	inputs := [][]byte{
		[]byte("hello"),
		[]byte("\x1b[200~" + img + "\x1b[201~"),
		[]byte("\x1b[2"),
		[]byte("A"),
		[]byte("\x1b[200~partial"),
		[]byte("body\x1b[201~"),
		[]byte("héllo → ✓"),
	}

	nilScanner := newPasteScanner(nil)
	for _, in := range inputs {
		if got := nilScanner.feed(in); !bytes.Equal(got, in) {
			t.Errorf("nil-claim feed(%q) = %q, want unchanged", in, got)
		}
	}
	if nilScanner.state != pasteStateIdle || len(nilScanner.held) != 0 || len(nilScanner.body) != 0 {
		t.Errorf("nil-claim scanner kept state: %+v", nilScanner)
	}

	// Prove the fixture above is a real trigger, so the identity result is
	// meaningful rather than vacuous.
	rec := &recorder{}
	live := newPasteScanner(rec.claim)
	if got := live.feed([]byte("\x1b[200~" + img + "\x1b[201~")); len(got) != 0 {
		t.Fatalf("live scanner forwarded %q for a claimable paste", got)
	}
	if rec.claimed != 1 {
		t.Fatalf("live scanner claimed %d times, want 1 (fixture is not a real trigger)", rec.claimed)
	}
}

func TestPasteScannerPassthrough(t *testing.T) {
	realNonImage := writeTempImage(t, "notes.txt", 16)
	dir := filepath.Join(t.TempDir(), "screenshots.png")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	oversizeBody := strings.Repeat("x", maxPendingPasteBytes+100)

	cases := []struct {
		name  string
		input []byte
	}{
		{"ordinary_typing", []byte("git status\r")},
		{"arrow_keys", []byte("\x1b[A\x1b[B")},
		{"paste_of_prose", []byte("\x1b[200~please look at the screenshot\x1b[201~")},
		{"paste_of_non_image_file", []byte("\x1b[200~" + realNonImage + "\x1b[201~")},
		{"paste_of_missing_image", []byte("\x1b[200~/definitely/not/here/shot.png\x1b[201~")},
		{"paste_of_directory", []byte("\x1b[200~" + dir + "\x1b[201~")},
		{"paste_of_relative_image", []byte("\x1b[200~shot.png\x1b[201~")},
		{"utf8_typing", []byte("héllo → ✓ 日本語")},
		{"utf8_in_paste_body", []byte("\x1b[200~héllo → ✓ 日本語\x1b[201~")},
		{"over_cap_paste", []byte("\x1b[200~" + oversizeBody + "\x1b[201~")},
		{"paste_with_two_tokens", []byte("\x1b[200~/a/one.png /a/two.png\x1b[201~")},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := &recorder{}
			s := newPasteScanner(rec.claim)
			got := s.feed(tc.input)
			if !bytes.Equal(got, tc.input) {
				t.Errorf("feed(%q) = %q, want unchanged", tc.input, got)
			}
			if rec.claimed != 0 {
				t.Errorf("claimed %d pastes, want 0", rec.claimed)
			}

			// The same input under the NON-CLAIMING observation hook every
			// non---host attach installs (BOS-849). It is the wiring most users
			// run, it inspects bodies the uploader path never sees, and it must
			// not move a byte — paste_of_missing_image is the case where it
			// actually fires, and even there conservation is the contract.
			n := newPasteScanner(noticeClaim(t))
			if got := n.feed(tc.input); !bytes.Equal(got, tc.input) {
				t.Errorf("with the image-paste notice: feed(%q) = %q, want unchanged", tc.input, got)
			}
		})
	}
}

// TestPasteScannerSplitAtEveryOffsetWithImagePasteNotice is the chunk-boundary
// half of the safety proof for the observation hook.
//
// A terminal chunks reads wherever it likes, so the introducer and terminator
// arrive split as readily as whole, and the scanner carries state across feeds
// to cope. Under the hook that state exists on a path where NOTHING may ever be
// removed, so every split point has to conserve the stream exactly — including
// the ones that land inside \x1b[200~ and \x1b[201~, and the ones that cut a
// multi-byte rune in half.
//
// The stream deliberately contains the reported symptom (an absolute image path
// that does not resolve here), which is the one body the hook reacts to.
func TestPasteScannerSplitAtEveryOffsetWithImagePasteNotice(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "cmux-drop-383cd973.png")
	stream := []byte("ab\x1b[200~" + missing + "\x1b[201~héllo → ✓\x1b[A")

	for i := 0; i <= len(stream); i++ {
		s := newPasteScanner(noticeClaim(t))
		if got := feedSplit(s, stream, i); !bytes.Equal(got, stream) {
			t.Fatalf("split at %d: forwarded %q, want the stream conserved %q", i, got, stream)
		}
	}
}

// TestPasteScannerUnterminatedPasteIsForwardedNotSwallowed proves the over-cap
// fail-open across reads: a body that never terminates streams through once it
// crosses the cap instead of being held forever.
func TestPasteScannerOverCapStreamsThroughAcrossChunks(t *testing.T) {
	rec := &recorder{}
	s := newPasteScanner(rec.claim)

	chunk := strings.Repeat("y", 4096)
	var got []byte
	got = append(got, s.feed([]byte("\x1b[200~"+chunk))...)
	got = append(got, s.feed([]byte(chunk))...)
	got = append(got, s.feed([]byte(chunk))...)
	want := "\x1b[200~" + chunk + chunk + chunk
	if string(got) != want {
		t.Fatalf("over-cap stream = %d bytes, want %d", len(got), len(want))
	}
	if rec.claimed != 0 || len(rec.bodies) != 0 {
		t.Fatalf("over-cap paste was offered to claim: %d bodies", len(rec.bodies))
	}

	// The terminator returns the scanner to idle, so a later paste is still
	// eligible for interception.
	img := writeTempImage(t, "later.png", 8)
	tail := s.feed([]byte("\x1b[201~"))
	if string(tail) != "\x1b[201~" {
		t.Fatalf("terminator forwarded as %q, want verbatim", tail)
	}
	if out := s.feed([]byte("\x1b[200~" + img + "\x1b[201~")); len(out) != 0 {
		t.Fatalf("post-flush paste forwarded %q, want interception", out)
	}
	if rec.claimed != 1 {
		t.Fatalf("post-flush claimed = %d, want 1", rec.claimed)
	}
}

// TestPasteScannerReleasesTheDetachKeyFromAnUnterminatedPaste is the liveness
// guard on the one escape hatch the user is guaranteed.
//
// While buffering a paste body the scanner forwards nothing, so Run's detach
// scan sees nothing — and if the terminator never arrives (a terminal that
// aborts mid-paste, or one eaten by the query-reply strip immediately upstream)
// that state can only be left by accumulating 8 KiB. Ctrl+X would be swallowed
// for the whole of it, which is a hung terminal with the exit disabled. The
// scanner must instead flush verbatim the moment a detach key appears, so the
// key reaches the scan on the very chunk it was typed on.
func TestPasteScannerReleasesTheDetachKeyFromAnUnterminatedPaste(t *testing.T) {
	// Every form the detach key arrives in — the raw control bytes and the two
	// enhanced-keyboard encodings Claude Code negotiates — has to escape.
	keys := map[string]string{
		"ctrlX":            "\x18",
		"ctrlBracket":      "\x1d",
		"kittyCtrlX":       "\x1b[120;5u",
		"modifyOtherKeysX": "\x1b[27;5;120~",
	}
	for name, key := range keys {
		t.Run(name, func(t *testing.T) {
			rec := &recorder{}
			s := newPasteScanner(rec.claim)

			// A paste that has begun and will never terminate.
			if out := s.feed([]byte("\x1b[200~half a path")); len(out) != 0 {
				t.Fatalf("an in-flight paste body forwarded %q, want it buffered", out)
			}

			got := string(s.feed([]byte(key)))
			if !containsDetachSequence([]byte(got)) {
				t.Fatalf("feed returned %q, which Run's detach scan would not fire on: "+
					"the detach key is stuck behind the paste buffer", got)
			}
			// Nothing may be lost on the way out either: the buffered body is
			// the user's input and has to reach the agent verbatim.
			if want := "\x1b[200~half a path" + key; got != want {
				t.Fatalf("flushed %q, want the held bytes verbatim %q", got, want)
			}
			if len(rec.bodies) != 0 {
				t.Fatalf("a paste carrying a detach key was offered to claim: %q", rec.bodies)
			}

			// And the scanner is back in passthrough, not stranded mid-paste.
			if out := string(s.feed([]byte("ls\r"))); out != "ls\r" {
				t.Fatalf("post-detach typing forwarded %q, want passthrough", out)
			}
		})
	}
}

// feedSplit drives the scanner with stream split into two chunks at offset i.
func feedSplit(s *pasteScanner, stream []byte, i int) []byte {
	var got []byte
	got = append(got, s.feed(stream[:i])...)
	got = append(got, s.feed(stream[i:])...)
	return got
}

func TestPasteScannerSplitAtEveryOffsetMatching(t *testing.T) {
	img := writeTempImage(t, "shot.png", 64)
	stream := []byte("ab\x1b[200~" + img + "\x1b[201~cd")

	for i := 0; i <= len(stream); i++ {
		rec := &recorder{}
		s := newPasteScanner(rec.claim)
		got := feedSplit(s, stream, i)
		if string(got) != "abcd" {
			t.Fatalf("split at %d: forwarded %q, want %q", i, got, "abcd")
		}
		if rec.claimed != 1 {
			t.Fatalf("split at %d: claimed %d times, want 1", i, rec.claimed)
		}
		if len(rec.bodies) != 1 || rec.bodies[0] != img {
			t.Fatalf("split at %d: bodies = %q, want [%q]", i, rec.bodies, img)
		}
	}
}

func TestPasteScannerSplitAtEveryOffsetNonMatching(t *testing.T) {
	stream := []byte("ab\x1b[200~just some prose\x1b[201~cd\x1b[A")

	for i := 0; i <= len(stream); i++ {
		rec := &recorder{}
		s := newPasteScanner(rec.claim)
		got := feedSplit(s, stream, i)
		if !bytes.Equal(got, stream) {
			t.Fatalf("split at %d: forwarded %q, want %q", i, got, stream)
		}
		if rec.claimed != 0 {
			t.Fatalf("split at %d: claimed %d times, want 0", i, rec.claimed)
		}
	}
}

func TestPasteScannerSplitInsideIntroducerAndTerminator(t *testing.T) {
	img := writeTempImage(t, "shot.png", 8)

	t.Run("introducer", func(t *testing.T) {
		rec := &recorder{}
		s := newPasteScanner(rec.claim)
		var got []byte
		got = append(got, s.feed([]byte("\x1b[2"))...)
		got = append(got, s.feed([]byte("00~"+img+"\x1b[201~"))...)
		if len(got) != 0 {
			t.Errorf("forwarded %q, want nothing", got)
		}
		if rec.claimed != 1 {
			t.Errorf("claimed %d, want 1", rec.claimed)
		}
	})

	t.Run("terminator", func(t *testing.T) {
		rec := &recorder{}
		s := newPasteScanner(rec.claim)
		var got []byte
		got = append(got, s.feed([]byte("\x1b[200~"+img+"\x1b[20"))...)
		got = append(got, s.feed([]byte("1~"))...)
		if len(got) != 0 {
			t.Errorf("forwarded %q, want nothing", got)
		}
		if rec.claimed != 1 {
			t.Errorf("claimed %d, want 1", rec.claimed)
		}
	})
}

// TestPasteScannerHeldPrefixFalseAlarm covers the ambiguity that matters most:
// bytes held because they might start an introducer must reappear in their
// original position and order once they turn out to be an arrow key.
func TestPasteScannerHeldPrefixFalseAlarm(t *testing.T) {
	rec := &recorder{}
	s := newPasteScanner(rec.claim)

	if got := s.feed([]byte("\x1b[2")); len(got) != 0 {
		t.Fatalf("first feed forwarded %q, want the prefix held", got)
	}
	if got := s.feed([]byte("A")); string(got) != "\x1b[2A" {
		t.Fatalf("second feed = %q, want %q", got, "\x1b[2A")
	}
	if rec.claimed != 0 || len(rec.bodies) != 0 {
		t.Fatalf("claim consulted for a false alarm: %q", rec.bodies)
	}
}

func TestPasteScannerInterceptsExistingImage(t *testing.T) {
	img := writeTempImage(t, "shot.png", 128)
	rec := &recorder{}
	s := newPasteScanner(rec.claim)

	got := s.feed([]byte("\x1b[200~" + img + "\x1b[201~"))
	if len(got) != 0 {
		t.Errorf("forwarded %q, want nothing", got)
	}
	if len(rec.bodies) != 1 {
		t.Fatalf("claim called %d times, want 1", len(rec.bodies))
	}
	if rec.bodies[0] != img {
		t.Errorf("claim body = %q, want %q", rec.bodies[0], img)
	}
}

func TestPasteScannerQuotedForms(t *testing.T) {
	dir := t.TempDir()
	spaced := filepath.Join(dir, "my shot.png")
	if err := os.WriteFile(spaced, []byte{0x89}, 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	fileURL := (&url.URL{Scheme: "file", Path: spaced}).String()

	intercepted := []struct {
		name string
		body string
	}{
		{"single_quoted", "'" + spaced + "'"},
		{"double_quoted", `"` + spaced + `"`},
		{"backslash_escaped_space", strings.ReplaceAll(spaced, " ", `\ `)},
		{"file_url", fileURL},
		{"file_url_localhost", strings.Replace(fileURL, "file://", "file://localhost", 1)},
		{"trailing_space", escapeSpaces(spaced) + " "},
	}
	for _, tc := range intercepted {
		t.Run(tc.name, func(t *testing.T) {
			rec := &recorder{}
			s := newPasteScanner(rec.claim)
			if got := s.feed(BracketedPaste(tc.body)); len(got) != 0 {
				t.Errorf("forwarded %q, want interception", got)
			}
			if rec.claimed != 1 {
				t.Errorf("claimed %d, want 1 (body %q)", rec.claimed, tc.body)
			}
		})
	}

	passthrough := []struct {
		name string
		body string
	}{
		{"partially_quoted", "'" + spaced},
		{"quote_closed_early", "'" + spaced + "'extra"},
		{"two_tokens", "/a/one.png /a/two.png"},
		{"unescaped_space", spaced},
	}
	for _, tc := range passthrough {
		t.Run(tc.name, func(t *testing.T) {
			rec := &recorder{}
			s := newPasteScanner(rec.claim)
			in := BracketedPaste(tc.body)
			if got := s.feed(in); !bytes.Equal(got, in) {
				t.Errorf("feed = %q, want unchanged %q", got, in)
			}
			if rec.claimed != 0 {
				t.Errorf("claimed %d, want 0", rec.claimed)
			}
		})
	}
}

// escapeSpaces backslash-escapes the spaces in a path, the way a terminal does
// when a file with a space in its name is dragged in.
func escapeSpaces(path string) string { return strings.ReplaceAll(path, " ", `\ `) }

func TestImagePastePath(t *testing.T) {
	dir := t.TempDir()

	png := filepath.Join(dir, "shot.png")
	if err := os.WriteFile(png, []byte{0x89}, 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	upper := filepath.Join(dir, "SHOT.JPEG")
	if err := os.WriteFile(upper, []byte{0xff}, 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	txt := filepath.Join(dir, "notes.txt")
	if err := os.WriteFile(txt, []byte("hi"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	subdir := filepath.Join(dir, "album.png")
	if err := os.Mkdir(subdir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	huge := filepath.Join(dir, "huge.png")
	if err := os.WriteFile(huge, bytes.Repeat([]byte{0}, maxPasteImageBytes+1), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	dangling := filepath.Join(dir, "dangling.png")
	if err := os.Symlink(filepath.Join(dir, "nowhere.png"), dangling); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	cases := []struct {
		name string
		body string
		want string // "" means not intercepted
	}{
		{"bare_path", png, png},
		{"surrounding_whitespace", "  " + png + "\t", png},
		{"uppercase_extension", upper, upper},
		{"empty_body", "", ""},
		{"whitespace_only", "   ", ""},
		{"prose", "look at " + png, ""},
		{"embedded_newline", png + "\n" + png, ""},
		{"embedded_carriage_return", png + "\rhello", ""},
		{"two_tokens", png + " " + png, ""},
		{"relative_path", "shot.png", ""},
		{"relative_dotted_path", "./shot.png", ""},
		{"non_image_extension", txt, ""},
		{"no_extension", filepath.Join(dir, "shot"), ""},
		{"missing_file", filepath.Join(dir, "absent.png"), ""},
		{"directory", subdir, ""},
		{"dangling_symlink", dangling, ""},
		{"over_size_cap", huge, ""},
		{"unterminated_single_quote", "'" + png, ""},
		{"unterminated_double_quote", `"` + png, ""},
		{"single_quote_trailing_junk", "'" + png + "'x", ""},
		{"unknown_double_quote_escape", `"` + dir + `\n` + `shot.png"`, ""},
		{"trailing_backslash", png + `\`, ""},
		{"file_url_bad_percent_escape", "file://" + dir + "/%zzshot.png", ""},
		{"file_url_remote_host", "file://example.com" + png, ""},
		{"file_url_with_fragment", "file://" + png + "#frag", ""},
		{"tilde_only", "~", ""},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := imagePastePath([]byte(tc.body))
			if tc.want == "" {
				if ok {
					t.Fatalf("imagePastePath(%q) = (%q, true), want not intercepted", tc.body, got)
				}
				return
			}
			if !ok {
				t.Fatalf("imagePastePath(%q) = not intercepted, want %q", tc.body, tc.want)
			}
			if got != tc.want {
				t.Errorf("imagePastePath(%q) = %q, want %q", tc.body, got, tc.want)
			}
		})
	}
}

func TestImagePastePathHomeExpansion(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skipf("no home dir: %v", err)
	}
	f, err := os.CreateTemp(home, "boss-paste-*.png")
	if err != nil {
		t.Skipf("cannot write to home dir: %v", err)
	}
	name := f.Name()
	_ = f.Close()
	t.Cleanup(func() { _ = os.Remove(name) })

	rel, err := filepath.Rel(home, name)
	if err != nil {
		t.Fatalf("rel: %v", err)
	}
	got, ok := imagePastePath([]byte("~/" + rel))
	if !ok {
		t.Fatalf("imagePastePath(~/%s) = not intercepted, want %q", rel, name)
	}
	if got != filepath.Clean(name) {
		t.Errorf("imagePastePath(~/%s) = %q, want %q", rel, got, name)
	}
}

// TestPasteScannerStreamConservation is the whole-session invariant: across an
// arbitrary sequence of chunks, everything the user sent reaches the agent
// except exactly the pastes that were claimed.
func TestPasteScannerStreamConservation(t *testing.T) {
	img := writeTempImage(t, "shot.png", 4)
	claimedPaste := "\x1b[200~" + img + "\x1b[201~"

	chunks := []string{
		"ls -la\r",
		"\x1b[200~some prose\x1b[201~",
		"\x1b[2",
		"A",
		claimedPaste,
		"more typing",
		"\x1b[200~",
		img,
		"\x1b[201~",
		"\x1b",
		"[B",
	}

	rec := &recorder{}
	s := newPasteScanner(rec.claim)
	var in, out []byte
	for _, c := range chunks {
		in = append(in, c...)
		out = append(out, s.feed([]byte(c))...)
	}

	want := strings.ReplaceAll(string(in), claimedPaste, "")
	if string(out) != want {
		t.Errorf("forwarded %q, want %q", out, want)
	}
	if rec.claimed != 2 {
		t.Errorf("claimed %d pastes, want 2", rec.claimed)
	}

	// The same session under the observation hook, where the subtrahend is
	// empty: nothing is ever claimed, so the invariant tightens from "everything
	// except the claimed pastes" to "everything". The chunk list deliberately
	// carries a body that reaches the hook's reacting branch, so the path under
	// test is not the inert one.
	//
	// This test asserts only the bytes — that the record and the overlay
	// actually happen for such a body is asserted where it can be observed, by
	// TestImagePasteNoticeExplainsAMissingImageWithoutClaimingIt.
	missing := filepath.Join(t.TempDir(), "cmux-drop-383cd973.png")
	noticeChunks := append([]string{}, chunks...)
	noticeChunks = append(noticeChunks,
		"\x1b[200~"+missing+"\x1b[201~",
		"héllo → ✓ 日本語",
	)

	n := newPasteScanner(noticeClaim(t))
	var noticeIn, noticeOut []byte
	for _, c := range noticeChunks {
		noticeIn = append(noticeIn, c...)
		noticeOut = append(noticeOut, n.feed([]byte(c))...)
	}
	if !bytes.Equal(noticeOut, noticeIn) {
		t.Errorf("with the image-paste notice: forwarded %q, want the whole stream %q", noticeOut, noticeIn)
	}
}
