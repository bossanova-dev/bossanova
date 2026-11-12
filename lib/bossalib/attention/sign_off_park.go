package attention

import pb "github.com/recurser/bossalib/gen/bossanova/v1"

// SignOffPark is the verify park on in's current head when its deciding code
// asks only for a sign-off: the REVIEW Compute answers when nothing outranks
// the park. A question, a working chat or a fault outranks it in Compute, but
// the head stays parked for the operator, so this answer stands whatever
// Compute says. It is nil for an unparked head, a merged or closed PR, and a
// blocked park, which Compute alone answers.
func SignOffPark(in Input) *pb.SessionAttention {
	if !in.HasPR || in.PRState != PRStateOpen {
		return nil
	}
	codes := parkCodesFor(in)
	code, parked := decidingParkCode(codes)
	if !parked || !knownParkCodes[code].signOff {
		return nil
	}
	return withRowFacts(in, computePark(in, codes, code))
}
