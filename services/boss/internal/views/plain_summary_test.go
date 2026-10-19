package views

import (
	"fmt"
	"runtime"
	"slices"
	"strings"
	"testing"
	"unicode"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

// surveyQuestion is the shape that lost its question: background, an options
// list, a recommendation, and the question last.
const surveyQuestion = "I dug into the flaky `TestDeployRetry` failures on main. The fixture server in `services/bossd/internal/deploy/testdata/server.go` starts listening after the health check fires, so about one run in twenty sees a refused connection and gives up.\n\nThere are three ways to fix it:\n\n1. **Retry the health check** with a short backoff (smallest change, but it hides the race)\n2. **Wait for the listener** before the check starts (fixes the race, touches the fixture API)\n3. **Drop the health check** from the test (fastest, loses coverage)\n\nI'd go with option 2: it removes the race instead of papering over it, and the fixture change is about 30 lines.\n\nShould I go ahead with option 2 and open a PR for it?"

const questionFirstMessage = "Should I merge #3323 now, before the release cut? The verifier passed it with notes, and none of the notes block. The remaining work is a follow-up for the summary's question handling, the CJK sentence ends and a few markdown edge cases, which can land after the cut without touching the release branch. CI is green on the head commit and the PTY drive at 80x30 shows the answer under its label."

const noQuestionMessage = "I finished the migration and pushed it to the branch. The schema change adds two columns and backfills them from the existing rows, which took about four minutes on a copy of production. Every caller now reads the new columns, the old ones are unused, and dropping them is a separate PR once this has soaked for a week. I'm waiting for you to look at the PR."

const cjkQuestion = "我检查了部署测试失败的原因。夹具服务器在健康检查开始之后才开始监听，所以大约每二十次运行就有一次连接被拒绝并放弃重试。有三种修复方法：重试健康检查、等待监听器启动、或者完全删除健康检查。我建议等待监听器启动，因为它消除了竞争条件而不是掩盖它，而且夹具的改动大约只有三十行代码。要我现在按第二种方法修复并提交PR吗？"

const unclosedFenceQuestion = "I rewrote the retry loop so it backs off instead of spinning:\n\n```go\nfor attempt := 0; attempt < 5; attempt++ {\n\tif err := check(); err == nil {\n\t\treturn nil\n\t}\n\nShould I push it?"

const pathsMessage = "The failing fixture lives at services/bossd/internal/deploy/testdata/fixtures/health-check-server-with-a-very-long-name.go and the run is https://github.com/recurser/bossanova/actions/runs/1234567890/job/9876543210.\n\nThe second paragraph explains what I tried, which was a retry with a backoff and then a wait for the listener, and neither held up across twenty runs on CI."

// importBackground opens the shapes below with more than the summary's rows
// hold at every width, so whatever they ask comes after the opening that fits.
const importBackground = "I looked into last night's import failures. The batch job ran out of disk on the staging host after the old staging table grew past 40 GB, and the automatic retry filled the same disk again before anyone was paged. The nightly cleanup that should have trimmed the table has been failing silently since the scheduler moved to the new cluster three weeks ago, because its service account never got the delete grant there. Last week's batches finished, but four of them wrote partial results before the disk filled."

// closingListQuestion asks its questions as a numbered list after a
// colon-ended introduction.
const closingListQuestion = importBackground + "\n\nBefore I continue:\n\n1. Should I delete the old staging table?\n2. Do you want a backfill for last week's batches?"

// codeWordQuestion asks nothing: each "?" ends a word in code or a URL.
const codeWordQuestion = importBackground + "\n\nI changed `valid?` to `decodable?` in the model and the run is green. The field is now `String?` in the model, and I replaced the check with `ok ? retry() : fail()` so a failed decode retries once. The encoding notes are at https://example.com/search? for the docs, and the filter ?status=failed lists the earlier runs."

// quotedQuestion asks nothing: its only question is quoted inside a statement.
const quotedQuestion = importBackground + "\n\nThe vendor's ticket asks \"can you accept both encodings?\" and I think we can. The change is on the branch and CI is green."

// headingQuestion asks its question as a heading.
const headingQuestion = "I finished the migration and pushed it to the branch. The schema change adds two columns and backfills them from the existing rows.\n\n## Should I open the PR?"

// japaneseQuestion's first sentence fills a 40-column answer row, so its full
// stop wraps onto a row of its own.
const japaneseQuestion = "夜間インポートの失敗を調査しました。ステージング環境のディスクが古いステージングテーブルで一杯になり再試行も同じディスクを使い切ったため処理が止まっていました。古いテーブルを削除してから再実行してもよいですか？"

// squashed is text with its whitespace gone, so a CJK sentence wrapped
// across rows compares equal to the sentence.
func squashed(text string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsSpace(r) {
			return -1
		}
		return r
	}, text)
}

