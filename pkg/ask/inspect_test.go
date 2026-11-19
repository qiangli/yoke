package ask

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestInspectPendingDoesNotReapOrDecide(t *testing.T) {
	t.Setenv("BASHY_ASK_DIR", t.TempDir())
	r, err := Raise(ApprovalSpec{Source: SourceConfirm, Argv: []string{"write", "candidate"}})
	if err != nil {
		t.Fatal(err)
	}
	// A stale request stays on disk when merely observed.
	stale := r
	stale.ID = "expired"
	stale.Expires = time.Now().Add(-time.Hour)
	stale.ValueExpires = stale.Expires
	if err := save(stale); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		xs, err := InspectPending()
		if err != nil || len(xs) != 1 || xs[0].ID != r.ID {
			t.Fatalf("%+v %v", xs, err)
		}
	}
	if _, err := os.Stat(filepath.Join(Dir(), stale.ID)); err != nil {
		t.Fatal("inspection reaped request", err)
	}
	if _, ok := LoadDecision(r.ID); ok {
		t.Fatal("inspection decided approval")
	}
}
