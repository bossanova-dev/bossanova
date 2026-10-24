package realstack

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// GHResponse is one explicitly supported invocation of the fake GitHub CLI.
// Any invocation absent from the fixture fails instead of returning empty success.
type GHResponse struct {
	Args     []string `json:"args"`
	Stdout   string   `json:"stdout"`
	Stderr   string   `json:"stderr,omitempty"`
	ExitCode int      `json:"exit_code"`
}

// SetGHResponses replaces this stack's exact-argv GitHub responses.
func (s *Stack) SetGHResponses(t testing.TB, responses []GHResponse) {
	t.Helper()
	data, err := json.Marshal(struct {
		Responses []GHResponse `json:"responses"`
	}{responses})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(s.home, "gh.json"), data, 0o600); err != nil {
		t.Fatal(err)
	}
}
