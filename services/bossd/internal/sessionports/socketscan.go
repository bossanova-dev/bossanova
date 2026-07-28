package sessionports

// SocketScan is the outcome of inspecting listening sockets for a batch of
// PIDs. It deliberately separates positive evidence (Listeners, always kept)
// from completeness so a partial failure never discards a real listener.
type SocketScan struct {
	// Listeners are the valid listening TCP sockets discovered, in source
	// order. The Detector attributes, filters, sorts, and dedups them.
	Listeners []Listener
	// IncompletePIDs holds PIDs whose socket evidence could not be fully or
	// safely gathered (a malformed row, a permission-denied /proc read, a fd
	// that vanished mid-read). Only the sessions owning these PIDs are marked
	// incomplete — the failure is scoped, not global.
	IncompletePIDs map[int]bool
	// GlobalIncomplete is set when the whole inspection is untrustworthy — the
	// command timed out or was killed, or output could not be attributed to
	// PIDs at all. Every requested session is then marked incomplete.
	GlobalIncomplete bool
}

// markIncomplete records that pid's socket evidence is partial.
func (s *SocketScan) markIncomplete(pid int) {
	if s.IncompletePIDs == nil {
		s.IncompletePIDs = make(map[int]bool)
	}
	s.IncompletePIDs[pid] = true
}
