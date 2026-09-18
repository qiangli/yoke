package meet

import (
	"bytes"
	"regexp"
	"strings"
	"testing"
)

func TestPublicMeetSurfacesSayFacilitatorNotChair(t *testing.T) {
	st := newRoom(t)
	st.Chair = "facilitator-agent"
	st.Participants = []string{"participant-agent"}
	var preview bytes.Buffer
	printPreview(&preview, st)
	var coverage bytes.Buffer
	writeShow(&coverage, st, nil, nil)

	legacyChair := regexp.MustCompile(`(?i)\bchair(?:ed|ing)?\b`)
	for name, text := range map[string]string{
		"preview":   preview.String(),
		"coverage":  coverage.String(),
		"reference": referenceMD,
	} {
		if !strings.Contains(strings.ToLower(text), "facilitator") {
			t.Errorf("%s does not name the facilitator:\n%s", name, text)
		}
		if legacyChair.MatchString(text) {
			t.Errorf("%s exposes legacy chair terminology:\n%s", name, text)
		}
	}
}

func TestPublicRoleErrorsUseFacilitatorAndCanonicalAgentList(t *testing.T) {
	st := newRoom(t)
	st.Chair = "codex"
	st.Participants = []string{"codex"}
	if err := st.Validate(); err == nil || !strings.Contains(err.Error(), "facilitator and participant") || strings.Contains(err.Error(), "chair") {
		t.Fatalf("role error = %v", err)
	}

	pinFleet(t)
	err := routableSeat("not-registered-anywhere")
	if err == nil || !strings.Contains(err.Error(), "bashy agent list") || strings.Contains(err.Error(), "--all") {
		t.Fatalf("roster guidance = %v", err)
	}
}
