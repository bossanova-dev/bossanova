package server

import (
	"testing"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/models"
	"github.com/recurser/bossd/internal/status"
)

func TestHydrateServiceLinks(t *testing.T) {
	entry := &status.DisplayEntry{
		ServeRunning: true,
		ServeError:   "port busy",
		ServiceLinks: []status.ServiceLink{{Name: "api", URL: "https://api.example.test"}},
	}
	p := &pb.Session{}
	HydrateDisplayEntry(p, entry)
	if !p.GetIsServeRunning() || p.GetServeError() != "port busy" {
		t.Fatalf("serve state = (%v, %q), want (true, port busy)", p.GetIsServeRunning(), p.GetServeError())
	}
	if len(p.GetServiceLinks()) != 1 || p.ServiceLinks[0].Name != "api" || p.ServiceLinks[0].Url != "https://api.example.test" {
		t.Fatalf("service links = %v", p.GetServiceLinks())
	}
	p.ServiceLinks[0].Name = "changed"
	if entry.ServiceLinks[0].Name != "api" {
		t.Fatal("proto aliased entry")
	}
	HydrateDisplayEntry(p, &status.DisplayEntry{})
	if len(p.ServiceLinks) != 0 || p.GetIsServeRunning() || p.GetServeError() != "" {
		t.Fatal("clear retained old serve state")
	}
}

func TestHydrateRepoServe(t *testing.T) {
	script := "pnpm dev"
	empty := ""
	p := &pb.Session{}
	HydrateRepoServe(p, &models.Repo{ServeScript: &script})
	if !p.GetHasServeCommand() {
		t.Fatal("expected HasServeCommand true for non-empty serve script")
	}
	HydrateRepoServe(p, &models.Repo{ServeScript: &empty})
	if p.GetHasServeCommand() {
		t.Fatal("expected HasServeCommand false for empty serve script")
	}
	HydrateRepoServe(p, &models.Repo{})
	if p.GetHasServeCommand() {
		t.Fatal("expected HasServeCommand false for nil serve script")
	}
}
