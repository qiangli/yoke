package dbts

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/qiangli/yoke/pkg/dbstore"
)

// vmFixture is a scripted VictoriaMetrics stand-in: a test-local HTTP
// fixture, never a provisioned service and never live evidence.
type vmFixture struct {
	t       *testing.T
	imports []string
	query   string
	fail    int
}

func (v *vmFixture) handler(w http.ResponseWriter, r *http.Request) {
	switch {
	case strings.HasPrefix(r.URL.Path, vmQueryPath):
		v.query = r.URL.Query().Get("query")
		if v.fail != 0 {
			http.Error(w, "storage is closed", v.fail)
			return
		}
		_, _ = fmt.Fprint(w, `{"status":"success","data":{"resultType":"matrix","result":[`+
			`{"metric":{"__name__":"up","job":"api"},"values":[[1725148800,"1"],[1725148860,"0.5"]]}`+
			`]}}`)
	case r.URL.Path == vmImportPath:
		b, _ := io.ReadAll(r.Body)
		v.imports = append(v.imports, string(b))
		_, _ = fmt.Fprint(w, "")
	default:
		http.NotFound(w, r)
	}
}

func startVM(t *testing.T, fail int) (*vmFixture, string) {
	t.Helper()
	v := &vmFixture{t: t, fail: fail}
	srv := httptest.NewServer(http.HandlerFunc(v.handler))
	t.Cleanup(srv.Close)
	return v, srv.URL
}

func tagArr(t *testing.T, v any) map[string]any {
	t.Helper()
	b, _ := json.Marshal(v)
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	return m
}

func TestTSRemoteQueryAgainstFixture(t *testing.T) {
	_, ep := startVM(t, 0)
	c, err := NewClient(ep, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	series, err := c.Query(ctx, "up", 1_725_148_800_000_000_000, 1_725_148_920_000_000_000, 60*time.Second, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(series) != 1 || series[0].Name != "up" || len(series[0].Points) != 2 {
		t.Fatalf("series = %+v", series)
	}
	if series[0].Labels["job"] != "api" {
		t.Fatalf("labels = %v", series[0].Labels)
	}
	if v := series[0].Points[0].Value["v"]; v != float64(1) {
		t.Fatalf("value = %v", v)
	}
	if v := series[0].Points[0].Ts["v"]; v != "2024-09-01T00:00:00Z" {
		t.Fatalf("ts = %v", v)
	}
}

func TestTSRemoteEndpointErrors(t *testing.T) {
	ctx := context.Background()
	t.Run("http 500", func(t *testing.T) {
		_, ep := startVM(t, 500)
		c, _ := NewClient(ep, 5*time.Second)
		_, err := c.Query(ctx, "up", 1, 120_000_000_000, 60*time.Second, 100)
		if dbstore.Code(err) != "backend_error" || !strings.Contains(err.Error(), "500") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("vm error envelope", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = fmt.Fprint(w, `{"status":"error","errorType":"bad_data","error":"bad query"}`)
		}))
		defer srv.Close()
		c, _ := NewClient(srv.URL, 5*time.Second)
		_, err := c.Query(ctx, "up", 1, 120_000_000_000, 60*time.Second, 100)
		if dbstore.Code(err) != "backend_error" || !strings.Contains(err.Error(), "bad query") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("unreachable", func(t *testing.T) {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		addr := ln.Addr().String()
		_ = ln.Close()
		c, _ := NewClient("http://"+addr, 2*time.Second)
		_, err = c.Query(ctx, "up", 1, 120_000_000_000, 60*time.Second, 100)
		if dbstore.Code(err) != "backend_error" || !strings.Contains(err.Error(), "unreachable") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("cancelled", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			time.Sleep(2 * time.Second)
		}))
		defer srv.Close()
		c, _ := NewClient(srv.URL, 30*time.Second)
		cctx, cancel := context.WithCancel(ctx)
		cancel()
		_, err := c.Query(cctx, "up", 1, 120_000_000_000, 60*time.Second, 100)
		if dbstore.Code(err) != "cancelled" {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("timeout", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			time.Sleep(2 * time.Second)
		}))
		defer srv.Close()
		c, _ := NewClient(srv.URL, 200*time.Millisecond)
		_, err := c.Query(ctx, "up", 1, 120_000_000_000, 60*time.Second, 100)
		if code := dbstore.Code(err); code != "timeout" && code != "backend_error" {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("over limit", func(t *testing.T) {
		_, ep := startVM(t, 0)
		c, _ := NewClient(ep, 5*time.Second)
		_, err := c.Query(ctx, "up", 1, 120_000_000_000, 60*time.Second, 1)
		if dbstore.Code(err) != "limit_exceeded" {
			t.Fatalf("err = %v", err)
		}
	})
}

func TestTSRemoteIngestRendersPrometheusText(t *testing.T) {
	v, ep := startVM(t, 0)
	c, err := NewClient(ep, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	n, err := c.Ingest(context.Background(), []RemoteSeries{{
		Name:   "http_requests",
		Labels: map[string]string{"job": `a"b`, "path": "/x"},
		Points: []RemotePoint{
			{Ts: tagArr(t, encodeTS(1_725_148_800_000_000_000)), Value: tagArr(t, encodeValue(12.5))},
			{Ts: tagArr(t, encodeTS(1_725_148_860_000_000_000)), Value: tagArr(t, encodeValue(13))},
		},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("n = %d", n)
	}
	if len(v.imports) != 1 {
		t.Fatalf("imports = %d", len(v.imports))
	}
	body := v.imports[0]
	if !strings.Contains(body, `http_requests{job="a\"b",path="/x"} 12.5 1725148800000`) {
		t.Fatalf("body:\n%s", body)
	}
	if v.query != "" {
		t.Fatalf("ingest issued a query: %q", v.query)
	}
}

func TestTSRemoteOpsThroughLifecycle(t *testing.T) {
	v, ep := startVM(t, 0)
	f := newFixture(t, dbstore.Persistent) // admission anchor only
	q := Input{Endpoint: ep, Expr: "up",
		Start: json.RawMessage(intTS(1_725_148_800_000_000_000)),
		End:   json.RawMessage(intTS(1_725_148_920_000_000_000)),
		Step:  "60s", Limit: 100}
	out := f.must(OpRemoteQuery, q, false)
	r := out.Result.(map[string]any)
	if r["endpoint"] != EndpointID(ep) {
		t.Fatalf("endpoint = %v", r["endpoint"])
	}
	if len(r["series"].([]RemoteSeries)) != 1 {
		t.Fatalf("series = %v", r["series"])
	}
	in := Input{Endpoint: ep, SeriesList: []Series{{
		Name: "batch", Points: []Point{tsPoint(intTS(1_725_148_800_000_000_000), f64v(1))},
	}}}
	res := f.must(OpRemoteIngest, in, false).Result.(map[string]any)
	if res["points"] != 1 {
		t.Fatalf("ingest = %v", res)
	}
	if len(v.imports) != 1 || !strings.Contains(v.imports[0], "batch{} 1 1725148800000") {
		t.Fatalf("imports = %q", v.imports)
	}
	// Specs: remote modes, network effects, provider-validated preview.
	if s := Spec(OpRemoteQuery, dbstore.Persistent, 0); s.DryRun != "provider-validated-preview" {
		t.Fatalf("query spec = %+v", s)
	}
	if s := Spec(OpRemoteIngest, dbstore.Memory, 1); len(s.Modes) != 1 || s.Modes[0] != "remote" {
		t.Fatalf("ingest spec = %+v", s)
	}
}
