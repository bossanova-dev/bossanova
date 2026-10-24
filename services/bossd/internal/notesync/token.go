package notesync

// AdoptReRegisteredToken keeps a concurrent rotation when adopting a refreshed token.
func AdoptReRegisteredToken(h TokenHolder, failed, fresh string) (use string, alreadyRotated bool) {
	if h.CompareAndSwap(failed, fresh) {
		return fresh, false
	}
	if current := h.Get(); current != "" {
		return current, true
	}
	h.Set(fresh)
	return fresh, false
}
