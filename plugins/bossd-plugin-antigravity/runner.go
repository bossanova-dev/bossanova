package main

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/rs/zerolog"

	"github.com/recurser/bossalib/contract/plugin/agentruntime"
	"github.com/recurser/bossalib/contract/plugin/loginshell"
)

// conversationIDWait bounds how long StartAgentRun waits for agy's init event.
// agy takes 5 to 8 seconds to print it, past agentruntime's own 5-second
// SessionIDFromOutput window, so the runner waits itself. bossd allows
// StartAgentRun 90 seconds.
const conversationIDWait = 45 * time.Second

// Runner wraps agentruntime.Runner with agy argv building and conversation-id
// discovery. agentruntime keys each process under the id the runner started it
// with; once agy reports its conversation id, that id becomes the one bossd
// sees, and aliases maps it back to the process key.
type Runner struct {
	*agentruntime.Runner
	launchConfig
	cmdFactory       agentruntime.CommandFactory
	conversationWait time.Duration
	logger           zerolog.Logger

	mu      sync.Mutex
	aliases map[string]string
}

// Option configures a Runner.
type Option func(*Runner)

// WithModel sets the fallback --model for launches without their own.
func WithModel(model string) Option { return func(r *Runner) { r.model = model } }

// WithMode sets agy's --mode (accept-edits, auto or plan).
func WithMode(mode string) Option { return func(r *Runner) { r.mode = mode } }

// WithDangerouslySkipPermissions adds --dangerously-skip-permissions.
func WithDangerouslySkipPermissions(v bool) Option {
	return func(r *Runner) { r.dangerouslySkipPermissions = v }
}

// WithLoginShell runs agy through the operator's login shell so PATH shims
// resolve it.
func WithLoginShell(shell string) Option { return func(r *Runner) { r.loginShell = shell } }

// WithCommandFactory overrides process creation (tests).
func WithCommandFactory(f agentruntime.CommandFactory) Option {
	return func(r *Runner) { r.cmdFactory = f }
}

// WithConversationWait overrides conversationIDWait (tests).
func WithConversationWait(d time.Duration) Option { return func(r *Runner) { r.conversationWait = d } }

// NewRunner creates the agy process runner.
func NewRunner(logger zerolog.Logger, opts ...Option) *Runner {
	r := &Runner{conversationWait: conversationIDWait, logger: logger, aliases: make(map[string]string)}
	for _, opt := range opts {
		opt(r)
	}
	var extra []agentruntime.Option
	if r.cmdFactory != nil {
		extra = append(extra, agentruntime.WithCommandFactory(r.cmdFactory))
	}
	r.Runner = agentruntime.NewRunner(logger, agentruntime.Options{
		BinaryName: pluginName,
		BuildArgv:  r.buildArgv,
		PostExit: func(orig error, tail []byte) error {
			return classifyExit(logger, orig, tail, time.Now())
		},
	}, extra...)
	return r
}

const agyOptionUnattendedPolicy = "unattended_policy"

// buildArgv renders the headless argv. The plan arrives on stdin as one
// stream-json turn (see encodeStreamTurn), never on argv.
func (r *Runner) buildArgv(in agentruntime.BuildArgvInput) []string {
	args := []string{"agy", "--input-format", "stream-json", "--output-format", "stream-json"}
	if in.Resume != nil && *in.Resume != "" {
		args = append(args, "--conversation", *in.Resume)
	}
	args = append(args, r.sessionFlags(in.Options["model"], in.Options["effort"], in.WorkDir, in.Options[agyOptionUnattendedPolicy] == "1")...)
	return loginshell.Wrap(r.loginShell, loginshell.Flags(r.loginShell), args)
}

// Start launches a headless run and returns agy's conversation id, or the
// process key when agy never announced one.
//
// agy silently starts a new conversation when --conversation names one it does
// not know. A resumed plan run without its history would report success on
// work it never saw, so that run is stopped and the resume fails.
// errConversationNotResumed reports that agy started a new conversation instead
// of resuming the requested one.
var errConversationNotResumed = errors.New("agy did not resume the conversation")

func (r *Runner) Start(ctx context.Context, workDir, plan string, resume *string, sessionID, logPath, model, effort string, extraEnv map[string]string) (string, error) {
	return r.startRun(ctx, workDir, plan, resume, sessionID, logPath, model, effort, extraEnv, false)
}

func (r *Runner) startRun(ctx context.Context, workDir, plan string, resume *string, sessionID, logPath, model, effort string, extraEnv map[string]string, hasUnattendedPolicy bool) (string, error) {
	input, err := encodeStreamTurn(plan)
	if err != nil {
		return "", fmt.Errorf("encode plan: %w", err)
	}
	key := sessionID
	if key == "" {
		key = uuid.NewString()
	}
	opts := map[string]string{"model": model, "effort": effort}
	if hasUnattendedPolicy {
		opts[agyOptionUnattendedPolicy] = "1"
	}
	key, err = r.StartWithOptions(ctx, workDir, input, resume, key, logPath, extraEnv, opts)
	if err != nil {
		return "", err
	}
	conversation := r.awaitConversationID(key)
	if conversation == "" {
		r.logger.Warn().Str("session", key).Msg("agy announced no conversation id; returning the process key")
		return key, nil
	}
	if resume != nil && *resume != "" && *resume != conversation {
		_ = r.Stop(key)
		return "", fmt.Errorf("%w: asked for %s, agy started %s", errConversationNotResumed, *resume, conversation)
	}
	r.mu.Lock()
	r.aliases[conversation] = key
	r.mu.Unlock()
	return conversation, nil
}

func (r *Runner) awaitConversationID(key string) string {
	ctx, cancel := context.WithTimeout(context.Background(), r.conversationWait)
	defer cancel()
	lines, err := r.Subscribe(ctx, key)
	if err != nil {
		return ""
	}
	if id := conversationIDFromHistory(r.History(key)); id != "" {
		return id
	}
	// The channel closes when the process exits or ctx expires; a full channel
	// drops lines, so the ring buffer is checked once more at the end.
	for line := range lines {
		if id := conversationIDFromInit(line.Text); id != "" {
			return id
		}
	}
	return conversationIDFromHistory(r.History(key))
}

func conversationIDFromHistory(lines []agentruntime.OutputLine) string {
	for _, line := range lines {
		if id := conversationIDFromInit(line.Text); id != "" {
			return id
		}
	}
	return ""
}

// processKey maps a conversation id bossd holds back to agentruntime's key.
func (r *Runner) processKey(id string) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	if key, ok := r.aliases[id]; ok {
		return key
	}
	return id
}

func (r *Runner) stop(id string) error      { return r.Stop(r.processKey(id)) }
func (r *Runner) isRunning(id string) bool  { return r.IsRunning(r.processKey(id)) }
func (r *Runner) exitError(id string) error { return r.ExitError(r.processKey(id)) }
