package server

import (
	"bytes"
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"connectrpc.com/connect"
	"github.com/rs/zerolog"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/models"
	"github.com/recurser/bossd/internal/domain"
)

// fakeChatAttachments is a ChatAttachmentResolver holding one uploader's
// finished uploads in one chat.
type fakeChatAttachments struct {
	chatID   string
	uploader string
	paths    map[string]string
	released []string
}

func (f *fakeChatAttachments) Attachments(chatID, uploaderID string, uploadIDs []string) ([]string, error) {
	var out []string
	for _, id := range uploadIDs {
		path, ok := f.paths[id]
		if !ok || chatID != f.chatID || uploaderID != f.uploader {
			return nil, errors.New("attachment " + id + " is unknown or expired")
		}
		out = append(out, path)
	}
	return out, nil
}

func (f *fakeChatAttachments) ReleaseAttachments(_, _ string, uploadIDs []string) {
	f.released = append(f.released, uploadIDs...)
}

const (
	attachmentPath1 = "/tmp/bossd-chat-uploads-501/upload-111.png"
	attachmentPath2 = "/tmp/bossd-chat-uploads-501/upload-222.pdf"
)

func newAttachmentSendServer(t *testing.T, chatID string, tmuxer *fakeTmuxClient) (*Server, *fakeChatAttachments) {
	t.Helper()
	chat := &domain.AgentChat{ID: "c1", AgentSessionID: "agent-1", SessionID: "s1"}
	sess := &models.Session{ID: "s1", RepoID: "r1", WorktreePath: t.TempDir()}
	s := newSendMessageTestServer(t, chat, sess, tmuxer)
	attachments := &fakeChatAttachments{chatID: chatID, uploader: "user-alice", paths: map[string]string{"up-1": attachmentPath1, "up-2": attachmentPath2}}
	s.chatAttachments = attachments
	return s, attachments
}

func sendWithAttachments(s *Server, message string, ids ...string) (*connect.Response[pb.SendChatMessageResponse], error) {
	return s.SendChatMessage(context.Background(), connect.NewRequest(&pb.SendChatMessageRequest{
		AgentSessionId:      "agent-1",
		ActingUserId:        "user-alice",
		Message:             message,
		Submit:              true,
		AttachmentUploadIds: ids,
	}))
}

func typedTexts(tmuxer *fakeTmuxClient) []string {
	tmuxer.mu.Lock()
	defer tmuxer.mu.Unlock()
	var out []string
	for _, m := range tmuxer.sentMessages {
		out = append(out, m.text)
	}
	return out
}

func TestSendChatMessage_AttachmentPathsAreTypedAheadOfTheMessage(t *testing.T) {
	cases := []struct {
		name    string
		message string
		ids     []string
		want    string
	}{
		{"one file", "what is in this?", []string{"up-1"}, attachmentPath1 + " what is in this?"},
		{"two files in order", "compare these", []string{"up-2", "up-1"}, attachmentPath2 + " " + attachmentPath1 + " compare these"},
		{"files and no text", "", []string{"up-1", "up-2"}, attachmentPath1 + " " + attachmentPath2},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tmuxer := &fakeTmuxClient{available: true, hasSession: true}
			s, attachments := newAttachmentSendServer(t, "agent-1", tmuxer)

			resp, err := sendWithAttachments(s, tc.message, tc.ids...)
			if err != nil {
				t.Fatalf("send: %v", err)
			}
			if !resp.Msg.GetDelivered() {
				t.Fatalf("send not delivered: %+v", resp.Msg)
			}
			if got := typedTexts(tmuxer); len(got) != 1 || got[0] != tc.want {
				t.Fatalf("typed %q, want [%q]", got, tc.want)
			}
			if !slices.Equal(attachments.released, tc.ids) {
				t.Fatalf("released %q, want %q", attachments.released, tc.ids)
			}
		})
	}
}

func TestSendChatMessage_UnresolvedAttachmentTypesNothing(t *testing.T) {
	cases := map[string]struct {
		owner string
		ids   []string
	}{
		"unknown id":     {owner: "agent-1", ids: []string{"up-1", "up-missing"}},
		"another chat's": {owner: "agent-2", ids: []string{"up-1"}},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			tmuxer := &fakeTmuxClient{available: true, hasSession: true}
			s, attachments := newAttachmentSendServer(t, tc.owner, tmuxer)

			_, err := sendWithAttachments(s, "look", tc.ids...)
			if connect.CodeOf(err) != connect.CodeFailedPrecondition || !strings.Contains(err.Error(), "unknown or expired") {
				t.Fatalf("err = %v, want a FailedPrecondition naming the unknown attachment", err)
			}
			if got := typedTexts(tmuxer); len(got) != 0 {
				t.Fatalf("a refused send typed %q", got)
			}
			if len(attachments.released) != 0 {
				t.Fatalf("a refused send released %q", attachments.released)
			}
		})
	}
}

