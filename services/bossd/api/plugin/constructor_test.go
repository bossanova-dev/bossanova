package plugin

import (
	"github.com/recurser/bossalib/kit/vcs"
	"github.com/recurser/bossd/internal/adapter/dotenv"
)

func newTestHostServiceServer(provider vcs.Provider) *HostServiceServer {
	return NewHostServiceServer(Config{Provider: provider, EnvOverlay: dotenv.OverlayWithRepo})
}
