package evidence

import "context"

// RecordPublishedPage records page as the proof page this daemon itself
// published for head on the session's PR pr, so the page is served before the
// next comment read finds verify's proof note. It applies only while the
// session is unknown to the reader or is on pr at head; the poller's view of
// the session's head wins over an upload for any other head. Later comment
// reads of head keep the page unless they record one of their own, and a new
// head drops it with the rest of the old head's state.
func (r *Reader) RecordPublishedPage(ctx context.Context, sessionID string, pr int, head, page string) {
	if r == nil || sessionID == "" || pr < 1 || !proofHead.MatchString(head) || !isHTTPSURL(page) {
		return
	}
	r.mu.Lock()
	st := r.sessions[sessionID]
	switch {
	case st == nil:
		st = &headState{head: head, pr: pr, cycle: r.cycle}
		r.sessions[sessionID] = st
	case st.pr != pr || !sameHead(st.head, head):
		r.mu.Unlock()
		return
	}
	st.published = page
	proof, latest := st.withPublished(st.proof, st.latest)
	changed := !st.proof.equal(proof) || !st.latest.equal(latest)
	st.proof, st.latest = proof, latest
	onChange := r.onChange
	r.mu.Unlock()
	if changed && onChange != nil {
		onChange(ctx, sessionID)
	}
}

// withPublished merges st's published page into the head's proof record and
// the PR's latest record. A record that already names a page for head wins:
// the comments are the shared record. Otherwise the published page becomes the
// head's page, keeping whatever else the comments recorded, and, being on the
// current head, the PR's latest record.
func (st *headState) withPublished(proof, latest *ProofRecord) (*ProofRecord, *ProofRecord) {
	if st.published == "" || proof != nil && proof.Page != "" {
		return proof, latest
	}
	merged := ProofRecord{Head: st.head}
	if proof != nil {
		merged = proof.clone()
	}
	merged.Page, merged.Legacy = st.published, false
	return &merged, &merged
}
