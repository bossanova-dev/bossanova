package setupcheck_test

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/recurser/bossd/internal/setupcheck"
)

//go:embed ids.json
var committedIDs []byte

var updateGolden = flag.Bool("update", false, "rewrite golden files")

const (
	idsFilePath   = "services/bossd/internal/setupcheck/ids.json"
	idsGenCommand = "make setup-check-ids"
)

type generatedIDsFile struct {
	GeneratedBy string   `json:"generatedBy"`
	IDs         []string `json:"ids"`
}

func renderIDs(ids []string) ([]byte, error) {
	if ids == nil {
		ids = []string{}
	}
	out, err := json.MarshalIndent(generatedIDsFile{
		GeneratedBy: idsGenCommand,
		IDs:         ids,
	}, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(out, '\n'), nil
}

func checkIDsMatch(got, committed []byte) error {
	if !bytes.Equal(got, committed) {
		return fmt.Errorf("%s is stale: the setup check registry changed without regenerating it; regenerate with `%s`", idsFilePath, idsGenCommand)
	}
	return nil
}

func TestIDsGenerated(t *testing.T) {
	got, err := renderIDs(setupcheck.IDs())
	if err != nil {
		t.Fatal(err)
	}
	if *updateGolden {
		if err := os.WriteFile("ids.json", got, 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	if err := checkIDsMatch(got, committedIDs); err != nil {
		t.Fatal(err)
	}
}

func TestCheckIDsMatch(t *testing.T) {
	got, err := renderIDs([]string{"first-check", "second-check"})
	if err != nil {
		t.Fatalf("renderIDs() error = %v", err)
	}
	want := "{\n  \"generatedBy\": \"make setup-check-ids\",\n  \"ids\": [\n    \"first-check\",\n    \"second-check\"\n  ]\n}\n"
	if string(got) != want {
		t.Fatalf("renderIDs() =\n%s\nwant:\n%s", got, want)
	}
	if err := checkIDsMatch(got, []byte(want)); err != nil {
		t.Fatalf("checkIDsMatch() unexpected error for matching bytes: %v", err)
	}

	stale := []byte("{\"generatedBy\": \"make setup-check-ids\", \"ids\": []}\n")
	err = checkIDsMatch(got, stale)
	if err == nil {
		t.Fatal("checkIDsMatch() = nil for mismatched bytes, want error")
	}
	if !strings.Contains(err.Error(), idsGenCommand) {
		t.Errorf("checkIDsMatch() error = %q, want mention of %q", err.Error(), idsGenCommand)
	}
}