// TestChatPickerNeededFromYouKeepsTheQuestion: at every width the answer is at
// most summaryMaxRows rows, none blank and no ellipsis on a row without text,
// opens with the message's opening, and shows the message's question, however
// far down the message it sits. A message with no question shows its opening,
// as before, and never a "?" that asks nothing.
func TestChatPickerNeededFromYouKeepsTheQuestion(t *testing.T) {
	for _, tt := range []struct {
		name, text, opening, question, absent string
	}{
		{"question last", surveyQuestion, "I dug into the flaky TestDeployRetry", "Should I go ahead with option 2 and open a PR for it?", ""},
		{"question first", questionFirstMessage, "Should I merge #3323 now", "Should I merge #3323 now, before the release cut?", ""},
		{"no question", noQuestionMessage, "I finished the migration", "", ""},
		{"CJK question", cjkQuestion, "我检查了部署测试失败的原因。", "要我现在按第二种方法修复并提交PR吗？", ""},
		{"question after an unclosed fence", unclosedFenceQuestion, "I rewrote the retry loop", "Should I push it?", ""},
		{"markdown question first and last", operatorMarkdownQuestion, "Should I keep the YAML rewrite in this PR, or split it out?", "Which do you prefer?", ""},
		{"closing list of questions", closingListQuestion, "I looked into last night's import", "Should I delete the old staging table?", ""},
		{"question marks in code", codeWordQuestion, "I looked into last night's import", "", "?"},
		{"quoted question", quotedQuestion, "I looked into last night's import", "", "encodings?"},
		{"heading question", headingQuestion, "I finished the migration", "Should I open the PR?", ""},
		{"Japanese question", japaneseQuestion, "夜間インポートの失敗を調査しました", "古いテーブルを削除してから再実行してもよいですか？", ""},
	} {
		for _, width := range []int{40, 80, 120} {
			t.Run(fmt.Sprintf("%s/%d", tt.name, width), func(t *testing.T) {
				m := questionTimelineModel(width, 40, operatorQuestion)
				m.fullQuestion = tt.text
				rows := neededRows(t, m)
				t.Logf("answer at %d columns:\n%s", width, stripANSI(m.neededBlock()))
				if len(rows) == 0 || len(rows) > summaryMaxRows {
					t.Fatalf("answer shows %d rows, want 1 to %d", len(rows), summaryMaxRows)
				}
				for i, row := range rows {
					if strings.HasSuffix(row, "…") && strings.TrimRight(row, " …"+sentenceStops+sentenceClosers) == "" {
						t.Errorf("row %d holds an ellipsis and no text: %q", i, rows)
					}
					if strings.Contains(row, "…") && !strings.HasSuffix(row, "…") {
						t.Errorf("ellipsis inside row %d: %q", i, row)
					}
					if w := ansi.StringWidth(row); w > m.neededTextWidth() {
						t.Errorf("row %d is %d wide, want at most %d: %q", i, w, m.neededTextWidth(), row)
					}
				}
				if !strings.HasPrefix(squashed(strings.Join(rows, " ")), squashed(tt.opening)) {
					t.Errorf("answer does not open with %q: %q", tt.opening, rows)
				}
				if tt.question != "" && !strings.Contains(squashed(strings.Join(rows, " ")), squashed(tt.question)) {
					t.Fatalf("answer lost the question %q: %q", tt.question, rows)
				}
				if tt.absent != "" && strings.Contains(strings.Join(rows, " "), tt.absent) {
					t.Fatalf("answer shows %q, which asks nothing: %q", tt.absent, rows)
				}
			})
		}
	}
}

