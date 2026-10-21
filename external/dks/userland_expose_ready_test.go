package dks

import (
	"net/http"
	"strings"
	"testing"

	"github.com/qiangli/yoke/pkg/dagrun"
)

// planeDeletes returns the recorded runner calls that delete from the plane.
func planeDeletes(calls []runnerCall) []runnerCall {
	var out []runnerCall
	for _, c := range calls {
		if action(c.argv) == "delete" {
			out = append(out, c)
		}
	}
	return out
}

// assertExposeRolledBack is the one post-condition both failing waits share:
// the Outpost app is gone and the expose's Secret was deleted by its label —
// exactly what a failed apply leaves behind, which is nothing.
func assertExposeRolledBack(t *testing.T, et *exposeTest, name string) {
	t.Helper()
	if len(et.apps.rms) != 1 || et.apps.rms[0] != name {
		t.Errorf("Outpost app removals = %v, want exactly [%s]", et.apps.rms, name)
	}
	deletes := planeDeletes(et.runner.calls)
	if len(deletes) != 1 {
		t.Fatalf("plane delete calls = %d, want exactly 1 (the expose's Secret)", len(deletes))
	}
	argv := strings.Join(deletes[0].argv, " ")
	wantSel := exposeLabel + "=" + name
	if !strings.Contains(argv, wantSel) {
		t.Errorf("delete argv %q does not select %q", argv, wantSel)
	}
	if !strings.Contains(argv, dagrun.LabelManagedBy+"="+dagrun.ManagedByBashy) {
		t.Errorf("delete argv %q is not label-owned", argv)
	}
}

// RED (S487.2 live fix B): `dks expose` returns only once the app route is
// LIVE AND GUARDED.
//
// What went wrong live: right after `outpost apps add` the satellite edge
// still believed the new app was login-required and answered a token-less
// request 401 {"error":"You need to be signed in to view this app."} for
// ~35 s. Only once it has learned otherwise does the request reach the
// Outpost expose guard, which refuses a request without the token with 403.
// So 403 — not 200 — is the ready signal, and expose, not the run, pays the
// wait: pods started inside that window simply failed.
func TestExposeWaitsForGuardedRoute(t *testing.T) {
	et := newExposeTest(t)
	et.selectWorkspace(t, "skel")
	et.probe = []int{http.StatusUnauthorized, http.StatusUnauthorized, http.StatusForbidden}

	code, stdout, stderr := et.exec(t, "expose", "http://127.0.0.1:9000", "--as", "web")
	if code != 0 {
		t.Fatalf("expose exited %d: %s", code, stderr)
	}
	if len(et.probed) != 3 {
		t.Fatalf("probed the app route %d times, want 3 (401, 401, 403)", len(et.probed))
	}
	for i, got := range et.probed {
		if got != testAppURL("web") {
			t.Errorf("probe %d asked for %q, want the app route %q", i, got, testAppURL("web"))
		}
	}
	// One probe every 2s on the injected clock: two waits before the guard.
	if et.sleeps != 2 {
		t.Errorf("slept %d times between probes, want 2", et.sleeps)
	}
	if !strings.Contains(stderr, "ready after 4s") {
		t.Errorf("stderr %q does not report how long the app route took", stderr)
	}
	// Nothing rolled back: the expose landed.
	if len(et.apps.rms) != 0 {
		t.Errorf("a successful expose removed the Outpost app: %v", et.apps.rms)
	}
	if got := planeDeletes(et.runner.calls); len(got) != 0 {
		t.Errorf("a successful expose deleted from the plane: %v", got)
	}
	if objs := appliedObjects(t, et.runner.calls); objs["Secret"] == nil {
		t.Error("expose applied no Secret")
	}
	if want := (exposeRecord{Name: "web"}).envVarName(); !strings.Contains(stdout, want) {
		t.Errorf("stdout %q does not name the variable runs will carry (%s)", stdout, want)
	}
}

// RED (S487.2 live fix B): a 2xx on the TOKEN-LESS probe is a hard failure —
// the route answered a request carrying no expose token, so the guard is not
// in place and the service would be reachable by anyone who can reach the
// satellite. Nothing is left behind.
func TestExposeRollsBackWhenRouteAnswersWithoutToken(t *testing.T) {
	et := newExposeTest(t)
	et.selectWorkspace(t, "skel")
	et.probe = []int{http.StatusOK}

	code, stdout, stderr := et.exec(t, "expose", "http://127.0.0.1:9000", "--as", "web")
	if code == 0 {
		t.Fatalf("an unguarded route exposed successfully (stdout %q)", stdout)
	}
	for _, want := range []string{"without the expose token", "guard", "rolled back"} {
		if !strings.Contains(stderr, want) {
			t.Errorf("stderr %q does not carry %q", stderr, want)
		}
	}
	if strings.TrimSpace(stdout) != "" {
		t.Errorf("a rolled-back expose printed %q on stdout: no run may be told to use it", stdout)
	}
	if len(et.probed) != 1 {
		t.Errorf("probed %d times, want 1: a 2xx is terminal, not retried", len(et.probed))
	}
	assertExposeRolledBack(t, et, "web")
	if strings.Contains(stderr, testExposeToken) {
		t.Error("the failure printed the token")
	}
}

// RED (S487.2 live fix B): a route that never reaches the guard fails on the
// deadline with the last status named, and rolls back exactly like a failed
// apply.
func TestExposeRollsBackOnReadyDeadline(t *testing.T) {
	et := newExposeTest(t)
	et.selectWorkspace(t, "skel")
	et.probe = []int{http.StatusUnauthorized}

	code, stdout, stderr := et.exec(t, "expose", "http://127.0.0.1:9000", "--as", "web")
	if code == 0 {
		t.Fatalf("expose exited 0 with a route that never got guarded (stdout %q)", stdout)
	}
	if !strings.Contains(stderr, "app route not ready after 120s") {
		t.Errorf("stderr %q does not report the deadline", stderr)
	}
	if !strings.Contains(stderr, "401") {
		t.Errorf("stderr %q does not name the last status", stderr)
	}
	// 120s at one probe every 2s: 60 waits, 61 probes.
	if et.sleeps != 60 {
		t.Errorf("slept %d times, want 60 (120s / 2s)", et.sleeps)
	}
	if len(et.probed) != 61 {
		t.Errorf("probed %d times, want 61", len(et.probed))
	}
	assertExposeRolledBack(t, et, "web")
}
