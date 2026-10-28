package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestCommands(t *testing.T) {
	dir := t.TempDir()
	good := filepath.Join(dir, "valid.json")
	bad := filepath.Join(dir, "invalid.json")
	if err := os.WriteFile(good, []byte(`{"version":1,"name":"empty"}`), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(bad, []byte(`{"version":1,"name":"x","sessions":[{"key":"s","repo":"missing","title":"S","state":"BLOCKED","displayIntent":1}]}`), 0600); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		args   []string
		status int
	}{
		{[]string{"schema"}, 0}, {[]string{"capabilities"}, 0}, {[]string{"validate", good}, 0}, {[]string{"validate", bad}, 1},
		{[]string{"validate", filepath.Join(dir, "absent")}, 1}, {nil, 2}, {[]string{"unknown"}, 2}, {[]string{"validate"}, 2}, {[]string{"schema", "extra"}, 2},
	}
	for _, tt := range tests {
		t.Run("args="+stringArgs(tt.args), func(t *testing.T) {
			var out bytes.Buffer
			if got := run(tt.args, &out); got != tt.status {
				t.Fatalf("exit=%d want=%d output=%s", got, tt.status, &out)
			}
			var result map[string]any
			dec := json.NewDecoder(&out)
			if err := dec.Decode(&result); err != nil {
				t.Fatal(err)
			}
			if dec.More() {
				t.Fatal("trailing stdout")
			}
			if len(tt.args) > 0 && tt.args[0] == "validate" && len(tt.args) == 2 {
				if result["ok"] != (tt.status == 0) {
					t.Fatal(result)
				}
				if _, ok := result["problems"].([]any); !ok {
					t.Fatal(result)
				}
			}
		})
	}
}
func stringArgs(args []string) string { b, _ := json.Marshal(args); return string(b) }
