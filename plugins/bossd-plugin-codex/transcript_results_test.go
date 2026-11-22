package main

import (
	"encoding/json"
	"testing"
)

// Items in the shape codex 0.160+ writes into item_completed; the values are
// synthetic.
func TestItemStepResultsReadOnlyWhatTheRolloutStates(t *testing.T) {
	cases := []struct {
		name   string
		item   string
		want   string
		failed bool
	}{
		{"command failed with an exit", `{"type":"CommandExecution","command":["/bin/zsh","-lc","make test"],"status":"failed","exit_code":2}`, "exit 2", true},
		{"command exited zero", `{"type":"CommandExecution","command":["/bin/zsh","-lc","ls"],"status":"completed","exit_code":0}`, "", false},
		{"command killed without a status", `{"type":"CommandExecution","command":["/bin/zsh","-lc","sleep 9"],"status":"failed","exit_code":-1}`, "", true},
		{"command with no exit recorded", `{"type":"CommandExecution","command":["/bin/zsh","-lc","ls"]}`, "", false},
		{"parsed read that failed", `{"type":"CommandExecution","command":["/bin/zsh","-lc","cat x"],"parsed_cmd":[{"type":"read","cmd":"cat x","path":"/w/x"}],"status":"failed","exit_code":1}`, "exit 1", true},
		{"added file", `{"type":"FileChange","changes":{"/w/a.go":{"type":"add","content":"package a\n\nfunc A() {}\n"}},"status":"completed"}`, "+3", false},
		{"updated file", `{"type":"FileChange","changes":{"/w/b.go":{"type":"update","unified_diff":"@@ -1,3 +1,4 @@\n package b\n-var x = 1\n+var x = 2\n+var y = 3\n","move_path":null}},"status":"completed"}`, "+2 −1", false},
		{"diff headers are not lines", `{"type":"FileChange","changes":{"/w/b.go":{"type":"update","unified_diff":"--- a/b.go\n+++ b/b.go\n@@ -1 +1 @@\n-old\n+new\n"}},"status":"completed"}`, "+1 −1", false},
		{"update without a diff", `{"type":"FileChange","changes":{"/w/b.go":{"type":"update"}},"status":"completed"}`, "", false},
		{"deleted file", `{"type":"FileChange","changes":{"/w/c.go":{"type":"delete"}},"status":"completed"}`, "", false},
		{"failed patch reports no size", `{"type":"FileChange","changes":{"/w/a.go":{"type":"add","content":"package a\n"}},"status":"failed"}`, "", true},
		{"mcp call", `{"type":"McpToolCall","server":"linear","tool":"get_issue","status":"completed"}`, "", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var it activityItem
			if err := json.Unmarshal([]byte(c.item), &it); err != nil {
				t.Fatal(err)
			}
			msgs := itemMessages(it, "2026-11-19T04:00:00.000Z", "/w")
			if len(msgs) != 1 {
				t.Fatalf("got %d messages, want one step", len(msgs))
			}
			s := msgs[0].GetStep()
			if s.GetResult() != c.want || s.GetIsError() != c.failed {
				t.Errorf("step %q result %q failed %v, want result %q failed %v", msgs[0].GetText(), s.GetResult(), s.GetIsError(), c.want, c.failed)
			}
		})
	}
}
