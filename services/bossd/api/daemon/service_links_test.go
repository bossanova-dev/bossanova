package server

import (
	"testing"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossd/internal/status"
)

func TestHydrateServiceLinks(t *testing.T) {
	entry := &status.DisplayEntry{ServiceLinks: []status.ServiceLink{{Name: "api", URL: "https://api.example.test"}}}
	p := &pb.Session{}
	HydrateDisplayEntry(p, entry)
	if len(p.GetServiceLinks()) != 1 || p.ServiceLinks[0].Name != "api" || p.ServiceLinks[0].Url != "https://api.example.test" {
		t.Fatalf("service links = %v", p.GetServiceLinks())
	}
	p.ServiceLinks[0].Name = "changed"
	if entry.ServiceLinks[0].Name != "api" {
		t.Fatal("proto aliased entry")
	}
	HydrateDisplayEntry(p, &status.DisplayEntry{})
	if len(p.ServiceLinks) != 0 {
		t.Fatal("clear retained old links")
	}
}
