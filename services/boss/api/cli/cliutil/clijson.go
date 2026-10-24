package cliutil

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/recurser/boss/internal/domain/rpcerr"
	"github.com/spf13/cobra"
)

// JSONFlagName is the flag every `--json` command registers. Named once because
// the root error path (EmitRootJSONFailure) has to look it up on a command it
// resolved itself, without the flag having necessarily parsed.
const JSONFlagName = "json"

// EmitJSON marshals v as indented JSON to the command's stdout. It is the
// success-envelope emitter shared by every `--json` command; EmitJSONFailure
// below is its failure counterpart.
//
// The write error is returned rather than discarded: a truncated envelope on a
// full disk or a closed pipe is exactly the case where a driver must not read
// exit 0 as "merged and reported".
func EmitJSON(cmd *cobra.Command, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal JSON output: %w", err)
	}
	if _, err := fmt.Fprintln(cmd.OutOrStdout(), string(b)); err != nil {
		return fmt.Errorf("write JSON output: %w", err)
	}
	return nil
}

// The stable `--json` error-code vocabulary.
//
// These codes exist so a driver can branch on an outcome without matching
// message text. The daemon already distinguishes its failures — a merge-strategy
// incompatibility, a merge-gate refusal and an unknown session are three
// different things — but they reach the CLI flattened into one non-zero exit
// with prose on stderr. `code` is the discriminator that survives that trip.
//
// The exit status is deliberately NOT part of the vocabulary: every failure
// exits 1, and there is no numeric taxonomy to learn. Callers read `code`.
const (
	// CodeConfirmationRequired is emitted when `--json` is used without
	// `--yes` on a command that would otherwise prompt. It is a usage error,
	// not a refusal by the daemon, so it never reaches the wire.
	CodeConfirmationRequired = "CONFIRMATION_REQUIRED"
	// CodeNotFound tags a session id that matched nothing locally, so it
	// classifies the same way as the daemon's own not_found.
	CodeNotFound = "NOT_FOUND"
	// CodeAmbiguousPrefix tags a session-id prefix matching several sessions.
	// It is distinct from NOT_FOUND because the caller's remedy differs: a
	// longer prefix fixes this, a different id fixes that.
	CodeAmbiguousPrefix = "AMBIGUOUS_PREFIX"
	// CodeAmbiguousPR tags a PR number that matches sessions in more than one
	// place (`boss pr`). The remedy is --repo, not a longer id.
	CodeAmbiguousPR = "AMBIGUOUS_PR"
	// CodeMergeStrategyIncompatible mirrors the daemon's
	// mergepolicy.ErrMergeStrategyIncompatible sentinel.
	CodeMergeStrategyIncompatible = "MERGE_STRATEGY_INCOMPATIBLE"
	// CodeAgentNotSpawned tags a chat row the daemon persisted without a live
	// agent behind it — RecordChat succeeds on a host with no tmux. It is
	// distinct from a transport failure: the RPC worked, the chat exists, and
	// nothing is listening, so the caller's remedy is on the daemon host rather
	// than in the request.
	CodeAgentNotSpawned = "AGENT_NOT_SPAWNED"
	// CodeChatStatusUnavailable tags a `boss chats --json` run whose per-chat
	// status read failed — typically a daemon too old to serve the call
	// answering Unimplemented. (--remote is no longer such a transport: since
	// BOS-824 it proxies the read through ProxyGetChatStatuses.) It is distinct
	// from the connect code because the caller's remedy is the same for every
	// underlying cause: stop, do not treat any chat as settled, and re-read over
	// a transport that can answer.
	CodeChatStatusUnavailable = "CHAT_STATUS_UNAVAILABLE"
	// CodeInvalidArgument tags a flag value the CLI rejects before it reaches
	// the wire — an unknown `boss new --tracker-source`, say. It reuses the
	// connect vocabulary's own INVALID_ARGUMENT spelling so a driver branching
	// on `code` sees one name whether the rejection happened locally or at the
	// daemon, rather than the UNKNOWN an untagged local error would resolve to.
	CodeInvalidArgument = "INVALID_ARGUMENT"
	// CodeHeadMismatch mirrors the daemon's vcs.ErrHeadMismatch sentinel: a
	// `boss merge --match-head` refused because the PR head is not the pinned
	// commit. It travels as FailedPrecondition, like a gate refusal, so the
	// token is the only discriminator.
	CodeHeadMismatch = "HEAD_MISMATCH"
)

