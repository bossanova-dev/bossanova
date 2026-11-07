package daemoncaps

import (
	"fmt"
	"slices"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"google.golang.org/protobuf/reflect/protoreflect"
)

// CommandAccess describes the authority a cloud-sent command requires.
type CommandAccess int

const (
	// AccessObserve reads daemon state without changing persisted state.
	AccessObserve CommandAccess = iota
	// AccessControl steers sessions or changes persisted local state.
	AccessControl
	// AccessHousekeeping changes internal state under authority verified locally.
	AccessHousekeeping
)

func (a CommandAccess) String() string {
	switch a {
	case AccessObserve:
		return "observe"
	case AccessControl:
		return "control"
	case AccessHousekeeping:
		return "housekeeping"
	default:
		return "unknown"
	}
}

var terminalOneof = (&pb.TerminalClientMessage{}).ProtoReflect().Descriptor().Oneofs().ByName("msg")

var commandAccess = accessTable(commandOneof, map[protoreflect.Name]CommandAccess{
	"stop":     AccessControl,
	"pause":    AccessControl,
	"resume":   AccessControl,
	"transfer": AccessControl,
	// Read-only dispatch discards payloads and only triggers authoritative re-reads.
	"webhook":                          AccessHousekeeping,
	"attach":                           AccessObserve,
	"transfer_confirmed":               AccessControl,
	"transfer_cancel":                  AccessControl,
	"wake_chat":                        AccessControl,
	"merge":                            AccessControl,
	"archive":                          AccessControl,
	"record_chat":                      AccessControl,
	"delete_chat":                      AccessControl,
	"create_session":                   AccessControl,
	"list_repos":                       AccessObserve,
	"list_agents":                      AccessObserve,
	"list_repo_prs":                    AccessObserve,
	"list_tracker_issues":              AccessObserve,
	"get_chat_transcript":              AccessObserve,
	"send_chat_message":                AccessControl,
	"switch_account":                   AccessControl,
	"list_accounts":                    AccessObserve,
	"get_repo":                         AccessObserve,
	"update_repo":                      AccessControl,
	"remove_repo":                      AccessControl,
	"list_cron_jobs":                   AccessObserve,
	"create_cron_job":                  AccessControl,
	"update_cron_job":                  AccessControl,
	"delete_cron_job":                  AccessControl,
	"run_cron_job_now":                 AccessControl,
	"add_account":                      AccessControl,
	"refresh_account":                  AccessControl,
	"update_account":                   AccessControl,
	"remove_account":                   AccessControl,
	"test_account":                     AccessControl,
	"list_chats":                       AccessObserve,
	"get_session_statuses":             AccessObserve,
	"list_check_snapshots":             AccessObserve,
	"list_plugins":                     AccessObserve,
	"get_cron_job":                     AccessObserve,
	"repair_doctor":                    AccessObserve,
	"close_session":                    AccessControl,
	"resurrect_session":                AccessControl,
	"remove_session":                   AccessControl,
	"empty_trash":                      AccessControl,
	"retry_session":                    AccessControl,
	"update_session":                   AccessControl,
	"link_session_pr":                  AccessControl,
	"update_chat_title":                AccessControl,
	"report_chat_status":               AccessControl,
	"create_github_callback":           AccessControl,
	"list_github_callbacks":            AccessObserve,
	"delete_github_callback":           AccessControl,
	"create_note":                      AccessControl,
	"get_note":                         AccessObserve,
	"list_notes":                       AccessObserve,
	"update_note":                      AccessControl,
	"delete_note":                      AccessControl,
	"broadcast":                        AccessControl,
	"get_chat_statuses":                AccessObserve,
	"command_cancel":                   AccessHousekeeping,
	"move_session":                     AccessControl,
	"launch_trigger_session":           AccessControl,
	"run_trigger_gate":                 AccessControl,
	"dismiss_session_alert":            AccessControl,
	"resume_session_run":               AccessControl,
	"operator_merge_session":           AccessControl,
	"apply_agent_account_sync":         AccessControl,
	"apply_agent_account_usage":        AccessControl,
	"agent_credential_available":       AccessHousekeeping,
	"agent_credential_devices_changed": AccessHousekeeping,
	"send_back_to_repair":              AccessControl,
	"get_session_timeline":             AccessControl,
})

var terminalAccess = accessTable(terminalOneof, map[protoreflect.Name]CommandAccess{
	"attach":        AccessObserve,
	"ready":         AccessObserve,
	"ping":          AccessObserve,
	"close":         AccessObserve,
	"input":         AccessControl,
	"resize":        AccessControl,
	"upload_start":  AccessControl,
	"upload_chunk":  AccessControl,
	"upload_finish": AccessControl,
	"upload_cancel": AccessControl,
})

func accessTable(oneof protoreflect.OneofDescriptor, names map[protoreflect.Name]CommandAccess) map[protoreflect.FieldNumber]CommandAccess {
	table := make(map[protoreflect.FieldNumber]CommandAccess, len(names))
	for name, access := range names {
		field := oneof.Fields().ByName(name)
		if field == nil {
			panic(fmt.Sprintf("unknown %s arm %s", oneof.FullName(), name))
		}
		table[field.Number()] = access
	}
	return table
}

// OrchestratorCommandAccess classifies a cloud-sent command. An unset or unknown
// arm returns false; callers must treat false as control (fail-closed).
func OrchestratorCommandAccess(cmd *pb.OrchestratorCommand) (CommandAccess, bool) {
	return messageAccess(cmd.ProtoReflect(), commandOneof, commandAccess)
}

// TerminalMessageAccess classifies a cloud-sent terminal message. An unset or
// unknown arm returns false; callers must treat false as control (fail-closed).
func TerminalMessageAccess(msg *pb.TerminalClientMessage) (CommandAccess, bool) {
	return messageAccess(msg.ProtoReflect(), terminalOneof, terminalAccess)
}

func messageAccess(msg protoreflect.Message, oneof protoreflect.OneofDescriptor, table map[protoreflect.FieldNumber]CommandAccess) (CommandAccess, bool) {
	field := msg.WhichOneof(oneof)
	if field == nil {
		return AccessControl, false
	}
	access, ok := table[field.Number()]
	if !ok {
		return AccessControl, false
	}
	return access, true
}

// ObserveCommandFields lists the observe and housekeeping command fields allowed
// while cloud control is read-only, sorted by field number.
func ObserveCommandFields() []int32 {
	fields := make([]int32, 0, len(commandAccess))
	for field, access := range commandAccess {
		if access == AccessObserve || access == AccessHousekeeping {
			fields = append(fields, int32(field))
		}
	}
	slices.Sort(fields)
	return fields
}

// CloudControlDisabledText tells the operator how to enable cloud control locally.
func CloudControlDisabledText(hostname string) string {
	return fmt.Sprintf("cloud control is read-only on %s; set `cloud_control` to `full` in that machine's local settings.json to allow it", hostname)
}
