package main

import (
	"encoding/json"
	"path/filepath"
	"testing"
)

// The records below follow the toolUseResult shapes Claude Code writes for
// each tool; the values are synthetic.
func TestResultSummaryReadsOnlyWhatTheRecordStates(t *testing.T) {
	ok := contentBlock{Type: "tool_result"}
	failed := func(content string) contentBlock {
		raw, _ := json.Marshal(content)
		return contentBlock{Type: "tool_result", IsError: true, Content: raw}
	}
	cases := []struct {
		name   string
		tool   string
		record string
		block  contentBlock
		want   string
	}{
		{"grep files", "Grep", `{"mode":"files_with_matches","filenames":["a.go","b.go","c.go"],"numFiles":3}`, ok, "3 files"},
		{"grep one file", "Grep", `{"mode":"files_with_matches","filenames":["a.go"],"numFiles":1}`, ok, "1 file"},
		{"grep content counts lines, not its zero numFiles", "Grep", `{"mode":"content","numFiles":0,"filenames":[],"content":"7:x","numLines":26}`, ok, "26 lines"},
		{"grep count", "Grep", `{"mode":"count","numFiles":2,"filenames":[],"content":"a:3","numMatches":1204}`, ok, "1,204 matches"},
		{"grep cut by its limit", "Grep", `{"mode":"content","numFiles":0,"filenames":[],"content":"","numLines":30,"appliedLimit":30}`, ok, "30+ lines"},
		{"grep with no mode", "Grep", `{"numFiles":3}`, ok, ""},
		{"grep failed", "Grep", `"Error: No such file"`, failed("<tool_use_error>No such file</tool_use_error>"), ""},
		{"glob", "Glob", `{"filenames":[],"durationMs":9,"numFiles":11,"truncated":false}`, ok, "11 files"},
		{"glob none", "Glob", `{"filenames":[],"durationMs":9,"numFiles":0,"truncated":false}`, ok, "0 files"},
		{"glob truncated with a complete count", "Glob", `{"numFiles":100,"truncated":true,"totalMatches":340,"countIsComplete":true}`, ok, "340 files"},
		{"glob truncated without one", "Glob", `{"numFiles":100,"truncated":true}`, ok, "100+ files"},
		{"read text", "Read", `{"type":"text","file":{"filePath":"/a.go","content":"x","numLines":420,"startLine":1,"totalLines":1456}}`, ok, "420 lines"},
		{"read image", "Read", `{"type":"image","file":{"base64":"","type":"image/png","originalSize":10}}`, ok, ""},
		{"read without a line count", "Read", `{"type":"text"}`, ok, ""},
		{"edit", "Edit", `{"filePath":"/a.go","structuredPatch":[{"oldStart":1,"oldLines":3,"newStart":1,"newLines":4,"lines":[" a","-b","+c","+d"," e"]},{"oldStart":9,"oldLines":1,"newStart":10,"newLines":0,"lines":["-z"]}]}`, ok, "+2 −2"},
		{"edit adding only", "MultiEdit", `{"structuredPatch":[{"lines":[" a","+b","\\ No newline at end of file"]}]}`, ok, "+1"},
		{"edit without a patch", "Edit", `{"filePath":"/a.go"}`, ok, ""},
		{"edit rejected", "Edit", `"Error: File has not been read yet"`, failed("<tool_use_error>File has not been read yet</tool_use_error>"), ""},
		{"write create", "Write", `{"type":"create","filePath":"/a.go","content":"a\nb\nc\n","structuredPatch":[],"originalFile":null}`, ok, "+3"},
		{"write create without a final newline", "Write", `{"type":"create","content":"a\nb","structuredPatch":[]}`, ok, "+2"},
		{"write update", "Write", `{"type":"update","content":"a\n","structuredPatch":[{"lines":["-old","+new","+more"]}],"originalFile":"old\n"}`, ok, "+2 −1"},
		{"write unknown type", "Write", `{"content":"a\n"}`, ok, ""},
		{"bash exit", "Bash", `"Error: Exit code 2\nboom"`, failed("Exit code 2\nboom"), "exit 2"},
		{"bash exit in blocks", "Bash", ``, contentBlock{Type: "tool_result", IsError: true, Content: json.RawMessage(`[{"type":"text","text":"Exit code 127\nnot found"}]`)}, "exit 127"},
		{"bash failure that states no exit", "Bash", `"Error: Blocked"`, failed("<tool_use_error>Blocked</tool_use_error>"), ""},
		{"bash output that only mentions an exit", "Bash", `{"stdout":"Exit code 3","stderr":"","interrupted":false}`, contentBlock{Type: "tool_result", Content: json.RawMessage(`"Exit code 3"`)}, ""},
		{"bash success", "Bash", `{"stdout":"ok","stderr":"","interrupted":false,"isImage":false}`, ok, ""},
		{"bash interrupted", "Bash", `{"stdout":"","stderr":"","interrupted":true}`, ok, "interrupted"},
		{"untracked tool", "WebFetch", `{"bytes":10,"code":200,"codeText":"OK"}`, ok, ""},
		{"no record", "Grep", ``, ok, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := resultSummary(c.tool, json.RawMessage(c.record), c.block); got != c.want {
				t.Errorf("resultSummary(%s) = %q, want %q", c.tool, got, c.want)
			}
		})
	}
}

// testdata/results.jsonl pairs tool calls with result lines in Claude Code's
// shape, including one line that carries two results under one record.
func TestReadActivitySetsStepResults(t *testing.T) {
	got, _, _, err := readActivity(filepath.Join("testdata", "results.jsonl"), activityWorkDir, 0)
	if err != nil {
		t.Fatal(err)
	}
	want := []struct {
		text    string
		result  string
		isError bool
	}{
		{"Edited services/a.go", "+2 −1", false},
		{"Searched levelAreasFor", "4 files", false},
		{"Ran make test", "exit 2", true},
		{"Read services/a.go", "", false},
		{"Read services/b.go", "", true},
	}
	if len(got) != len(want) {
		t.Fatalf("got %d messages, want %d:\n%s", len(got), len(want), dump(got))
	}
	for i, w := range want {
		s := got[i].GetStep()
		if got[i].GetText() != w.text || s.GetResult() != w.result || s.GetIsError() != w.isError {
			t.Errorf("message %d = %q result %q failed %v, want %q result %q failed %v",
				i, got[i].GetText(), s.GetResult(), s.GetIsError(), w.text, w.result, w.isError)
		}
	}
}
