package apiversion

// ResetUnresolvedGateReadsForTest clears the per-call-site dedupe behind
// gateAtLeast so a test can assert that the unresolved-read warning fires,
// independently of whatever earlier tests in the same process already reported
// from their own source lines. Without it the "exactly one WARN" arm would
// silently depend on test ordering.
func ResetUnresolvedGateReadsForTest() {
	reportedUnresolvedGateReads.Range(func(key, _ any) bool {
		reportedUnresolvedGateReads.Delete(key)
		return true
	})
}
