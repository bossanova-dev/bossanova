package session

import (
	"bytes"
	"io"
	"os"
	"testing"

	daemonrpc "github.com/recurser/boss/internal/adapter/daemonrpc"
	"github.com/spf13/cobra"
)

func testGroup() *Group {
	return New(func(*cobra.Command) (Client, error) {
		path, err := daemonrpc.DefaultSocketPath()
		if err != nil {
			return nil, err
		}
		return daemonrpc.NewLocal(path), nil
	}, Options{})
}
func stubEnv(t *testing.T, values map[string]string) {
	t.Helper()
	for _, key := range []string{"BOSS_SESSION_ID", "BOSS_AGENT_SESSION_ID"} {
		t.Setenv(key, values[key])
	}
}
func ptrTo[T any](v T) *T { return &v }
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()

	oldStdout := os.Stdout
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	os.Stdout = writer
	var buf bytes.Buffer
	done := make(chan error, 1)
	go func() {
		_, err := io.Copy(&buf, reader)
		done <- err
	}()
	defer func() {
		os.Stdout = oldStdout
		_ = reader.Close()
	}()

	fn()

	if err := writer.Close(); err != nil {
		t.Fatalf("close stdout writer: %v", err)
	}
	if err := <-done; err != nil {
		t.Fatalf("copy stdout: %v", err)
	}
	return buf.String()
}

func sessionCmd() *cobra.Command { return testGroup().SessionCmd() }
func showCmd() *cobra.Command    { return testGroup().ShowCmd() }
func chatsCmd() *cobra.Command   { return testGroup().ChatsCmd() }
func newCmd() *cobra.Command     { return testGroup().NewCmd() }
func archiveCmd() *cobra.Command { return testGroup().ArchiveCmd() }
func prCmd() *cobra.Command      { return testGroup().PRCmd() }
