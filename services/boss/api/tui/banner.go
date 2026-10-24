package tui

import (
	"charm.land/bubbles/v2/spinner"
	"github.com/recurser/boss/api/tui/ui"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

type bannerOpts struct {
	session      *pb.Session
	repo         *pb.Repo
	spinner      spinner.Model
	archiving    bool
	line1, line2 string
	width        int
}

func renderBanner(active ui.View, opts bannerOpts) string {
	return ui.RenderBanner(active, ui.BannerOpts{Session: opts.session, Repo: opts.repo, Spinner: opts.spinner, Archiving: opts.archiving, Line1: opts.line1, Line2: opts.line2, Width: opts.width})
}
