package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func TestProactive5hThreshold(t *testing.T) {
	for _, tc := range []struct {
		percent int
		want    float64
	}{
		{0, 0.90}, {85, 0.85}, {150, 0.90}, {-1, 0.90}, {1, 0.01}, {100, 1},
	} {
		c := ManagedAgentAccountsConfig{Proactive5hThresholdPercent: tc.percent}
		if got := c.Proactive5hThreshold(); got != tc.want {
			t.Errorf("threshold for %d = %v, want %v", tc.percent, got, tc.want)
		}
	}
}

func TestLoadFromWarnsOnceForInvalidProactive5hThreshold(t *testing.T) {
	previousWarning := proactive5hThresholdWarning
	proactive5hThresholdWarning = new(sync.Once)
	t.Cleanup(func() { proactive5hThresholdWarning = previousWarning })
	previousLogger := slog.Default()
	var logs bytes.Buffer
	slog.SetDefault(slog.New(slog.NewJSONHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(previousLogger) })

	path := filepath.Join(t.TempDir(), "settings.json")
	for _, percent := range []int{0, 85, 150, -1} {
		data, err := json.Marshal(Settings{ManagedAccounts: ManagedAgentAccountsConfig{
			Proactive5hThresholdPercent: percent,
		}})
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, data, 0o600); err != nil {
			t.Fatal(err)
		}
		loaded, err := LoadFrom(path)
		if err != nil {
			t.Fatal(err)
		}
		if loaded.ManagedAccounts.Proactive5hThreshold() != (ManagedAgentAccountsConfig{Proactive5hThresholdPercent: percent}).Proactive5hThreshold() {
			t.Fatal("loaded threshold differs from configured threshold")
		}
		if percent == 0 || percent == 85 {
			if logs.Len() != 0 {
				t.Fatal("valid threshold logged a warning")
			}
		}
	}
	decoder := json.NewDecoder(&logs)
	var record struct{ Level string }
	if err := decoder.Decode(&record); err != nil {
		t.Fatal(err)
	}
	if record.Level != "WARN" {
		t.Errorf("log level = %s, want WARN", record.Level)
	}
	if err := decoder.Decode(&record); !errors.Is(err, io.EOF) {
		t.Errorf("expected exactly one warning, next record: %v", err)
	}
}
