package newsession

import (
	"context"
	"testing"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

// Pointer presence belongs at the consumer boundary: a real daemon resolves
// an omitted agent before storing it, so its session cannot expose omission.
func TestNewSessionPortedAgentFlagRequest(t *testing.T) {
	for _, agent := range []string{"", "opencode"} {
		t.Run(agent, func(t *testing.T) {
			sc := &stubClient{repos: oneRepo(), created: &pb.Session{Id: "created"}}
			m := NewModel(sc, context.Background())
			if agent != "" {
				m.SetInitialAgent(agent)
			}
			m = sendMsg(t, m, ReposMsg{Repos: sc.repos})
			m.selectedType = sessionTypeNewPR
			m.fd = &formData{title: "agent override"}
			cmd := m.startCreating()
			if cmd == nil {
				t.Fatal("no create command")
			}
			cmd()
			if sc.createReq == nil {
				t.Fatal("no create request")
			}
			if agent == "" {
				if sc.createReq.AgentName != nil {
					t.Fatalf("agent = %q, want omitted", sc.createReq.GetAgentName())
				}
			} else if sc.createReq.AgentName == nil || sc.createReq.GetAgentName() != agent {
				t.Fatalf("agent = %v, want explicit %q", sc.createReq.AgentName, agent)
			}
		})
	}
}
