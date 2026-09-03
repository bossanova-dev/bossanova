package apiversion

import (
	"fmt"
	"strings"
	"testing"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/gen/bossanova/v1/bossanovav1connect"
)

type responseProbe struct {
	procedure string
	build     func() any
	mutated   func(any) bool
}

func TestProductionChanges_CoverDerivedCarrierProcedures(t *testing.T) {
	changes := ProductionChanges()
	asserted := 0
	for _, change := range changes.changes {
		probes := productionCoverageProbes(change)
		if len(probes) == 0 {
			if _, ok := change.(ErrorTransform); ok {
				continue
			}
			t.Fatalf("%T has no response coverage probe", change)
		}
		for _, probe := range probes {
			asserted++
			t.Run(fmt.Sprintf("%T/%s", change, procedureMethod(probe.procedure)), func(t *testing.T) {
				msg := probe.build()
				change.TransformResponse(probe.procedure, msg)
				if !probe.mutated(msg) {
					t.Fatalf("%T did not mutate carrier for %s", change, probe.procedure)
				}
			})
		}
	}
	if asserted == 0 {
		t.Fatal("production coverage asserted zero transform/procedure pairs")
	}
}

func productionCoverageProbes(change VersionChange) []responseProbe {
	switch change.(type) {
	case StaleCheckStateChange:
		return sessionProbes(func() *pb.Session {
			return &pb.Session{
				LastCheckState:         pb.ChecksOverall_CHECKS_OVERALL_UNSPECIFIED,
				LastCheckStateObserved: pb.ChecksOverall_CHECKS_OVERALL_FAILED,
			}
		}, func(s *pb.Session) bool {
			return s.GetLastCheckState() == pb.ChecksOverall_CHECKS_OVERALL_FAILED
		})
	case OrphanedStateChange:
		sess := func() *pb.Session {
			return &pb.Session{State: pb.SessionState_SESSION_STATE_ORPHANED}
		}
		return sessionProbes(sess, func(s *pb.Session) bool {
			return s.GetState() == pb.SessionState_SESSION_STATE_IMPLEMENTING_PLAN
		})
	case AgentAuthFailedChange:
		sess := func() *pb.Session {
			reason := "agent auth failed"
			return &pb.Session{
				BlockedReason: &reason,
				AttentionStatus: &pb.AttentionStatus{
					NeedsAttention: true,
					Reason:         pb.AttentionReason_ATTENTION_REASON_AGENT_AUTH_FAILED,
				},
			}
		}
		return sessionProbes(sess, func(s *pb.Session) bool {
			return s.GetAttentionStatus() == nil && s.GetBlockedReason() == ""
		})
	case UnmanagedLabelChange:
		label := unmanagedLocalCredentialsLabel
		probes := sessionProbes(func() *pb.Session {
			return &pb.Session{AccountLabel: &label}
		}, func(s *pb.Session) bool {
			return s.GetAccountLabel() == systemDefaultAccountLabel
		})
		return append(probes, responseProbe{
			procedure: bossanovav1connect.OrchestratorServiceProxySwitchSessionAccountProcedure,
			build: func() any {
				return &pb.ProxySwitchSessionAccountResponse{TargetLabel: unmanagedLocalCredentialsLabel}
			},
			mutated: func(msg any) bool {
				return msg.(*pb.ProxySwitchSessionAccountResponse).GetTargetLabel() == systemDefaultAccountLabel
			},
		})
	case LimitedChatStatusChange:
		probes := sessionProbes(func() *pb.Session {
			return &pb.Session{
				DisplayLabel:  "usage-limited (resets soon)",
				DisplayIntent: pb.DisplayIntent_DISPLAY_INTENT_WARNING,
			}
		}, func(s *pb.Session) bool {
			return !strings.HasPrefix(s.GetDisplayLabel(), "usage-limited")
		})
		return append(probes, statusProbes(
			pb.ChatStatus_CHAT_STATUS_LIMITED,
			"",
			func(status pb.ChatStatus, waitingReason string) bool {
				return status == pb.ChatStatus_CHAT_STATUS_IDLE && waitingReason == ""
			},
		)...)
	case NoEligibleAccountChange:
		return sessionProbes(func() *pb.Session {
			return &pb.Session{RotationEvents: []*pb.RotationEvent{{Outcome: pb.RotationOutcome_ROTATION_OUTCOME_STATUS_ONLY_NO_ELIGIBLE_ACCOUNT}}}
		}, func(s *pb.Session) bool {
			return s.GetRotationEvents()[0].GetOutcome() == pb.RotationOutcome_ROTATION_OUTCOME_STATUS_ONLY_NO_CAPABILITY
		})
	case ErroredStatusChange:
		return sessionProbes(func() *pb.Session {
			return &pb.Session{
				State:          pb.SessionState_SESSION_STATE_BLOCKED,
				DisplayLabel:   "working",
				DisplayIntent:  pb.DisplayIntent_DISPLAY_INTENT_DANGER,
				DisplaySpinner: true,
			}
		}, func(s *pb.Session) bool {
			return s.GetDisplayIntent() != pb.DisplayIntent_DISPLAY_INTENT_DANGER
		})
	case RespawnSameAccountOutcomeChange:
		return sessionProbes(func() *pb.Session {
			return &pb.Session{RotationEvents: []*pb.RotationEvent{{Outcome: pb.RotationOutcome_ROTATION_OUTCOME_RESPAWNED_SAME_ACCOUNT}}}
		}, func(s *pb.Session) bool {
			return s.GetRotationEvents()[0].GetOutcome() == pb.RotationOutcome_ROTATION_OUTCOME_STATUS_ONLY_NO_CAPABILITY
		})
	case AgentStalledChange:
		reason := "agent-stalled"
		return sessionProbes(func() *pb.Session {
			return &pb.Session{
				BlockedReason: &reason,
				AttentionStatus: &pb.AttentionStatus{
					NeedsAttention: true,
					Reason:         pb.AttentionReason_ATTENTION_REASON_AGENT_STALLED,
				},
			}
		}, func(s *pb.Session) bool {
			return s.GetAttentionStatus() == nil && s.GetBlockedReason() == ""
		})
	case WaitingChatStatusChange:
		probes := sessionProbes(func() *pb.Session {
			return &pb.Session{
				DisplayLabel:  "waiting",
				DisplayIntent: pb.DisplayIntent_DISPLAY_INTENT_INFO,
			}
		}, func(s *pb.Session) bool {
			return s.GetDisplayLabel() == "working"
		})
		return append(probes, statusProbes(
			pb.ChatStatus_CHAT_STATUS_WAITING,
			"callback",
			func(status pb.ChatStatus, waitingReason string) bool {
				return status == pb.ChatStatus_CHAT_STATUS_WORKING && waitingReason == ""
			},
		)...)
	case DraftPRFailureLabelChange:
		reason := draftPRFailureReasonForCoverage()
		return append([]responseProbe{
			{
				procedure: bossanovav1connect.OrchestratorServiceProxyCreateSessionProcedure,
				build: func() any {
					return &pb.ProxyCreateSessionResponse{
						Body: &pb.ProxyCreateSessionResponse_Created{Created: draftPRFailureSession(reason)},
					}
				},
				mutated: func(msg any) bool {
					body := msg.(*pb.ProxyCreateSessionResponse).GetBody().(*pb.ProxyCreateSessionResponse_Created)
					return body.Created.GetDisplayLabel() == "? PR failed"
				},
			},
		}, sessionProbes(func() *pb.Session {
			return draftPRFailureSession(reason)
		}, func(s *pb.Session) bool {
			return s.GetDisplayLabel() == "? PR failed"
		})...)
	case GateFailedOutcomeChange:
		return gateFailedProbes()
	case AbandonedCheckoutStatusChange:
		return abandonedCheckoutProbes()
	default:
		return nil
	}
}

