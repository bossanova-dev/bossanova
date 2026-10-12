package gatecmd

import (
	"slices"
	"testing"
)

func TestArgv(t *testing.T) {
	const home = "/h"
	tests := []struct {
		name string
		cmd  string
		home string
		want []string // nil means ok == false
	}{
		{name: "plain words", cmd: "node /h/x.mjs", home: home, want: []string{"node", "/h/x.mjs"}},
		{name: "surrounding and repeated blanks", cmd: "  node \t  /h/x.mjs  ", home: home, want: []string{"node", "/h/x.mjs"}},
		{name: "double-quoted $HOME", cmd: `node "$HOME/x.mjs"`, home: home, want: []string{"node", "/h/x.mjs"}},
		{name: "double-quoted ${HOME}", cmd: `node "${HOME}/x.mjs"`, home: home, want: []string{"node", "/h/x.mjs"}},
		{name: "unquoted $HOME", cmd: `node $HOME/x.mjs`, home: home, want: []string{"node", "/h/x.mjs"}},
		{name: "single quotes are literal", cmd: `node '$HOME/x.mjs'`, home: home, want: []string{"node", "$HOME/x.mjs"}},
		{name: "leading tilde slash", cmd: `node ~/x.mjs`, home: home, want: []string{"node", "/h/x.mjs"}},
		{name: "bare tilde word", cmd: `cd ~`, home: home, want: []string{"cd", "/h"}},
		{name: "mid-word tilde is literal", cmd: `node a~/x`, home: home, want: []string{"node", "a~/x"}},
		{name: "quoted tilde is literal", cmd: `node "~/x"`, home: home, want: []string{"node", "~/x"}},
		{name: "adjacent quoting joins one word", cmd: `node "$HOME"'/x'.mjs`, home: home, want: []string{"node", "/h/x.mjs"}},
		{name: "empty quoted word is kept", cmd: `node ''`, home: home, want: []string{"node", ""}},
		{name: "unquoted backslash escapes", cmd: `node a\ b`, home: home, want: []string{"node", "a b"}},
		{name: "double-quote escapes", cmd: `node "a\"b\\c\$d\e"`, home: home, want: []string{"node", `a"b\c$d\e`}},
		{name: "mid-word hash is literal", cmd: `node a#b`, home: home, want: []string{"node", "a#b"}},

		{name: "direct exec splits on whitespace only", cmd: `/opt/node "$HOME/x"`, home: home, want: []string{"/opt/node", `"$HOME/x"`}},
		{name: "relative direct exec", cmd: `./gate.sh  'a b'`, home: home, want: []string{"./gate.sh", "'a", "b'"}},
		{name: "parent-relative direct exec", cmd: `../gate.sh x`, home: home, want: []string{"../gate.sh", "x"}},

		{name: "empty", cmd: "   ", home: home},
		{name: "empty home with $HOME", cmd: `node "$HOME/x.mjs"`, home: ""},
		{name: "empty home with tilde", cmd: `node ~/x.mjs`, home: ""},
		{name: "unquoted $HOME in a home with a blank", cmd: `node $HOME/x.mjs`, home: "/Users/Jane Doe"},
		{name: "unquoted $HOME in a home with a glob byte", cmd: `node $HOME/x.mjs`, home: "/h*"},
		{name: "semicolon", cmd: "node /h/x.mjs; true", home: home},
		{name: "ampersand", cmd: "node /h/x.mjs && true", home: home},
		{name: "pipe", cmd: "node /h/x.mjs | cat", home: home},
		{name: "redirect", cmd: "node /h/x.mjs > /dev/null", home: home},
		{name: "subshell", cmd: "(node /h/x.mjs)", home: home},
		{name: "backtick", cmd: "node `pwd`/x.mjs", home: home},
		{name: "backtick in double quotes", cmd: "node \"`pwd`/x.mjs\"", home: home},
		{name: "glob star", cmd: "node /h/*.mjs", home: home},
		{name: "glob question", cmd: "node /h/x?.mjs", home: home},
		{name: "glob bracket", cmd: "node /h/[x].mjs", home: home},
		{name: "brace", cmd: "node /h/{x,y}.mjs", home: home},
		{name: "comment", cmd: "node /h/x.mjs #note", home: home},
		{name: "command substitution", cmd: `node "$(pwd)/x.mjs"`, home: home},
		{name: "other variable", cmd: `node $PATH/x.mjs`, home: home},
		{name: "other variable quoted", cmd: `node "$PATH/x.mjs"`, home: home},
		{name: "HOME prefix of a longer name", cmd: `node $HOMEDIR/x.mjs`, home: home},
		{name: "newline", cmd: "node /h/x.mjs\ntrue", home: home},
		{name: "unterminated single quote", cmd: `node '/h/x.mjs`, home: home},
		{name: "unterminated double quote", cmd: `node "/h/x.mjs`, home: home},
		{name: "trailing backslash", cmd: `node /h/x.mjs\`, home: home},
		{name: "tilde user", cmd: `node ~bob/x.mjs`, home: home},
		{name: "env assignment prefix", cmd: `FOO=1 node /h/x.mjs`, home: home},
		{name: "reserved word", cmd: `! node /h/x.mjs`, home: home},
		{name: "reserved word if", cmd: `if node /h/x.mjs`, home: home},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := Argv(tt.cmd, tt.home)
			if tt.want == nil {
				if ok {
					t.Fatalf("Argv(%q) = %q, true; want ok == false", tt.cmd, got)
				}
				return
			}
			if !ok || !slices.Equal(got, tt.want) {
				t.Fatalf("Argv(%q) = %q, %v; want %q, true", tt.cmd, got, ok, tt.want)
			}
		})
	}
}

func TestEquivalent(t *testing.T) {
	const home = "/h"
	tests := []struct {
		name string
		a, b string
		home string
		want bool
	}{
		{name: "identical", a: "node /h/x.mjs", b: "node /h/x.mjs", home: home, want: true},
		{name: "double-quoted $HOME", a: "node /h/x.mjs", b: `node "$HOME/x.mjs"`, home: home, want: true},
		{name: "double-quoted ${HOME}", a: "node /h/x.mjs", b: `node "${HOME}/x.mjs"`, home: home, want: true},
		{name: "single-quoted absolute", a: "node /h/x.mjs", b: `node '/h/x.mjs'`, home: home, want: true},
		{name: "tilde", a: "node /h/x.mjs", b: `node ~/x.mjs`, home: home, want: true},
		{name: "extra spaces", a: "node /h/x.mjs", b: `node   /h/x.mjs`, home: home, want: true},
		{name: "factory default against absolute unquoted", a: `node "$HOME/.claude/skills/boss-plan/toolbox/cron-gates/boss-plan.mjs"`, b: "node /h/.claude/skills/boss-plan/toolbox/cron-gates/boss-plan.mjs", home: home, want: true},
		{name: "identical unparseable", a: "node /h/x.mjs; true", b: " node /h/x.mjs; true ", home: home, want: true},
		{name: "legacy $HOME rule on an unparseable pair", a: `node "$HOME/x.mjs"; true`, b: `node "/h/x.mjs"; true`, home: home, want: true},

		{name: "different script", a: "node /h/x.mjs", b: "node /h/y.mjs", home: home, want: false},
		{name: "different interpreter", a: "/opt/node /h/x.mjs", b: "node /h/x.mjs", home: home, want: false},
		{name: "single-quoted $HOME stays literal", a: `node '$HOME/x.mjs'`, b: "node /h/x.mjs", home: home, want: false},
		{name: "empty home cannot expand $HOME", a: `node "$HOME/x.mjs"`, b: "node /h/x.mjs", home: "", want: false},
		{name: "unparseable", a: "node /h/x.mjs; true", b: "node /h/x.mjs", home: home, want: false},
		{name: "direct exec does not expand", a: `/opt/node "$HOME/x"`, b: "/opt/node /h/x", home: home, want: false},
		{name: "single-quoted $HOME against single-quoted absolute", a: `node '$HOME/x.mjs'`, b: `node '/h/x.mjs'`, home: home, want: false},
		{name: "direct exec keeps a literal $HOME", a: `/opt/node "$HOME/x"`, b: `/opt/node "/h/x"`, home: home, want: false},
		{name: "unquoted $HOME in a home with a blank is split by sh", a: `node $HOME/x.mjs`, b: `node "$HOME/x.mjs"`, home: "/Users/Jane Doe", want: false},
		{name: "env assignment is not an argument", a: "FOO=1 node /h/x.mjs", b: "'FOO=1' node /h/x.mjs", home: home, want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Equivalent(tt.a, tt.b, tt.home); got != tt.want {
				t.Fatalf("Equivalent(%q, %q, %q) = %v, want %v", tt.a, tt.b, tt.home, got, tt.want)
			}
			if got := Equivalent(tt.b, tt.a, tt.home); got != tt.want {
				t.Fatalf("Equivalent(%q, %q, %q) = %v, want %v (symmetry)", tt.b, tt.a, tt.home, got, tt.want)
			}
		})
	}
}
