package ui

import pb "github.com/recurser/bossalib/gen/bossanova/v1"

func SubscriptionNeedsCheckout(status *pb.CloudAccessStatus) bool {
	switch status.GetState() {
	case pb.CloudAccessState_CLOUD_ACCESS_STATE_NEEDS_SUBSCRIPTION,
		pb.CloudAccessState_CLOUD_ACCESS_STATE_PAST_DUE,
		pb.CloudAccessState_CLOUD_ACCESS_STATE_CANCELED:
		return true
	default:
		return false
	}
}

func SubscriptionIsActive(status *pb.CloudAccessStatus) bool {
	return status.GetState() == pb.CloudAccessState_CLOUD_ACCESS_STATE_ACTIVE
}

func SubscriptionCanPoll(status *pb.CloudAccessStatus) bool {
	return status.GetState() == pb.CloudAccessState_CLOUD_ACCESS_STATE_PENDING_ENTITLEMENT_REFRESH
}

func SubscriptionTerminalLocal(status *pb.CloudAccessStatus, err error) bool {
	if err != nil {
		return true
	}
	return status.GetState() == pb.CloudAccessState_CLOUD_ACCESS_STATE_BILLING_UNAVAILABLE
}
