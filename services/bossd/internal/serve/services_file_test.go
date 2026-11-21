package serve

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestServicesFileValidation(t *testing.T) {
	many := map[string]string{}
	for i := range 17 {
		many[strings.Repeat("a", i+1)] = "https://example.com"
	}
	manyJSON, _ := json.Marshal(many)
	for _, tt := range []struct {
		name, body string
		count      int
		invalid    bool
	}{
		{"valid", `{"z":"https://example.com/z","a":"https://example.com/a"}`, 2, false},
		{"array", `[]`, 0, true}, {"null", `null`, 0, true}, {"large", strings.Repeat(" ", 65537), 0, true},
		{"javascript", `{"api":"javascript:alert(1)"}`, 0, true},
		{"http", `{"api":"http://localhost:3000"}`, 0, true},
		{"userinfo", `{"api":"https://user@example.com"}`, 0, true},
		{"name", `{"bad/name":"https://example.com"}`, 0, true},
		{"too many", string(manyJSON), 0, true},
		{"mixed", `{"bad/name":"https://example.com","api":"https://example.com"}`, 1, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "services.json")
			if err := os.WriteFile(path, []byte(tt.body), 0o600); err != nil {
				t.Fatal(err)
			}
			links, err := readServicesFile(path)
			if (err != nil) != tt.invalid || len(links) != tt.count {
				t.Fatalf("links=%v error=%v", links, err)
			}
			if tt.name == "valid" && (links[0].Name != "a" || links[1].Name != "z") {
				t.Fatalf("unsorted: %v", links)
			}
		})
	}
}
