package broker

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/qiangli/yoke/pkg/quotaobs"
)

func TestProxyObservations(t *testing.T) {
	for _, status := range []int{429, 402, 403, 500, 200} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			t.Setenv("BASHY_HOME", t.TempDir())
			payload := "quota exceeded Bearer abcdefghijklmnopqrstuvwxyz" + strings.Repeat("x", 3000)
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Retry-After", "120")
				w.Header().Set("X-RateLimit-Reset-Tokens", "1234")
				w.Header().Set("Authorization", "never-store-this")
				w.WriteHeader(status)
				io.WriteString(w, payload)
			}))
			defer upstream.Close()
			b := &Broker{engine: engineClient{base: upstream.URL}}
			rec := httptest.NewRecorder()
			b.proxyEngine(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader("private request")), nil)
			if rec.Code != status || rec.Body.String() != payload {
				t.Fatalf("response changed: %d", rec.Code)
			}
			got, err := quotaobs.Read(time.Time{}, nil)
			if err != nil || len(got) != 1 {
				t.Fatalf("observations %v: %+v", err, got)
			}
			o := got[0]
			if o.Source != "door" || o.Status != status || o.Headers["retry-after"] != "120" || o.Headers["x-ratelimit-reset-tokens"] != "1234" || len(o.Headers) != 2 {
				t.Fatalf("metadata: %+v", o)
			}
			if strings.Contains(o.Excerpt, "abcdefghijklmnop") || strings.Contains(o.Excerpt, "private request") || len(o.Excerpt) > 2048 {
				t.Fatalf("unsafe excerpt: %q", o.Excerpt)
			}
			if status == 200 && o.Excerpt != "" {
				t.Fatal("success body recorded")
			}
			if status != 200 && !strings.Contains(o.Excerpt, "[REDACTED]") {
				t.Fatal("missing redaction")
			}
		})
	}
}
