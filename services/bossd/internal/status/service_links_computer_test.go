package status

import (
	"testing"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

func TestDisplayComputerServiceLinks(t *testing.T) {
	entry := &DisplayEntry{ServiceLinks: []ServiceLink{{Name: "api", URL: "https://api.example.test"}}}
	p := &pb.Session{}
	hydrateServiceLinks(p, entry)
	if len(p.GetServiceLinks()) != 1 || p.ServiceLinks[0].Name != "api" || p.ServiceLinks[0].Url != "https://api.example.test" {
		t.Fatalf("display proto links = %v", p.ServiceLinks)
	}
	p.ServiceLinks[0].Name = "changed"
	if entry.ServiceLinks[0].Name != "api" {
		t.Fatal("display proto aliases tracker")
	}
	hydrateServiceLinks(p, &DisplayEntry{})
	if len(p.ServiceLinks) != 0 {
		t.Fatal("clear retained old links")
	}
}