// TestChatPickerNeededFromYouQuestionLastRenders pins the question-last
// message at 80 columns: the opening paragraph, an ellipsis, and the question
// on its own row.
func TestChatPickerNeededFromYouQuestionLastRenders(t *testing.T) {
	m := questionTimelineModel(80, 40, operatorQuestion)
	m.fullQuestion = surveyQuestion
	want := []string{
		"I dug into the flaky TestDeployRetry failures on main. The fixture",
		"server in services/bossd/internal/deploy/testdata/server.go starts",
		"listening after the health check fires, so about one run in twenty sees",
		"a refused connection and gives up. …",
		"Should I go ahead with option 2 and open a PR for it?",
	}
	if rows := neededRows(t, m); !slices.Equal(rows, want) {
		t.Fatalf("answer = %q, want %q", rows, want)
	}
}

// TestChatPickerNeededFromYouShortTerminalKeepsTheQuestion: a terminal too
// short for the whole summary summarises into the rows left, so the question
// stays and the opening gives way.
func TestChatPickerNeededFromYouShortTerminalKeepsTheQuestion(t *testing.T) {
	for height := 14; height <= 24; height++ {
		m := questionTimelineModel(80, height, operatorQuestion)
		m.fullQuestion = surveyQuestion
		rows := neededRows(t, m)
		if len(rows) == 0 || rows[len(rows)-1] != "Should I go ahead with option 2 and open a PR for it?" {
			t.Fatalf("at 80x%d the answer lost its question: %q", height, rows)
		}
	}
}

// TestChatPickerNeededFromYouLongQuestionRunKeepsItsLastQuestion: a run of
// question sentences too long for the rows keeps the last ones.
func TestChatPickerNeededFromYouLongQuestionRunKeepsItsLastQuestion(t *testing.T) {
	m := questionTimelineModel(80, 40, operatorQuestion)
	m.fullQuestion = strings.Repeat("Should we keep this behavior for all supported platforms? ", 40) + "Can you confirm the final choice?"
	rows := neededRows(t, m)
	if len(rows) > summaryMaxRows || !strings.HasSuffix(rows[len(rows)-1], "Can you confirm the final choice?") {
		t.Fatalf("answer = %q, want it to end on the last question", rows)
	}
}

// TestChatPickerNeededFromYouMeasuresPathsByWidth: a paragraph whose path and
// URL wrap across rows is measured by the rows it takes, so it shows whole
// when it fits rather than being cut short as if each piece were a word.
func TestChatPickerNeededFromYouMeasuresPathsByWidth(t *testing.T) {
	for _, width := range []int{80, 120} {
		m := questionTimelineModel(width, 40, operatorQuestion)
		m.fullQuestion = pathsMessage
		rows := neededRows(t, m)
		shown := squashed(strings.Join(rows, ""))
		if !strings.Contains(shown, "job/9876543210.") || strings.Contains(shown, "secondparagraph") || !strings.HasSuffix(rows[len(rows)-1], "…") {
			t.Fatalf("at %d columns answer = %q, want the whole first paragraph, cut", width, rows)
		}
	}
}

// TestChatPickerNeededFromYouNeverRendersABlankRow: a message that is only
// markup falls back to the attention's sentence instead of a blank row.
func TestChatPickerNeededFromYouNeverRendersABlankRow(t *testing.T) {
	for _, text := range []string{"```\n```", "****", "~~~\n~~~", "**"} {
		m := questionTimelineModel(80, 40, "Should this PR be merged?")
		m.fullQuestion = text
		if rows := neededRows(t, m); !slices.Equal(rows, []string{"Should this PR be merged?"}) {
			t.Fatalf("answer for %q = %q, want the attention's sentence", text, rows)
		}
	}
}

