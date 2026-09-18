package engine

import (
	"testing"
)

func TestSessionLabel(t *testing.T) {
	if SessionLabel != "ycode.session" {
		t.Errorf("expected ycode.session, got %q", SessionLabel)
	}
}
