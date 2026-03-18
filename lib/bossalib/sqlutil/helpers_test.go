package sqlutil

import (
	"testing"
	"time"
)

func TestTimeNow(t *testing.T) {
	before := time.Now().UTC()
	got := TimeNow()
	after := time.Now().UTC()

	parsed := ParseTime(got)
	if parsed.IsZero() {
		t.Fatalf("ParseTime(%q) returned zero time", got)
	}
	if parsed.Before(before.Truncate(time.Millisecond)) {
		t.Errorf("TimeNow() = %s, want >= %s", parsed, before)
	}
	if parsed.After(after.Add(time.Millisecond)) {
		t.Errorf("TimeNow() = %s, want <= %s", parsed, after)
	}
}

func TestParseTime(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  time.Time
	}{
		{
			name:  "ISO8601 millis",
			input: "2024-06-15T10:30:45.123Z",
			want:  time.Date(2024, 6, 15, 10, 30, 45, 123_000_000, time.UTC),
		},
		{
			name:  "RFC3339Nano fallback",
			input: "2024-06-15T10:30:45.123456789Z",
			want:  time.Date(2024, 6, 15, 10, 30, 45, 123_456_789, time.UTC),
		},
		{
			name:  "empty string",
			input: "",
			want:  time.Time{},
		},
		{
			name:  "invalid string",
			input: "not-a-date",
			want:  time.Time{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ParseTime(tt.input)
			if !got.Equal(tt.want) {
				t.Errorf("ParseTime(%q) = %v, want %v", tt.input, got, tt.want)
			}
		})
	}
}

func TestParseOptionalTime(t *testing.T) {
	t.Run("nil input", func(t *testing.T) {
		got := ParseOptionalTime(nil)
		if got != nil {
			t.Errorf("ParseOptionalTime(nil) = %v, want nil", got)
		}
	})

	t.Run("valid time", func(t *testing.T) {
		s := "2024-06-15T10:30:45.000Z"
		got := ParseOptionalTime(&s)
		if got == nil {
			t.Fatal("ParseOptionalTime(&valid) = nil, want non-nil")
		}
		want := time.Date(2024, 6, 15, 10, 30, 45, 0, time.UTC)
		if !got.Equal(want) {
			t.Errorf("ParseOptionalTime(%q) = %v, want %v", s, *got, want)
		}
	})

	t.Run("empty string", func(t *testing.T) {
		s := ""
		got := ParseOptionalTime(&s)
		if got != nil {
			t.Errorf("ParseOptionalTime(&empty) = %v, want nil", got)
		}
	})

	t.Run("invalid string", func(t *testing.T) {
		s := "garbage"
		got := ParseOptionalTime(&s)
		if got != nil {
			t.Errorf("ParseOptionalTime(&garbage) = %v, want nil", got)
		}
	})
}

func TestBoolToInt(t *testing.T) {
	if BoolToInt(true) != 1 {
		t.Error("BoolToInt(true) != 1")
	}
	if BoolToInt(false) != 0 {
		t.Error("BoolToInt(false) != 0")
	}
}
