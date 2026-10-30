package upstream

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/recurser/bossd/internal/domain"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/kit/gatecmd"
	"github.com/recurser/bossalib/models"
	"github.com/rs/zerolog"
)

// maxTriggerGateTimeout caps a trigger gate's run time. A gate decides whether
// to start work and must be cheap; bosso's own wait is bounded too.
const maxTriggerGateTimeout = 300 * time.Second

// maxGateOutput caps how much of a gate command's output is returned for the
// invocation record and logged.
const maxGateOutput = 4 << 10

// Environment variables naming the trigger invocation a gate command runs for.
const (
	envTriggerID           = "BOSS_TRIGGER_ID"
	envTriggerInvocationID = "BOSS_TRIGGER_INVOCATION_ID"
)

// ProofResolver supplies the proof overlay to the injected gate policy.
type ProofResolver interface{ Resolve() map[string]string }

// TriggerGateRunner answers RunTriggerGateCommand (BOS-1467): it runs a
// trigger's gate command in the repo's local clone, with the invocation's
// payload on stdin, and reports the verdict and a bounded tail of the output.
// A trigger gate is a shell command exactly like a cron job's gate_command and
// shares the cron gate's execution (gatecmd) and environment composition, so a
// command behaves the same whichever kind of gate runs it.
type TriggerGateRunner struct {
	Repos TriggerRepoResolver
	// ProofEnv resolves the proof overlay; a gate of a trusted repo receives
	// only its Anthropic key (see proofenv.GateEnvFor). Nil means no proof
	// injection.
	ProofEnv ProofResolver
	// GateEnv applies the repository trust policy and lists ambient keys to unset.
	// Required; cmd injects proofenv.GateEnvFor.
	GateEnv func(ProofResolver, *models.Repo) (string, []string)
	Logger  zerolog.Logger
}

// Run implements the RunTriggerGate command. Argument and repo problems are
// command errors (INVALID_ARGUMENT, NOT_FOUND, FAILED_PRECONDITION) and run
// nothing; every command outcome — including one that could not run — is a
// successful reply carrying the verdict.
func (r *TriggerGateRunner) Run(ctx context.Context, cmd *pb.RunTriggerGateCommand) (*pb.RunTriggerGateResult, pb.CommandResult_ErrorCode, error) {
	invocationID := strings.TrimSpace(cmd.GetInvocationId())
	triggerID := strings.TrimSpace(cmd.GetTriggerId())
	originURL := strings.TrimSpace(cmd.GetRepoOriginUrl())
	gateCommand := strings.TrimSpace(cmd.GetGateCommand())
	if invocationID == "" || triggerID == "" || originURL == "" || gateCommand == "" {
		return nil, pb.CommandResult_ERROR_CODE_INVALID_ARGUMENT,
			errors.New("run_trigger_gate: invocation_id, trigger_id, repo_origin_url and gate_command are required")
	}
	if r.Repos == nil || r.GateEnv == nil {
		return nil, pb.CommandResult_ERROR_CODE_UNSPECIFIED, errors.New("run_trigger_gate: runner not wired")
	}

	repo, code, err := resolveTriggerRepo(ctx, r.Repos, originURL)
	if err != nil {
		return nil, code, fmt.Errorf("run_trigger_gate: %w", err)
	}
	if strings.TrimSpace(repo.LocalPath) == "" {
		return nil, pb.CommandResult_ERROR_CODE_FAILED_PRECONDITION,
			fmt.Errorf("run_trigger_gate: repository %s has no local path", originURL)
	}

	logger := r.Logger.With().
		Str("trigger_id", triggerID).
		Str("invocation_id", invocationID).
		Str("repo_id", repo.ID).
		Logger()

	var output bytes.Buffer
	key, unset := r.GateEnv(r.ProofEnv, repo) // trusted repos only (BOS-1562)
	result := gatecmd.Run(ctx, gatecmd.Options{
		Command:              gateCommand,
		RepoPath:             repo.LocalPath,
		LinearAPIKey:         repo.LinearAPIKey,
		SentryAPIKey:         repo.SentryAPIKey,
		SentryOrg:            repo.SentryOrg,
		ProofAnthropicAPIKey: key,
		UnsetEnv:             unset,
		ExtraEnv: map[string]string{
			envTriggerID:           triggerID,
			envTriggerInvocationID: invocationID,
		},
		Stdin:   strings.NewReader(cmd.GetInputJson()),
		Timeout: triggerGateTimeout(cmd.GetTimeoutSeconds()),
		Output:  &output,
	})

	gateOutput := gateOutputTail(output.Bytes(), maxGateOutput)
	res := &pb.RunTriggerGateResult{
		Verdict:     triggerGateVerdict(result),
		FailureKind: result.Failure.String(),
		ExitCode:    int32(result.ExitCode), // #nosec G115 -- a process exit status or -1, always within int32
		Output:      gateOutput,
	}
	switch res.GetVerdict() {
	case pb.TriggerGateVerdict_TRIGGER_GATE_VERDICT_COULD_NOT_RUN:
		logger.Warn().
			Err(result.Err).
			Str("gate_output", gateOutput).
			Str("gate_failure", result.Failure.String()).
			Int("gate_exit_code", result.ExitCode).
			Msg("run_trigger_gate: gate command could not be evaluated")
	case pb.TriggerGateVerdict_TRIGGER_GATE_VERDICT_BLOCKED:
		logger.Info().
			Str("gate_output", gateOutput).
			Int("gate_exit_code", result.ExitCode).
			Msg("run_trigger_gate: gate command blocked the launch")
	default:
		logger.Debug().Str("gate_output", gateOutput).Msg("run_trigger_gate: gate command passed")
	}
	return res, pb.CommandResult_ERROR_CODE_UNSPECIFIED, nil
}

