package apiversion

import (
	"reflect"
	"testing"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/gen/bossanova/v1/bossanovav1connect"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/descriptorpb"
)

func TestUnaryProceduresContainingCarrier_SessionSet(t *testing.T) {
	got := UnaryProceduresContainingCarrier(
		(&pb.Session{}).ProtoReflect().Descriptor().FullName(),
		"OrchestratorService",
	)
	want := []string{
		bossanovav1connect.OrchestratorServiceProxyArchiveSessionProcedure,
		bossanovav1connect.OrchestratorServiceProxyCloseSessionProcedure,
		bossanovav1connect.OrchestratorServiceProxyGetSessionProcedure,
		bossanovav1connect.OrchestratorServiceProxyLinkSessionPRProcedure,
		bossanovav1connect.OrchestratorServiceProxyListSessionsProcedure,
		bossanovav1connect.OrchestratorServiceProxyMergeSessionProcedure,
		bossanovav1connect.OrchestratorServiceProxyPauseSessionProcedure,
		bossanovav1connect.OrchestratorServiceProxyResumeSessionProcedure,
		bossanovav1connect.OrchestratorServiceProxyResurrectSessionProcedure,
		bossanovav1connect.OrchestratorServiceProxyRetrySessionProcedure,
		bossanovav1connect.OrchestratorServiceProxyRunCronJobNowProcedure,
		bossanovav1connect.OrchestratorServiceProxyStopSessionProcedure,
		bossanovav1connect.OrchestratorServiceProxyUpdateSessionProcedure,
		bossanovav1connect.OrchestratorServiceTransferSessionProcedure,
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("UnaryProceduresContainingCarrier(Session) = %#v, want %#v", got, want)
	}
}

func TestUnaryProceduresContainingCarrier_StatusSets(t *testing.T) {
	sessionStatus := UnaryProceduresContainingCarrier(
		(&pb.SessionStatusEntry{}).ProtoReflect().Descriptor().FullName(),
		"OrchestratorService",
		"DaemonService",
	)
	wantSessionStatus := []string{
		bossanovav1connect.DaemonServiceGetSessionStatusesProcedure,
		bossanovav1connect.OrchestratorServiceProxyGetSessionStatusesProcedure,
	}
	if !reflect.DeepEqual(sessionStatus, wantSessionStatus) {
		t.Fatalf("UnaryProceduresContainingCarrier(SessionStatusEntry) = %#v, want %#v", sessionStatus, wantSessionStatus)
	}

	chatStatus := UnaryProceduresContainingCarrier(
		(&pb.ChatStatusEntry{}).ProtoReflect().Descriptor().FullName(),
		"OrchestratorService",
		"DaemonService",
	)
	wantChatStatus := []string{
		bossanovav1connect.DaemonServiceGetChatStatusesProcedure,
		bossanovav1connect.OrchestratorServiceProxyGetChatStatusesProcedure,
	}
	if !reflect.DeepEqual(chatStatus, wantChatStatus) {
		t.Fatalf("UnaryProceduresContainingCarrier(ChatStatusEntry) = %#v, want %#v", chatStatus, wantChatStatus)
	}
}

func TestMessageContainsCarrier_TerminatesOnCycles(t *testing.T) {
	name := func(s string) *string { return &s }
	label := func(n int32) *int32 { return &n }
	file, err := protodesc.NewFile(&descriptorpb.FileDescriptorProto{
		Syntax:  name("proto3"),
		Name:    name("cycle.proto"),
		Package: name("cycle"),
		MessageType: []*descriptorpb.DescriptorProto{
			{
				Name: name("Carrier"),
			},
			{
				Name: name("A"),
				Field: []*descriptorpb.FieldDescriptorProto{
					{
						Name:     name("b"),
						Number:   label(1),
						Label:    descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(),
						Type:     descriptorpb.FieldDescriptorProto_TYPE_MESSAGE.Enum(),
						TypeName: name(".cycle.B"),
					},
				},
			},
			{
				Name: name("B"),
				Field: []*descriptorpb.FieldDescriptorProto{
					{
						Name:     name("a"),
						Number:   label(1),
						Label:    descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(),
						Type:     descriptorpb.FieldDescriptorProto_TYPE_MESSAGE.Enum(),
						TypeName: name(".cycle.A"),
					},
					{
						Name:     name("carrier"),
						Number:   label(2),
						Label:    descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(),
						Type:     descriptorpb.FieldDescriptorProto_TYPE_MESSAGE.Enum(),
						TypeName: name(".cycle.Carrier"),
					},
				},
			},
		},
	}, nil)
	if err != nil {
		t.Fatalf("NewFile: %v", err)
	}
	msgA := file.Messages().ByName("A")
	if !messageContainsCarrier(msgA, protoreflect.FullName("cycle.Carrier"), map[protoreflect.FullName]struct{}{}) {
		t.Fatal("messageContainsCarrier(A, Carrier) = false, want true through cycle")
	}
	if messageContainsCarrier(msgA, protoreflect.FullName("cycle.Missing"), map[protoreflect.FullName]struct{}{}) {
		t.Fatal("messageContainsCarrier(A, Missing) = true, want false")
	}
}
