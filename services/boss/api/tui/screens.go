package tui

import (
	"context"

	"github.com/recurser/boss/api/tui/bugreport"
	"github.com/recurser/boss/api/tui/cron"
	"github.com/recurser/boss/api/tui/login"
	"github.com/recurser/boss/api/tui/onboarding"
	"github.com/recurser/boss/api/tui/trash"
)

type TrashModel = trash.Model

var NewTrashModel = trash.NewModel

type BugReportModel = bugreport.Model

var NewBugReportModel = bugreport.NewModel

type CronListModel = cron.ListModel

var NewCronListModel = cron.NewListModel

type CronFormModel = cron.FormModel

var NewCronFormModel = cron.NewFormModel

type LoginModel = login.Model

var NewLoginModel = login.NewModel

type loginCompleteHook = func(context.Context)
type OnboardingModel = onboarding.Model

var NewOnboardingModel = onboarding.NewModel
var NewProviderSelectionModel = onboarding.NewProviderSelectionModel
var NewProviderInstallRequiredModel = onboarding.NewProviderInstallRequiredModel
var onboardingProviders = onboarding.Providers

type cronJobsLoadedMsg = cron.JobsLoadedMsg
type cronJobDeletedMsg = cron.JobDeletedMsg
type cronJobUpdatedMsg = cron.JobUpdatedMsg
type cronRunNowMsg = cron.RunNowMsg
type cronFormOpenMsg = cron.FormOpenMsg
type cronFormDoneMsg = cron.FormDoneMsg
type trashListMsg = trash.ListMsg
type sessionRestoredMsg = trash.SessionRestoredMsg
type sessionDeletedMsg = trash.SessionDeletedMsg
type deleteProgressMsg = trash.DeleteProgressMsg

type bugReportSubmitMsg = bugreport.SubmitMsg
type cronFormReposMsg = cron.FormReposMsg
type cronFormAgentsMsg = cron.FormAgentsMsg
