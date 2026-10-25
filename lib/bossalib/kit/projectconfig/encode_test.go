package projectconfig

import (
	"bytes"
	"encoding/json"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"
)

func mustEncode(t *testing.T, cfg *Config) []byte {
	t.Helper()
	out, err := Encode(cfg)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	return out
}

// writeShuffled renders v as JSON with every object's keys in reverse order
// and every array reversed, except a filter's values (whose order matters).
func writeShuffled(b *strings.Builder, v any, key string) {
	switch x := v.(type) {
	case map[string]any:
		keys := make([]string, 0, len(x))
		for k := range x {
			keys = append(keys, k)
		}
		sort.Sort(sort.Reverse(sort.StringSlice(keys)))
		b.WriteString("{")
		for i, k := range keys {
			if i > 0 {
				b.WriteString(",")
			}
			b.WriteString(strconv.Quote(k) + ":")
			writeShuffled(b, x[k], k)
		}
		b.WriteString("}")
	case []any:
		items := slices.Clone(x)
		if key != "values" {
			slices.Reverse(items)
		}
		b.WriteString("[")
		for i, el := range items {
			if i > 0 {
				b.WriteString(",")
			}
			writeShuffled(b, el, "")
		}
		b.WriteString("]")
	default:
		data, _ := json.Marshal(x)
		b.Write(data)
	}
}

func TestEncodeCanonical(t *testing.T) {
	full := readFixture(t, "full.json")

	t.Run("canonical fixture round-trips byte-identically", func(t *testing.T) {
		cfg, _ := mustDecode(t, full)
		if got := mustEncode(t, cfg); !bytes.Equal(got, full) {
			t.Fatalf("round trip differs:\n%s", got)
		}
	})

	t.Run("shuffled arrays and reordered nested keys encode canonically", func(t *testing.T) {
		var b strings.Builder
		writeShuffled(&b, fixtureMap(t), "")
		shuffled := []byte(b.String())
		if bytes.Equal(shuffled, full) {
			t.Fatal("shuffle produced the canonical bytes; the test proves nothing")
		}
		cfg, _ := mustDecode(t, shuffled)
		if cfg.Crons[0].Name == "factory-build" {
			t.Fatal("arrays were not shuffled")
		}
		if got := mustEncode(t, cfg); !bytes.Equal(got, full) {
			t.Fatalf("shuffled input not canonicalised:\n%s", got)
		}
		if cfg.Crons[0].Name == "factory-build" {
			t.Fatal("Encode mutated its input")
		}
	})

	t.Run("empty arrays survive and absent stays absent", func(t *testing.T) {
		cfg, _ := mustDecode(t, []byte(`{"version":1,"project":"p","repo":{"originUrl":"https://github.com/a/b"},"crons":[],"cloud":{"triggers":[]}}`))
		got := string(mustEncode(t, cfg))
		if !strings.Contains(got, `"crons": []`) || !strings.Contains(got, `"triggers": []`) {
			t.Fatalf("explicit [] lost:\n%s", got)
		}
		for _, absent := range []string{`"daemons"`, `"webhooks"`} {
			if strings.Contains(got, absent) {
				t.Fatalf("absent %s appeared:\n%s", absent, got)
			}
		}
		minimal := readFixture(t, "minimal.json")
		cfg, _ = mustDecode(t, minimal)
		if got := mustEncode(t, cfg); !bytes.Equal(got, minimal) {
			t.Fatalf("minimal file changed:\n%s", got)
		}
	})

	t.Run("schema reference preserved and one trailing newline", func(t *testing.T) {
		cfg, _ := mustDecode(t, full)
		got := mustEncode(t, cfg)
		if !bytes.Contains(got, []byte(`"$schema": "./lib/bossalib/kit/projectconfig/schema/boss-project.schema.json"`)) {
			t.Fatal("$schema dropped")
		}
		if !bytes.HasSuffix(got, []byte("}\n")) || bytes.HasSuffix(got, []byte("\n\n")) {
			t.Fatalf("want exactly one trailing newline, got %q", got[len(got)-3:])
		}
	})

	t.Run("prompts are not HTML-escaped", func(t *testing.T) {
		cfg, _ := mustDecode(t, mutated(t, func(m map[string]any) {
			obj(t, m, "crons", 1)["prompt"] = "<a> & <b>"
			obj(t, m, "cloud", "triggers", 0, "launch")["promptTemplate"] = "x <y> & z"
		}))
		got := mustEncode(t, cfg)
		if !bytes.Contains(got, []byte(`"<a> & <b>"`)) || !bytes.Contains(got, []byte(`"x <y> & z"`)) {
			t.Fatalf("HTML escaped:\n%s", got)
		}
	})

	t.Run("encoding is idempotent on every testdata file", func(t *testing.T) {
		files, err := filepath.Glob(filepath.Join("testdata", "*.json"))
		if err != nil || len(files) == 0 {
			t.Fatalf("no testdata: %v", err)
		}
		for _, f := range files {
			cfg, _ := mustDecode(t, readFixture(t, filepath.Base(f)))
			once := mustEncode(t, cfg)
			again, _ := mustDecode(t, once)
			if twice := mustEncode(t, again); !bytes.Equal(once, twice) {
				t.Fatalf("%s: encoding not idempotent", f)
			}
		}
	})

	t.Run("nil config", func(t *testing.T) {
		if _, err := Encode(nil); err == nil {
			t.Fatal("nil config encoded")
		}
	})
}