// TestChatPickerPageKeysMoveTheTableWhileAQuestionShows: with a question
// showing, pgdown and pgup page the chat table.
func TestChatPickerPageKeysMoveTheTableWhileAQuestionShows(t *testing.T) {
	m := questionTimelineModel(80, 40, operatorQuestion)
	m.fullQuestion = surveyQuestion
	base := m.chats[0]
	for i := range 30 {
		m.chats = append(m.chats, &pb.ClaudeChat{AgentSessionId: fmt.Sprintf("chat-%d", i), Title: fmt.Sprintf("Chat %d", i), CreatedAt: base.GetCreatedAt(), AgentName: "claude"})
	}
	m.buildTableRows()
	m.table.SetHeight(m.tableHeight())
	if m.neededBlock() == "" {
		t.Fatal("no question showing")
	}
	press := func(code rune) {
		t.Helper()
		updated, _ := m.Update(tea.KeyPressMsg{Code: code})
		m = updated.(ChatPickerModel)
	}
	press(tea.KeyPgDown)
	if m.table.Cursor() == 0 {
		t.Fatal("pgdown left the table cursor on the first chat")
	}
	down := m.table.Cursor()
	press(tea.KeyPgUp)
	if m.table.Cursor() >= down {
		t.Fatalf("pgup left the table cursor at %d, was %d", m.table.Cursor(), down)
	}
}

// TestSummaryLinesEllipsisStaysOnATextRow: the ellipsis ends the last row of
// text and never takes a row of its own, in English or CJK.
func TestSummaryLinesEllipsisStaysOnATextRow(t *testing.T) {
	for _, tt := range []struct {
		name, text  string
		width, most int
		want        []string
	}{
		{"sentence filling its row", "Ship it now. More words here to cut away.", 12, 2, []string{"Ship it now.", "More words…"}},
		{"CJK sentence end", "我检查了原因。夹具服务器在健康检查开始之后才开始监听所以连接被拒绝", 20, 1, []string{"我检查了原因。…"}},
		{"CJK question", "我检查了原因。要修复吗？", 20, 1, []string{"要修复吗？"}},
		{"closing list too long for the rows", closingListQuestion, 34, 2, []string{"1. Should I delete the old staging", "table? …"}},
		{"CJK full stop wrapped onto its own row", "夜間インポートの失敗を調査しました。ステージング環境のディスクが古いステージングテーブルで一杯になりました。", 34, 2, []string{"夜間インポートの失敗を調査しました", "。ステージング環境のディスクが古…"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := summaryLines(tt.text, tt.width, tt.most); !slices.Equal(got, tt.want) {
				t.Fatalf("summaryLines(%q, %d, %d) = %q, want %q", tt.text, tt.width, tt.most, got, tt.want)
			}
		})
	}
}

// TestSummaryLinesPicksTheAsk: after an opening too long for the rows, the
// summary ends on the message's question, or on the cut opening when it asks
// nothing.
func TestSummaryLinesPicksTheAsk(t *testing.T) {
	const width, most = 76, 5
	for _, tt := range []struct {
		name, tail, last, absent string
	}{
		{"questions in a list followed by a statement ask nothing", "1. Why did the cleanup fail?\n2. Why did nobody notice?\n\nBoth answers are in the incident doc, and the grant is fixed now.", "", "Why did"},
		{"a question mark followed by a letter ends no sentence", "Open the dashboard at runs?status=failed to see the four batches.", "", "runs?status"},
		{"a question mark after a space ends no sentence", "I replaced the check with ok ? retry() : fail() in the loader.", "", "ok ?"},
		{"a question mark ending a URL ends no sentence", "The notes are at https://example.com/search? for the docs.", "", "search?"},
		{"a statement ends the run of questions", "Should I retry the batches? I will wait for your call either way. Can you confirm the grant is in place?", "Can you confirm the grant is in place?", "I will wait"},
		{"a closing quote keeps a question", "I staged the cleanup fix behind a flag. \"Should I turn the flag on tonight?\"", "\"Should I turn the flag on tonight?\"", ""},
		{"a closing bracket keeps a question", "I staged the cleanup fix behind a flag. (Should I turn the flag on tonight?)", "(Should I turn the flag on tonight?)", ""},
		{"a question quoted mid-sentence asks nothing", "The user asked \"should I retry?\" and I did.", "", "retry?"},
		{"a question in a closing list asks it", "Before I continue:\n\n- Should I delete the old staging table?", "Should I delete the old staging table?", ""},
	} {
		t.Run(tt.name, func(t *testing.T) {
			rows := summaryLines(importBackground+"\n\n"+tt.tail, width, most)
			last := rows[len(rows)-1]
			if tt.last == "" && !strings.HasSuffix(last, "…") {
				t.Errorf("summary ends %q, want the cut opening ending in an ellipsis: %q", last, rows)
			}
			if tt.last != "" && !strings.HasSuffix(last, tt.last) {
				t.Errorf("summary ends %q, want %q: %q", last, tt.last, rows)
			}
			if tt.absent != "" && strings.Contains(strings.Join(rows, " "), tt.absent) {
				t.Errorf("summary shows %q: %q", tt.absent, rows)
			}
		})
	}
}

