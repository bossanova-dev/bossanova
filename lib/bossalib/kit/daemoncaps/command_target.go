package daemoncaps

import (
	"fmt"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"google.golang.org/protobuf/reflect/protoreflect"
)

// CommandTargetField identifies an object acted on by a command. Optional fields
// are resolved whenever present; their absence does not invent an authority.
type CommandTargetField struct {
	Field protoreflect.Name
	// Companion completes a composite repository name (owner/name).
	Companion protoreflect.Name
	Kind      string
	Optional  bool
}

// CommandTargets describes all local objects and machine authority required by an arm.
type CommandTargets struct {
	Fields  []CommandTargetField
	Machine bool
	Fanout  bool
}

var commandTargets = targetTable(map[protoreflect.Name]CommandTargets{
	"stop":     {Fields: []CommandTargetField{{Field: "session_id", Kind: "session", Optional: false}}},
	"pause":    {Fields: []CommandTargetField{{Field: "session_id", Kind: "session", Optional: false}}},
	"resume":   {Fields: []CommandTargetField{{Field: "session_id", Kind: "session", Optional: false}}},
	"transfer": {Fields: []CommandTargetField{{Field: "session_id", Kind: "transfer", Optional: false}}},
	// Webhooks are locally verified re-read hints, not object mutations.
	"webhook":                          {},
	"attach":                           {Fields: []CommandTargetField{{Field: "session_id", Kind: "session", Optional: false}}},
	"transfer_confirmed":               {Fields: []CommandTargetField{{Field: "session_id", Kind: "transfer", Optional: false}}},
	"transfer_cancel":                  {Fields: []CommandTargetField{{Field: "session_id", Kind: "transfer", Optional: false}}},
	"wake_chat":                        {Fields: []CommandTargetField{{Field: "agent_session_id", Kind: "chat", Optional: false}}},
	"merge":                            {Fields: []CommandTargetField{{Field: "session_id", Kind: "session", Optional: false}}},
	"archive":                          {Fields: []CommandTargetField{{Field: "session_id", Kind: "session", Optional: false}}},
	"record_chat":                      {Fields: []CommandTargetField{{Field: "session_id", Kind: "session", Optional: false}, {Field: "agent_session_id", Kind: "chat", Optional: true}}},
	"delete_chat":                      {Fields: []CommandTargetField{{Field: "session_id", Kind: "session", Optional: false}, {Field: "agent_session_id", Kind: "chat", Optional: false}}},
	"create_session":                   {Fields: []CommandTargetField{{Field: "repo_id", Kind: "repo", Optional: false}, {Field: "account_id", Kind: "account", Optional: true}}},
	"list_repos":                       {},
	"list_agents":                      {},
	"list_repo_prs":                    {Fields: []CommandTargetField{{Field: "repo_id", Kind: "repo", Optional: false}}},
	"list_tracker_issues":              {Fields: []CommandTargetField{{Field: "repo_id", Kind: "repo", Optional: false}}},
	"get_chat_transcript":              {Fields: []CommandTargetField{{Field: "session_id", Kind: "session", Optional: true}, {Field: "agent_session_id", Kind: "chat", Optional: false}}},
	"send_chat_message":                {Fields: []CommandTargetField{{Field: "agent_session_id", Kind: "chat", Optional: false}}},
	"switch_account":                   {Fields: []CommandTargetField{{Field: "session_id", Kind: "session", Optional: false}, {Field: "agent_session_id", Kind: "chat", Optional: true}, {Field: "account_id", Kind: "account", Optional: true}}, Machine: true},
	"list_accounts":                    {Machine: true},
	"get_repo":                         {Fields: []CommandTargetField{{Field: "repo_id", Kind: "repo", Optional: false}}},
	"update_repo":                      {Fields: []CommandTargetField{{Field: "repo_id", Kind: "repo", Optional: false}}},
	"remove_repo":                      {Fields: []CommandTargetField{{Field: "repo_id", Kind: "repo", Optional: false}}},
	"list_cron_jobs":                   {},
	"create_cron_job":                  {Fields: []CommandTargetField{{Field: "repo_id", Kind: "repo", Optional: false}}},
	"update_cron_job":                  {Fields: []CommandTargetField{{Field: "id", Kind: "cron", Optional: false}}},
	"delete_cron_job":                  {Fields: []CommandTargetField{{Field: "id", Kind: "cron", Optional: false}}},
	"run_cron_job_now":                 {Fields: []CommandTargetField{{Field: "id", Kind: "cron", Optional: false}}},
	"add_account":                      {Machine: true},
	"refresh_account":                  {Fields: []CommandTargetField{{Field: "id", Kind: "account"}}, Machine: true},
	"update_account":                   {Fields: []CommandTargetField{{Field: "id", Kind: "account"}}, Machine: true},
	"remove_account":                   {Fields: []CommandTargetField{{Field: "id", Kind: "account"}}, Machine: true},
	"test_account":                     {Fields: []CommandTargetField{{Field: "id", Kind: "account"}}, Machine: true},
	"list_chats":                       {Fields: []CommandTargetField{{Field: "session_id", Kind: "session", Optional: false}}},
	"get_session_statuses":             {Fields: []CommandTargetField{{Field: "session_ids", Kind: "session", Optional: true}}},
	"list_check_snapshots":             {Fields: []CommandTargetField{{Field: "session_id", Kind: "session", Optional: false}}},
	"list_plugins":                     {},
	"get_cron_job":                     {Fields: []CommandTargetField{{Field: "id", Kind: "cron", Optional: false}}},
	"repair_doctor":                    {},
	"close_session":                    {Fields: []CommandTargetField{{Field: "session_id", Kind: "session", Optional: false}}},
	"resurrect_session":                {Fields: []CommandTargetField{{Field: "session_id", Kind: "session", Optional: false}}},
	"remove_session":                   {Fields: []CommandTargetField{{Field: "session_id", Kind: "session", Optional: false}}},
	"empty_trash":                      {Fanout: true},
	"retry_session":                    {Fields: []CommandTargetField{{Field: "session_id", Kind: "session", Optional: false}}},
	"update_session":                   {Fields: []CommandTargetField{{Field: "session_id", Kind: "session", Optional: false}}},
	"link_session_pr":                  {Fields: []CommandTargetField{{Field: "session_id", Kind: "session", Optional: false}}},
	"update_chat_title":                {Fields: []CommandTargetField{{Field: "agent_session_id", Kind: "chat", Optional: false}}},
	"report_chat_status":               {Fields: []CommandTargetField{{Field: "reports", Kind: "chat", Optional: true}}},
	"create_github_callback":           {Fields: []CommandTargetField{{Field: "target_chat_id", Kind: "chat", Optional: false}, {Field: "repo_owner", Companion: "repo_name", Kind: "repo-name", Optional: false}}},
	"list_github_callbacks":            {Fields: []CommandTargetField{{Field: "target_chat_id", Kind: "chat", Optional: true}, {Field: "repo_owner", Companion: "repo_name", Kind: "repo-name", Optional: true}}},
	"delete_github_callback":           {Fields: []CommandTargetField{{Field: "id", Kind: "callback", Optional: false}, {Field: "expect_target_chat_id", Kind: "chat", Optional: true}}},
	"create_note":                      {Fields: []CommandTargetField{{Field: "repo_id", Kind: "repo", Optional: false}, {Field: "session_id", Kind: "session", Optional: true}, {Field: "chat_id", Kind: "chat", Optional: true}}},
	"get_note":                         {Fields: []CommandTargetField{{Field: "id", Kind: "note", Optional: false}}},
	"list_notes":                       {Fields: []CommandTargetField{{Field: "repo_id", Kind: "repo", Optional: true}, {Field: "session_id", Kind: "session", Optional: true}, {Field: "chat_id", Kind: "chat", Optional: true}}},
	"update_note":                      {Fields: []CommandTargetField{{Field: "id", Kind: "note", Optional: false}}},
	"delete_note":                      {Fields: []CommandTargetField{{Field: "id", Kind: "note", Optional: false}}},
	"broadcast":                        {Fanout: true},
	"get_chat_statuses":                {Fields: []CommandTargetField{{Field: "session_id", Kind: "session", Optional: false}}},
	"command_cancel":                   {},
	"move_session":                     {Fields: []CommandTargetField{{Field: "session_id", Kind: "session", Optional: false}, {Field: "repo_id", Kind: "repo", Optional: true}}},
	"launch_trigger_session":           {Fields: []CommandTargetField{{Field: "repo_origin_url", Kind: "trigger", Optional: false}}},
	"run_trigger_gate":                 {Fields: []CommandTargetField{{Field: "repo_origin_url", Kind: "trigger", Optional: false}}},
	"dismiss_session_alert":            {Fields: []CommandTargetField{{Field: "session_id", Kind: "session", Optional: false}}},
	"resume_session_run":               {Fields: []CommandTargetField{{Field: "session_id", Kind: "session", Optional: false}}},
	"operator_merge_session":           {Fields: []CommandTargetField{{Field: "session_id", Kind: "session", Optional: false}}},
	"apply_agent_account_sync":         {Fields: []CommandTargetField{{Field: "local_agent_account_id", Kind: "account"}, {Field: "synced_agent_account_id", Kind: "account"}}, Machine: true},
	"apply_agent_account_usage":        {Fields: []CommandTargetField{{Field: "local_agent_account_id", Kind: "account"}}, Machine: true},
	"agent_credential_available":       {Fields: []CommandTargetField{{Field: "local_agent_account_id", Kind: "account"}, {Field: "synced_agent_account_id", Kind: "account"}}},
	"agent_credential_devices_changed": {},
	"send_back_to_repair":              {Fields: []CommandTargetField{{Field: "session_id", Kind: "session", Optional: false}}},
	"get_session_timeline":             {Fields: []CommandTargetField{{Field: "session_id", Kind: "session", Optional: false}}},
})

