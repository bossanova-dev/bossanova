package home

import (
	"charm.land/bubbles/v2/spinner"
	"github.com/recurser/boss/api/tui/ui"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

type BannerOpts struct {
	Session      *pb.Session
	repo         *pb.Repo
	Spinner      spinner.Model
	archiving    bool
	line1, line2 string
	Width        int
}

func RenderBanner(active ui.View, opts BannerOpts) string {
	return ui.RenderBanner(active, ui.BannerOpts{Session: opts.Session, Repo: opts.repo, Spinner: opts.Spinner, Archiving: opts.archiving, Line1: opts.line1, Line2: opts.line2, Width: opts.Width})
}
