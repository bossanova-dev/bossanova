package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"connectrpc.com/connect"
	"github.com/spf13/cobra"
)

// TestErrorCodeFor pins the stable `--json` error-code vocabulary and, above
// all, its resolution order. The load-bearing case is the first one: the
// daemon's MERGE_STRATEGY_INCOMPATIBLE token and an ordinary merge-gate refusal
// both arrive as FailedPrecondition, so a resolver that consulted the connect
// code first would collapse them into one code and hand drivers back exactly
// the message-matching this vocabulary exists to remove.
func TestErrorCodeFor(t *testing.T) {
	tests := []struct {
		name            string
		err             error
		wantCode        string
		wantConnectCode string
	}{
		{
			name: "daemon token beats the connect code it travels under",
			err: connect.NewError(connect.CodeFailedPrecondition, errors.New(
				"MERGE_STRATEGY_INCOMPATIBLE: branch has 1 merge commit(s) and the repo requires rebase")),
			wantCode:        "MERGE_STRATEGY_INCOMPATIBLE",
			wantConnectCode: "failed_precondition",
		},
		{
			name: "token survives the CLI's own error wrapping",
			err: fmt.Errorf("merge session: %w", connect.NewError(connect.CodeFailedPrecondition, errors.New(
				"MERGE_STRATEGY_INCOMPATIBLE: branch has 1 merge commit(s)"))),
			wantCode:        "MERGE_STRATEGY_INCOMPATIBLE",
			wantConnectCode: "failed_precondition",
		},
		{
			name: "gate refusal carrying no known token falls to the connect code",
			err: connect.NewError(connect.CodeFailedPrecondition, errors.New(
				"merge blocked: gate=checks; 2 required checks are still failing")),
			wantCode:        "FAILED_PRECONDITION",
			wantConnectCode: "failed_precondition",
		},
		{
			name:            "not found",
			err:             connect.NewError(connect.CodeNotFound, errors.New(`session "nope" not found`)),
			wantCode:        "NOT_FOUND",
			wantConnectCode: "not_found",
		},
		{
			name:            "unavailable",
			err:             connect.NewError(connect.CodeUnavailable, errors.New("daemon is not running")),
			wantCode:        "UNAVAILABLE",
			wantConnectCode: "unavailable",
		},
		{
			name:            "permission denied",
			err:             connect.NewError(connect.CodePermissionDenied, errors.New("not authorised")),
			wantCode:        "PERMISSION_DENIED",
			wantConnectCode: "permission_denied",
		},
		{
			name:            "unimplemented",
			err:             connect.NewError(connect.CodeUnimplemented, errors.New("not implemented")),
			wantCode:        "UNIMPLEMENTED",
			wantConnectCode: "unimplemented",
		},
		{
			name:            "invalid argument",
			err:             connect.NewError(connect.CodeInvalidArgument, errors.New("bad id")),
			wantCode:        "INVALID_ARGUMENT",
			wantConnectCode: "invalid_argument",
		},
		{
			name:            "a bare error is UNKNOWN with connect_code unknown",
			err:             errors.New("something went sideways"),
			wantCode:        "UNKNOWN",
			wantConnectCode: "unknown",
		},
		{
			name:            "a CLI-tagged failure wins over the connect fallback",
			err:             codedError(codeConfirmationRequired, errors.New("--json requires --yes")),
			wantCode:        "CONFIRMATION_REQUIRED",
			wantConnectCode: "unknown",
		},
		{
			name:            "a locally resolved miss is NOT_FOUND though it never hit the wire",
			err:             codedError(codeNotFound, errors.New(`no session found matching prefix "nope"`)),
			wantCode:        "NOT_FOUND",
			wantConnectCode: "unknown",
		},
		{
			name:            "an ambiguous prefix is distinct from a miss",
			err:             codedError(codeAmbiguousPrefix, errors.New(`ambiguous prefix "sess-" matches 3 sessions`)),
			wantCode:        "AMBIGUOUS_PREFIX",
			wantConnectCode: "unknown",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := errorCodeFor(tc.err); got != tc.wantCode {
				t.Errorf("errorCodeFor() = %q, want %q", got, tc.wantCode)
			}
			if got := connectCodeFor(tc.err); got != tc.wantConnectCode {
				t.Errorf("connectCodeFor() = %q, want %q", got, tc.wantConnectCode)
			}
		})
	}
}

