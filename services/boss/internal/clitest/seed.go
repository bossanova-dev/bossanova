package clitest

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/recurser/boss/internal/adapter/realstackrpc"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"google.golang.org/protobuf/proto"
)

// ID resolves a fixture key to the identifier assigned by the real daemon.
// Unknown values remain unchanged so missing-identifier scenarios reach bossd.
func (h *Harness) ID(key string) string {
	for _, ids := range []map[string]string{h.RepoIDs, h.SessionIDs, h.ChatIDs, h.CronJobIDs, h.AccountIDs, h.NoteIDs, h.CallbackIDs} {
		if id, ok := ids[key]; ok {
			return id
		}
	}
	return key
}

func (h *Harness) args(args []string) []string {
	result := make([]string, len(args))
	for i, arg := range args {
		if key, value, ok := strings.Cut(arg, "="); ok {
			result[i] = key + "=" + h.ID(value)
		} else {
			result[i] = h.ID(arg)
		}
	}
	return result
}

func (h *Harness) seed(t *testing.T, cfg harnessConfig) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	client := h.Stack.Client()
	for _, chat := range cfg.chats {
		response, err := client.RecordChat(ctx, realstackrpc.Request(&pb.RecordChatRequest{SessionId: h.ID(chat.SessionId), AgentSessionId: chat.AgentSessionId, Title: chat.Title, AgentName: proto.String("stub"), Role: chat.Role}))
		if err != nil {
			t.Fatalf("record fixture chat: %v", err)
		}
		h.ChatIDs[chat.Id] = response.Msg.Chat.AgentSessionId
	}
	for _, job := range cfg.cronJobs {
		response, err := client.CreateCronJob(ctx, realstackrpc.Request(&pb.CreateCronJobRequest{RepoId: h.ID(job.RepoId), Name: job.Name, Prompt: job.Prompt, Schedule: job.Schedule, Timezone: job.Timezone, IsEnabled: job.IsEnabled, AgentName: "stub", Model: job.Model, GateCommand: job.GateCommand, ShouldRunSetupCommand: &job.ShouldRunSetupCommand, IsZeroOutput: &job.IsZeroOutput, ConcurrencyPolicy: &job.ConcurrencyPolicy, ManagedBy: job.ManagedBy}))
		if err != nil {
			t.Fatalf("create fixture cron job: %v", err)
		}
		h.CronJobIDs[job.Id] = response.Msg.CronJob.Id
	}
	for _, account := range cfg.accounts {
		response, err := client.AddAgentAccount(ctx, realstackrpc.Request(&pb.AddAgentAccountRequest{Provider: account.Provider, Label: account.Label, Priority: account.Priority, Credential: []byte("fixture-credential-" + account.Id)}))
		if err != nil {
			t.Fatalf("create fixture account: %v", err)
		}
		id := response.Msg.Account.Id
		h.AccountIDs[account.Id] = id
		request := &pb.UpdateAgentAccountRequest{Id: id, AllowedModels: account.AllowedModels}
		if account.Status != "" {
			request.Status = &account.Status
		}
		if _, err := client.UpdateAgentAccount(ctx, realstackrpc.Request(request)); err != nil {
			t.Fatalf("configure fixture account: %v", err)
		}
	}
	for _, note := range cfg.notes {
		request := &pb.CreateNoteRequest{RepoId: h.ID(note.RepoId), Body: note.Body, Tags: note.Tags}
		if note.SessionId != "" {
			request.SessionId = proto.String(h.ID(note.SessionId))
		}
		if note.ChatId != "" {
			request.ChatId = proto.String(h.ID(note.ChatId))
		}
		response, err := client.CreateNote(ctx, realstackrpc.Request(request))
		if err != nil {
			t.Fatalf("create fixture note: %v", err)
		}
		h.NoteIDs[note.Id] = response.Msg.Note.Id
	}
	for _, callback := range cfg.githubCallbacks {
		response, err := client.CreateGitHubCallback(ctx, realstackrpc.Request(&pb.CreateGitHubCallbackRequest{GroupId: &callback.GroupId, TargetChatId: h.ID(callback.TargetChatId), RepoOwner: callback.RepoOwner, RepoName: callback.RepoName, PrNumber: callback.PrNumber, Trigger: callback.Trigger, Message: callback.Message, ExpiresAt: callback.ExpiresAt, ShouldRequireTransition: &callback.ShouldRequireTransition}))
		if err != nil {
			t.Fatalf("create fixture callback: %v", err)
		}
		h.CallbackIDs[callback.Id] = response.Msg.GithubCallback.Id
	}
}

// Repos reads registered repositories through the real daemon API.
func (h *Harness) Repos(t testing.TB) []*pb.Repo {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	response, err := h.Stack.Client().ListRepos(ctx, realstackrpc.Request(&pb.ListReposRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	return response.Msg.Repos
}

// Sessions reads active and archived sessions through the real daemon API.
func (h *Harness) Sessions(t testing.TB) []*pb.Session {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	response, err := h.Stack.Client().ListSessions(ctx, realstackrpc.Request(&pb.ListSessionsRequest{IncludeArchived: true}))
	if err != nil {
		t.Fatal(err)
	}
	return response.Msg.Sessions
}

// Notes reads persisted notes through the real daemon API.
func (h *Harness) Notes(t testing.TB) []*pb.Note {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	response, err := h.Stack.Client().ListNotes(ctx, realstackrpc.Request(&pb.ListNotesRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	return response.Msg.Notes
}
