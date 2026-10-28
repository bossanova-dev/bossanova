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
	"path/filepath"
	"regexp"
	"slices"
	"time"
)

type response struct {
	Args     []string `json:"args"`
	Stdout   string   `json:"stdout"`
	Stderr   string   `json:"stderr"`
	ExitCode int      `json:"exit_code"`
	Hold     string   `json:"hold,omitempty"`
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
		if r.Hold != "" && !holdName.MatchString(r.Hold) {
			_, _ = fmt.Fprintf(stderr, "fake gh: invalid hold %q\n", r.Hold)
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
	if matched.Hold != "" {
		if err := waitHold(specPath, matched.Hold, args); err != nil {
			_, _ = fmt.Fprintf(stderr, "fake gh: %v\n", err)
			return 1
		}
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

var holdName = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,63}$`)

func waitHold(specPath, name string, argv []string) error {
	timeout := 15 * time.Minute
	if value := os.Getenv("BOSS_FAKE_GH_HOLD_TIMEOUT"); value != "" {
		var err error
		timeout, err = time.ParseDuration(value)
		if err != nil || timeout <= 0 {
			return fmt.Errorf("invalid BOSS_FAKE_GH_HOLD_TIMEOUT %q", value)
		}
	}
	dir := filepath.Join(filepath.Dir(specPath), "holds")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create holds: %w", err)
	}
	data, err := json.Marshal(struct {
		PID       int       `json:"pid"`
		Argv      []string  `json:"argv"`
		EnteredAt time.Time `json:"enteredAt"`
	}{os.Getpid(), argv, time.Now().UTC()})
	if err != nil {
		return err
	}
	file, err := os.CreateTemp(dir, ".entered-*")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(file.Name()) }()
	_, writeErr := file.Write(data)
	closeErr := file.Close()
	if writeErr != nil {
		return writeErr
	}
	if closeErr != nil {
		return closeErr
	}
	if err := os.Rename(file.Name(), filepath.Join(dir, name+".entered")); err != nil {
		return err
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		if _, err := os.Stat(filepath.Join(dir, name+".release")); err == nil {
			return nil
		} else if !os.IsNotExist(err) {
			return err
		}
		select {
		case <-timer.C:
			return fmt.Errorf("hold %s expired", name)
		case <-ticker.C:
		}
	}
}
