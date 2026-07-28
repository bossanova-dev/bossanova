package sessionports

import (
	"testing"

	"go.uber.org/goleak"
)

// TestMain asserts the package leaks no goroutines: the batched scan is
// sequential and every subprocess runs under a context-bounded exec, so a
// completed or cancelled scan must leave nothing running.
func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}