func sessionProbes(buildSession func() *pb.Session, mutated func(*pb.Session) bool) []responseProbe {
	var probes []responseProbe
	for _, procedure := range UnaryProceduresContainingCarrier(
		(&pb.Session{}).ProtoReflect().Descriptor().FullName(),
		"OrchestratorService",
	) {
		procedure := procedure
		probes = append(probes, responseProbe{
			procedure: procedure,
			build: func() any {
				msg, _ := sessionResponse(procedure, buildSession())
				return msg
			},
			mutated: func(msg any) bool {
				_, get := sessionResponse(procedure, nil)
				return mutated(get(msg))
			},
		})
	}
	return probes
}

func sessionResponse(procedure string, sess *pb.Session) (any, func(any) *pb.Session) {
	switch procedure {
	case bossanovav1connect.OrchestratorServiceProxyArchiveSessionProcedure:
		return &pb.ProxyArchiveSessionResponse{Session: sess}, func(msg any) *pb.Session { return msg.(*pb.ProxyArchiveSessionResponse).GetSession() }
	case bossanovav1connect.OrchestratorServiceProxyCloseSessionProcedure:
		return &pb.ProxyCloseSessionResponse{Session: sess}, func(msg any) *pb.Session { return msg.(*pb.ProxyCloseSessionResponse).GetSession() }
	case bossanovav1connect.OrchestratorServiceProxyGetSessionProcedure:
		return &pb.ProxyGetSessionResponse{Session: sess}, func(msg any) *pb.Session { return msg.(*pb.ProxyGetSessionResponse).GetSession() }
	case bossanovav1connect.OrchestratorServiceProxyLinkSessionPRProcedure:
		return &pb.ProxyLinkSessionPRResponse{Session: sess}, func(msg any) *pb.Session { return msg.(*pb.ProxyLinkSessionPRResponse).GetSession() }
	case bossanovav1connect.OrchestratorServiceProxyListSessionsProcedure:
		return &pb.ProxyListSessionsResponse{Sessions: []*pb.Session{sess}}, func(msg any) *pb.Session { return msg.(*pb.ProxyListSessionsResponse).GetSessions()[0] }
	case bossanovav1connect.OrchestratorServiceProxyListSessionsAcrossOrganizationsProcedure:
		return &pb.ProxyListSessionsAcrossOrganizationsResponse{Sessions: []*pb.Session{sess}}, func(msg any) *pb.Session {
			return msg.(*pb.ProxyListSessionsAcrossOrganizationsResponse).GetSessions()[0]
		}
	case bossanovav1connect.OrchestratorServiceProxyMergeSessionProcedure:
		return &pb.ProxyMergeSessionResponse{Session: sess}, func(msg any) *pb.Session { return msg.(*pb.ProxyMergeSessionResponse).GetSession() }
	case bossanovav1connect.OrchestratorServiceProxyPauseSessionProcedure:
		return &pb.ProxyPauseSessionResponse{Session: sess}, func(msg any) *pb.Session { return msg.(*pb.ProxyPauseSessionResponse).GetSession() }
	case bossanovav1connect.OrchestratorServiceProxyResumeSessionProcedure:
		return &pb.ProxyResumeSessionResponse{Session: sess}, func(msg any) *pb.Session { return msg.(*pb.ProxyResumeSessionResponse).GetSession() }
	case bossanovav1connect.OrchestratorServiceProxyResurrectSessionProcedure:
		return &pb.ProxyResurrectSessionResponse{Session: sess}, func(msg any) *pb.Session { return msg.(*pb.ProxyResurrectSessionResponse).GetSession() }
	case bossanovav1connect.OrchestratorServiceProxyRetrySessionProcedure:
		return &pb.ProxyRetrySessionResponse{Session: sess}, func(msg any) *pb.Session { return msg.(*pb.ProxyRetrySessionResponse).GetSession() }
	case bossanovav1connect.OrchestratorServiceProxyRunCronJobNowProcedure:
		return &pb.ProxyRunCronJobNowResponse{Session: sess}, func(msg any) *pb.Session { return msg.(*pb.ProxyRunCronJobNowResponse).GetSession() }
	case bossanovav1connect.OrchestratorServiceProxyStopSessionProcedure:
		return &pb.ProxyStopSessionResponse{Session: sess}, func(msg any) *pb.Session { return msg.(*pb.ProxyStopSessionResponse).GetSession() }
	case bossanovav1connect.OrchestratorServiceProxyUpdateSessionProcedure:
		return &pb.ProxyUpdateSessionResponse{Session: sess}, func(msg any) *pb.Session { return msg.(*pb.ProxyUpdateSessionResponse).GetSession() }
	case bossanovav1connect.OrchestratorServiceTransferSessionProcedure:
		return &pb.TransferSessionResponse{Session: sess}, func(msg any) *pb.Session { return msg.(*pb.TransferSessionResponse).GetSession() }
	default:
		panic("unhandled session procedure: " + procedure)
	}
}

