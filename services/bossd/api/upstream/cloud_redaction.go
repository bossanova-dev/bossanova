package upstream

import (
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"google.golang.org/protobuf/proto"
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
