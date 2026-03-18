// Package log provides shared logging initialization for Bossanova services.
package log

import (
	"os"

	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
)

// Setup configures zerolog with human-friendly console output and a service tag.
func Setup(service string) {
	log.Logger = zerolog.New(zerolog.ConsoleWriter{Out: os.Stderr}).
		With().Timestamp().Str("service", service).Logger()
}
