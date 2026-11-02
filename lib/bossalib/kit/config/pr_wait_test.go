package config

import (
	"testing"
	"time"
)

func TestPRWaitTimeout(t *testing.T) {
	for _, tc := range []struct {
		seconds int32
		want    time.Duration
	}{
		{0, PRWaitDefault},
		{-5, PRWaitDefault},
		{90, 90 * time.Second},
		{int32(PRWaitCeiling / time.Second), PRWaitCeiling},
		{int32(PRWaitCeiling/time.Second) + 1, PRWaitCeiling},
		{1 << 30, PRWaitCeiling},
	} {
		if got := PRWaitTimeout(tc.seconds); got != tc.want {
			t.Fatalf("PRWaitTimeout(%d) = %v, want %v", tc.seconds, got, tc.want)
		}
	}
}
