package instance

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	"github.com/qiangli/yoke/pkg/fleet"
)

func run(t *testing.T, store *fleet.InstanceStore, args ...string) (string, error) {
	t.Helper()
	var out, errb bytes.Buffer
	cmd := NewCmd(store)
	cmd.SetOut(&out)
	cmd.SetErr(&errb)
	cmd.SetArgs(args)
	err := cmd.Execute()
	return out.String(), err
}

func isolate(t *testing.T) *fleet.InstanceStore {
	t.Helper()
	for _, k := range []string{"BASHY_FLEET_DIR", "BASHY_ROOM_DIR", "BASHY_MB_DIR", "BASHY_MEET_DIR", "BASHY_SPRINT_DIR"} {
		t.Setenv(k, t.TempDir())
	}
	t.Setenv("BASHY_AGENTS_DIR", "")
	t.Setenv("BASHY_AGENTS_PATH", "")
	return fleet.NewInstanceStore(t.TempDir())
}

// GAP 3. open prints the export line, list shows the instance, retire archives
// and frees the label, and the cap error's advice names a verb that exists.
func TestInstanceOpenListRetire(t *testing.T) {
	store := isolate(t).WithCap(1)

	out, err := run(t, store, "open", "claude:opus5.5")
	if err != nil {
		t.Fatal(err)
	}
	insts, _ := store.List()
	if len(insts) != 1 {
		t.Fatalf("store holds %d instances after open", len(insts))
	}
	inst := insts[0]
	want := "export BASHY_PRINCIPAL=" + inst.URN() + " BASHY_INSTANCE=" + inst.UUID
	if strings.TrimSpace(out) != want {
		t.Fatalf("open printed %q; want %q", out, want)
	}

	list, err := run(t, store, "list")
	if err != nil || !strings.Contains(list, inst.UUID) || !strings.Contains(list, "active") {
		t.Fatalf("list = %q, %v", list, err)
	}

	// Cap exhausted: actionable, and the verb it names is this one.
	_, err = run(t, store, "open", "claude:opus5.5")
	var capErr *fleet.CapError
	if !errors.As(err, &capErr) || !strings.Contains(err.Error(), "instance retire") {
		t.Fatalf("second open under cap 1: %v", err)
	}

	if _, err := run(t, store, "retire", inst.UUID); err != nil {
		t.Fatal(err)
	}
	list, _ = run(t, store, "list")
	if !strings.Contains(list, "retired") {
		t.Fatalf("list after retire = %q", list)
	}
	if _, err := run(t, store, "open", "claude:opus5.5"); err != nil {
		t.Fatalf("open after retire: %v", err)
	}
}

func TestInstanceOpenRefusesAnUnknownName(t *testing.T) {
	store := isolate(t)
	if _, err := run(t, store, "open", "nosuchthing"); err == nil {
		t.Fatal("open of a name that is neither agent nor binding succeeded")
	}
}
