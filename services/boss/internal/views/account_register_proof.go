//go:build e2e

package views

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/recurser/boss/internal/accountflow"
)

// account_register_proof.go is the e2e-only subprocess stand-in that lets a TUI
// proof scenario capture the BOS-1142 reauthentication flow end to end.
//
// It exists because the flow's honesty is the thing being demonstrated. The
// success line is gated on the daemon's post-save verdict, not on err == nil
// (BOS-943), so a capture that stopped short of a real RefreshAccount round
// trip would prove only that a screen can print the word "Reauthenticated" —
// exactly the class of evidence this ticket exists to stop accepting. The mock
// daemon already answers RefreshAccount faithfully; the provider login was the
// one step with no deterministic stand-in.
//
// It is compiled out of every production build by the e2e tag, the same
// arrangement general_settings_proof.go uses, so no shipped binary contains a
// path that can substitute a fabricated credential for a real device login.
//
// The credential it writes is synthetic and inert: obviously-fake token
// strings, never a captured secret, and it never reaches a rendered frame
// (accountflow hands the bytes to the daemon and shows only the verdict).

// proofCodexDeviceLine is the device-flow prompt the flow parses. It must match
// agentcred's URL and code regexes or the walkthrough surfaces no prompt.
const proofCodexDeviceLine = "Open https://auth.openai.com/codex/device and enter code PRF0-FAKE1"

// proofCodexAuthJSON is a SYNTHETIC codex auth.json. Every value is a visibly
// fake sentinel so a copy of it that escaped a fixture could not be mistaken
// for a live credential.
const proofCodexAuthJSON = `{"tokens":{"access_token":"FAKE-proof-access","refresh_token":"FAKE-proof-refresh","id_token":"FAKE-proof-id"}}`

// proofRegisterExec returns the scripted login stand-in when
// BOSS_PROOF_CODEX_REAUTH=1, and nil otherwise so the ordinary production seam
// selection in startFlow runs unchanged.
func proofRegisterExec() accountflow.Exec {
	if os.Getenv("BOSS_PROOF_CODEX_REAUTH") != "1" {
		return nil
	}
	return proofCodexExec{}
}

type proofCodexExec struct{}

// Start writes the synthetic auth.json into the isolated CODEX_HOME the flow
// created, then emits the two lines a real device login prints and exits. The
// dir comes from the flow's own extraEnv rather than from configuration: this
// stand-in must not be able to write anywhere the flow did not already choose
// and is not already deleting on every exit path.
func (proofCodexExec) Start(_ context.Context, _ string, _, extraEnv []string) (accountflow.Proc, error) {
	home := ""
	for _, kv := range extraEnv {
		if v, ok := strings.CutPrefix(kv, "CODEX_HOME="); ok {
			home = v
		}
	}
	if home == "" {
		return nil, errors.New("proof codex exec: no CODEX_HOME in the flow environment")
	}
	if err := os.WriteFile(filepath.Join(home, "auth.json"), []byte(proofCodexAuthJSON), 0o600); err != nil {
		return nil, err
	}
	lines := make(chan string, 2)
	proc := &proofCodexProc{lines: lines, done: make(chan struct{})}
	go func() {
		defer close(lines)
		lines <- "Starting device authorization…"
		lines <- proofCodexDeviceLine
		// Hold the login open the way a real one is held open: a device flow
		// waits on a human at a browser. Returning instantly instead would make
		// the flow's own progress screen — the frame that names which account is
		// being overwritten — exist for less than one render, so the capture
		// could only ever show the before and after of a step nobody can see.
		select {
		case <-time.After(proofCodexDeviceDwell):
		case <-proc.done:
		}
	}()
	return proc, nil
}

// proofCodexDeviceDwell is how long the scripted login stays open. Long enough
// that the replay's screen poll cannot miss the frame, short enough to stay far
// inside the flow's own timeout.
const proofCodexDeviceDwell = 2500 * time.Millisecond

type proofCodexProc struct {
	lines chan string
	// done releases the dwell early on Kill so a cancelled flow does not leave
	// this goroutine sleeping.
	done     chan struct{}
	killOnce sync.Once
}

func (p *proofCodexProc) Lines() <-chan string { return p.lines }
func (p *proofCodexProc) Wait() error          { return nil }

func (p *proofCodexProc) Kill() error {
	p.killOnce.Do(func() { close(p.done) })
	return nil
}

var _ accountflow.Exec = proofCodexExec{}
var _ accountflow.Proc = (*proofCodexProc)(nil)
