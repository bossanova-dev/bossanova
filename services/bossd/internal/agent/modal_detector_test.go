package agent

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/rs/zerolog"

	bossanovav1 "github.com/recurser/bossalib/gen/bossanova/v1"
)

// modalFakeClient answers HasQuestionPrompt from the test's script and records
// what it was asked. It embeds fakeAgentClient so the rest of the (large)
// AgentRunnerClient surface stays in one place.
type modalFakeClient struct {
	*fakeAgentClient
	resp      *bossanovav1.HasQuestionPromptResponse
	err       error
	callCount int
	lastPane  []byte
}

func (f *modalFakeClient) HasQuestionPrompt(_ context.Context, req *bossanovav1.HasQuestionPromptRequest) (*bossanovav1.HasQuestionPromptResponse, error) {
	f.callCount++
	f.lastPane = req.GetPaneContent()
	if f.err != nil {
		return nil, f.err
	}
	return f.resp, nil
}

func newModalFakeClient(blocks bool, err error) *modalFakeClient {
	return &modalFakeClient{
		fakeAgentClient: &fakeAgentClient{},
		resp:            &bossanovav1.HasQuestionPromptResponse{BlocksInput: blocks},
		err:             err,
	}
}

// TestNewModalPaneCheckerReadsBlocksInput pins the predicate. has_prompt and
// blocks_input are different questions with opposite failure costs: an agent
// that asked a conversational "…?" with a live composer sets has_prompt, and
// gating delivery on that would refuse to answer the very question the agent
// just asked. Only blocks_input may refuse.
func TestNewModalPaneCheckerReadsBlocksInput(t *testing.T) {
	for _, tc := range []struct {
		name   string
		blocks bool
	}{
		{"modal pane blocks", true},
		{"live composer does not", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client := newModalFakeClient(tc.blocks, nil)
			check := NewModalPaneChecker(client, "codex", zerolog.Nop())
			if check == nil {
				t.Fatal("NewModalPaneChecker returned nil for a non-nil client")
			}
			got, err := check(context.Background(), []byte("› 1. Update now\n  2. Skip\n"))
			if err != nil {
				t.Fatalf("check: %v", err)
			}
			if got != tc.blocks {
				t.Errorf("blocks = %v, want %v", got, tc.blocks)
			}
			if client.callCount != 1 {
				t.Errorf("HasQuestionPrompt called %d times, want 1", client.callCount)
			}
			if string(client.lastPane) != "› 1. Update now\n  2. Skip\n" {
				t.Errorf("plugin saw pane %q; the capture must reach the grammar unaltered", client.lastPane)
			}
		})
	}
}

// TestNewModalPaneCheckerNilClientDisablesCheck pins the fail-open shape. The
// gate compares the detector against nil, so "no plugin loaded" has to produce
// a nil func — not a func that returns an error, which would look identical to
// a wedged plugin in the logs and cost a call per probe.
func TestNewModalPaneCheckerNilClientDisablesCheck(t *testing.T) {
	if check := NewModalPaneChecker(nil, "codex", zerolog.Nop()); check != nil {
		t.Fatal("NewModalPaneChecker built a checker over a nil client; " +
			"the readiness gate would call it instead of skipping the check")
	}
}

// TestNewModalPaneCheckerReportsFailureOnce covers the degraded gate. An
// unreachable plugin must not become a new reason delivery fails, so the error
// is returned (the gate reads it as "not a modal") — but silently restoring the
// pre-BOS-600 behaviour is how the next Enter into a menu becomes an
// unexplained regression. Exactly one warning per checker: enough to find, not
// enough to drown a poll loop.
func TestNewModalPaneCheckerReportsFailureOnce(t *testing.T) {
	var logs bytes.Buffer
	logger := zerolog.New(&logs)
	pluginErr := errors.New("plugin unreachable")
	client := newModalFakeClient(false, pluginErr)

	check := NewModalPaneChecker(client, "codex", logger)
	for i := 0; i < 3; i++ {
		blocks, err := check(context.Background(), []byte("pane"))
		if !errors.Is(err, pluginErr) {
			t.Fatalf("probe %d: err = %v, want the plugin error surfaced", i, err)
		}
		if blocks {
			t.Fatalf("probe %d: a failed check must not report a modal", i)
		}
	}

	const warning = "modal check unavailable"
	if n := strings.Count(logs.String(), warning); n != 1 {
		t.Errorf("logged %q %d times over 3 failing probes, want exactly 1:\n%s", warning, n, logs.String())
	}
	if !strings.Contains(logs.String(), `"agent":"codex"`) {
		t.Errorf("degraded-gate warning does not name the agent:\n%s", logs.String())
	}
}

// TestModalPaneCheckerIsAssignableToABareFunc is a compile-time guard on the
// seam. ModalPaneChecker is an ALIAS so the result drops into
// tmux.ModalDetector untouched; turning it into a defined type would still
// compile everywhere inside this package and break only at the call sites in
// internal/tmux's callers. This package cannot import internal/tmux to assert
// against the real type — that is the dependency the extraction exists to avoid
// — so it asserts against the identical bare signature instead.
func TestModalPaneCheckerIsAssignableToABareFunc(t *testing.T) {
	var bare func(ctx context.Context, pane []byte) (bool, error)
	bare = NewModalPaneChecker(newModalFakeClient(true, nil), "codex", zerolog.Nop())
	if bare == nil {
		t.Fatal("checker was nil")
	}
}
