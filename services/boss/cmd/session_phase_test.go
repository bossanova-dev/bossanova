package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/recurser/boss/internal/client"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/spf13/cobra"
)

type phaseClient struct {
	client.BossClient
	calls                    int
	sessionID, chatID, phase string
	err                      error
}

func (c *phaseClient) SetChatPhase(_ context.Context, sessionID, chatID, phase string) error {
	c.calls++
	c.sessionID, c.chatID, c.phase = sessionID, chatID, phase
	return c.err
}
func TestSessionPhaseCommand(t *testing.T) {
	for _, tt := range []struct {
		name                                                  string
		args                                                  []string
		env                                                   map[string]string
		wantSession, wantChat, wantPhase, wantOutput, wantErr string
		daemonErr                                             error
	}{
		{name: "environment defaults", args: []string{"reviewing"}, env: map[string]string{"BOSS_SESSION_ID": "session-env", "BOSS_AGENT_SESSION_ID": "chat-env"}, wantSession: "session-env", wantChat: "chat-env", wantPhase: "reviewing", wantOutput: "phase set: reviewing\n"},
		{name: "flags override", args: []string{"building", "--session", "session-flag", "--chat", "chat-flag"}, env: map[string]string{"BOSS_SESSION_ID": "session-env", "BOSS_AGENT_SESSION_ID": "chat-env"}, wantSession: "session-flag", wantChat: "chat-flag", wantPhase: "building", wantOutput: "phase set: building\n"},
		{name: "clear", args: []string{"--clear", "--chat", "chat"}, wantChat: "chat", wantOutput: "phase cleared\n"},
		{name: "free text normalization", args: []string{" fixing ci ", "--chat", "chat"}, wantChat: "chat", wantPhase: "fixing ci", wantOutput: "phase set: fixing ci\n"},
		{name: "both", args: []string{"reviewing", "--clear"}, wantErr: "exactly one"},
		{name: "neither", wantErr: "exactly one"},
		{name: "extra name", args: []string{"reviewing", "building"}, wantErr: "at most 1"},
		{name: "missing chat", args: []string{"reviewing"}, wantErr: "--chat or BOSS_AGENT_SESSION_ID"},
		{name: "invalid", args: []string{"review\ning", "--chat", "chat"}, wantErr: "printable"},
		{name: "daemon error", args: []string{"reviewing", "--chat", "chat"}, wantChat: "chat", wantPhase: "reviewing", daemonErr: errors.New("daemon unavailable"), wantErr: "daemon unavailable"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			stubEnv(t, tt.env)
			c := &phaseClient{err: tt.daemonErr}
			factories := 0
			cmd := sessionPhaseCmd(func(*cobra.Command) (client.BossClient, error) { factories++; return c, nil })
			var out bytes.Buffer
			cmd.SetOut(&out)
			cmd.SetErr(&out)
			cmd.SilenceUsage = true
			cmd.SilenceErrors = true
			cmd.SetArgs(tt.args)
			err := cmd.Execute()
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("error = %v, want %q", err, tt.wantErr)
				}
			} else if err != nil {
				t.Fatal(err)
			}
			wantCalls := 1
			if tt.wantErr != "" && tt.daemonErr == nil {
				wantCalls = 0
			}
			if c.calls != wantCalls || factories != wantCalls {
				t.Fatalf("calls=%d factories=%d, want %d", c.calls, factories, wantCalls)
			}
			if wantCalls == 1 && (c.sessionID != tt.wantSession || c.chatID != tt.wantChat || c.phase != tt.wantPhase) {
				t.Fatalf("request = (%q,%q,%q)", c.sessionID, c.chatID, c.phase)
			}
			if out.String() != tt.wantOutput {
				t.Fatalf("output=%q, want %q", out.String(), tt.wantOutput)
			}
		})
	}
}
func TestSessionPhaseRegistered(t *testing.T) {
	cmd, _, err := sessionCmd().Find([]string{"phase"})
	if err != nil || cmd.Name() != "phase" {
		t.Fatalf("phase command missing: %v", err)
	}
}
func TestChatJSONPhaseAlwaysEmitted(t *testing.T) {
	chats := []*pb.ClaudeChat{{AgentSessionId: "phased"}, {AgentSessionId: "plain"}}
	statuses := map[string]*pb.ChatStatusEntry{"phased": {Status: pb.ChatStatus_CHAT_STATUS_WORKING, Phase: "reviewing"}}
	raw, err := json.Marshal(newChatsJSON(chats, statuses))
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		Chats []map[string]any `json:"chats"`
	}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	for i, want := range []string{"reviewing", ""} {
		if phase, ok := got.Chats[i]["phase"]; !ok || phase != want {
			t.Fatalf("row %d phase = %v, present=%v", i, phase, ok)
		}
	}
}
