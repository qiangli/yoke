package fleet

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestSuperadminHostsLsAndRm(t *testing.T) {
	var got []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer step-up-session" {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		got = append(got, r.Method+" "+r.URL.RequestURI())
		switch r.Method {
		case http.MethodGet:
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`[{"name":"lj2ivy","owner":"other@example.com","pair_mode":"legacy"},{"name":"box","owner":"me@example.com"}]`))
		case http.MethodDelete:
			w.WriteHeader(http.StatusNoContent)
		}
	}))
	defer srv.Close()
	t.Setenv(SuperadminSessionEnv, "step-up-session")
	t.Setenv("BASHY_FLEET_TOKEN", "owner-token-must-not-be-used")

	run := func(args ...string) string {
		cmd := NewSuperadminCmd()
		var out bytes.Buffer
		cmd.SetOut(&out)
		cmd.SetErr(&out)
		cmd.SetArgs(append(args, "--master", srv.URL))
		if err := cmd.Execute(); err != nil {
			t.Fatalf("%v: %v\n%s", args, err, out.String())
		}
		return out.String()
	}
	if out := run("hosts", "ls", "--owner", "other@example.com"); !strings.Contains(out, "other@example.com\tlj2ivy\tlegacy") || strings.Contains(out, "box") {
		t.Fatalf("ls --owner: %q", out)
	}
	if out := run("hosts", "rm", "other@example.com/lj2ivy"); !strings.Contains(out, "removed other@example.com/lj2ivy") {
		t.Fatalf("rm: %q", out)
	}
	if want := "DELETE /api/superadmin/hosts/other@example.com/lj2ivy?confirm=lj2ivy"; got[len(got)-1] != want {
		t.Fatalf("request = %q, want %q", got[len(got)-1], want)
	}
}

func TestSuperadminNeedsStepUpSession(t *testing.T) {
	t.Setenv(SuperadminSessionEnv, "")
	t.Setenv("BASHY_FLEET_TOKEN", "owner-token")
	cmd := NewSuperadminCmd()
	cmd.SetArgs([]string{"hosts", "ls", "--master", "http://127.0.0.1:1"})
	err := cmd.Execute()
	if err == nil || !strings.Contains(err.Error(), SuperadminSessionEnv) {
		t.Fatalf("err = %v, want a missing-session error", err)
	}
	cmd = NewSuperadminCmd()
	cmd.SetArgs([]string{"hosts", "rm", "no-slash", "--master", "http://127.0.0.1:1"})
	if err := cmd.Execute(); err == nil || !strings.Contains(err.Error(), "OWNER/HOST") {
		t.Fatalf("rm without OWNER/HOST: %v", err)
	}
}
