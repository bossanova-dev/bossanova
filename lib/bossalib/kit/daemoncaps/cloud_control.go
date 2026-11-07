package daemoncaps

import (
	"fmt"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"google.golang.org/protobuf/reflect/protoreflect"
)

const (
	privateRemedyMode    = pb.CloudControl_CLOUD_CONTROL_READ_ONLY
	privateRemedySetting = "read-only"
)

var commandContent = contentTable(commandOneof, map[protoreflect.Name]bool{
	"attach":                true,
	"list_repos":            false,
	"list_agents":           false,
	"list_repo_prs":         false,
	"list_tracker_issues":   true,
	"get_chat_transcript":   true,
	"list_accounts":         false,
	"get_repo":              false,
	"list_cron_jobs":        true,
	"list_chats":            false,
	"get_session_statuses":  false,
	"list_check_snapshots":  false,
	"list_plugins":          false,
	"get_cron_job":          true,
	"repair_doctor":         false,
	"list_github_callbacks": true,
	"get_note":              true,
	"list_notes":            true,
	"get_chat_statuses":     false,
})

var terminalContent = contentTable(terminalOneof, map[protoreflect.Name]bool{
	"attach": true,
	"ready":  false,
	"ping":   false,
	"close":  false,
})

func contentTable(oneof protoreflect.OneofDescriptor, names map[protoreflect.Name]bool) map[protoreflect.FieldNumber]bool {
	table := make(map[protoreflect.FieldNumber]bool, len(names))
	for name, content := range names {
		field := oneof.Fields().ByName(name)
		if field == nil {
			panic(fmt.Sprintf("unknown %s arm %s", oneof.FullName(), name))
		}
		table[field.Number()] = content
	}
	return table
}

// OrchestratorCommandCarriesContent reports whether an observe command returns
// agent content. Unset or unclassified arms fail closed.
func OrchestratorCommandCarriesContent(cmd *pb.OrchestratorCommand) bool {
	return messageCarriesContent(cmd.ProtoReflect(), commandOneof, commandContent)
}

// TerminalMessageCarriesContent reports whether a terminal observe message
// exposes content. Unset or unclassified arms fail closed.
func TerminalMessageCarriesContent(msg *pb.TerminalClientMessage) bool {
	return messageCarriesContent(msg.ProtoReflect(), terminalOneof, terminalContent)
}

func messageCarriesContent(msg protoreflect.Message, oneof protoreflect.OneofDescriptor, table map[protoreflect.FieldNumber]bool) bool {
	field := msg.WhichOneof(oneof)
	if field == nil {
		return true
	}
	content, ok := table[field.Number()]
	return !ok || content
}

// CloudControlAllows is the shared daemon/cloud command decision. Unspecified
// preserves compatibility with old daemons; restricted modes fail closed.
func CloudControlAllows(mode pb.CloudControl, cmd *pb.OrchestratorCommand) bool {
	access, ok := OrchestratorCommandAccess(cmd)
	return cloudControlAllows(mode, access, ok, OrchestratorCommandCarriesContent(cmd))
}

// CloudControlRefusesControl reports whether mode refuses cmd because cmd is
// control (or unclassified, which the daemon treats as control). bosso uses it
// to answer such a command itself with CloudControlDisabledText; content
// refusals under private mode are left to the daemon.
func CloudControlRefusesControl(mode pb.CloudControl, cmd *pb.OrchestratorCommand) bool {
	if mode != pb.CloudControl_CLOUD_CONTROL_READ_ONLY && mode != pb.CloudControl_CLOUD_CONTROL_PRIVATE {
		return false
	}
	access, ok := OrchestratorCommandAccess(cmd)
	return !ok || access == AccessControl
}

// CloudControlAllowsTerminal is the terminal equivalent of CloudControlAllows.
func CloudControlAllowsTerminal(mode pb.CloudControl, msg *pb.TerminalClientMessage) bool {
	access, ok := TerminalMessageAccess(msg)
	return cloudControlAllows(mode, access, ok, TerminalMessageCarriesContent(msg))
}

func cloudControlAllows(mode pb.CloudControl, access CommandAccess, classified, content bool) bool {
	switch mode {
	case pb.CloudControl_CLOUD_CONTROL_FULL, pb.CloudControl_CLOUD_CONTROL_UNSPECIFIED:
		return true
	case pb.CloudControl_CLOUD_CONTROL_READ_ONLY:
		return classified && access != AccessControl
	case pb.CloudControl_CLOUD_CONTROL_PRIVATE:
		return classified && (access == AccessHousekeeping || (access == AccessObserve && !content))
	default:
		return false
	}
}

// CloudControlPrivateText explains a content refusal without exposing payloads.
func CloudControlPrivateText(hostname string) string {
	return fmt.Sprintf("boss on %s keeps agent content on the machine (cloud control is private); set `cloud_control` to `%s` in that machine's local settings.json to share content without allowing remote changes", hostname, privateRemedySetting)
}
