package display

import (
	"strings"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

// ChatRoleName is the lower-case role a driver compares against ("verify");
// a legacy chat with no role is "".
func ChatRoleName(r pb.ChatRole) string {
	if r == pb.ChatRole_CHAT_ROLE_UNSPECIFIED {
		return ""
	}
	if _, known := pb.ChatRole_name[int32(r)]; !known {
		return ""
	}
	return strings.ToLower(strings.TrimPrefix(r.String(), "CHAT_ROLE_"))
}