func statusProbes(status pb.ChatStatus, waitingReason string, mutated func(pb.ChatStatus, string) bool) []responseProbe {
	procedures := append(
		UnaryProceduresContainingCarrier((&pb.ChatStatusEntry{}).ProtoReflect().Descriptor().FullName(), "OrchestratorService", "DaemonService"),
		UnaryProceduresContainingCarrier((&pb.SessionStatusEntry{}).ProtoReflect().Descriptor().FullName(), "OrchestratorService", "DaemonService")...,
	)
	var probes []responseProbe
	for _, procedure := range procedures {
		procedure := procedure
		probes = append(probes, responseProbe{
			procedure: procedure,
			build: func() any {
				switch procedure {
				case bossanovav1connect.DaemonServiceGetChatStatusesProcedure:
					return &pb.GetChatStatusesResponse{Statuses: []*pb.ChatStatusEntry{{Status: status, WaitingReason: waitingReason}}}
				case bossanovav1connect.OrchestratorServiceProxyGetChatStatusesProcedure:
					return &pb.ProxyGetChatStatusesResponse{Statuses: []*pb.ChatStatusEntry{{Status: status, WaitingReason: waitingReason}}}
				case bossanovav1connect.DaemonServiceGetSessionStatusesProcedure:
					return &pb.GetSessionStatusesResponse{Statuses: []*pb.SessionStatusEntry{{Status: status, WaitingReason: waitingReason}}}
				case bossanovav1connect.OrchestratorServiceProxyGetSessionStatusesProcedure:
					return &pb.ProxyGetSessionStatusesResponse{Statuses: []*pb.SessionStatusEntry{{Status: status, WaitingReason: waitingReason}}}
				default:
					panic("unhandled status procedure: " + procedure)
				}
			},
			mutated: func(msg any) bool {
				switch m := msg.(type) {
				case *pb.GetChatStatusesResponse:
					return mutated(m.GetStatuses()[0].GetStatus(), m.GetStatuses()[0].GetWaitingReason())
				case *pb.ProxyGetChatStatusesResponse:
					return mutated(m.GetStatuses()[0].GetStatus(), m.GetStatuses()[0].GetWaitingReason())
				case *pb.GetSessionStatusesResponse:
					return mutated(m.GetStatuses()[0].GetStatus(), m.GetStatuses()[0].GetWaitingReason())
				case *pb.ProxyGetSessionStatusesResponse:
					return mutated(m.GetStatuses()[0].GetStatus(), m.GetStatuses()[0].GetWaitingReason())
				default:
					panic(fmt.Sprintf("unhandled status message: %T", msg))
				}
			},
		})
	}
	return probes
}