func TestSendChatMessage_AnotherUsersAttachmentTypesNothing(t *testing.T) {
	tmuxer := &fakeTmuxClient{available: true, hasSession: true}
	s, attachments := newAttachmentSendServer(t, "agent-1", tmuxer)

	_, err := s.SendChatMessage(context.Background(), connect.NewRequest(&pb.SendChatMessageRequest{
		AgentSessionId:      "agent-1",
		ActingUserId:        "user-bob",
		Message:             "look",
		Submit:              true,
		AttachmentUploadIds: []string{"up-1"},
	}))
	if connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Fatalf("err = %v, want FailedPrecondition for another user's upload", err)
	}
	if got := typedTexts(tmuxer); len(got) != 0 || len(attachments.released) != 0 {
		t.Fatalf("typed %q and released %q for another user's upload", got, attachments.released)
	}
}

func TestSendChatMessage_AttachmentsNeedAnUploadManager(t *testing.T) {
	tmuxer := &fakeTmuxClient{available: true, hasSession: true}
	s, _ := newAttachmentSendServer(t, "agent-1", tmuxer)
	s.chatAttachments = nil

	if _, err := sendWithAttachments(s, "look", "up-1"); connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Fatalf("err = %v, want FailedPrecondition", err)
	}
	if got := typedTexts(tmuxer); len(got) != 0 {
		t.Fatalf("typed %q without an upload manager", got)
	}
}

func TestSendChatMessage_AttachmentSendIsPastedAsProse(t *testing.T) {
	tmuxer := &fakeTmuxClient{available: true, hasSession: true}
	s, _ := newAttachmentSendServer(t, "agent-1", tmuxer)

	if _, err := sendWithAttachments(s, "plain"); err != nil {
		t.Fatalf("plain send: %v", err)
	}
	plainProse := tmuxer.lastProse
	if _, err := sendWithAttachments(s, "look", "up-1"); err != nil {
		t.Fatalf("attachment send: %v", err)
	}
	if plainProse || !tmuxer.lastProse {
		t.Fatalf("Prose = %v without attachments and %v with them, want false then true", plainProse, tmuxer.lastProse)
	}
}

// paneQuotingError is a not-submitted verdict whose text quotes the pane, as
// the tmux verifier's errors do.
type paneQuotingError string

func (e paneQuotingError) Error() string                       { return string(e) }
func (e paneQuotingError) SubmitOutcome() domain.SubmitOutcome { return domain.OutcomeNotSubmitted }

// TestSendChatMessage_FailedAttachmentSendWithholdsThePaneText: a verifier
// error quotes the composer, which shows the typed paths. The send still
// reports its delivery state, but no path leaves the daemon.
func TestSendChatMessage_FailedAttachmentSendWithholdsThePaneText(t *testing.T) {
	quoted := paneQuotingError("the composer did not draw the typed line; it last showed \"" + attachmentPath1 + " look\"")
	tmuxer := &fakeTmuxClient{available: true, hasSession: true, sendMessageErr: quoted}
	s, _ := newAttachmentSendServer(t, "agent-1", tmuxer)

	_, err := sendWithAttachments(s, "look", "up-1")
	if connect.CodeOf(err) != connect.CodeInternal || !strings.Contains(err.Error(), "DELIVERY_NOT_SUBMITTED") {
		t.Fatalf("err = %v, want the not-submitted delivery failure", err)
	}
	if strings.Contains(err.Error(), attachmentPath1) || strings.Contains(err.Error(), "bossd-chat-uploads") {
		t.Fatalf("the send error carries an attachment path: %v", err)
	}
}

func TestSendChatMessage_FailedSendKeepsItsAttachments(t *testing.T) {
	tmuxer := &fakeTmuxClient{available: true, hasSession: true, sendMessageErr: errTestDelivery}
	s, attachments := newAttachmentSendServer(t, "agent-1", tmuxer)

	if _, err := sendWithAttachments(s, "look", "up-1"); err == nil {
		t.Fatal("expected the failed delivery to surface")
	}
	if len(attachments.released) != 0 {
		t.Fatalf("a failed send spent its attachments %q; the retry would lose them", attachments.released)
	}
}

func TestSendChatMessage_AttachmentPathsStayOutOfTheLog(t *testing.T) {
	tmuxer := &fakeTmuxClient{available: true, hasSession: true}
	s, _ := newAttachmentSendServer(t, "agent-1", tmuxer)
	var buf bytes.Buffer
	s.logger = zerolog.New(&buf)

	if _, err := sendWithAttachments(s, "look", "up-1", "up-2"); err != nil {
		t.Fatalf("send: %v", err)
	}
	if _, err := sendWithAttachments(s, "look", "up-missing"); err == nil {
		t.Fatal("expected the unknown id to refuse the send")
	}
	for _, path := range []string{attachmentPath1, attachmentPath2, "bossd-chat-uploads"} {
		if strings.Contains(buf.String(), path) {
			t.Fatalf("log carries the attachment path %q: %s", path, buf.String())
		}
	}
}
