package settings

import (
	"bytes"
	"context"
	"io"
	"os"
	"testing"

	"github.com/recurser/bossalib/kit/config"
	"github.com/recurser/bossalib/kit/telemetry"
)

func newTestGroup() *Group {
	return New(Options{
		TelemetryEnabled: func() bool { s, err := config.Load(); return err == nil && s.EventTracingEnabled },
		DistinctID:       func() string { return "test-user" },
	})
}
func enableCommandTelemetryForTest(t *testing.T) {
	t.Helper()
	_, cleanup := setupTestConfigEnv(t)
	t.Cleanup(cleanup)
	s := config.DefaultSettings()
	s.EventTracingEnabled = true
	if err := config.Save(s); err != nil {
		t.Fatal(err)
	}
}

type fakeTelemetry struct {
	events      []telemetry.Event
	distinctIDs []string
	props       []map[string]any
	identifies  []struct {
		distinctID string
		props      map[string]any
	}
	aliases [][2]string
}

func (f *fakeTelemetry) Capture(_ context.Context, event telemetry.Event, distinctID string, props map[string]any) {
	f.events = append(f.events, event)
	f.distinctIDs = append(f.distinctIDs, distinctID)
	f.props = append(f.props, props)
}

func (f *fakeTelemetry) Identify(_ context.Context, distinctID string, props map[string]any) {
	f.identifies = append(f.identifies, struct {
		distinctID string
		props      map[string]any
	}{distinctID: distinctID, props: props})
}

func (f *fakeTelemetry) Alias(_ context.Context, alias, distinctID string) {
	f.aliases = append(f.aliases, [2]string{alias, distinctID})
}

func (f *fakeTelemetry) Close() {}

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