// TestCodedErrorPreservesMessage is the guard that keeps the tag invisible to
// the human path: codedError must not decorate the message, or every non-json
// caller of resolveSessionID changes its output.
func TestCodedErrorPreservesMessage(t *testing.T) {
	const msg = `no session found matching prefix "nope"`
	err := codedError(codeNotFound, errors.New(msg))
	if got := err.Error(); got != msg {
		t.Fatalf("Error() = %q, want the message unchanged (%q)", got, msg)
	}
}

// TestEnvelopeMessageStripsWrappingAndCode pins that error.message carries what
// the daemon actually said — not the connect code prefix Error() adds, and not
// the CLI's own "merge session:" wrapper.
func TestEnvelopeMessageStripsWrappingAndCode(t *testing.T) {
	const blocked = "merge blocked: gate=checks; 2 required checks are still failing"
	wrapped := fmt.Errorf("merge session: %w", connect.NewError(connect.CodeFailedPrecondition, errors.New(blocked)))

	if got := envelopeMessage(wrapped); got != blocked {
		t.Fatalf("envelopeMessage() = %q, want the daemon's message verbatim (%q)", got, blocked)
	}
	// Sanity: the wrapped Error() really does carry the noise we stripped, so
	// this test cannot pass vacuously.
	if !strings.Contains(wrapped.Error(), "merge session: failed_precondition: ") {
		t.Fatalf("wrapped.Error() = %q, expected it to carry the wrapper and code prefix", wrapped.Error())
	}
}

// TestEmitJSONFailureWritesEnvelopeAndReturnsErr covers the two obligations of
// the shared failure exit: a parseable envelope on stdout, and the original
// error still returned so main.go exits 1.
func TestEmitJSONFailureWritesEnvelopeAndReturnsErr(t *testing.T) {
	cmd := &cobra.Command{}
	var out strings.Builder
	cmd.SetOut(&out)

	orig := connect.NewError(connect.CodeFailedPrecondition, errors.New("merge blocked: gate=checks; nope"))
	got := emitJSONFailure(cmd, true, orig)

	if !errors.Is(got, orig) {
		t.Fatalf("emitJSONFailure returned %v, want the original error so the command still fails", got)
	}
	var env jsonErrorEnvelope
	if err := json.Unmarshal([]byte(out.String()), &env); err != nil {
		t.Fatalf("stdout is not parseable JSON (%v): %q", err, out.String())
	}
	if env.Error.Code != "FAILED_PRECONDITION" {
		t.Errorf("error.code = %q, want FAILED_PRECONDITION", env.Error.Code)
	}
	if env.Error.ConnectCode != "failed_precondition" {
		t.Errorf("error.connect_code = %q, want failed_precondition", env.Error.ConnectCode)
	}
	if env.Error.Message != "merge blocked: gate=checks; nope" {
		t.Errorf("error.message = %q, want the daemon's message verbatim", env.Error.Message)
	}
}

// TestEmitJSONFailureIsPassThroughWithoutJSON pins that the human path writes
// nothing at all — the envelope must never leak into non-json output.
func TestEmitJSONFailureIsPassThroughWithoutJSON(t *testing.T) {
	cmd := &cobra.Command{}
	var out strings.Builder
	cmd.SetOut(&out)

	orig := errors.New("boom")
	if got := emitJSONFailure(cmd, false, orig); !errors.Is(got, orig) {
		t.Fatalf("emitJSONFailure returned %v, want the original error", got)
	}
	if out.String() != "" {
		t.Fatalf("stdout = %q, want nothing written without --json", out.String())
	}
}
