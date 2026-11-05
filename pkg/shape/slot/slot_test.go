package slot

import (
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func executable(t *testing.T, dir, name, body string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if e := os.WriteFile(path, []byte(body), 0755); e != nil {
		t.Fatal(e)
	}
	return path
}
func stage(t *testing.T, s Store, dir, revision string) Version {
	t.Helper()
	v, e := s.Stage(executable(t, dir, revision, "binary-"+revision), revision, "sealed-evidence-"+revision)
	if e != nil {
		t.Fatal(e)
	}
	return v
}
func selected(t *testing.T, s Store, want string) Version {
	t.Helper()
	v, ok, e := s.Select()
	if e != nil || !ok || v.Revision != want {
		t.Fatalf("selection revision=%q present=%v err=%v, want %q", v.Revision, ok, e, want)
	}
	return v
}
func TestSlotStageActivateUndoRestorePreserveRelease(t *testing.T) {
	home, inputs := t.TempDir(), t.TempDir()
	s := Store{Home: home, App: "example", ReleaseRevision: "release-source"}
	release := executable(t, inputs, "released", "released-binary")
	one := stage(t, s, inputs, "source-one")
	if _, ok, e := s.Select(); e != nil || ok {
		t.Fatalf("staging activated executable: %v %v", ok, e)
	}
	if _, e := s.Activate(one, release); e != nil {
		t.Fatal(e)
	}
	selected(t, s, "source-one")
	// Another principal's caller sees the same host selection: Store has no
	// principal component and reopening it reads the same atomic record.
	selected(t, Store{Home: home, App: "example"}, "source-one")
	two := stage(t, s, inputs, "source-two")
	if _, e := s.Activate(two, release); e != nil {
		t.Fatal(e)
	}
	selected(t, s, "source-two")
	if _, e := s.Undo(); e != nil {
		t.Fatal(e)
	}
	selected(t, s, "source-one")
	if _, e := s.Restore("release"); e != nil {
		t.Fatal(e)
	}
	v := selected(t, s, "release-source")
	if v.Path == release {
		t.Fatal("release snapshot points at mutable installation")
	}
	raw, e := os.ReadFile(release)
	if e != nil || string(raw) != "released-binary" {
		t.Fatal("installed release changed")
	}
	if _, e := s.Undo(); e != nil {
		t.Fatal(e)
	}
	selected(t, s, "source-one")
	if _, e := s.Restore(two.Digest); e != nil {
		t.Fatal(e)
	}
	selected(t, s, "source-two")
	if _, e := s.Restore("unknown"); !errors.Is(e, ErrUnknownVersion) {
		t.Fatalf("unknown restore=%v", e)
	}
	selected(t, s, "source-two")
}
func TestSlotDigestTamperingFailsClosedAndUndoRecovers(t *testing.T) {
	home, inputs := t.TempDir(), t.TempDir()
	s := Store{Home: home, App: "example"}
	release := executable(t, inputs, "released", "released-binary")
	one := stage(t, s, inputs, "source-one")
	if _, e := s.Activate(one, release); e != nil {
		t.Fatal(e)
	}
	if e := os.Chmod(one.Path, 0755); e != nil {
		t.Fatal(e)
	}
	if e := os.WriteFile(one.Path, []byte("changed-bytes"), 0755); e != nil {
		t.Fatal(e)
	}
	if _, ok, e := s.Select(); e == nil || ok {
		t.Fatal("tampered artifact selected")
	}
	if _, ok, err := s.RetainedRelease(); err != nil || !ok {
		t.Fatalf("damaged active artifact hid recovery release: %v", err)
	}
	if _, e := s.Undo(); e != nil {
		t.Fatal(e)
	}
	selected(t, s, "unknown")
	if _, e := s.Restore(one.Digest); e == nil {
		t.Fatal("tampered artifact restored")
	}
}
func TestSlotActivationRejectsUnconfinedPath(t *testing.T) {
	home, inputs := t.TempDir(), t.TempDir()
	s := Store{Home: home, App: "example"}
	one := stage(t, s, inputs, "source-one")
	one.Path = executable(t, inputs, "outside", "binary-source-one")
	if _, e := s.Activate(one, one.Path); e == nil {
		t.Fatal("outside artifact accepted")
	}
	if _, ok, e := s.Select(); e != nil || ok {
		t.Fatal("failed activation changed pointer")
	}
	for _, app := range []string{"", "..", "../other", "/absolute", "back\\slash"} {
		if _, e := (Store{Home: home, App: app}).Dir(); e == nil {
			t.Errorf("invalid app %q accepted", app)
		}
	}
}
func TestSlotInterruptedPublication(t *testing.T) {
	home, inputs := t.TempDir(), t.TempDir()
	s := Store{Home: home, App: "example"}
	release := executable(t, inputs, "released", "released-binary")
	one := stage(t, s, inputs, "source-one")
	two := stage(t, s, inputs, "source-two")
	if _, e := s.Activate(one, release); e != nil {
		t.Fatal(e)
	}
	request := filepath.Join(inputs, "request.json")
	b, e := json.Marshal(two)
	if e != nil {
		t.Fatal(e)
	}
	if e = os.WriteFile(request, b, 0600); e != nil {
		t.Fatal(e)
	}
	exe, e := os.Executable()
	if e != nil {
		t.Fatal(e)
	}
	for _, test := range []struct {
		phase, want string
		exit        int
	}{{"before", "source-one", 31}, {"after", "source-two", 32}} {
		cmd := exec.Command(exe, "-test.run=^TestSlotCrashHelper$")
		cmd.Env = append(os.Environ(), "BASHY_SLOT_TEST_HOME="+home, "BASHY_SLOT_TEST_REQUEST="+request, "BASHY_SLOT_TEST_PHASE="+test.phase)
		out, e := cmd.CombinedOutput()
		var exit *exec.ExitError
		if !errors.As(e, &exit) || exit.ExitCode() != test.exit {
			t.Fatalf("helper phase %s exit=%v output=%s", test.phase, e, out)
		}
		selected(t, s, test.want)
	}
	raw, e := os.ReadFile(release)
	if e != nil || string(raw) != "released-binary" {
		t.Fatal("interruption modified installed release")
	}
}
func TestSlotCrashHelper(t *testing.T) {
	home := os.Getenv("BASHY_SLOT_TEST_HOME")
	if home == "" {
		return
	}
	b, e := os.ReadFile(os.Getenv("BASHY_SLOT_TEST_REQUEST"))
	if e != nil {
		t.Fatal(e)
	}
	var v Version
	if e = json.Unmarshal(b, &v); e != nil {
		t.Fatal(e)
	}
	s := Store{Home: home, App: "example"}
	if os.Getenv("BASHY_SLOT_TEST_PHASE") == "before" {
		s.beforePublish = func(State) error { os.Exit(31); return nil }
	} else {
		s.afterPublish = func(State) error { os.Exit(32); return nil }
	}
	if _, e = s.Activate(v, ""); e != nil {
		t.Fatal(e)
	}
	t.Fatal("crash hook was not called")
}
