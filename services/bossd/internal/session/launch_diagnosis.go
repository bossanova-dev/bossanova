package session

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	bossanovav1 "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossd/internal/adapter/tmux"
	"github.com/recurser/bossd/internal/detach"
	"github.com/rs/zerolog"
)

type launchDiagnosis struct {
	inputFile, argvCarriesInput, working, account, resolveReason string
	composer                                                     tmux.LaunchComposerState
}

func (d launchDiagnosis) suffix() string {
	return fmt.Sprintf("; diagnosis: input-file=%s argv-carries-input=%s composer=%s working=%s account=%s resolve=%q", d.inputFile, d.argvCarriesInput, d.composer, d.working, d.account, d.resolveReason)
}

func (d launchDiagnosis) logFields(e *zerolog.Event) *zerolog.Event {
	return e.Str("inputFile", d.inputFile).Str("argvCarriesInput", d.argvCarriesInput).
		Str("composer", string(d.composer)).Str("working", d.working).
		Str("account", d.account).Str("resolveReason", d.resolveReason)
}

func (l *Lifecycle) diagnoseLaunch(ctx context.Context, launch launchInput, w deliveryWait, res deliveryResult, pane string, live bool) launchDiagnosis {
	d := launchDiagnosis{inputFile: "inline", argvCarriesInput: "unknown", composer: tmux.LaunchComposerNoComposer, working: "unknown", account: w.account, resolveReason: res.resolveReason}
	if d.account == "" {
		d.account = "ambient"
	}
	if launch.path != "" {
		d.inputFile = "unknown"
		if _, err := os.Stat(launch.path); err == nil {
			d.inputFile = "unread"
		} else if os.IsNotExist(err) {
			d.inputFile = "consumed"
		}
	}
	payload := launch.prompt
	if launch.command != "" {
		payload = launch.command
	}
	detach.Cleanup(ctx, detach.CleanupBudget, func(probeCtx context.Context) {
		if pid, err := l.tmux.PanePID(probeCtx, w.tmuxName); err == nil && pid > 0 {
			snapshot := l.launchProcessSnapshot
			if snapshot == nil {
				snapshot = launchProcesses
			}
			if data, err := snapshot(probeCtx); err == nil {
				d.argvCarriesInput = launchArgvCarriesInput(string(data), pid, payload)
			}
		}
	})
	if live {
		d.composer = tmux.ClassifyLaunchComposer(pane, payload)
		detach.Cleanup(ctx, detach.CleanupBudget, func(probeCtx context.Context) {
			resp, err := w.client.HasWorkingIndicator(probeCtx, &bossanovav1.HasWorkingIndicatorRequest{PaneContent: []byte(pane)})
			if err == nil && resp != nil {
				d.working = "no"
				if resp.GetIsWorking() {
					d.working = "yes"
				}
			}
		})
	}
	return d
}

func launchProcesses(ctx context.Context) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "ps", "-axo", "pid=,ppid=,args=")
	cmd.WaitDelay = time.Second
	return cmd.Output()
}

func launchArgvCarriesInput(snapshot string, panePID int, payload string) string {
	prefix := ""
	for _, line := range strings.Split(payload, "\n") {
		if prefix = strings.TrimSpace(line); prefix != "" {
			break
		}
	}
	if prefix == "" {
		return "unknown"
	}
	if len(prefix) > 120 {
		prefix = prefix[:120]
	}
	type process struct {
		pid, parent int
		args        string
	}
	var processes []process
	for _, line := range strings.Split(snapshot, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 3 {
			continue
		}
		pid, err := strconv.Atoi(fields[0])
		if err != nil {
			continue
		}
		parent, err := strconv.Atoi(fields[1])
		if err != nil {
			continue
		}
		rest := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), fields[0]))
		args := strings.TrimSpace(strings.TrimPrefix(rest, fields[1]))
		processes = append(processes, process{pid, parent, args})
	}
	descendants := map[int]bool{panePID: true}
	for changed := true; changed; {
		changed = false
		for _, p := range processes {
			if descendants[p.parent] && !descendants[p.pid] {
				descendants[p.pid] = true
				changed = true
			}
		}
	}
	for _, p := range processes {
		if descendants[p.pid] && strings.Contains(p.args, prefix) {
			return "yes"
		}
	}
	return "no"
}

func launchAccountLabel(id *string) string {
	if id == nil || *id == "" {
		return "ambient"
	}
	return *id
}