// longMessage is a 43 KB agent message, the size that made the details screen
// slow to draw.
var longMessage = strings.Repeat(surveyQuestion+"\n\n", 43000/len(surveyQuestion))

// TestSummaryLinesAllocatesLinearly: summarising a 43 KB message allocates at
// most twice the four times what an 11 KB one does. Copying the text once per
// space, splitting it into words at each period, or wrapping a run of
// questions from each of its sentences made it about sixteen times.
func TestSummaryLinesAllocatesLinearly(t *testing.T) {
	const ceiling = 8
	allocated := func(text string) uint64 {
		var before, after runtime.MemStats
		runtime.ReadMemStats(&before)
		summaryLines(text, 76, 5)
		runtime.ReadMemStats(&after)
		return after.TotalAlloc - before.TotalAlloc
	}
	for name, unit := range map[string]string{
		"message of paragraphs":  surveyQuestion + "\n\n",
		"run of questions":       "Should we keep this behavior for all supported platforms? ",
		"paragraph of sentences": "The batch finished. ",
	} {
		small := strings.Repeat(unit, 11000/len(unit))
		large := strings.Repeat(small, 4)
		growth := float64(allocated(large)) / float64(allocated(small))
		t.Logf("%s: %d bytes allocated %.1f times what %d bytes did", name, len(large), growth, len(small))
		if growth > ceiling {
			t.Errorf("summarising a %s of %d bytes allocated %.1f times what %d bytes did, want at most %d", name, len(large), growth, len(small), ceiling)
		}
	}
}

func BenchmarkSummaryLinesLongMessage(b *testing.B) {
	b.ReportAllocs()
	for b.Loop() {
		summaryLines(longMessage, 76, 5)
	}
}

// TestPlainMarkdownEdgeCases: emphasis goes only where it is emphasis, fences
// of either kind go, an unclosed fence hides only its own line, and a table
// reads as its cells.
func TestPlainMarkdownEdgeCases(t *testing.T) {
	for _, tt := range []struct{ in, want string }{
		{"update __init__.py and __bold__.", "update __init__.py and bold."},
		{"an *italic* word, ***both*** and *.go files", "an italic word, both and *.go files"},
		{"**bold *nested* text** and 5 * 3 * 2", "bold nested text and 5 * 3 * 2"},
		{"~~~\ncode\n~~~\nafter", "after"},
		{"before\n\n````md\n```\nstill code\n````\nafter", "before after"},
		{"| Option | Cost |\n|---|:---:|\n| Retry | low |", "• Option, Cost • Retry, low"},
		{"text\n```go\nx := 1\n\nShould I push it?", "text x := 1 Should I push it?"},
		{"```\n```", ""},
		{"****", ""},
		{"**`config.yaml`** is `**raw**`", "config.yaml is **raw**"},
	} {
		if got := plainMarkdown(tt.in); got != tt.want {
			t.Errorf("plainMarkdown(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}
