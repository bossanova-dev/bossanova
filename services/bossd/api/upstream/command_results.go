package upstream

import pb "github.com/recurser/bossalib/gen/bossanova/v1"

// commandOK builds a success CommandResult. session may be nil for
// commands whose response doesn't include a session payload (e.g.
// attach start).
func commandOK(cmdID string, session *pb.Session) *pb.DaemonEvent {
	result := &pb.CommandResult{CommandId: cmdID, Ok: true}
	if session != nil {
		result.Payload = &pb.CommandResult_Session{Session: session}
	}
	return &pb.DaemonEvent{Event: &pb.DaemonEvent_Result{Result: result}}
}

// commandErr wraps an error message into a failed CommandResult with
// ERROR_CODE_UNSPECIFIED. Use commandErrCode when the failure mode is
// known (e.g. NOT_FOUND) so bosso can map back to the right ConnectRPC
// code without parsing the human-readable message.
func commandErr(cmdID, msg string) *pb.DaemonEvent {
	return commandErrCode(cmdID, msg, pb.CommandResult_ERROR_CODE_UNSPECIFIED)
}

// commandErrCode is commandErr with a typed error_code attached.
func commandErrCode(cmdID, msg string, code pb.CommandResult_ErrorCode) *pb.DaemonEvent {
	return &pb.DaemonEvent{Event: &pb.DaemonEvent_Result{
		Result: &pb.CommandResult{CommandId: cmdID, Ok: false, Error: msg, ErrorCode: code},
	}}
}

// webhookAck wraps a webhook response. Distinct from CommandResult so
// the orchestrator's webhook correlator doesn't need to unpack the
// payload oneof.
func webhookAck(cmdID string, ok bool, errMsg string) *pb.DaemonEvent {
	return &pb.DaemonEvent{Event: &pb.DaemonEvent_Ack{
		Ack: &pb.WebhookAck{CommandId: cmdID, Ok: ok, Error: errMsg},
	}}
}