func targetTable(names map[protoreflect.Name]CommandTargets) map[protoreflect.FieldNumber]CommandTargets {
	table := make(map[protoreflect.FieldNumber]CommandTargets, len(names))
	for name, target := range names {
		arm := commandOneof.Fields().ByName(name)
		if arm == nil {
			panic(fmt.Sprintf("unknown command target arm %s", name))
		}
		for _, field := range target.Fields {
			if field.Companion != "" && arm.Message().Fields().ByName(field.Companion) == nil {
				panic(fmt.Sprintf("unknown target companion %s.%s", name, field.Companion))
			}
			if arm.Message().Fields().ByName(field.Field) == nil {
				panic(fmt.Sprintf("unknown target field %s.%s", name, field.Field))
			}
		}
		table[arm.Number()] = target
	}
	return table
}

// OrchestratorCommandTargets returns the complete target declaration. Unknown
// arms have no declaration and must be refused by the daemon.
func OrchestratorCommandTargets(cmd *pb.OrchestratorCommand) (CommandTargets, bool) {
	field := cmd.ProtoReflect().WhichOneof(commandOneof)
	if field == nil {
		return CommandTargets{}, false
	}
	target, ok := commandTargets[field.Number()]
	// Do not expose the shared descriptor table's backing slice to consumers.
	target.Fields = append([]CommandTargetField(nil), target.Fields...)
	return target, ok
}
