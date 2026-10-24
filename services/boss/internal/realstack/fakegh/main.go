// Command fakegh serves explicit, exact-argv gh responses for real-stack tests.
// BOSS_FAKE_GH_SPEC points to a JSON fixture of the form
// {"responses":[{"args":["auth","status"],"stdout":"logged in\n","exit_code":0}]}.
// There are no implicit successful responses: an unfixtured command fails.
package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"slices"
)

type response struct {
	Args     []string `json:"args"`
	Stdout   string   `json:"stdout"`
	Stderr   string   `json:"stderr"`
	ExitCode int      `json:"exit_code"`
}

type spec struct {
	Responses []response `json:"responses"`
}

func main() {
	os.Exit(run(os.Args[1:], os.Getenv("BOSS_FAKE_GH_SPEC"), os.Stdout, os.Stderr))
}

func run(args []string, specPath string, stdout, stderr io.Writer) int {
	if specPath == "" {
		_, _ = fmt.Fprintln(stderr, "fake gh: BOSS_FAKE_GH_SPEC is required")
		return 1
	}
	data, err := os.ReadFile(specPath)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "fake gh: read fixture: %v\n", err)
		return 1
	}
	var fixture spec
	if err := json.Unmarshal(data, &fixture); err != nil {
		_, _ = fmt.Fprintf(stderr, "fake gh: decode fixture: %v\n", err)
		return 1
	}
	var matched *response
	for i := range fixture.Responses {
		r := &fixture.Responses[i]
		if r.ExitCode < 0 || r.ExitCode > 255 {
			_, _ = fmt.Fprintf(stderr, "fake gh: invalid exit_code %d\n", r.ExitCode)
			return 1
		}
		if slices.Equal(args, r.Args) {
			if matched != nil {
				_, _ = fmt.Fprintf(stderr, "fake gh: duplicate fixture for argv %q\n", args)
				return 1
			}
			matched = r
		}
	}
	if matched == nil {
		_, _ = fmt.Fprintf(stderr, "fake gh: unrecognised argv %q\n", args)
		return 1
	}
	if _, err := io.WriteString(stdout, matched.Stdout); err != nil {
		_, _ = fmt.Fprintf(stderr, "fake gh: write stdout: %v\n", err)
		return 1
	}
	if _, err := io.WriteString(stderr, matched.Stderr); err != nil {
		return 1
	}
	return matched.ExitCode
}
