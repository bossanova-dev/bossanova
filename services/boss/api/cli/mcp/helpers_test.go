package mcp

import (
	"bytes"
	"io"
	"io/fs"
	"os"
	"strings"
	"testing"

	"github.com/recurser/bossalib/contract/plugin/skillinstall"
	"github.com/recurser/bossalib/contract/plugin/skillpayload"
)

func newTestGroup() *Group {
	return New(Config{
		SkillPayload: func() (fs.FS, skillinstall.PayloadRecord, error) {
			return skillpayload.SkillsFS, skillinstall.EmbeddedPayloadRecord("", "", skillinstall.WriterExplicit), nil
		},
		ShellQuote:          shellQuote,
		InstallSettingsLine: func() string { return "" },
	})
}
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

func shellQuote(s string) string {
	if s == "" {
		return "''"
	}
	const safeExtra = "_@%+=:,./-"
	for _, r := range s {
		switch {
		case r >= 'A' && r <= 'Z', r >= 'a' && r <= 'z', r >= '0' && r <= '9':
		case strings.ContainsRune(safeExtra, r):
		default:
			return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
		}
	}
	return s
}
