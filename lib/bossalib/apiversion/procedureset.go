package apiversion

import (
	"sort"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"google.golang.org/protobuf/reflect/protoreflect"
)

// UnaryProceduresContainingCarrier returns the Connect procedure paths for the
// named Bossanova unary services whose response message transitively contains
// carrier.
func UnaryProceduresContainingCarrier(carrier protoreflect.FullName, serviceNames ...protoreflect.Name) []string {
	if len(serviceNames) == 0 {
		serviceNames = []protoreflect.Name{"OrchestratorService", "DaemonService"}
	}
	files := []protoreflect.FileDescriptor{
		pb.File_bossanova_v1_orchestrator_proto,
		pb.File_bossanova_v1_daemon_proto,
	}
	var procedures []string
	for _, serviceName := range serviceNames {
		for _, file := range files {
			service := file.Services().ByName(serviceName)
			if service == nil {
				continue
			}
			methods := service.Methods()
			for i := 0; i < methods.Len(); i++ {
				method := methods.Get(i)
				if method.IsStreamingClient() || method.IsStreamingServer() {
					continue
				}
				if messageContainsCarrier(method.Output(), carrier, map[protoreflect.FullName]struct{}{}) {
					procedures = append(procedures, "/"+string(service.FullName())+"/"+string(method.Name()))
				}
			}
		}
	}
	sort.Strings(procedures)
	return procedures
}

func messageContainsCarrier(msg protoreflect.MessageDescriptor, carrier protoreflect.FullName, seen map[protoreflect.FullName]struct{}) bool {
	if msg == nil {
		return false
	}
	if msg.FullName() == carrier {
		return true
	}
	if _, ok := seen[msg.FullName()]; ok {
		return false
	}
	seen[msg.FullName()] = struct{}{}

	fields := msg.Fields()
	for i := 0; i < fields.Len(); i++ {
		field := fields.Get(i)
		if field.Message() == nil {
			continue
		}
		if messageContainsCarrier(field.Message(), carrier, seen) {
			return true
		}
	}
	return false
}
