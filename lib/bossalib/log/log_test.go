package log

import (
	"testing"

	zlog "github.com/rs/zerolog/log"
)

func TestSetup(t *testing.T) {
	// Setup should not panic and should configure the global logger.
	Setup("test-service")

	// Verify the logger has the service field by checking it doesn't panic
	// when used. We can't easily inspect internal zerolog state, but we can
	// verify the global logger is usable.
	zlog.Info().Msg("test log after Setup")
}
