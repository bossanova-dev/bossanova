package agentcred

import (
	"errors"
	"testing"
)

func TestParseClaudeSetupTokenOutput(t *testing.T) {
	tests := []struct {
		name   string
		output string
		want   string
		ok     bool
	}{
		{"plain token line", "Your token:\nsk-ant-oat01-" + repeat("a", 40) + "\n", "sk-ant-oat01-" + repeat("a", 40), true},
		{"ansi wrapped", "\x1b[1msk-ant-oat01-" + repeat("b", 40) + "\x1b[0m", "sk-ant-oat01-" + repeat("b", 40), true},
		{"osc8 hyperlink noise", "\x1b]8;;https://claude.com/x\x1b\\link\x1b]8;;\x1b\\ sk-ant-oat01-" + repeat("c", 40), "sk-ant-oat01-" + repeat("c", 40), true},
		{"embedded in prose", "Copy this: sk-ant-oat01-" + repeat("d", 40) + " and keep it safe", "sk-ant-oat01-" + repeat("d", 40), true},
		// A '-' is a valid token character but not a word character; a \b boundary
		// would drop the trailing '-' and leave a mangled token that still passes
		// ValidateClaudeToken. Both a trailing-newline and end-of-string form must
		// preserve the final '-'.
		{"trailing hyphen before newline", "sk-ant-oat01-" + repeat("e", 40) + "-\n", "sk-ant-oat01-" + repeat("e", 40) + "-", true},
		{"trailing hyphen at eof", "sk-ant-oat01-" + repeat("f", 40) + "-", "sk-ant-oat01-" + repeat("f", 40) + "-", true},
		{"no token", "please visit the url and approve", "", false},
		{"too short", "sk-ant-short", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := ParseClaudeSetupTokenOutput(tt.output)
			if ok != tt.ok || got != tt.want {
				t.Fatalf("got (%q,%v), want (%q,%v)", got, ok, tt.want, tt.ok)
			}
		})
	}
}

func TestValidateClaudeToken(t *testing.T) {
	if err := ValidateClaudeToken("sk-ant-oat01-" + repeat("a", 40)); err != nil {
		t.Fatalf("valid token rejected: %v", err)
	}
	for _, bad := range []string{"", "sk-ant-", "sk-ant-short", "not-a-token", " sk-ant-oat01-" + repeat("a", 40)} {
		if err := ValidateClaudeToken(bad); !errors.Is(err, ErrInvalidClaudeToken) {
			t.Fatalf("ValidateClaudeToken(%q) = %v, want ErrInvalidClaudeToken", bad, err)
		}
	}
}

func TestMaskToken(t *testing.T) {
	got := MaskToken("sk-ant-oat01-" + repeat("a", 36) + "wxyz")
	want := "sk-ant-…wxyz"
	if got != want {
		t.Fatalf("MaskToken = %q, want %q", got, want)
	}
	if edge := MaskToken("12345678wxyz"); edge != "sk-ant-…wxyz" {
		t.Fatalf("MaskToken(12-byte token) = %q, want %q", edge, "sk-ant-…wxyz")
	}
	if short := MaskToken("abc"); short != "…" {
		t.Fatalf("MaskToken(short) = %q, want …", short)
	}
}

func repeat(s string, n int) string {
	out := ""
	for i := 0; i < n; i++ {
		out += s
	}
	return out
}
