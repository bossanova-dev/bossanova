package tui

import "github.com/recurser/boss/api/tui/home"

var openInNewTab = home.OpenInNewTab
var hasNewTabSupport = home.HasNewTabSupport

type HomeModel = home.Model

var NewHomeModel = home.NewModel

var cloneSessionIDSet = home.CloneSessionIDSet

type queuedMove = home.QueuedMove

var sessionIDOrder = home.SessionIDOrder
var fetchSessions = home.FetchSessions

type notifyRequest = home.NotifyRequest

type homeStatusExpiredMsg = home.StatusExpiredMsg
type CloudAccessClient = home.CloudAccessClient
type sessionListMsg = home.SessionListMsg
type repoCountMsg = home.RepoCountMsg
type authStatusMsg = home.AuthStatusMsg
type startSubscriptionFlowMsg = home.StartSubscriptionFlowMsg
type logoutMsg = home.LogoutMsg
type sessionRenamedMsg = home.SessionRenamedMsg
type tickMsg = home.TickMsg

var tickCmd = home.TickCmd
var ErrPreflightCancelled = home.ErrPreflightCancelled

type ProviderStartupResult = home.ProviderStartupResult

var CaptureLoginShell = home.CaptureLoginShell
var DetectInstalledProviderPlugins = home.DetectInstalledProviderPlugins
var PlanProviderStartup = home.PlanProviderStartup
var ApplyProviderOnboarding = home.ApplyProviderOnboarding

type DaemonSupervision = home.DaemonSupervision
type DaemonSupervisionProbe = home.DaemonSupervisionProbe
type daemonSupervisionMsg = home.DaemonSupervisionMsg
type heartbeatTickMsg = home.HeartbeatTickMsg

var heartbeatTickCmd = home.HeartbeatTickCmd
var sendHeartbeatsCmd = home.SendHeartbeatsCmd

type HomeStatusExpiredMsg = home.StatusExpiredMsg
type DaemonSupervisionMsg = home.DaemonSupervisionMsg
