package config

import (
	"encoding/json"
	"testing"
)

func TestRepairConfigMaxConcurrentRepairs(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		cfg  RepairConfig
		want int
	}{
		{name: "unset uses default 3", cfg: RepairConfig{}, want: 3},
		{name: "zero uses default 3", cfg: RepairConfig{MaxConcurrentRepairs: 0}, want: 3},
		{name: "negative uses default 3", cfg: RepairConfig{MaxConcurrentRepairs: -2}, want: 3},
		{name: "positive 1 honoured", cfg: RepairConfig{MaxConcurrentRepairs: 1}, want: 1},
		{name: "positive 5 honoured", cfg: RepairConfig{MaxConcurrentRepairs: 5}, want: 5},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := tt.cfg.MaxConcurrentRepairsOrDefault(); got != tt.want {
				t.Errorf("MaxConcurrentRepairsOrDefault() = %d, want %d", got, tt.want)
			}
		})
	}

	t.Run("json unmarshal and omitempty", func(t *testing.T) {
		t.Parallel()
		var cfg RepairConfig
		if err := json.Unmarshal([]byte(`{"max_concurrent_repairs":4}`), &cfg); err != nil {
			t.Fatalf("Unmarshal: %v", err)
		}
		if got := cfg.MaxConcurrentRepairsOrDefault(); got != 4 {
			t.Errorf("MaxConcurrentRepairsOrDefault() = %d, want 4", got)
		}

		out, err := json.Marshal(RepairConfig{})
		if err != nil {
			t.Fatalf("Marshal: %v", err)
		}
		var m map[string]any
		if err := json.Unmarshal(out, &m); err != nil {
			t.Fatalf("Unmarshal round-trip: %v", err)
		}
		if _, ok := m["max_concurrent_repairs"]; ok {
			t.Errorf("expected max_concurrent_repairs omitted when zero, got %s", out)
		}
	})
}