// abandonedCheckoutProbes derives its procedure list from the proto descriptors
// rather than hard-coding it, so a fourth response that starts carrying a
// CloudAccessStatus fails this test until AbandonedCheckoutStatusChange covers
// it too.
func abandonedCheckoutProbes() []responseProbe {
	var probes []responseProbe
	for _, procedure := range UnaryProceduresContainingCarrier(
		(&pb.CloudAccessStatus{}).ProtoReflect().Descriptor().FullName(),
		"OrchestratorService",
	) {
		procedure := procedure
		probes = append(probes, responseProbe{
			procedure: procedure,
			build: func() any {
				msg, _ := cloudAccessResponse(procedure, abandonedCheckoutStatus())
				return msg
			},
			mutated: func(msg any) bool {
				_, get := cloudAccessResponse(procedure, nil)
				st := get(msg)
				return st.GetState() == pb.CloudAccessState_CLOUD_ACCESS_STATE_PENDING_ENTITLEMENT_REFRESH &&
					st.GetMessage() == cloudActivatingMessage &&
					!st.GetCanCreateCheckout() &&
					st.GetCheckoutStarted() &&
					st.GetDenialReason() == cloudPendingEntitlementDenialReason
			},
		})
	}
	return probes
}

func cloudAccessResponse(procedure string, st *pb.CloudAccessStatus) (any, func(any) *pb.CloudAccessStatus) {
	switch procedure {
	case bossanovav1connect.OrchestratorServiceGetCloudAccessStatusProcedure:
		return &pb.GetCloudAccessStatusResponse{Status: st}, func(msg any) *pb.CloudAccessStatus {
			return msg.(*pb.GetCloudAccessStatusResponse).GetStatus()
		}
	case bossanovav1connect.OrchestratorServiceCreateCheckoutSessionProcedure:
		return &pb.CreateCheckoutSessionResponse{Status: st}, func(msg any) *pb.CloudAccessStatus {
			return msg.(*pb.CreateCheckoutSessionResponse).GetStatus()
		}
	case bossanovav1connect.OrchestratorServiceRefreshCloudEntitlementsProcedure:
		return &pb.RefreshCloudEntitlementsResponse{Status: st}, func(msg any) *pb.CloudAccessStatus {
			return msg.(*pb.RefreshCloudEntitlementsResponse).GetStatus()
		}
	default:
		panic("apiversion: unhandled CloudAccessStatus carrier procedure " + procedure)
	}
}

