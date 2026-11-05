package reviewqueue

import (
	"cmp"
	"hash/fnv"
	"strconv"
	"strings"
	"time"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/kit/vcs"
)

// AuditWindow is how long merged High PRs are eligible for spot checks.
const AuditWindow = 7 * 24 * time.Hour

// IsAuditSample picks a stable one-in-ten sample by repository and PR identity.
func IsAuditSample(originURL string, prNumber int32) bool {
	origin := vcs.CanonicalRepoURL(originURL)
	if origin == "" || prNumber <= 0 {
		return false
	}
	h := fnv.New64a()
	_, _ = h.Write([]byte(origin + "#" + strconv.Itoa(int(prNumber))))
	return h.Sum64()%10 == 0
}
func isAuditRow(s *pb.Session, now time.Time) bool {
	return s.GetState() == pb.SessionState_SESSION_STATE_MERGED &&
		s.GetMergedConfidenceLevel() == pb.ConfidenceLevel_CONFIDENCE_LEVEL_HIGH &&
		s.GetStateEnteredAt() != nil && now.Sub(s.GetStateEnteredAt().AsTime()) < AuditWindow &&
		IsAuditSample(s.GetRepoOriginUrl(), s.GetPrNumber())
}
func compareAudit(left, right *pb.ReviewQueueRow) int {
	if left.GetIsAuditSample() != right.GetIsAuditSample() {
		if left.GetIsAuditSample() {
			return 1
		}
		return -1
	}
	if !left.GetIsAuditSample() {
		return 0
	}
	l, r := left.GetSession().GetStateEnteredAt(), right.GetSession().GetStateEnteredAt()
	return cmp.Or(cmp.Compare(r.GetSeconds(), l.GetSeconds()), cmp.Compare(r.GetNanos(), l.GetNanos()),
		strings.Compare(vcs.NormalizeRepoURL(left.GetSession().GetRepoOriginUrl()), vcs.NormalizeRepoURL(right.GetSession().GetRepoOriginUrl())),
		cmp.Compare(left.GetSession().GetPrNumber(), right.GetSession().GetPrNumber()), strings.Compare(left.GetSession().GetId(), right.GetSession().GetId()))
}
