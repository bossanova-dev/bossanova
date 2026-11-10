package upstream

import (
	"context"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
)

func redactSessionForCloud(s *pb.Session, mode pb.CloudControl) *pb.Session {
	if s == nil || mode != pb.CloudControl_CLOUD_CONTROL_PRIVATE {
		return s
	}
	redacted := proto.CloneOf(s)
	redacted.Plan = ""
	return redacted
}

func redactChatForCloud(c *pb.AgentChatMetadata, mode pb.CloudControl) *pb.AgentChatMetadata {
	if c == nil || mode != pb.CloudControl_CLOUD_CONTROL_PRIVATE {
		return c
	}
	redacted := proto.CloneOf(c)
	redacted.LastMessagePreview = ""
	return redacted
}

// cloudRedactor is scoped to one serialization, so a mixed snapshot resolves each
// repository policy once and shares the session ownership projection with chats.
type cloudRedactor struct {
	client   *StreamClient
	modes    map[string]pb.CloudControl
	sessions map[string]string
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
	mode := cloudControlProto(r.client.cloudControl)
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

// redactForCloud applies local ownership policy before any message leaves bossd,
// including nested entries in command results. It never mutates store-owned data.
func (r *cloudRedactor) redactForCloud(ctx context.Context, obj proto.Message) proto.Message {
	if obj == nil {
		return nil
	}
	switch value := obj.(type) {
	case *pb.Session:
		r.sessions[value.GetId()] = value.GetRepoId()
		return redactSessionForCloud(value, r.mode(ctx, value.GetRepoId()))
	case *pb.AgentChatMetadata:
		return redactChatForCloud(value, r.mode(ctx, r.sessionRepo(ctx, value.GetSessionId())))
	case *pb.DaemonSnapshot:
		for _, session := range value.GetSessions() {
			r.sessions[session.GetId()] = session.GetRepoId()
		}
	case *pb.CronJob:
		if r.mode(ctx, value.GetRepoId()) == pb.CloudControl_CLOUD_CONTROL_PRIVATE {
			redacted := proto.CloneOf(value)
			redacted.Prompt = ""
			redacted.GateCommand = ""
			return redacted
		}
	case *pb.Note:
		if r.mode(ctx, value.GetRepoId()) == pb.CloudControl_CLOUD_CONTROL_PRIVATE {
			redacted := proto.CloneOf(value)
			redacted.Body = ""
			return redacted
		}
	}
	cloned := proto.Clone(obj)
	msg := cloned.ProtoReflect()
	msg.Range(func(field protoreflect.FieldDescriptor, value protoreflect.Value) bool {
		if field.Message() == nil || field.IsMap() {
			return true
		}
		if field.IsList() {
			list := value.List()
			for i := 0; i < list.Len(); i++ {
				list.Set(i, protoreflect.ValueOfMessage(r.redactForCloud(ctx, list.Get(i).Message().Interface()).ProtoReflect()))
			}
		} else {
			msg.Set(field, protoreflect.ValueOfMessage(r.redactForCloud(ctx, value.Message().Interface()).ProtoReflect()))
		}
		return true
	})
	return cloned
}
