package main

import (
	"context"
	"strings"
	"testing"

	bossanovav1 "github.com/recurser/bossalib/gen/bossanova/v1"
)

func TestBuildInteractiveCommand_Sessions_AC103_AppendSystemPrompt(t *testing.T) {
	t.Setenv("CODEX_HOME", t.TempDir())
	for _, launch := range []struct {
		name   string
		resume bool
	}{{"fresh", false}, {"resume", true}} {
		for _, suffix := range []struct {
			name string
			text string
		}{{"suffix", "Boss context\nquoted \"instructions\" in C:\\work"}, {"blank", ""}, {"whitespace", " \t\n"}} {
			for _, input := range []struct {
				name    string
				command string
			}{{"command", "boss-repair"}, {"no-input", ""}} {
				t.Run(launch.name+"/"+suffix.name+"/"+input.name, func(t *testing.T) {
					req := &bossanovav1.BuildInteractiveCommandRequest{
						SessionId: "agent-1", Resume: launch.resume,
						AppendSystemPrompt: suffix.text, InitialCommand: input.command,
					}
					resp, err := (&Server{}).BuildInteractiveCommand(context.Background(), req)
					if err != nil {
						t.Fatalf("BuildInteractiveCommand: %v", err)
					}
					argv := resp.GetArgv()
					overrideIndex := -1
					separatorIndex := len(argv)
					for i, arg := range argv {
						if arg == "--" {
							separatorIndex = i
						}
						if strings.HasPrefix(arg, "developer_instructions=") {
							if overrideIndex != -1 || i == 0 || argv[i-1] != "-c" {
								t.Fatalf("expected one adjacent -c developer_instructions pair: %q", argv)
							}
							overrideIndex = i
							if strings.ContainsAny(arg, "\r\n") {
								t.Fatalf("override contains a raw line break: %q", arg)
							}
							encoded := strings.TrimPrefix(arg, "developer_instructions=")
							if !strings.HasPrefix(encoded, `"`) {
								t.Fatalf("override must use a TOML basic string: %q", arg)
							}
							scanner := tomlScanner{s: encoded}
							decoded, err := scanner.parseBasicString()
							if err != nil || decoded != suffix.text {
								t.Fatalf("override round trip = %q, %v; want %q", decoded, err, suffix.text)
							}
						}
					}
					wantOverride := strings.TrimSpace(suffix.text) != ""
					if (overrideIndex >= 0) != wantOverride {
						t.Fatalf("developer_instructions present = %v, want %v: %q", overrideIndex >= 0, wantOverride, argv)
					}
					if overrideIndex >= separatorIndex {
						t.Fatalf("developer_instructions must precede --: %q", argv)
					}
					assertAppendSystemPromptDeclarationMatchesArgv(t, resp)
				})
			}
		}
	}
}

// The declaration describes the returned argv, including input-less launches.
func assertAppendSystemPromptDeclarationMatchesArgv(t *testing.T, resp *bossanovav1.BuildInteractiveCommandResponse) {
	t.Helper()
	carriesContext := false
	for i, arg := range resp.GetArgv() {
		if arg == "-c" && i+1 < len(resp.GetArgv()) && strings.HasPrefix(resp.GetArgv()[i+1], "developer_instructions=") {
			carriesContext = true
		}
	}
	want := bossanovav1.AppendSystemPromptSupport_APPEND_SYSTEM_PROMPT_SUPPORT_NONE
	if carriesContext {
		want = bossanovav1.AppendSystemPromptSupport_APPEND_SYSTEM_PROMPT_SUPPORT_IN_ARGV
	}
	if got := resp.GetAppendSystemPromptSupport(); got != want {
		t.Fatalf("append_system_prompt_support = %v, want %v for argv %q", got, want, resp.GetArgv())
	}
}
