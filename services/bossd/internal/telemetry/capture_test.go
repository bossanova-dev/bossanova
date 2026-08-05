package telemetry

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/recurser/bossalib/config"
	libtelemetry "github.com/recurser/bossalib/telemetry"
)

type recordingClient struct {
	calls      int
	distinctID string
	properties map[string]any
}

func (r *recordingClient) Capture(_ context.Context, _ libtelemetry.Event, distinctID string, properties map[string]any) {
	r.calls++
	r.distinctID = distinctID
	r.properties = properties
}

func TestCaptureUsesDaemonIdentityAndSource(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("BOSS_SETTINGS_PATH", filepath.Join(tmp, "settings.json"))
	settings := config.DefaultSettings()
	settings.EventTracingEnabled = true
	if err := config.Save(settings); err != nil {
		t.Fatal(err)
	}
	r := &recordingClient{}
	Capture(context.Background(), r, libtelemetry.EventDaemonStarted, nil)
	if r.calls != 1 {
		t.Fatalf("captures = %d, want 1", r.calls)
	}
	hostname, err := os.Hostname()
	if err != nil {
		hostname = ""
	}
	if want := libtelemetry.DaemonDistinctID(hostname); r.distinctID != want {
		t.Fatalf("distinct id = %q, want %q", r.distinctID, want)
	}
	if r.properties["source"] != "daemon" {
		t.Fatalf("properties = %#v", r.properties)
	}
}
func (r *recordingClient) Identify(context.Context, string, map[string]any) {}
func (r *recordingClient) Alias(context.Context, string, string)            {}
func (r *recordingClient) Close()                                           {}

type blockingCloseClient struct {
	recordingClient
	started chan struct{}
	release chan struct{}
	closed  chan struct{}
}

func (c *blockingCloseClient) Close() {
	close(c.started)
	<-c.release
	close(c.closed)
}

func TestCaptureNilClient(t *testing.T) {
	Capture(context.Background(), nil, libtelemetry.EventDaemonStarted, nil)
}

func TestCaptureRespectsDisabledGate(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("BOSS_SETTINGS_PATH", filepath.Join(tmp, "settings.json"))
	r := &recordingClient{}
	Capture(context.Background(), r, libtelemetry.EventDaemonStarted, nil)
	if r.calls != 0 {
		t.Fatalf("captures = %d, want 0", r.calls)
	}
}

func TestCaptureRefreshesClientAfterDirectSettingsSave(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("BOSS_SETTINGS_PATH", filepath.Join(tmp, "settings.json"))

	c := NewClient(config.DefaultSettings())
	settings := config.DefaultSettings()
	settings.EventTracingEnabled = true
	settings.PostHogProjectToken = "phc-direct-save"
	settings.PostHogHost = "https://telemetry.example"
	if err := config.Save(settings); err != nil {
		t.Fatal(err)
	}

	Capture(context.Background(), c, libtelemetry.EventDaemonStarted, nil)
	if want := ConfigFromSettings(settings); c.config != want {
		t.Fatalf("client config = %#v, want %#v", c.config, want)
	}
}

func TestRefreshDoesNotBlockOnPreviousClientClose(t *testing.T) {
	settings := config.DefaultSettings()
	settings.EventTracingEnabled = true
	settings.PostHogProjectToken = "phc-refresh"
	settings.PostHogHost = "https://telemetry.example"
	previous := &blockingCloseClient{
		started: make(chan struct{}),
		release: make(chan struct{}),
		closed:  make(chan struct{}),
	}
	c := &Client{config: ConfigFromSettings(config.DefaultSettings()), current: previous}
	done := make(chan struct{})
	go func() {
		c.Refresh(settings)
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(100 * time.Millisecond):
		close(previous.release)
		<-done
		t.Fatal("Refresh blocked waiting for the previous client to close")
	}

	close(previous.release)
	select {
	case <-previous.closed:
	case <-time.After(time.Second):
		t.Fatal("previous client was not closed")
	}
	c.Close()
}

func TestCloseWaitsForRetiredClient(t *testing.T) {
	settings := config.DefaultSettings()
	settings.EventTracingEnabled = true
	settings.PostHogProjectToken = "phc-refresh"
	settings.PostHogHost = "https://telemetry.example"
	previous := &blockingCloseClient{
		started: make(chan struct{}),
		release: make(chan struct{}),
		closed:  make(chan struct{}),
	}
	c := &Client{config: ConfigFromSettings(config.DefaultSettings()), current: previous}
	c.Refresh(settings)
	<-previous.started

	done := make(chan struct{})
	go func() {
		c.Close()
		close(done)
	}()

	select {
	case <-done:
		t.Fatal("Close returned before a retired client finished closing")
	case <-time.After(100 * time.Millisecond):
	}

	close(previous.release)
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Close did not wait for the retired client")
	}
}
