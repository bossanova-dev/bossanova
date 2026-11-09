package upstream

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/kit/config"
	"github.com/recurser/bossalib/kit/daemoncaps"
	"github.com/rs/zerolog"
	"google.golang.org/protobuf/reflect/protoreflect"
)

type targetPolicy struct {
	machine config.CloudControlMode
	repos   map[string]config.CloudControlMode
}

func (p targetPolicy) Mode() config.CloudControlMode { return p.machine }
func (p targetPolicy) RepoMode(_ context.Context, id string) (config.CloudControlMode, config.CloudControlSource) {
	if mode, ok := p.repos[id]; ok {
		return mode, config.CloudControlSourceGrant
	}
	return p.machine, config.CloudControlSourceMachine
}

type targetFixture struct {
	objects map[targetKey]CommandTargetObject
}

func (s *targetFixture) GetTarget(_ context.Context, kind, id string) (CommandTargetObject, error) {
	object, ok := s.objects[targetKey{kind, id}]
	if !ok {
		return CommandTargetObject{}, errTargetNotFound
	}
	return object, nil
}
func (s *targetFixture) ListTargetRepos(context.Context) ([]CommandTargetObject, error) {
	var repos []CommandTargetObject
	for key, object := range s.objects {
		if key.kind == "repo" {
			repos = append(repos, object)
		}
	}
	return repos, nil
}
func targetClient() (*StreamClient, *fakeCommandHandler, *targetFixture) {
	handler := &fakeCommandHandler{session: &pb.Session{Id: "sa"}}
	stores := &targetFixture{objects: map[targetKey]CommandTargetObject{
		{"repo", "a"}: {RepoID: "a", Name: "acme/granted"}, {"repo", "b"}: {RepoID: "b", Name: "acme/protected"},
		{"session", "sa"}: {RepoID: "a"}, {"session", "sb"}: {RepoID: "b"},
		{"chat", "ca"}: {RepoID: "a", SessionID: "sa"}, {"chat", "cb"}: {RepoID: "b", SessionID: "sb"},
		{"cron", "ja"}: {RepoID: "a"}, {"cron", "jb"}: {RepoID: "b"},
		{"note", "na"}: {RepoID: "a"}, {"note", "nb"}: {RepoID: "b"},
	}}
	client := NewStreamClient(StreamClientConfig{Logger: zerolog.Nop(), CommandHandler: handler, CloudControl: targetPolicy{machine: config.CloudControlReadOnly, repos: map[string]config.CloudControlMode{"a": config.CloudControlFull}}, TargetObjects: stores})
	return client, handler, stores
}
func TestRepoTargetDispatch(t *testing.T) {
	client, handler, _ := targetClient()
	for _, id := range []string{"sa", "sb", "missing"} {
		result := client.dispatchCommand(context.Background(), &pb.OrchestratorCommand{Cmd: &pb.OrchestratorCommand_Stop{Stop: &pb.StopSessionCommand{SessionId: id}}}, nil).GetResult()
		if result.GetOk() != (id == "sa") {
			t.Fatalf("%s: %v", id, result)
		}
		if id != "sa" && result.GetErrorCode() != pb.CommandResult_ERROR_CODE_CLOUD_CONTROL_DISABLED {
			t.Fatalf("%s: %v", id, result)
		}
	}
	if handler.stopCalls.Load() != 1 {
		t.Fatalf("handler calls=%d", handler.stopCalls.Load())
	}
}
func TestRepoTargetRejectsSplicedReferences_Notes_AC237801(t *testing.T) {
	client, _, _ := targetClient()
	for _, cmd := range []*pb.CreateNoteCommand{
		{RepoId: "a", SessionId: targetString("sb")}, {RepoId: "a", ChatId: targetString("cb")},
		{RepoId: "a", SessionId: targetString("missing")}, {RepoId: "missing"},
	} {
		result := client.dispatchCommand(context.Background(), &pb.OrchestratorCommand{Cmd: &pb.OrchestratorCommand_CreateNote{CreateNote: cmd}}, nil).GetResult()
		if result.GetErrorCode() != pb.CommandResult_ERROR_CODE_CLOUD_CONTROL_DISABLED {
			t.Fatalf("%v: %v", cmd, result)
		}
	}
}

func targetString(s string) *string { return &s }

