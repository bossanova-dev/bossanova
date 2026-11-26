package main

import (
	"context"
	"strings"
	"testing"

	bossanovav1 "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/rs/zerolog"
	"google.golang.org/protobuf/proto"
)

func readAskMenu(t *testing.T, pane []byte) *bossanovav1.ChatQuestion {
	t.Helper()
	resp, err := newServer(nil, zerolog.Nop()).ReadQuestionMenu(context.Background(), &bossanovav1.ReadQuestionMenuRequest{PaneContent: pane})
	if err != nil {
		t.Fatal(err)
	}
	return resp.GetQuestion()
}

func askTabs(active int) []*bossanovav1.ChatQuestionTab {
	return []*bossanovav1.ChatQuestionTab{{Title: "Question 1", IsActive: active == 1}, {Title: "Question 2", IsActive: active == 2}}
}

func TestReadQuestionMenu_ReadsEveryCapturedAskQuestionMenu(t *testing.T) {
	ask := func(words string, edit func(*bossanovav1.ChatQuestion), options ...string) *bossanovav1.ChatQuestion {
		q := &bossanovav1.ChatQuestion{Kind: "question", Text: words, Options: options, Id: askQuestionID(words, options), HasFreeText: true}
		edit(q)
		return q
	}
	colours := []string{"Red (warm)", "Green (calm)", "Blue (cool)"}
	effects := []string{"Shadow (depth)", "Glow (light)", "Border (outline)"}
	cases := []struct {
		fixture string
		want    *bossanovav1.ChatQuestion
	}{
		{"single", ask("Which colour should the button be?", func(*bossanovav1.ChatQuestion) {}, colours...)},
		{"single_on_writein", ask("Which colour should the button be?", func(*bossanovav1.ChatQuestion) {}, colours...)},
		{"single_writein_typed", ask("Which colour should the button be?", func(q *bossanovav1.ChatQuestion) { q.FreeText = "Purple" }, colours...)},
		{"two_q1", ask("Which size should the button be?", func(q *bossanovav1.ChatQuestion) { q.Tabs = askTabs(1) }, "Small (compact)", "Large (prominent)")},
		{"two_q2", ask("Which effects should the button have?", func(q *bossanovav1.ChatQuestion) {
			q.Tabs, q.IsMultiSelect = askTabs(2), true
		}, effects...)},
		{"two_q2_space", ask("Which effects should the button have?", func(q *bossanovav1.ChatQuestion) {
			q.Tabs, q.IsMultiSelect, q.SelectedOptionIndexes = askTabs(2), true, []uint32{0}
		}, effects...)},
		{"two_q1_back", ask("Which size should the button be?", func(q *bossanovav1.ChatQuestion) {
			q.Tabs, q.SelectedOptionIndexes = askTabs(1), []uint32{0}
		}, "Small (compact)", "Large (prominent)")},
	}
	for _, tc := range cases {
		t.Run(tc.fixture, func(t *testing.T) {
			if got := readAskMenu(t, readTestdata(t, "panes/askquestion/"+tc.fixture+".txt")); !proto.Equal(got, tc.want) {
				t.Fatalf("question = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestReadQuestionMenu_KeepsTheAskQuestionIdWhileSomeoneAnswers(t *testing.T) {
	q2 := readAskMenu(t, readTestdata(t, "panes/askquestion/two_q2.txt")).GetId()
	if got := readAskMenu(t, readTestdata(t, "panes/askquestion/two_q2_space.txt")).GetId(); got != q2 {
		t.Fatalf("id after Space = %q, want %q", got, q2)
	}
	if readAskMenu(t, readTestdata(t, "panes/askquestion/two_q1.txt")).GetId() == q2 {
		t.Fatal("two questions of one call read with the same id")
	}
}

func TestReadQuestionMenu_ReadsNoAskQuestionMenuThatIsNotShown(t *testing.T) {
	staleMenu := string(readTestdata(t, "panes/askquestion/single.txt"))
	cut := strings.LastIndex(staleMenu, "esc to cancel")
	cases := map[string][]byte{
		"answered":                  readTestdata(t, "panes/askquestion/two_answered.txt"),
		"an approval menu":          readTestdata(t, "panes/question-create-file.txt"),
		"the trust modal":           readTestdata(t, "panes/trust-dialog.txt"),
		"idle after a turn":         readTestdata(t, "panes/idle-after-turn.txt"),
		"a menu cut off its footer": []byte(staleMenu[:cut]),
		"an empty pane":             nil,
	}
	for name, pane := range cases {
		t.Run(name, func(t *testing.T) {
			if got := readAskMenu(t, pane); got != nil {
				t.Fatalf("question = %v, want none", got)
			}
		})
	}
}
