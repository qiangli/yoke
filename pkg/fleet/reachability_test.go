package fleet

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
)

func TestPairedHostReachabilityWire(t *testing.T) {
	const status = `{"host":"fixture","state":"degraded","last_seen_at":"2026-11-21T00:00:00Z","monitoring":true,"threshold_seconds":900}`
	for _, test := range []struct {
		args  []string
		patch map[string]any
	}{
		{[]string{"status", "fixture"}, nil},
		{[]string{"ack", "fixture", "fixture-alert"}, map[string]any{"ack_alert_id": "fixture-alert"}},
		{[]string{"mute", "fixture", "20m"}, map[string]any{"mute_seconds": float64(1200)}},
		{[]string{"threshold", "fixture", "default"}, map[string]any{"threshold_seconds": float64(900)}},
		{[]string{"threshold", "fixture", "off"}, map[string]any{"threshold_seconds": float64(0)}},
		{[]string{"threshold", "@all", "20m"}, map[string]any{"threshold_seconds": float64(1200)}},
		{[]string{"threshold", "fixture", "inherit"}, map[string]any{"threshold_seconds": float64(-1)}},
	} {
		t.Run(test.args[0]+test.args[len(test.args)-1], func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				host := test.args[1]
				if host == "@all" {
					host = "~all"
				}
				if r.URL.Path != "/api/v1/hosts/"+host+"/reachability" ||
					r.Header.Get("Authorization") != "Bearer fixture-token" {
					t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
				}
				if test.patch == nil {
					if r.Method != http.MethodGet {
						t.Errorf("status used %s", r.Method)
					}
				} else {
					var got map[string]any
					if r.Method != http.MethodPatch || json.NewDecoder(r.Body).Decode(&got) != nil || !reflect.DeepEqual(got, test.patch) {
						t.Errorf("unexpected mutation: %s %v", r.Method, got)
					}
				}
				_, _ = w.Write([]byte(status))
			}))
			defer srv.Close()
			cmd := NewPairedHostCmd()
			var out bytes.Buffer
			cmd.SetOut(&out)
			cmd.SetArgs(append([]string{"--master", srv.URL, "--token", "fixture-token", "--json", "reachability"}, test.args...))
			if err := cmd.Execute(); err != nil {
				t.Fatal(err)
			}
			if out.String() != status+"\n" {
				t.Fatalf("CLI changed server view: %s", &out)
			}
		})
	}
}
