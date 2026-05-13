package telemetry

const (
	ProductionProjectToken = "phc_p4W5gaEDYv2Nke88eAL8FFVBtCrieXaK9EZQtUz4kpzm"
	StagingProjectToken    = "phc_BuZNLVTZjpeqaLMErvf2DDeHqnSyQ7yHg6mQiA5u5ifQ"
	DefaultHost            = "https://eu.i.posthog.com"
)

type Event string

const (
	EventCLICommandInvoked  Event = "cli_command_invoked"
	EventDaemonStarted      Event = "daemon_started"
	EventSessionCreated     Event = "session_created"
	EventChatCreated        Event = "chat_created"
	EventChatAttached       Event = "chat_attached"
	EventAuthChanged        Event = "auth_changed"
	EventRepairStarted      Event = "repair_started"
	EventRepairCompleted    Event = "repair_completed"
	EventBugReportSubmitted Event = "bug_report_submitted"
)

var allowedEvents = map[Event]struct{}{
	EventCLICommandInvoked:  {},
	EventDaemonStarted:      {},
	EventSessionCreated:     {},
	EventChatCreated:        {},
	EventChatAttached:       {},
	EventAuthChanged:        {},
	EventRepairStarted:      {},
	EventRepairCompleted:    {},
	EventBugReportSubmitted: {},
}

func IsAllowed(event Event) bool {
	_, ok := allowedEvents[event]
	return ok
}
