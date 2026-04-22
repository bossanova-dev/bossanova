package sqlutil

import (
	"testing"
)

func TestNewID(t *testing.T) {
	id, err := NewID()
	if err != nil {
		t.Fatalf("NewID() err = %v, want nil", err)
	}

	// Should be 16 hex characters (8 bytes).
	if len(id) != 16 {
		t.Errorf("NewID() length = %d, want 16", len(id))
	}

	// Should be valid hex.
	for _, c := range id {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			t.Errorf("NewID() contains non-hex char %q in %q", c, id)
		}
	}
}

func TestNewID_Unique(t *testing.T) {
	seen := make(map[string]bool)
	for range 1000 {
		id, err := NewID()
		if err != nil {
			t.Fatalf("NewID() err = %v, want nil", err)
		}
		if seen[id] {
			t.Fatalf("NewID() produced duplicate: %s", id)
		}
		seen[id] = true
	}
}