// abandonedCheckoutStatus is the V20260904 shape for a Stripe Checkout session
// that was created and never completed.
func abandonedCheckoutStatus() *pb.CloudAccessStatus {
	return &pb.CloudAccessStatus{
		State:             pb.CloudAccessState_CLOUD_ACCESS_STATE_NEEDS_SUBSCRIPTION,
		Message:           "Checkout has not been completed. Resume checkout to activate Bossanova Cloud.",
		CanCreateCheckout: true,
		CheckoutStarted:   true,
		DenialReason:      "subscription_required",
	}
}

func gateFailedProbes() []responseProbe {
	return []responseProbe{
		{
			procedure: bossanovav1connect.OrchestratorServiceProxyListCronJobsProcedure,
			build: func() any {
				return &pb.ProxyListCronJobsResponse{Jobs: []*pb.CronJobWithDaemon{{Job: gateFailedCronJob()}}}
			},
			mutated: func(msg any) bool {
				return msg.(*pb.ProxyListCronJobsResponse).GetJobs()[0].GetJob().GetLastRunOutcome() == cronOutcomeGated
			},
		},
		{
			procedure: bossanovav1connect.OrchestratorServiceProxyCreateCronJobProcedure,
			build:     func() any { return &pb.ProxyCreateCronJobResponse{Job: gateFailedCronJob()} },
			mutated: func(msg any) bool {
				return msg.(*pb.ProxyCreateCronJobResponse).GetJob().GetLastRunOutcome() == cronOutcomeGated
			},
		},
		{
			procedure: bossanovav1connect.OrchestratorServiceProxyUpdateCronJobProcedure,
			build:     func() any { return &pb.ProxyUpdateCronJobResponse{Job: gateFailedCronJob()} },
			mutated: func(msg any) bool {
				return msg.(*pb.ProxyUpdateCronJobResponse).GetJob().GetLastRunOutcome() == cronOutcomeGated
			},
		},
		{
			procedure: bossanovav1connect.OrchestratorServiceProxyGetCronJobProcedure,
			build:     func() any { return &pb.ProxyGetCronJobResponse{CronJob: gateFailedCronJob()} },
			mutated: func(msg any) bool {
				return msg.(*pb.ProxyGetCronJobResponse).GetCronJob().GetLastRunOutcome() == cronOutcomeGated
			},
		},
		{
			procedure: bossanovav1connect.OrchestratorServiceProxyRunCronJobNowProcedure,
			build:     func() any { return &pb.ProxyRunCronJobNowResponse{SkippedReason: cronOutcomeGateFailed} },
			mutated: func(msg any) bool {
				return msg.(*pb.ProxyRunCronJobNowResponse).GetSkippedReason() == cronOutcomeGated
			},
		},
	}
}

func gateFailedCronJob() *pb.CronJob {
	return &pb.CronJob{
		LastRunOutcome: cronOutcomeGateFailed,
		LastRunStatus:  pb.CronJobStatus_CRON_JOB_STATUS_FAILED,
	}
}

func draftPRFailureSession(reason string) *pb.Session {
	return &pb.Session{
		BlockedReason:  &reason,
		DisplayLabel:   "working",
		DisplayIntent:  pb.DisplayIntent_DISPLAY_INTENT_SUCCESS,
		DisplaySpinner: true,
	}
}

func draftPRFailureReasonForCoverage() string {
	return "draft PR creation failed: create draft PR: gh pr create: authentication required"
}

func procedureMethod(procedure string) string {
	_, method, ok := strings.Cut(strings.TrimPrefix(procedure, "/"), "/")
	if !ok {
		return procedure
	}
	return method
}
