package otelcli

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func writeSpool(t *testing.T, lines ...string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "spans.jsonl")
	body := ""
	for _, l := range lines {
		body += l + "\n"
	}
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestImportSpoolTruncatesOnlyOnSuccess(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	p := writeSpool(t, `{"_msg":"a"}`, `{"_msg":"b"}`)
	n, err := importSpool(srv.URL, p)
	if err != nil {
		t.Fatalf("importSpool: %v", err)
	}
	if n != 2 {
		t.Errorf("imported %d records, want 2", n)
	}
	fi, _ := os.Stat(p)
	if fi.Size() != 0 {
		t.Errorf("spool should be truncated after success, size=%d", fi.Size())
	}
}

// A rejected import must LEAVE the spool intact. Truncating on failure would
// lose spans permanently; re-importing is harmless by comparison.
func TestImportSpoolKeepsDataWhenStoreRejects(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "bad request", http.StatusBadRequest)
	}))
	defer srv.Close()

	p := writeSpool(t, `{"_msg":"keep-me"}`)
	before, _ := os.Stat(p)
	if _, err := importSpool(srv.URL, p); err == nil {
		t.Fatal("expected an error when the store rejects the spool")
	}
	after, _ := os.Stat(p)
	if after.Size() != before.Size() {
		t.Errorf("spool changed on failure: %d -> %d", before.Size(), after.Size())
	}
}

// No spool, or an empty one, is the normal quiet case — not an error, since
// `serve` calls this on every start.
func TestImportSpoolQuietWhenNothingToDo(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "absent.jsonl")
	if n, err := importSpool("http://127.0.0.1:1", missing); err != nil || n != 0 {
		t.Errorf("missing spool: n=%d err=%v, want 0/nil", n, err)
	}
	empty := writeSpool(t)
	if n, err := importSpool("http://127.0.0.1:1", empty); err != nil || n != 0 {
		t.Errorf("empty spool: n=%d err=%v, want 0/nil", n, err)
	}
}

// The import must name the fields VictoriaLogs needs, or records land without a
// stream/time/message and the query verbs cannot group them.
func TestImportSpoolSendsStreamFields(t *testing.T) {
	var gotQuery, gotCT string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.RawQuery
		gotCT = r.Header.Get("Content-Type")
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	if _, err := importSpool(srv.URL, writeSpool(t, `{"_msg":"x"}`)); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"_stream_fields=service_name", "_time_field=_time", "_msg_field=_msg"} {
		if !contains(gotQuery, want) {
			t.Errorf("query %q missing %q", gotQuery, want)
		}
	}
	if gotCT != "application/stream+json" {
		t.Errorf("Content-Type=%q, want application/stream+json", gotCT)
	}
}

func contains(h, n string) bool {
	return len(h) >= len(n) && (h == n || len(n) == 0 || indexOf(h, n) >= 0)
}

func indexOf(h, n string) int {
	for i := 0; i+len(n) <= len(h); i++ {
		if h[i:i+len(n)] == n {
			return i
		}
	}
	return -1
}
