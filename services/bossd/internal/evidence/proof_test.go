package evidence_test

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/recurser/bossd/internal/evidence"
)

func proofPayload() map[string]any {
	return map[string]any{"schema": "boss-proof/1", "head": sha, "page": nil, "level": "high", "asks": []string{}, "digest": "sha256:" + strings.Repeat("a", 64)}
}

func proofComment(t *testing.T, payload map[string]any) string {
	t.Helper()
	b, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	return "<!-- boss-proof: " + string(b) + " -->"
}

func TestParseProof(t *testing.T) {
	payload := proofPayload()
	payload["head"] = " " + sha + " "
	payload["optional"] = true
	got, ok := evidence.ParseProof(evidence.Format(evidence.Summary{Head: sha}) + proofComment(t, payload))
	want := evidence.ProofLine{Head: sha, Level: "high", Asks: []string{}, Digest: "sha256:" + strings.Repeat("a", 64)}
	if !ok || !reflect.DeepEqual(got, want) {
		t.Fatalf("ParseProof = %+v, %v; want %+v", got, ok, want)
	}
	for _, level := range []string{"high", "moderate", "low", "blocked", "cannot-assess"} {
		payload["level"] = level
		if _, ok := evidence.ParseProof(proofComment(t, payload)); !ok {
			t.Errorf("rejected level %s", level)
		}
	}
}

func TestParseProofRejectsMalformedLines(t *testing.T) {
	tests := map[string]func(map[string]any){
		"missing schema":          func(p map[string]any) { delete(p, "schema") },
		"future schema":           func(p map[string]any) { p["schema"] = "boss-proof/2" },
		"wrong schema":            func(p map[string]any) { p["schema"] = "other" },
		"short head":              func(p map[string]any) { p["head"] = "abc" },
		"nonhex head":             func(p map[string]any) { p["head"] = strings.Repeat("g", 40) },
		"unknown level":           func(p map[string]any) { p["level"] = "certain" },
		"http page":               func(p map[string]any) { p["page"] = "http://example.test" },
		"javascript page":         func(p map[string]any) { p["page"] = "javascript:alert(1)" },
		"relative page":           func(p map[string]any) { p["page"] = "/proof" },
		"missing page":            func(p map[string]any) { delete(p, "page") },
		"userinfo page":           func(p map[string]any) { p["page"] = "https://user@example.test" },
		"empty userinfo page":     func(p map[string]any) { p["page"] = "https://@example.test" },
		"whitespace page":         func(p map[string]any) { p["page"] = "https://example.test/a b" },
		"unicode whitespace page": func(p map[string]any) { p["page"] = "https://example.test/a\u00a0b" },
		"long page":               func(p map[string]any) { p["page"] = "https://example.test/" + strings.Repeat("a", 2048) },
		"missing asks":            func(p map[string]any) { delete(p, "asks") },
		"null asks":               func(p map[string]any) { p["asks"] = nil },
		"many asks":               func(p map[string]any) { p["asks"] = strings.Split(strings.Repeat("area,", 32)+"area", ",") },
		"blank ask":               func(p map[string]any) { p["asks"] = []string{" "} },
		"long ask":                func(p map[string]any) { p["asks"] = []string{strings.Repeat("a", 301)} },
		"invalid identifier":      func(p map[string]any) { p["asks"] = []string{"Area name"} },
		"bad digest":              func(p map[string]any) { p["digest"] = "sha256:" + strings.Repeat("A", 64) },
	}
	for name, change := range tests {
		t.Run(name, func(t *testing.T) {
			p := proofPayload()
			change(p)
			if got, ok := evidence.ParseProof(proofComment(t, p)); ok {
				t.Fatalf("accepted %+v", got)
			}
		})
	}
	for _, body := range []string{"<!-- boss-proof: { -->", "<!-- boss-proof: {}"} {
		if _, ok := evidence.ParseProof(body); ok {
			t.Errorf("accepted %s", body)
		}
	}
}

func TestParseProofBoundsEscapingAndLastValid(t *testing.T) {
	p := proofPayload()
	p["asks"] = strings.Split(strings.Repeat("a,", 31)+strings.Repeat("a", 300), ",")
	prefix := "https://example.test/"
	page := prefix + strings.Repeat("a", 2048-len(prefix))
	p["page"] = page
	if got, ok := evidence.ParseProof(proofComment(t, p)); !ok || got.Page != page || len(got.Asks) != 32 {
		t.Fatalf("caps: %+v %v", got, ok)
	}
	p["page"] = "https://example.test/?q=<x>&n=1"
	first := proofComment(t, p)
	if got, ok := evidence.ParseProof(first + "<!-- boss-proof: {bad} -->"); !ok || got.Page != p["page"] {
		t.Fatalf("escaped/last valid: %+v %v", got, ok)
	}
	p["level"] = "low"
	if got, ok := evidence.ParseProof(first + proofComment(t, p)); !ok || got.Level != "low" {
		t.Fatalf("last wins: %+v %v", got, ok)
	}
}
