package fleet

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestPairedHostDiskPressureDistinguishesVolumes(t *testing.T) {
	const status = `{"host":"fixture","volumes":[{"id":"wsl-1111111111111111","display_name":"WSL Fixture-Alpha /","observed_at":"2026-11-25T23:00:00Z","used_bytes":800,"total_bytes":1000,"alerts":[{"threshold":80,"alert_id":"alert-alpha","delivery":"sent","growers":[{"label":"otel","delta_bytes":12}]}]},{"id":"wsl-2222222222222222","display_name":"WSL Fixture-Beta /","used_bytes":900,"total_bytes":1000,"alerts":[{"threshold":90,"alert_id":"alert-beta","delivery":"sent","growers":[{"label":"otel","delta_bytes":15}]}]}]}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/api/v1/hosts/fixture/disk-pressure" ||
			r.Header.Get("Authorization") != "Bearer fixture-token" {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		_, _ = w.Write([]byte(status))
	}))
	defer srv.Close()

	for _, jsonMode := range []bool{false, true} {
		cmd := NewPairedHostCmd()
		var out bytes.Buffer
		cmd.SetOut(&out)
		args := []string{"--master", srv.URL, "--token", "fixture-token", "reachability", "disk-pressure", "fixture"}
		if jsonMode {
			args = append(args, "--json")
		}
		cmd.SetArgs(args)
		if err := cmd.Execute(); err != nil {
			t.Fatal(err)
		}
		if jsonMode {
			if out.String() != status+"\n" {
				t.Fatalf("CLI changed disk status: %s", &out)
			}
			continue
		}
		for _, want := range []string{
			"WSL Fixture-Alpha / (wsl-1111111111111111)\t800/1000 bytes used\tobserved 2026-11-25T23:00:00Z\n  alert alert-alpha\t80%\tsent\n    otel\t+12 bytes at crossing",
			"WSL Fixture-Beta / (wsl-2222222222222222)\t900/1000 bytes used\n  alert alert-beta\t90%\tsent\n    otel\t+15 bytes at crossing",
		} {
			if !strings.Contains(out.String(), want) {
				t.Fatalf("disk status missing %q: %s", want, &out)
			}
		}
	}
}
