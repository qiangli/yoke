package cligw

import (
	"net/http"
	"testing"
)

func TestMuseSeatForwardsToolCallingRequestToPool(t *testing.T) {
	backend := NewAgentBackend("door-muse", "spark", nil)
	tools := `[{"type":"function","function":{"name":"bashy","parameters":{"type":"object","properties":{"script":{"type":"string"}},"required":["script"]}}}]`
	_, attempt := serveBackend(t, backend, `{"model":"door-muse","messages":[{"role":"user","content":"ls"}],"tools":`+tools+`}`, nil)
	if attempt.Status != http.StatusServiceUnavailable || !attempt.CanRetry {
		t.Fatalf("tool request attempt = %+v, want pool path", attempt)
	}
}
