package tui

import (
	"github.com/recurser/boss/api/tui/accounts"
	"github.com/recurser/boss/api/tui/ui"
)

type (
	tuiFeature = ui.TUIFeature
	tuiAction  = ui.TUIAction
	tuiStatus  = ui.TUIStatus
)

const (
	tuiFeatureAccounts          = ui.TUIFeatureAgentAccounts
	tuiFeatureCron              = ui.TUIFeatureCron
	tuiFeatureSession           = ui.TUIFeatureSession
	tuiActionAccountAdded       = ui.TUIActionAgentAccountAdded
	tuiActionAccountRemoved     = ui.TUIActionAgentAccountRemoved
	tuiActionAccountDisabled    = ui.TUIActionAgentAccountDisabled
	tuiActionAccountEnabled     = ui.TUIActionAgentAccountEnabled
	tuiActionAccountRefreshed   = ui.TUIActionAgentAccountRefreshed
	tuiActionAccountReauthed    = ui.TUIActionAgentAccountReauthed
	tuiActionAccountSwitched    = ui.TUIActionAgentAccountSwitched
	tuiActionCronJobCreated     = ui.TUIActionCronJobCreated
	tuiActionCronJobUpdated     = ui.TUIActionCronJobUpdated
	tuiActionCronJobDeleted     = ui.TUIActionCronJobDeleted
	tuiActionCronJobRunNow      = ui.TUIActionCronJobRunNow
	tuiActionSessionMerged      = ui.TUIActionSessionMerged
	tuiActionSessionArchived    = ui.TUIActionSessionArchived
	tuiActionSessionRemoved     = ui.TUIActionSessionRemoved
	tuiActionSessionResurrected = ui.TUIActionSessionResurrected
	tuiStatusSuccess            = ui.TUIStatusSuccess
	tuiStatusError              = ui.TUIStatusError
	tuiEntryPointHome           = ui.TUIEntryPointHome
	tuiEntryPointLogin          = ui.TUIEntryPointLogin
)

var (
	tuiActionStatus            = ui.TUIActionStatus
	captureTUIAction           = ui.CaptureTUIAction
	captureCloudConversionStep = ui.CaptureCloudConversionStep
)

// accountStatusAction maps the status an UpdateAgentAccount status flip requested to
// its tui_action action value. Both surfaces that flip a status — the list's
// [space] and the edit screen's status row — are *toggles* sharing one result
// message, so the requested status is the only thing that distinguishes an
// enable from a disable. It is carried on the message (and on the request) even
// when the RPC failed, so a failed enable is still reported as an enable.
//
// Both producers build the flip from accounts.AgentAccountStatusActive/accounts.AgentAccountStatusDisabled,
// which is the whole domain today. Should a third status ever appear, this maps
// it to account_enabled — wrong, but bounded: it can never emit the status
// string itself.
func accountStatusAction(status string) tuiAction {
	if status == accounts.AgentAccountStatusDisabled {
		return tuiActionAccountDisabled
	}
	return tuiActionAccountEnabled
}
