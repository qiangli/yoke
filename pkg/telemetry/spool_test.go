package telemetry

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// sanitizeField exists so LogsQL can FILTER on a field. "exit.code" parses as a
// field access in a query, so a dotted key is unqueryable — which would reduce
// the otel verbs to substring matching on the message.
func TestSanitizeFieldMakesKeysQueryable(t *testing.T) {
	for in, want := range map[string]string{
		"cmd.exit_code": "cmd_exit_code",
		"service.name":  "service_name",
		"a.b.c":         "a_b_c",
		"already_plain": "already_plain",
	} {
		if got := sanitizeField(in); got != want {
			t.Errorf("sanitizeField(%q)=%q, want %q", in, got, want)
		}
	}
}

func TestIsFileExporter(t *testing.T) {
	for _, tc := range []struct {
		name     string
		exporter string
		endpoint string
		want     bool
	}{
		{"file", "file", "", true},
		{"console", "console", "", true}, // synonym: never write spans to stdout, see spool.go
		{"case insensitive", "FILE", "", true},
		{"trimmed", " file ", "", true},
		{"otlp", "otlp", "", false},
		{"explicit opt-out", "none", "", false},
		{"default file sink", "", "", true},
		{"configured endpoint wins", "", "http://collector:4318", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("OTEL_TRACES_EXPORTER", tc.exporter)
			t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", tc.endpoint)
			if got := isFileExporter(); got != tc.want {
				t.Errorf("OTEL_TRACES_EXPORTER=%q OTEL_EXPORTER_OTLP_ENDPOINT=%q -> %v, want %v",
					tc.exporter, tc.endpoint, got, tc.want)
			}
		})
	}
}

// The spool must be append-only: several processes share one file, and a
// truncating open would silently discard another process's spans.
func TestSpoolExporterAppends(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "spans.jsonl")
	if err := os.WriteFile(filepath.Dir(path)+"x", nil, 0o644); err == nil {
		_ = os.Remove(filepath.Dir(path) + "x")
	}
	e1, err := newSpoolExporter(path) // also proves parent dirs are created
	if err != nil {
		t.Fatalf("newSpoolExporter: %v", err)
	}
	if err := e1.ExportSpans(context.Background(), nil); err != nil {
		t.Errorf("empty batch should be a no-op, got %v", err)
	}
	if err := os.WriteFile(path, []byte("{\"_msg\":\"pre-existing\"}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	_ = e1.Shutdown(context.Background())

	e2, err := newSpoolExporter(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer e2.Shutdown(context.Background())
	if b, _ := os.ReadFile(path); !strings.Contains(string(b), "pre-existing") {
		t.Error("reopening the spool truncated it; it must append")
	}
}

func TestSpoolExporterHardensExistingPaths(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("BASHY_OTEL_SPOOL", "")
	dir := filepath.Join(home, ".agents", "otel", "spool")
	if err := os.MkdirAll(filepath.Dir(dir), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "spans.jsonl")
	if err := os.WriteFile(path, []byte("old\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	e, err := newSpoolExporter(path)
	if err != nil {
		t.Fatal(err)
	}
	defer e.Shutdown(context.Background())
	for _, tc := range []struct {
		path string
		want os.FileMode
	}{
		{dir, 0o700},
		{path, 0o600},
	} {
		fi, err := os.Stat(tc.path)
		if err != nil {
			t.Fatal(err)
		}
		if got := fi.Mode().Perm(); got != tc.want {
			t.Errorf("%s permissions = %04o, want %04o", tc.path, got, tc.want)
		}
	}
}

func TestNewSpoolExporterRejectsSymlink(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "target.jsonl")
	if err := os.WriteFile(target, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "spans.jsonl")
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlinks unavailable on this platform: %v", err)
	}
	if _, err := newSpoolExporter(link); err == nil {
		t.Fatal("newSpoolExporter accepted a symlink")
	}
}

// Shutdown must tolerate being called twice — the SDK may shut an exporter down
// on a path that already closed it, and a panic there would take the shell out.
func TestSpoolExporterShutdownIdempotent(t *testing.T) {
	e, err := newSpoolExporter(filepath.Join(t.TempDir(), "s.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if err := e.Shutdown(context.Background()); err != nil {
		t.Errorf("first shutdown: %v", err)
	}
	if err := e.Shutdown(context.Background()); err != nil {
		t.Errorf("second shutdown must be a no-op, got %v", err)
	}
}

// Records must be one JSON object per line: that is exactly what VictoriaLogs
// /insert/jsonline consumes, and it is why no translation step is needed.
func TestSpoolPathHonoursOverride(t *testing.T) {
	t.Setenv("BASHY_OTEL_SPOOL", "/tmp/custom-spool.jsonl")
	if got := SpoolPath(); got != "/tmp/custom-spool.jsonl" {
		t.Errorf("SpoolPath()=%q, want the override", got)
	}
	t.Setenv("BASHY_OTEL_SPOOL", "")
	if got := SpoolPath(); !strings.HasSuffix(got, filepath.Join("otel", "spool", "spans.jsonl")) {
		t.Errorf("default SpoolPath()=%q, want it beside the stack's data", got)
	}
	var probe map[string]any
	if err := json.Unmarshal([]byte(`{"_msg":"x","cmd_exit_code":"127"}`), &probe); err != nil {
		t.Fatalf("record shape must be plain JSON: %v", err)
	}
}

// Rotation must keep the spool importable: cutting mid-record would make the
// whole file fail to ingest, turning a size problem into total data loss.
func TestRotateKeepsValidJSONLines(t *testing.T) {
	path := filepath.Join(t.TempDir(), "big.jsonl")
	e, err := newSpoolExporter(path)
	if err != nil {
		t.Fatal(err)
	}
	defer e.Shutdown(context.Background())

	// Write past the bound with distinguishable records.
	line := `{"_msg":"pad","filler":"` + strings.Repeat("x", 512) + `"}` + "\n"
	var buf strings.Builder
	for buf.Len() < maxSpoolBytes+(1<<20) {
		buf.WriteString(line)
	}
	if _, err := e.f.WriteString(buf.String()); err != nil {
		t.Fatal(err)
	}

	e.mu.Lock()
	e.rotateIfLarge()
	e.mu.Unlock()

	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Size() >= maxSpoolBytes {
		t.Errorf("spool not rotated: size=%d, bound=%d", fi.Size(), maxSpoolBytes)
	}
	if got := fi.Mode().Perm(); got != 0o600 {
		t.Errorf("rotated spool permissions = %04o, want 0600", got)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for i, ln := range strings.Split(strings.TrimRight(string(b), "\n"), "\n") {
		var v map[string]any
		if err := json.Unmarshal([]byte(ln), &v); err != nil {
			t.Fatalf("line %d is not valid JSON after rotation: %v", i, err)
		}
	}
	// The exporter must still be writable afterwards.
	if _, err := e.f.WriteString(`{"_msg":"after"}` + "\n"); err != nil {
		t.Errorf("spool not writable after rotation: %v", err)
	}
}
