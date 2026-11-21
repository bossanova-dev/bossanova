package main

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strings"
	"time"

	"github.com/recurser/bossalib/contract/plugin/childenv"
	"github.com/recurser/bossalib/contract/plugin/loginshell"
)

const oneShotWaitDelay = 5 * time.Second

// runAgy runs one short agy command and returns its stdout. stdin is always a
// pipe: with a pipe a logged-out agy fails at once with "authentication
// required", while a terminal or /dev/null makes it wait 60 seconds for a
// browser login.
func (r *Runner) runAgy(ctx context.Context, workDir string, args []string, stdin string, extraEnv map[string]string) (string, error) {
	argv := loginshell.Wrap(r.loginShell, loginshell.Flags(r.loginShell), append([]string{"agy"}, args...))
	factory := r.cmdFactory
	if factory == nil {
		factory = exec.CommandContext
	}
	cmd := factory(ctx, argv[0], argv[1:]...)
	cmd.Dir = workDir
	cmd.Env = oneShotEnviron(extraEnv)
	cmd.Stdin = strings.NewReader(stdin)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	cmd.WaitDelay = oneShotWaitDelay
	if err := cmd.Run(); err != nil {
		return stdout.String(), fmt.Errorf("agy %s: %w (stderr: %s)", args[0], err, strings.TrimSpace(stderr.String()))
	}
	return stdout.String(), nil
}

// oneShotEnviron mirrors the headless run's environment: the filtered
// inherited baseline plus the daemon's overlay. Overlay values are secret and
// never logged.
func oneShotEnviron(extraEnv map[string]string) []string {
	env := childenv.Build(os.Environ(), childenv.ForPlugin(pluginName, strings.Split(os.Getenv(childenv.PassthroughEnvVar), ",")))
	keys := make([]string, 0, len(extraEnv))
	for key := range extraEnv {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		env = append(env, key+"="+extraEnv[key])
	}
	return env
}
