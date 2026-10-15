package cligw

import (
	"strings"
	"testing"
	"time"

	"github.com/qiangli/yoke/pkg/quotaobs"
)

func TestWorkerQuotaObservations(t *testing.T) {
	for _, mode := range []string{"quota-event", "quota-stderr", "quota-stdout", "quota-http", "backend-text"} {
		t.Run(mode, func(t *testing.T) {
			backend, cleanup := testBackend(t, mode)
			defer cleanup()
			t.Setenv("BASHY_HOME", t.TempDir())
			serveBackend(t, backend, `{"model":"test-model","messages":[{"role":"user","content":"private request"}]}`, nil)
			got, err := quotaobs.Read(time.Time{}, nil)
			if err != nil {
				t.Fatal(err)
			}
			if mode == "backend-text" {
				if len(got) != 0 {
					t.Fatalf("ordinary output recorded: %+v", got)
				}
				return
			}
			if len(got) != 1 {
				t.Fatalf("observations: %+v", got)
			}
			o := got[0]
			if o.Source != "cligw" || o.Tool != "fake-"+mode || o.Agent != "test-agent" || o.Model != "quota-fixture-model" || o.Plan != "fixture-plan" {
				t.Fatalf("metadata: %+v", o)
			}
			if strings.Contains(o.Excerpt, "abcdefghijklmnopqrstuvwxyz") || strings.Contains(o.Excerpt, "private request") || strings.Contains(o.Excerpt, "Authorization") || !strings.Contains(o.Excerpt, "[REDACTED]") {
				t.Fatalf("excerpt: %q", o.Excerpt)
			}
			if mode == "quota-http" && (o.Status != 429 || o.Headers["retry-after"] != "120" || len(o.Headers) != 1) {
				t.Fatalf("HTTP metadata: %+v", o)
			}
		})
	}
}
