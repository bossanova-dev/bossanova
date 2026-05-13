package telemetry

import (
	"github.com/recurser/bossalib/config"
	libtelemetry "github.com/recurser/bossalib/telemetry"
)

func ConfigFromSettings(s config.Settings) libtelemetry.Config {
	return libtelemetry.FromSettings(s, "bossd")
}
