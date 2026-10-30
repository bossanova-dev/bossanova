package verifybridge

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/recurser/bossd/internal/domain"
)

func TestSendBackShapeAndNoteCleanup(t *testing.T) {
	for _, tc := range []struct {
		name, out string
		bad       bool
	}{
		{"success", `{"sentBack":true,"head":"` + strings.Repeat("a", 40) + `","commentUrl":"https://example/comment","autoRepair":true}`, false},
		{"refusal", `{"sentBack":false,"reason":"not-open"}`, false},
		{"missing boolean", `{}`, true},
		{"missing reason", `{"sentBack":false}`, true},
		{"missing head", `{"sentBack":true,"commentUrl":"https://example/comment"}`, true},
		{"missing comment", `{"sentBack":true,"head":"a"}`, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t, tc.out, "", 0)
			_, err := f.bridge.SendBack(context.Background(), Target{PR: 7, Dir: f.worktree}, domain.VerifySendBack{Head: strings.Repeat("a", 40), Note: "operator note\n```@user"})
			if tc.bad != errors.Is(err, ErrShape) || !tc.bad && err != nil {
				t.Fatalf("error = %v", err)
			}
			args := f.argv(t)
			var notePath string
			for _, a := range args {
				if strings.HasPrefix(a, "--note-file=") {
					notePath = strings.TrimPrefix(a, "--note-file=")
				}
			}
			if notePath == "" {
				t.Fatalf("no note file in %v", args)
			}
			if _, err := os.Stat(notePath); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("note file left behind: %v", err)
			}
			if !strings.Contains(strings.Join(args, " "), "--head="+strings.Repeat("a", 40)) {
				t.Fatalf("pin absent: %v", args)
			}
		})
	}
}

func TestSendBackNoteFileIsPrivateAndVerbatim(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("BSD stat permission probe")
	}
	f := newFixture(t, `{"sentBack":true,"head":"`+strings.Repeat("a", 40)+`","commentUrl":"https://example/comment"}`, "", 0)
	copyPath := filepath.Join(t.TempDir(), "note-copy")
	script, err := os.ReadFile(f.bridge.Node)
	if err != nil {
		t.Fatal(err)
	}
	capture := "for a in \"$@\"; do case \"$a\" in --note-file=*) p=${a#--note-file=}; test \"$(stat -f %Lp \"$p\")\" = 600 || exit 8; cat \"$p\" > '" + copyPath + "';; esac; done\n"
	if err := os.WriteFile(f.bridge.Node, []byte(strings.Replace(string(script), "#!/bin/sh\n", "#!/bin/sh\n"+capture, 1)), 0o755); err != nil {
		t.Fatal(err)
	}
	note := "note `$(literal)`\n<!-- @user -->"
	if _, err := f.bridge.SendBack(context.Background(), Target{PR: 7, Dir: f.worktree}, domain.VerifySendBack{Head: strings.Repeat("a", 40), Note: note}); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(copyPath)
	if err != nil || string(got) != note {
		t.Fatalf("note=%q err=%v", got, err)
	}
}