func TestRepoTargetMoveSessionScope(t *testing.T) {
	for _, tc := range []struct {
		name, repo     string
		machine, other config.CloudControlMode
		allowed        bool
	}{
		{"global protected repo", "", config.CloudControlReadOnly, config.CloudControlReadOnly, false},
		{"scoped grant", "a", config.CloudControlReadOnly, config.CloudControlReadOnly, true},
		{"forged scope", "b", config.CloudControlFull, config.CloudControlFull, false},
		{"global grants", "", config.CloudControlReadOnly, config.CloudControlFull, true},
		{"global machine full", "", config.CloudControlFull, config.CloudControlFull, true},
		{"global restrictive override", "", config.CloudControlFull, config.CloudControlPrivate, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client, _, _ := targetClient()
			client.cloudControl = targetPolicy{machine: tc.machine, repos: map[string]config.CloudControlMode{"a": config.CloudControlFull, "b": tc.other}}
			if tc.name == "global machine full" {
				client.cloudControl = targetPolicy{machine: tc.machine}
			}
			event := client.dispatchCommand(context.Background(), &pb.OrchestratorCommand{
				Cmd: &pb.OrchestratorCommand_MoveSession{MoveSession: &pb.MoveSessionCommand{SessionId: "sa", RepoId: targetString(tc.repo), Direction: pb.MoveDirection_MOVE_DIRECTION_UP}},
			}, nil)
			if event.GetResult().GetOk() != tc.allowed {
				t.Fatalf("allowed=%v: %v", tc.allowed, event)
			}
		})
	}
}

func TestRepoTargetChatOnlyTranscript(t *testing.T) {
	for _, sessionID := range []string{"", "sa", "sb", "missing"} {
		t.Run("session="+sessionID, func(t *testing.T) {
			client, handler, _ := targetClient()
			handler.transcript = &pb.GetChatTranscriptResponse{Exists: true}
			out := make(chan *pb.DaemonEvent, 1)
			event := client.dispatchCommand(context.Background(), &pb.OrchestratorCommand{
				Cmd: &pb.OrchestratorCommand_GetChatTranscript{GetChatTranscript: &pb.GetChatTranscriptCommand{
					SessionId: sessionID, AgentSessionId: "ca",
				}},
			}, out)
			if event == nil {
				event = recvEvent(t, out)
			}
			result := event.GetResult()
			allowed := sessionID == "" || sessionID == "sa"
			if result.GetOk() != allowed {
				t.Fatalf("allowed=%v: %v", allowed, result)
			}
			if allowed && !result.GetGetChatTranscript().GetExists() {
				t.Fatalf("transcript handler was not reached: %v", result)
			}
			if !allowed && result.GetErrorCode() != pb.CommandResult_ERROR_CODE_CLOUD_CONTROL_DISABLED {
				t.Fatalf("mismatched session escaped authorization: %v", result)
			}
		})
	}
}

func TestRepoTargetStoredObjects_ScheduledJobs_AC237801(t *testing.T) {
	for _, repo := range []string{"a", "b", "missing"} {
		for _, kind := range []string{"run_cron_job_now", "update_cron_job", "update_note", "delete_note"} {
			t.Run(repo+"/"+kind, func(t *testing.T) {
				client, _, _ := targetClient()
				cmd := &pb.OrchestratorCommand{}
				switch kind {
				case "run_cron_job_now":
					cmd.Cmd = &pb.OrchestratorCommand_RunCronJobNow{RunCronJobNow: &pb.RunCronJobNowCommand{Id: "j" + repo}}
				case "update_cron_job":
					cmd.Cmd = &pb.OrchestratorCommand_UpdateCronJob{UpdateCronJob: &pb.UpdateCronJobCommand{Id: "j" + repo}}
				case "update_note":
					cmd.Cmd = &pb.OrchestratorCommand_UpdateNote{UpdateNote: &pb.UpdateNoteCommand{Id: "n" + repo}}
				case "delete_note":
					cmd.Cmd = &pb.OrchestratorCommand_DeleteNote{DeleteNote: &pb.DeleteNoteCommand{Id: "n" + repo}}
				}
				out := make(chan *pb.DaemonEvent, 1)
				event := client.dispatchCommand(context.Background(), cmd, out)
				if event == nil {
					event = recvEvent(t, out)
				}
				if event.GetResult().GetOk() != (repo == "a") {
					t.Fatalf("result=%v", event)
				}
			})
		}
	}
}