// gateOutputTail returns the last max bytes of out, prefixed with a marker
// naming how many leading bytes were dropped. The cut is moved forward past
// any UTF-8 continuation bytes so the tail never starts mid-rune.
func gateOutputTail(out []byte, max int) string {
	if len(out) <= max {
		return string(out)
	}
	cut := len(out) - max
	for cut < len(out) && !utf8.RuneStart(out[cut]) {
		cut++
	}
	return fmt.Sprintf("[truncated %d bytes]\n%s", cut, out[cut:])
}

// triggerGateVerdict maps a gate result onto the wire verdict.
func triggerGateVerdict(res gatecmd.Result) pb.TriggerGateVerdict {
	switch {
	case res.Passed:
		return pb.TriggerGateVerdict_TRIGGER_GATE_VERDICT_PASSED
	case res.CouldNotRun():
		return pb.TriggerGateVerdict_TRIGGER_GATE_VERDICT_COULD_NOT_RUN
	default:
		return pb.TriggerGateVerdict_TRIGGER_GATE_VERDICT_BLOCKED
	}
}

// triggerGateTimeout converts the command's timeout_seconds: 0 means gatecmd's
// default, anything else is clamped to [1s, maxTriggerGateTimeout].
func triggerGateTimeout(seconds int32) time.Duration {
	switch {
	case seconds == 0:
		return gatecmd.DefaultTimeout
	case seconds < 1:
		return time.Second
	}
	timeout := time.Duration(seconds) * time.Second
	if timeout > maxTriggerGateTimeout {
		return maxTriggerGateTimeout
	}
	return timeout
}

// resolveTriggerRepo resolves a trigger command's repo by origin URL and
// classifies the failure for CommandResult.error_code: NOT_FOUND for an
// unregistered origin, FAILED_PRECONDITION for an ambiguous one.
func resolveTriggerRepo(ctx context.Context, repos TriggerRepoResolver, originURL string) (*models.Repo, pb.CommandResult_ErrorCode, error) {
	repo, err := repos.GetByOriginURL(ctx, originURL)
	switch {
	case errors.Is(err, domain.ErrNotFound) || (err == nil && repo == nil):
		return nil, pb.CommandResult_ERROR_CODE_NOT_FOUND,
			fmt.Errorf("repository %s is not registered on this daemon", originURL)
	case errors.Is(err, domain.ErrAmbiguousOrigin):
		return nil, pb.CommandResult_ERROR_CODE_FAILED_PRECONDITION, err
	case err != nil:
		return nil, pb.CommandResult_ERROR_CODE_UNSPECIFIED, fmt.Errorf("resolve repo: %w", err)
	}
	return repo, pb.CommandResult_ERROR_CODE_UNSPECIFIED, nil
}

// RunTriggerGate implements SessionCommandHandler.RunTriggerGate by delegating
// to the configured TriggerGateRunner (see trigger_gate.go).
func (a *CommandHandlerAdapter) RunTriggerGate(ctx context.Context, cmd *pb.RunTriggerGateCommand) (*pb.RunTriggerGateResult, pb.CommandResult_ErrorCode, error) {
	if a.TriggerGates == nil {
		return nil, pb.CommandResult_ERROR_CODE_UNSPECIFIED, errors.New("run_trigger_gate: runner not wired")
	}
	return a.TriggerGates.Run(ctx, cmd)
}