// The stable `--json` SUCCESS-outcome vocabulary for `boss daemon restart`.
//
// A separate vocabulary from the error codes above, and for the same reason
// they exist: a lifecycle command can succeed in several materially different
// ways, and the difference — above all, whether the daemon is still supervised
// — reached a driver only as prose on stdout. `code` is the discriminator that
// survives the trip.
const (
	// OutcomeDaemonRestarted is the ordinary supervised restart.
	OutcomeDaemonRestarted = "RESTARTED"
	// OutcomeDaemonRestartedStandalone / OutcomeDaemonStartedStandalone are the
	// standalone path: a daemon that was already running without a service
	// manager is restarted in kind, or none was running and one was started.
	// Both are supervised:false by construction, which is the state the host
	// was already in — not a degradation.
	OutcomeDaemonRestartedStandalone = "RESTARTED_STANDALONE"
	OutcomeDaemonStartedStandalone   = "STARTED_STANDALONE"
	// OutcomeDaemonRestartedUnsupervised is the DEGRADED announced fallback: the
	// service manager reported success and produced no socket, so the daemon
	// was started directly to restore service and is no longer supervised.
	// Distinct from every other outcome because the host LOST supervision it
	// previously had, and a driver that cannot see that will believe the
	// daemon is still managed.
	OutcomeDaemonRestartedUnsupervised = "RESTARTED_UNSUPERVISED"
	// OutcomeDaemonRestartedAfterFallback is the same fallback ending WELL: the
	// follow-up start landed under the service manager after all, so
	// supervision was kept. Separate from the plain RESTARTED because the first
	// attempt still failed, which is worth surfacing, and separate from
	// RESTARTED_UNSUPERVISED because nothing was lost — the supervision
	// question is probed, never inferred from the fact that the fallback ran.
	OutcomeDaemonRestartedAfterFallback = "RESTARTED_AFTER_FALLBACK"
)

// daemonErrorTokens are sentinel tokens the daemon embeds in an error message
// because the connect code alone cannot carry the distinction. A merge refused
// for strategy incompatibility and a merge refused by the gate both travel as
// FailedPrecondition, so the token — not the connect code — is what tells them
// apart. Tokens are therefore resolved BEFORE the connect code.
var daemonErrorTokens = []string{
	CodeMergeStrategyIncompatible,
	CodeHeadMismatch,
}

// cliError tags an error with a stable envelope code while leaving its message
// byte-identical. Failures the CLI detects for itself — an id that resolves to
// no session, a confirmation prompt it refuses to issue — never reach the
// daemon and so carry no connect code. Tagging them at the point where the
// semantics are known is what lets `--json` classify them as something better
// than UNKNOWN without reintroducing the message matching this vocabulary
// exists to remove.
type cliError struct {
	code string
	err  error
}

func (e *cliError) Error() string { return e.err.Error() }
func (e *cliError) Unwrap() error { return e.err }

// CodedError tags err with a stable envelope code. The wrapped error's message
// is preserved exactly, so the human (non-`--json`) output is unaffected.
func CodedError(code string, err error) error { return &cliError{code: code, err: err} }

// ErrorCodeFor resolves the stable envelope code for err. Resolution order:
//
//  1. an explicit cliError tag, for a failure the CLI itself classified;
//  2. a known daemon token found in the message (see daemonErrorTokens) — the
//     token beats the connect code, since several distinct refusals share
//     FailedPrecondition;
//  3. the connect code, upper-cased: FAILED_PRECONDITION, NOT_FOUND,
//     UNAVAILABLE, PERMISSION_DENIED, UNIMPLEMENTED, …
//
// A plain error that is neither tagged nor a connect error resolves to UNKNOWN,
// because rpcerr.KindOf reports CodeUnknown for anything it cannot recognise.
// Callers that get UNKNOWN should fall back on connect_code.
func ErrorCodeFor(err error) string {
	if err == nil {
		return ""
	}
	var tagged *cliError
	if errors.As(err, &tagged) {
		return tagged.code
	}
	msg := err.Error()
	for _, token := range daemonErrorTokens {
		if strings.Contains(msg, token) {
			return token
		}
	}
	return strings.ToUpper(rpcerr.KindOf(err).String())
}

// ConnectCodeFor is the raw connect code ("failed_precondition", "not_found",
// "unknown", …). It is always emitted alongside code so a driver that meets an
// UNKNOWN it does not recognise still has the transport-level classification to
// fall back on.
func ConnectCodeFor(err error) string { return rpcerr.KindOf(err).String() }

// EnvelopeMessage is the message a caller should read. For a connect error it
// is the daemon's own message verbatim — Error() would prefix the connect code
// and any wrapping the CLI added on the way out, neither of which the daemon
// said.
func EnvelopeMessage(err error) string {
	if err == nil {
		return ""
	}
	return rpcerr.Message(err)
}

// JSONErrorEnvelope is the failure envelope shared by every `--json` command.
type JSONErrorEnvelope struct {
	Error JSONErrorBody `json:"error"`
}

