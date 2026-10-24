package ui

import (
	"fmt"
	"time"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

func AgentAccountRowLabel(a *pb.AgentAccount) string {
	if a.GetLabel() != "" {
		return a.GetLabel()
	}
	return a.GetId()
}
func AgentAccountHealthLabel(a *pb.AgentAccount) string {
	if a == nil || a.GetHealth() == "" {
		return "ok"
	}
	return a.GetHealth()
}
func CompactFutureDuration(d time.Duration) string {
	if d < time.Minute {
		return "<1m"
	}
	if d < time.Hour {
		return fmt.Sprintf("%dm", int(d.Minutes()))
	}
	if d < 24*time.Hour {
		return fmt.Sprintf("%dh", int(d.Hours()))
	}
	return fmt.Sprintf("%dd", int(d.Hours()/24))
}
