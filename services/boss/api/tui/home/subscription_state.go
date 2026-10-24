package home

import (
	"github.com/recurser/boss/api/tui/ui"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

func SubscriptionNeedsCheckout(status *pb.CloudAccessStatus) bool {
	return ui.SubscriptionNeedsCheckout(status)
}
func SubscriptionIsActive(status *pb.CloudAccessStatus) bool { return ui.SubscriptionIsActive(status) }
