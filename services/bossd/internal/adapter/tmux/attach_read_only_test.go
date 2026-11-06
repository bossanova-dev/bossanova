package tmux

import (
	"context"
	"os/exec"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestTerminalAttach_ReadOnlyFlagsAndDimensions(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), integrationTestTimeout)
	defer cancel()
	var gotArgs []string
	factory := func(ctx context.Context, name string, args ...string) *exec.Cmd {
		if name != "tmux" {
			t.Fatalf("command=%q", name)
		}
		gotArgs = append([]string(nil), args...)
		return exec.CommandContext(ctx, "sh", "-c", "stty size; cat")
	}
	// Any SetAttachOptions call would execute real tmux against a missing session
	// and fail. A viewer must not change the session's persistent options.
	att, err := NewTerminalAttach(ctx, AttachConfig{
		AttachID: "viewer", SessionName: "viewed", ReadOnly: true, Cols: 220, Rows: 90,
		TmuxClient: NewClient(), CommandFactory: factory,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = att.Close() }()
	if want := []string{"-u", "attach", "-t", "viewed", "-f", "read-only,ignore-size"}; !reflect.DeepEqual(gotArgs, want) {
		t.Fatalf("args=%q want=%q", gotArgs, want)
	}
	var output strings.Builder
	for !strings.Contains(output.String(), "24 80") {
		select {
		case chunk := <-att.Output():
			output.Write(chunk.GetData())
		case <-time.After(integrationTestTimeout):
			t.Fatalf("requested dimensions affected viewer PTY: %q", output.String())
		}
	}
}
