package display

import (
	"strings"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

// ChatStatusName renders the enum as its bare suffix (IDLE, WORKING, WAITING,
// …), mirroring displayStatusName's treatment of DisplayStatus. A chat the
// daemon holds no cached status for stays UNSPECIFIED rather than being mapped
// to anything friendlier: only the caller can decide, and every safe caller
// treats UNSPECIFIED as not-settled.
//
// A value this build's enum does not know — a newer daemon reporting a status
// added after this binary was compiled — also renders UNSPECIFIED. The guard is
// load-bearing: protobuf-go's generated String() falls back to the *decimal
// number* for any value missing from ChatStatus_name, so TrimPrefix alone would
// leave a bare integer in the STATUS column and in the JSON `status` field.
// UNSPECIFIED is the only honest rendering, because it is the one value already
// documented as "not settled, do not merge on it".
func ChatStatusName(s pb.ChatStatus) string {
	if _, known := pb.ChatStatus_name[int32(s)]; !known {
		return "UNSPECIFIED"
	}
	return strings.TrimPrefix(s.String(), "CHAT_STATUS_")
}
