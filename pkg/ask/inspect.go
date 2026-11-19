package ask

import (
	"os"
	"time"
)

// InspectPending is a non-consuming, non-reaping projection for attention views.
// It reads metadata only: values and decisions stay in their existing stores.
// Like List, this is a local OS-user API, not an authorization for remote callers.
func InspectPending() ([]Request, error) {
	entries, err := os.ReadDir(Dir())
	if os.IsNotExist(err) {
		return []Request{}, nil
	}
	if err != nil {
		return nil, err
	}
	out := []Request{}
	now := time.Now()
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		r, err := Load(e.Name())
		if err != nil {
			return nil, err
		}
		if !r.Pending(now) || expired(r, now) {
			continue
		}
		if r.IsApproval() {
			if _, decided := LoadDecision(r.ID); decided {
				continue
			}
		} else if answered(requestDir(r.ID)) || hasValue(r.ID) {
			continue
		}
		out = append(out, r)
	}
	return out, nil
}