func TestRepoTargetMachineAccountsAndFanout(t *testing.T) {
	fields := (&pb.OrchestratorCommand{}).ProtoReflect().Descriptor().Oneofs().ByName("cmd").Fields()
	for _, name := range []string{"add_account", "refresh_account", "update_account", "remove_account", "test_account", "apply_agent_account_sync", "apply_agent_account_usage", "broadcast", "empty_trash"} {
		t.Run(name, func(t *testing.T) {
			client, _, _ := targetClient()
			field := fields.ByName(protoreflect.Name(name))
			cmd := &pb.OrchestratorCommand{}
			cmd.ProtoReflect().Set(field, cmd.ProtoReflect().NewField(field))
			target, err := client.resolveTarget(context.Background(), cmd)
			if err != nil {
				t.Fatal(err)
			}
			if daemoncaps.CloudControlAllows(targetModeProto(target.mode), cmd) {
				t.Fatal("repo grant enabled machine action")
			}
			client.cloudControl = targetPolicy{machine: config.CloudControlFull, repos: map[string]config.CloudControlMode{}}
			target, err = client.resolveTarget(context.Background(), cmd)
			if err != nil || !daemoncaps.CloudControlAllows(targetModeProto(target.mode), cmd) {
				t.Fatalf("machine full did not enable command: target=%v err=%v", target, err)
			}
			client.cloudControl = targetPolicy{machine: config.CloudControlFull, repos: map[string]config.CloudControlMode{"b": config.CloudControlReadOnly}}
			target, err = client.resolveTarget(context.Background(), cmd)
			if err != nil {
				t.Fatal(err)
			}
			if (name == "broadcast" || name == "empty_trash") && daemoncaps.CloudControlAllows(targetModeProto(target.mode), cmd) {
				t.Fatal("fanout bypassed protected repo")
			}
		})
	}
}

func TestRepoTargetPrivateLists(t *testing.T) {
	client, _, _ := targetClient()
	client.cloudControl = targetPolicy{machine: config.CloudControlReadOnly, repos: map[string]config.CloudControlMode{"a": config.CloudControlPrivate}}
	for _, cmd := range []*pb.OrchestratorCommand{
		{Cmd: &pb.OrchestratorCommand_ListNotes{ListNotes: &pb.ListNotesCommand{}}},
		{Cmd: &pb.OrchestratorCommand_ListCronJobs{ListCronJobs: &pb.ListCronJobsCommand{}}},
		{Cmd: &pb.OrchestratorCommand_ListGithubCallbacks{ListGithubCallbacks: &pb.ListGitHubCallbacksCommand{}}},
	} {
		event := client.dispatchCommand(context.Background(), cmd, nil)
		if event.GetResult().GetErrorCode() != pb.CommandResult_ERROR_CODE_CLOUD_CONTROL_DISABLED {
			t.Fatalf("result=%v", event)
		}
	}
}

func TestRepoTargetOwnershipRechecked(t *testing.T) {
	client, _, store := targetClient()
	cmd := &pb.OrchestratorCommand{Cmd: &pb.OrchestratorCommand_UpdateNote{UpdateNote: &pb.UpdateNoteCommand{Id: "na"}}}
	target, err := client.resolveTarget(context.Background(), cmd)
	if err != nil {
		t.Fatal(err)
	}
	ctx := client.authorizeTargetContext(context.Background(), target, cmd)
	store.objects[targetKey{"note", "na"}] = CommandTargetObject{RepoID: "b"}
	if assertCommandTarget(ctx) == nil {
		t.Fatal("ownership change survived execution recheck")
	}
}

type targetIdentityFixture struct {
	repoID   string
	identity config.RepoIdentity
	err      error
}