type JSONErrorBody struct {
	Code        string `json:"code"`
	ConnectCode string `json:"connect_code"`
	Message     string `json:"message"`
}

// newJSONErrorEnvelope builds the failure envelope for err.
func newJSONErrorEnvelope(err error) JSONErrorEnvelope {
	return JSONErrorEnvelope{Error: JSONErrorBody{
		Code:        ErrorCodeFor(err),
		ConnectCode: ConnectCodeFor(err),
		Message:     EnvelopeMessage(err),
	}}
}

// EmitJSONFailure is the single failure exit for a `--json` command. Under
// --json it writes the error envelope to stdout and still returns err, so
// main.go prints its usual human line on stderr and exits 1. stdout stays the
// machine channel and stderr the human one; the envelope's code, not the exit
// status, is the discriminator.
//
// When asJSON is false it is a pass-through, which keeps the human path
// byte-identical to what it was before `--json` existed.
func EmitJSONFailure(cmd *cobra.Command, asJSON bool, err error) error {
	if !asJSON || err == nil {
		return err
	}
	if emitErr := EmitJSON(cmd, newJSONErrorEnvelope(err)); emitErr != nil {
		// The envelope never reached stdout, so this error stays unmarked and
		// the root path is free to try again.
		return errors.Join(err, emitErr)
	}
	return &jsonReportedError{err: err}
}

// EmitJSONReportedFailure writes v as the command's OWN machine-readable
// envelope for a failure, then marks err as already reported so the root
// backstop does not write a second object into a stream a driver parses as a
// single value.
//
// It exists for the one shape the fixed JSONErrorEnvelope cannot express: a
// failure whose discriminating facts are structured. `boss chat wait --json`'s
// timeout is exactly that — the point of the envelope is the liveness fields
// that explain WHY the wait never settled, and folding them into the error
// message would put them back in prose a caller has to scrape. err is still
// returned, so the exit status and the human stderr line are unchanged.
func EmitJSONReportedFailure(cmd *cobra.Command, v any, err error) error {
	if emitErr := EmitJSON(cmd, v); emitErr != nil {
		// The envelope never reached stdout, so this error stays unmarked and
		// the root path is free to try again.
		return errors.Join(err, emitErr)
	}
	return &jsonReportedError{err: err}
}

// jsonReportedError marks an error whose envelope has already been written to
// stdout. It exists so EmitRootJSONFailure can tell "no envelope was emitted"
// from "one was", and never write a second object into a stream a driver parses
// as a single value. The message and the unwrap chain are untouched, so the
// human stderr line and every errors.Is/As caller are unaffected.
type jsonReportedError struct{ err error }

func (e *jsonReportedError) Error() string { return e.err.Error() }
func (e *jsonReportedError) Unwrap() error { return e.err }

// jsonEnvelopeRequested reports whether argv asks a command that supports
// `--json` to emit the envelope, without depending on flag parsing having
// succeeded.
//
// Reading cmd.Flags() is not enough for the root error path: pflag parses
// left to right and stops at the token it rejects, so on `boss merge --bad
// --json` the flag was never recorded even though the caller plainly asked for
// JSON. Scanning argv answers the question the same way regardless of how far
// the parse got. Everything after a bare "--" is a positional argument, so the
// scan stops there rather than treating a literal "--json" operand as a flag.
func jsonEnvelopeRequested(root *cobra.Command, argv []string) bool {
	cmd, _, err := root.Find(argv)
	if err != nil || cmd == nil || cmd.Flags().Lookup(JSONFlagName) == nil {
		return false
	}
	for _, arg := range argv {
		if arg == "--" {
			return false
		}
		switch arg {
		case "--" + JSONFlagName, "--" + JSONFlagName + "=true", "--" + JSONFlagName + "=1":
			return true
		}
	}
	return false
}

// EmitRootJSONFailure is the backstop for failures raised before a command's
// RunE ever runs — a flag-parse rejection, an Args refusal, PersistentPreRunE.
// Those return straight out of cobra's Execute, so they never reach the
// EmitJSONFailure call inside the command, and a `--json` caller would see an
// empty stdout and be pushed back to scraping stderr: the exact coupling the
// envelope exists to remove. Emitting here makes "every failure exit writes one
// envelope to stdout" true of the whole CLI rather than only of the paths that
// got far enough to run.
//
// It is a no-op when the command already emitted one, when the run succeeded,
// or when nothing asked for JSON.
func EmitRootJSONFailure(root *cobra.Command, argv []string, err error) error {
	var reported *jsonReportedError
	if err == nil || errors.As(err, &reported) || !jsonEnvelopeRequested(root, argv) {
		return err
	}
	return EmitJSONFailure(root, true, err)
}

// ReportedError marks an error whose command-specific output is already written.
func ReportedError(err error) error { return &jsonReportedError{err: err} }
