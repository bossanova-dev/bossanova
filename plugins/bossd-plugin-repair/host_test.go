package main

import (
	"fmt"
	"math"
	"os"
	"testing"
	"time"

	"github.com/recurser/bossalib/config"
)

func TestStartChatRunBudgetFromEnv(t *testing.T) {
	key := "BOSS_PLUGIN_" + config.SessionStartReadyDeadlinePluginKey
	defaultBudget := config.StartChatRunBudgetFor(config.DefaultSessionStartReadyDeadline)
	tests := []struct {
		name  string
		value *string
		want  time.Duration
	}{
		{name: "unset", want: defaultBudget},
		{name: "default readiness", value: strPtr("45"), want: 90 * time.Second},
		{name: "configured readiness", value: strPtr("300"), want: 600 * time.Second},
		{name: "empty", value: strPtr(""), want: defaultBudget},
		{name: "malformed", value: strPtr("abc"), want: defaultBudget},
		{name: "zero", value: strPtr("0"), want: defaultBudget},
		{name: "negative", value: strPtr("-5"), want: defaultBudget},
		{name: "whitespace", value: strPtr(" 300 "), want: defaultBudget},
		{name: "overflowing duration seconds saturates", value: strPtr(fmt.Sprintf("%d", math.MaxInt64/int64(time.Second)+1)), want: time.Duration(math.MaxInt64)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.value == nil {
				prev, hadPrev := os.LookupEnv(key)
				if err := os.Unsetenv(key); err != nil {
					t.Fatalf("unset %s: %v", key, err)
				}
				t.Cleanup(func() {
					if hadPrev {
						_ = os.Setenv(key, prev)
					} else {
						_ = os.Unsetenv(key)
					}
				})
			} else {
				t.Setenv(key, *tt.value)
			}

			if got := startChatRunBudgetFromEnv(); got != tt.want {
				t.Fatalf("startChatRunBudgetFromEnv() = %v, want %v", got, tt.want)
			}
		})
	}
}

func strPtr(s string) *string {
	return &s
}
