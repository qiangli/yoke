package gateway

import "net/http"

// GET /health — the operator's view of the pool: which backends the breaker
// is cooling, and the last capacity observation for each. Read-only and
// cache-only; it never probes, so hitting it in a tight loop costs nothing
// and cannot itself take a backend down.

// BackendHealth is one backend's entry in the health report.
type BackendHealth struct {
	Name string `json:"name"`

	// Cooling is true while the breaker is holding requests off this
	// backend after a failed attempt.
	Cooling bool `json:"cooling"`

	// Capacity is the cached observation, present only when one is
	// fresh. Absent means "no recent observation", NOT "no capacity".
	Capacity *Capacity `json:"capacity,omitempty"`

	// CapacityFresh distinguishes the two absent cases above.
	CapacityFresh bool `json:"capacity_fresh"`
}

// HealthReport is the /health response body.
type HealthReport struct {
	// Status is "ok" when at least one backend is out of cooldown, and
	// "degraded" when every known backend is cooling. A pool with no
	// backends configured at all reports "ok" — nothing is broken, there
	// is simply nothing there.
	Status   string          `json:"status"`
	Backends []BackendHealth `json:"backends"`
}

func (g *gateway) health(w http.ResponseWriter, r *http.Request) {
	report := HealthReport{Status: "ok", Backends: []BackendHealth{}}
	if g.cfg.Backends == nil {
		writeJSON(w, http.StatusOK, report)
		return
	}
	anyAvailable := false
	known := 0
	for _, backend := range g.cfg.Backends() {
		if backend == nil {
			continue
		}
		known++
		name := backend.Name()
		entry := BackendHealth{Name: name}
		if g.cfg.Breaker != nil {
			entry.Cooling = g.cfg.Breaker.InCooldown(name)
		}
		if !entry.Cooling {
			anyAvailable = true
		}
		if capacity, ok := g.cfg.Capacity.Get(name); ok {
			entry.Capacity = &capacity
			entry.CapacityFresh = true
		}
		report.Backends = append(report.Backends, entry)
	}
	if known > 0 && !anyAvailable {
		report.Status = "degraded"
	}
	writeJSON(w, http.StatusOK, report)
}
