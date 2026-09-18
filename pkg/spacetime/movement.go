package spacetime

import "time"

// watchMovement requires a new coordinate to remain unchanged for ten seconds
// before publishing it. Wi-Fi association and DHCP commonly flap for a few
// seconds while roaming; ten seconds suppresses that noise while leaving ample
// time to invalidate locality before the motivating 60-second SSH timeout.
func (ps *ProbeSet) watchMovement(baseline string) {
	defer close(ps.movementDone)
	var candidate string
	var candidateSet bool
	var candidateSince time.Time
	ticker := time.NewTicker(ps.movementPoll)
	defer ticker.Stop()

	for {
		select {
		case <-ps.movementStop:
			return
		case now := <-ticker.C:
			current, _ := ps.samplePlace()
			if current == baseline {
				candidate = ""
				candidateSet = false
				continue
			}
			if !candidateSet || current != candidate {
				candidate = current
				candidateSet = true
				candidateSince = now
				continue
			}
			if now.Sub(candidateSince) < ps.movementDebounce {
				continue
			}
			if ps.publishMovement([]string{"place.id"}) == nil {
				baseline = current
				candidate = ""
				candidateSet = false
			} else {
				// A failed append is retried at most once per debounce window.
				candidateSince = now
			}
		}
	}
}

func (ps *ProbeSet) samplePlace() (string, bool) {
	ps.mu.Lock()
	place := ps.core["place.id"]
	ps.mu.Unlock()

	if place.Eval != nil {
		if value, err := place.Eval(); err == nil {
			if value = normalize(value); value != "" {
				return value, true
			}
		}
	}
	return "", false
}

// observeLocality debounces explicit fresh readings rather than polling an
// externally registered resolver concurrently. A caller refreshes volatile
// locality with Forget; observing the old value again cancels a flap.
func (ps *ProbeSet) observeLocality(value string, ok bool) {
	if !ok {
		value = ""
	}
	ps.localityMu.Lock()
	defer ps.localityMu.Unlock()
	if !ps.localitySet {
		ps.localityBaseline = value
		ps.localitySet = true
		return
	}
	if value == ps.localityBaseline {
		ps.localityCandidate = ""
		if ps.localityTimer != nil {
			ps.localityTimer.Stop()
			ps.localityTimer = nil
		}
		return
	}
	if value == ps.localityCandidate {
		return
	}
	ps.localityCandidate = value
	if ps.localityTimer != nil {
		ps.localityTimer.Stop()
	}
	ps.localityTimer = time.AfterFunc(ps.movementDebounce, func() {
		ps.localityMu.Lock()
		defer ps.localityMu.Unlock()
		if ps.localityCandidate == "" {
			return
		}
		if ps.publishMovement([]string{"net.same_lan"}) == nil {
			ps.localityBaseline = ps.localityCandidate
			ps.localityCandidate = ""
			ps.localityTimer = nil
		}
	})
}
