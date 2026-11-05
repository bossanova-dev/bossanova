package home

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"charm.land/lipgloss/v2"
	"github.com/recurser/boss/api/tui/ui"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

func credentialReadinessFixture(state string) *pb.CredentialDeviceReadiness {
	return &pb.CredentialDeviceReadiness{State: state, RemediationSteps: []*pb.CredentialDeviceRemediationStep{
		{IsThisMachine: true, Command: "boss account devices request"},
		{MachineDaemonId: "0123456789abcdef", Command: "boss account devices approve fedcba9876543210 --fingerprint V6MW-U7ZO-PU4D-SMFF"},
		{Command: "boss account devices status"},
	}}
}

func TestCredentialDeviceLineStates_Sessions_AC224901(t *testing.T) {
	for _, state := range []string{"enrollment_expired", "enrollment_pending", "unenrolled", "revoked", "bootstrap_unpinned", "ready", "unknown", "unavailable", ""} {
		t.Run(state, func(t *testing.T) {
			h := Model{Width: 200, CredentialDeviceReadiness: credentialReadinessFixture(state)}
			got := h.CredentialDeviceLine()
			bad := state != "ready" && state != "unknown" && state != "unavailable" && state != ""
			if !bad {
				if got != "" {
					t.Fatalf("hidden state rendered %q", got)
				}
				return
			}
			for _, want := range []string{"Credential sharing is off", strings.ReplaceAll(state, "_", " "), "Synced Codex accounts are paused.", "On this machine:", "On 0123456789abcdef:", "On an enrolled machine:"} {
				if !strings.Contains(got, want) {
					t.Errorf("missing %q in %q", want, got)
				}
			}
			for _, step := range h.CredentialDeviceReadiness.RemediationSteps {
				for _, token := range strings.Fields(step.Command) {
					if !strings.Contains(got, token) {
						t.Errorf("missing command token %q", token)
					}
				}
			}
		})
	}
	if got := (Model{}).CredentialDeviceLine(); got != "" {
		t.Fatalf("no data rendered %q", got)
	}
}

func TestCredentialDeviceWarningWidthsAndHeight_Sessions_AC224901(t *testing.T) {
	for width := 60; width <= 200; width++ {
		for _, populated := range []bool{false, true} {
			h := reloginModel(nil)
			h.Width = width
			h.Height = 48
			h.CredentialDeviceReadiness = credentialReadinessFixture("enrollment_expired")
			if populated {
				for i := 0; i < 100; i++ {
					h.Sessions = append(h.Sessions, &pb.Session{Id: "session", Title: "Active work"})
				}
			}
			h.BuildTableRows()
			content := h.View().Content
			for _, step := range h.CredentialDeviceReadiness.RemediationSteps {
				for _, token := range strings.Fields(step.Command) {
					if !strings.Contains(content, token) {
						t.Fatalf("width %d populated %v split command token %q", width, populated, token)
					}
				}
			}
			if populated {
				if got := ui.BannerOverhead + lipgloss.Height(content); got > h.Height {
					t.Fatalf("frame height %d exceeds %d at width %d", got, h.Height, width)
				}
				before := h.SessionTableFooterLineCount()
				rows := lipgloss.Height(h.CredentialDeviceLine()) + 1
				h.CredentialDeviceReadiness = nil
				if got := before - h.SessionTableFooterLineCount(); got != rows {
					t.Fatalf("warning overhead %d, want %d", got, rows)
				}
			}
		}
	}
}

type credentialReadinessClient struct {
	Client
	response *pb.GetAgentCredentialDeviceStatusResponse
	err      error
	calls    int
}

func (c *credentialReadinessClient) GetAgentCredentialDeviceStatus(context.Context) (*pb.GetAgentCredentialDeviceStatusResponse, error) {
	c.calls++
	return c.response, c.err
}

func TestCredentialDevicePollingAndErrors_Sessions_AC224901(t *testing.T) {
	now := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	c := &credentialReadinessClient{response: &pb.GetAgentCredentialDeviceStatusResponse{Readiness: credentialReadinessFixture("enrollment_pending")}}
	h := Model{Client: c, Ctx: context.Background(), Now: func() time.Time { return now }}
	cmd := h.tickCredentialDeviceReadiness()
	if cmd == nil {
		t.Fatal("initial poll absent")
	}
	model, _ := h.Update(cmd())
	h = model.(Model)
	if h.CredentialDeviceLine() == "" {
		t.Fatal("poll did not show warning")
	}
	if cmd := h.tickCredentialDeviceReadiness(); cmd != nil {
		t.Fatal("polled twice inside a minute")
	}
	now = now.Add(time.Minute)
	c.err = errors.New("unimplemented")
	cmd = h.tickCredentialDeviceReadiness()
	if cmd == nil {
		t.Fatal("minute refresh absent")
	}
	model, _ = h.Update(cmd())
	h = model.(Model)
	if got := h.CredentialDeviceLine(); got != "" {
		t.Fatalf("RPC error left warning %q", got)
	}
	if c.calls != 2 {
		t.Fatalf("RPC calls %d, want 2", c.calls)
	}
}

func TestCredentialDeviceReadinessSurvivesHomeRebuild(t *testing.T) {
	now := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	previous := Model{Client: &credentialReadinessClient{}, Ctx: context.Background(), Now: func() time.Time { return now }, CredentialDeviceReadiness: credentialReadinessFixture("revoked")}
	previous.tickCredentialDeviceReadiness()
	rebuilt := Model{Client: previous.Client, Now: previous.Now, Width: 100}
	rebuilt.RetainCredentialDeviceReadiness(previous)
	if got := rebuilt.CredentialDeviceLine(); !strings.Contains(got, "revoked") {
		t.Fatalf("rebuilt Home lost warning: %q", got)
	}
	if cmd := rebuilt.tickCredentialDeviceReadiness(); cmd != nil {
		t.Fatal("Home rebuild polled inside a minute")
	}
	now = now.Add(time.Minute)
	if cmd := rebuilt.tickCredentialDeviceReadiness(); cmd == nil {
		t.Fatal("rebuilt Home did not resume polling")
	}
}
