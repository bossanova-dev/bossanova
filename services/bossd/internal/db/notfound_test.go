package db

import (
	"database/sql"
	"errors"
	"fmt"
	"testing"

	"github.com/recurser/bossd/internal/domain"
)

func TestNotFound(t *testing.T) {
	t.Run("wrapped no rows", func(t *testing.T) {
		original := fmt.Errorf("query record: %w", sql.ErrNoRows)
		got := notFound(original)
		if !errors.Is(got, domain.ErrNotFound) {
			t.Errorf("notFound(%v) = %v, want domain.ErrNotFound", original, got)
		}
		if errors.Is(got, sql.ErrNoRows) {
			t.Errorf("notFound(%v) = %v, must not expose sql.ErrNoRows", original, got)
		}
		if errors.Is(got, original) {
			t.Errorf("notFound(%v) must not expose original SQL error", original)
		}
	})
	t.Run("nil", func(t *testing.T) {
		if got := notFound(nil); got != nil {
			t.Fatalf("notFound(nil) = %v, want nil", got)
		}
	})
	t.Run("unrelated", func(t *testing.T) {
		original := errors.New("connection unavailable")
		if got := notFound(original); got != original {
			t.Fatalf("notFound(%v) = %v, want unchanged error", original, got)
		}
	})
}