func (f targetIdentityFixture) Identity(_ context.Context, id string) (config.RepoIdentity, error) {
	if f.repoID != "" && id != f.repoID {
		return config.RepoIdentity{}, errors.New("unknown identity")
	}
	return f.identity, f.err
}
func TestRepoTargetLocalStrictGrantReload(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")
	t.Setenv("BOSS_SETTINGS_PATH", path)
	checkout := t.TempDir()
	policy := NewCloudControlPolicy(zerolog.Nop())
	policy.RepoIdentities = targetIdentityFixture{identity: config.RepoIdentity{Host: "github.com", ProviderRepoID: 42, IsIDVerified: true, LocalPath: checkout}}
	grant := config.CloudControlRepoGrant{Host: "github.com", RepoID: 42, Checkout: checkout, Mode: "full"}
	save := func(settings config.Settings) {
		t.Helper()
		if err := config.SaveTo(path, settings); err != nil {
			t.Fatal(err)
		}
	}
	save(config.Settings{CloudControl: "read-only", CloudControlRepos: []config.CloudControlRepoGrant{grant}})
	mode, source := policy.RepoMode(context.Background(), "a")
	if mode != config.CloudControlFull || source != config.CloudControlSourceGrant {
		t.Fatalf("strict grant: %s %s", mode, source)
	}
	if policy.Mode() != config.CloudControlReadOnly {
		t.Fatal("repo grant widened machine")
	}
	grant.Mode = "private"
	save(config.Settings{CloudControl: "read-only", CloudControlRepos: []config.CloudControlRepoGrant{grant}})
	mode, _ = policy.RepoMode(context.Background(), "a")
	if mode != config.CloudControlPrivate {
		t.Fatal("revocation was not reloaded")
	}
	policy.RepoIdentities = targetIdentityFixture{err: errors.New("unavailable")}
	mode, _ = policy.RepoMode(context.Background(), "a")
	if mode != config.CloudControlReadOnly {
		t.Fatal("failed identity widened authority")
	}
	if policy.UnscopedMode() != config.CloudControlPrivate {
		t.Fatal("unregistered private grant exposed cross-repo content")
	}
}

func TestRepoTargetSwitchAccountNeedsBothAuthorities(t *testing.T) {
	client, _, _ := targetClient()
	cmd := &pb.OrchestratorCommand{Cmd: &pb.OrchestratorCommand_SwitchAccount{SwitchAccount: &pb.SwitchAgentAccountCommand{SessionId: "sa", AgentSessionId: "ca"}}}
	for _, tc := range []struct {
		machine, repo config.CloudControlMode
		allowed       bool
	}{
		{config.CloudControlReadOnly, config.CloudControlFull, false},
		{config.CloudControlFull, config.CloudControlReadOnly, false},
		{config.CloudControlFull, config.CloudControlFull, true},
	} {
		client.cloudControl = targetPolicy{machine: tc.machine, repos: map[string]config.CloudControlMode{"a": tc.repo}}
		target, err := client.resolveTarget(context.Background(), cmd)
		if err != nil || daemoncaps.CloudControlAllows(targetModeProto(target.mode), cmd) != tc.allowed {
			t.Fatalf("%+v: target=%v err=%v", tc, target, err)
		}
	}
}

func TestRepoTargetDispatchWithStrictLocalGrant(t *testing.T) {
	client, handler, _ := targetClient()
	checkout := t.TempDir()
	path := filepath.Join(t.TempDir(), "settings.json")
	t.Setenv("BOSS_SETTINGS_PATH", path)
	settings := config.Settings{CloudControl: "read-only", CloudControlRepos: []config.CloudControlRepoGrant{{Host: "github.com", RepoID: 42, Checkout: checkout, Mode: "full"}}}
	if err := config.SaveTo(path, settings); err != nil {
		t.Fatal(err)
	}
	policy := NewCloudControlPolicy(zerolog.Nop())
	policy.RepoIdentities = targetIdentityFixture{repoID: "a", identity: config.RepoIdentity{Host: "github.com", ProviderRepoID: 42, IsIDVerified: true, LocalPath: checkout}}
	client.cloudControl = policy
	for _, id := range []string{"sa", "sb"} {
		event := client.dispatchCommand(context.Background(), &pb.OrchestratorCommand{Cmd: &pb.OrchestratorCommand_Stop{Stop: &pb.StopSessionCommand{SessionId: id}}}, nil)
		if event.GetResult().GetOk() != (id == "sa") {
			t.Fatalf("%s: %v", id, event)
		}
	}
	if handler.stopCalls.Load() != 1 {
		t.Fatal("protected repo reached handler")
	}
}
