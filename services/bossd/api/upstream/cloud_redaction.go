package upstream

import (
	"context"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
)

type cloudRedactor struct {
	client     *StreamClient
	modes      map[string]pb.CloudControl
	sessions   map[string]string
	scope      []string
	registered map[string]struct{}
}

func (c *StreamClient) newCloudRedactor() *cloudRedactor {
	return &cloudRedactor{client: c, modes: make(map[string]pb.CloudControl), sessions: make(map[string]string)}
}

func (r *cloudRedactor) mode(ctx context.Context, repoID string) pb.CloudControl {
	if repoID == "" {
		return pb.CloudControl_CLOUD_CONTROL_PRIVATE
	}
	if mode, ok := r.modes[repoID]; ok {
		return mode
	}
	if !r.repoRegistered(ctx, repoID) {
		r.modes[repoID] = pb.CloudControl_CLOUD_CONTROL_PRIVATE
		return pb.CloudControl_CLOUD_CONTROL_PRIVATE
	}
	mode := pb.CloudControl_CLOUD_CONTROL_READ_ONLY
	if r.client.cloudControl != nil {
		resolved, _ := r.client.cloudControl.RepoMode(ctx, repoID)
		mode = targetModeProto(resolved)
	}
	r.modes[repoID] = mode
	return mode
}

func (r *cloudRedactor) sessionRepo(ctx context.Context, id string) string {
	if repo, ok := r.sessions[id]; ok {
		return repo
	}
	if r.client.targetObjects != nil {
		object, err := r.client.targetObjects.GetTarget(ctx, "session", id)
		if err == nil {
			r.sessions[id] = object.RepoID
			return object.RepoID
		}
	}
	return ""
}

func (r *cloudRedactor) redactForCloud(ctx context.Context, obj proto.Message) {
	if obj == nil || !obj.ProtoReflect().IsValid() {
		return
	}
	if r.redactContent(ctx, obj) {
		return
	}
	switch value := obj.(type) {
	case *pb.DaemonSnapshot:
		// RepoIds are origin URLs for webhook routing; the per-repo modes name
		// the registered repositories by the database id sessions carry.
		if value.RepoCloudControls != nil {
			ids := make([]string, 0, len(value.GetRepoCloudControls()))
			for _, entry := range value.GetRepoCloudControls() {
				ids = append(ids, entry.GetRepoId())
			}
			r.registerRepos(ids)
		}
		for _, session := range value.GetSessions() {
			r.sessions[session.GetId()] = session.GetRepoId()
		}
	}
	msg := obj.ProtoReflect()
	msg.Range(func(field protoreflect.FieldDescriptor, value protoreflect.Value) bool {
		if field.Message() == nil || field.IsMap() {
			return true
		}
		if field.IsList() {
			list := value.List()
			for i := 0; i < list.Len(); i++ {
				r.redactForCloud(ctx, list.Get(i).Message().Interface())
			}
		} else {
			r.redactForCloud(ctx, value.Message().Interface())
		}
		return true
	})
}

func (r *cloudRedactor) redactContent(ctx context.Context, obj proto.Message) bool {
	switch value := obj.(type) {
	case *pb.Session:
		r.sessions[value.GetId()] = value.GetRepoId()
		mode := r.mode(ctx, value.GetRepoId())
		// A repo-less session (a delete delta) has no effective mode to publish.
		if value.GetRepoId() != "" {
			value.CloudControl = mode
		}
		if mode == pb.CloudControl_CLOUD_CONTROL_PRIVATE {
			value.Plan = ""
		}
		return true
	case *pb.AgentChatMetadata:
		if r.mode(ctx, r.sessionRepo(ctx, value.GetSessionId())) == pb.CloudControl_CLOUD_CONTROL_PRIVATE {
			value.LastMessagePreview = ""
		}
		return true
	case *pb.GetChatTranscriptResponse:
		if r.scopePrivate(ctx) {
			value.Messages = nil
			value.FinalAssistantText = ""
			value.Reason = ""
		}
		return true
	case *pb.TrackerIssue:
		if r.scopePrivate(ctx) {
			value.Description = ""
		}
		return true
	case *pb.GitHubCallback:
		if r.mode(ctx, r.chatRepo(ctx, value.GetTargetChatId())) == pb.CloudControl_CLOUD_CONTROL_PRIVATE {
			value.Message = ""
		}
		return true
	case *pb.BroadcastEgress:
		mode := cloudControlProto(r.client.cloudControl)
		if value.GetOriginChatId() != "" {
			mode = r.mode(ctx, r.chatRepo(ctx, value.GetOriginChatId()))
		}
		if mode == pb.CloudControl_CLOUD_CONTROL_PRIVATE {
			value.Message = ""
		}
		return true
	case *pb.CronJob:
		if r.mode(ctx, value.GetRepoId()) == pb.CloudControl_CLOUD_CONTROL_PRIVATE {
			value.Prompt = ""
			value.GateCommand = ""
		}
		return true
	case *pb.Note:
		if r.mode(ctx, value.GetRepoId()) == pb.CloudControl_CLOUD_CONTROL_PRIVATE {
			value.Body = ""
		}
		return true
	}
	return false
}

func (r *cloudRedactor) scopePrivate(ctx context.Context) bool {
	if len(r.scope) == 0 {
		return true
	}
	for _, repo := range r.scope {
		if r.mode(ctx, repo) == pb.CloudControl_CLOUD_CONTROL_PRIVATE {
			return true
		}
	}
	return false
}

func (r *cloudRedactor) chatRepo(ctx context.Context, id string) string {
	if r.client.targetObjects != nil {
		object, err := r.client.targetObjects.GetTarget(ctx, "chat", id)
		if err == nil {
			return object.RepoID
		}
	}
	return ""
}

func (r *cloudRedactor) session(ctx context.Context, session *pb.Session) *pb.Session {
	cloned := proto.CloneOf(session)
	r.redactForCloud(ctx, cloned)
	return cloned
}
func (r *cloudRedactor) chat(ctx context.Context, chat *pb.AgentChatMetadata) *pb.AgentChatMetadata {
	cloned := proto.CloneOf(chat)
	r.redactForCloud(ctx, cloned)
	return cloned
}

func (r *cloudRedactor) registerRepos(ids []string) {
	r.registered = make(map[string]struct{}, len(ids))
	for _, id := range ids {
		r.registered[id] = struct{}{}
	}
}

func (r *cloudRedactor) repoRegistered(ctx context.Context, id string) bool {
	if r.registered != nil {
		if _, ok := r.registered[id]; !ok {
			return false
		}
	}
	if r.client.targetObjects != nil {
		object, err := r.client.targetObjects.GetTarget(ctx, "repo", id)
		return err == nil && object.RepoID == id
	}
	return true
}
