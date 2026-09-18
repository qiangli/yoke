package telemetry

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

// The spool sink writes spans to a FILE instead of a collector, so telemetry
// costs nothing to have on and needs nothing pre-started.
//
// The problem it solves: an OTLP endpoint means a service must already be
// running at the moment a span is produced. Anything that runs before it, or
// while it is down, or on a host that never started it, exports into a closed
// port and is lost — silently, because a dropped span looks exactly like a
// process that had nothing to say. That failure was found in the field: a stack
// ran for eight days with zero data because nothing was configured to send to
// it, and nobody could tell from the outside.
//
// Records are VictoriaLogs jsonline, which is Victoria's own native ingest
// format — NOT OTLP JSON. That choice is what keeps this cheap: a spool file
// imports with
//
//	curl --data-binary @spool.jsonl '<vlogs>/insert/jsonline?_stream_fields=service'
//
// and is then queryable with real LogsQL. Writing OTLP JSON instead would have
// required a translation step or a query engine of our own. Verified: importing
// a spool into a fresh store and filtering on a structured field (exit_code:2)
// returns the exact record, so the query verbs keep working over spooled data.
//
// Selected by the standard OTEL_TRACES_EXPORTER env var ("file"), per this
// package's rule of standard OTEL vars and nothing bespoke.

// spoolExporter appends one jsonline record per span.
//
// It is deliberately a plain appender: no batching beyond the SDK's, no index,
// no rotation-by-write. Concurrency safety comes from O_APPEND plus a mutex —
// a single write() of a line under the pipe-buffer size is atomic on POSIX, so
// several processes can share one spool without coordinating.
type spoolExporter struct {
	mu   sync.Mutex
	path string
	f    *os.File
	mode os.FileMode
}

func newSpoolExporter(path string) (*spoolExporter, error) {
	// Bashy is a shell, so its working directory changes routinely. Anchor an
	// operator's relative override once; rotation must never reinterpret it
	// against a later directory and touch an unrelated file.
	absPath, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("resolve spool path: %w", err)
	}
	path = absPath
	if err := secureSpoolDir(path); err != nil {
		return nil, fmt.Errorf("spool dir: %w", err)
	}
	f, err := openPrivateSpool(path)
	if err != nil {
		return nil, fmt.Errorf("open spool: %w", err)
	}
	fi, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("stat spool: %w", err)
	}
	return &spoolExporter{path: path, f: f, mode: fi.Mode().Perm()}, nil
}

// secureSpoolDir makes a directory created for the spool owner-only. It also
// hardens Bashy's canonical .../spool/spans.jsonl directory from older
// versions. An arbitrary operator override may name an existing project or a
// shared temporary directory; changing that directory's mode would be a much
// larger privilege boundary than protecting the spool file itself.
func secureSpoolDir(path string) error {
	dir := filepath.Dir(path)
	lfi, err := os.Lstat(dir)
	switch {
	case os.IsNotExist(err):
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return err
		}
		fi, err := os.Stat(dir)
		if err != nil {
			return err
		}
		if !fi.IsDir() {
			return fmt.Errorf("not a directory")
		}
		return os.Chmod(dir, 0o700)
	case err != nil:
		return err
	}
	fi, err := os.Stat(dir) // a symlink to a directory is a valid relocation
	if err != nil {
		return err
	}
	if !fi.IsDir() {
		return fmt.Errorf("not a directory")
	}

	// Only the exact default home path is Bashy-owned. Basenames are not proof:
	// /var/spool and an operator's group-shared or symlinked spool are not ours
	// to chmod.
	if lfi.Mode()&os.ModeSymlink == 0 && isCanonicalSpoolPath(path) {
		return os.Chmod(dir, 0o700)
	}
	return nil
}

// openPrivateSpool opens a regular spool file with owner-only permissions.
// Chmod is necessary for an existing file, since OpenFile's create mode is not
// applied when the file is already present.
func openPrivateSpool(path string) (*os.File, error) {
	existed := false
	if fi, err := os.Lstat(path); err == nil {
		existed = true
		if fi.Mode()&os.ModeSymlink != 0 {
			return nil, fmt.Errorf("not a regular file")
		}
		if !fi.Mode().IsRegular() {
			if isNullDevice(path, fi) {
				return os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0)
			}
			return nil, fmt.Errorf("not a regular file")
		}
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, err
	}
	fi, statErr := f.Stat()
	if statErr != nil || !fi.Mode().IsRegular() {
		_ = f.Close()
		if statErr != nil {
			return nil, statErr
		}
		return nil, fmt.Errorf("not a regular file")
	}
	// A file we created and Bashy's canonical file must be owner-only. An
	// existing operator override may deliberately be a shared append target;
	// changing its mode is outside the authority granted by naming it.
	if !existed || isDefaultSpoolPath(path) {
		if err := f.Chmod(0o600); err != nil {
			_ = f.Close()
			return nil, err
		}
	}
	return f, nil
}

// isNullDevice preserves the historical explicit discard override without
// treating arbitrary devices as spool files. OTEL_TRACES_EXPORTER=none is the
// preferred opt-out, but existing profiles may still name /dev/null.
func isNullDevice(path string, fi os.FileInfo) bool {
	nullInfo, err := os.Stat(os.DevNull)
	return err == nil && os.SameFile(fi, nullInfo)
}

// ExportSpans writes each span as one jsonline record.
//
// Span attributes are flattened to top-level fields rather than nested under an
// "attributes" object, because LogsQL filters on top-level field names: a
// nested shape would make `exit_code:2` unexpressible and reduce the verbs to
// substring matching on the message.
func (e *spoolExporter) ExportSpans(_ context.Context, spans []sdktrace.ReadOnlySpan) error {
	if len(spans) == 0 {
		return nil
	}
	e.mu.Lock()
	defer e.mu.Unlock()

	var b strings.Builder
	for _, s := range spans {
		rec := map[string]any{
			// _time/_msg are VictoriaLogs' conventional field names; the import
			// call names them explicitly so this stays readable as plain JSON.
			"_time":       s.EndTime().UTC().Format(time.RFC3339Nano),
			"_msg":        s.Name(),
			"trace_id":    s.SpanContext().TraceID().String(),
			"span_id":     s.SpanContext().SpanID().String(),
			"duration_ms": s.EndTime().Sub(s.StartTime()).Milliseconds(),
			"status":      s.Status().Code.String(),
		}
		if s.Parent().IsValid() {
			rec["parent_span_id"] = s.Parent().SpanID().String()
		}
		if d := s.Status().Description; d != "" {
			rec["status_message"] = d
		}
		for _, kv := range s.Resource().Attributes() {
			rec[sanitizeField(string(kv.Key))] = kv.Value.Emit()
		}
		// Span attributes last so they win over resource attributes on a clash —
		// the span is the more specific statement about that event.
		for _, kv := range s.Attributes() {
			rec[sanitizeField(string(kv.Key))] = kv.Value.Emit()
		}
		line, err := json.Marshal(rec)
		if err != nil {
			continue // one unmarshalable span must not drop the batch
		}
		b.Write(line)
		b.WriteByte('\n')
	}
	if e.f == nil {
		return nil // rotation failed to reopen; drop rather than crash the host process
	}
	_, err := e.f.WriteString(b.String())
	e.rotateIfLarge()
	return err
}

func (e *spoolExporter) Shutdown(context.Context) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.f == nil {
		return nil
	}
	err := e.f.Close()
	e.f = nil
	return err
}

// sanitizeField maps OTel's dotted attribute keys to LogsQL-friendly names.
// "exit.code" would parse as a field access in a query; "exit_code" filters.
func sanitizeField(k string) string {
	return strings.ReplaceAll(k, ".", "_")
}

// SpoolPath is where spans land when the file sink is selected.
// $BASHY_OTEL_SPOOL overrides; otherwise it sits beside the stack's own data so
// one directory holds all observability state.
func SpoolPath() string {
	if p := strings.TrimSpace(os.Getenv("BASHY_OTEL_SPOOL")); p != "" {
		return p
	}
	p, _ := defaultSpoolPath()
	return p
}

// defaultSpoolPath also says whether Bashy owns the containing directory. The
// fallback is a file in the shared temporary directory, so only the file is
// ours there; the user's home-local spool directory is dedicated to Bashy.
func defaultSpoolPath() (path string, ownsDir bool) {
	home, err := os.UserHomeDir()
	if err != nil || strings.TrimSpace(home) == "" {
		return filepath.Join(os.TempDir(), "bashy-otel-spool.jsonl"), false
	}
	return filepath.Join(home, ".agents", "otel", "spool", "spans.jsonl"), true
}

func isCanonicalSpoolPath(path string) bool {
	canonical, ownsDir := defaultSpoolPath()
	if !ownsDir {
		return false
	}
	return sameSpoolPath(path, canonical)
}

func isDefaultSpoolPath(path string) bool {
	canonical, _ := defaultSpoolPath()
	return sameSpoolPath(path, canonical)
}

func sameSpoolPath(path, canonical string) bool {
	absCanonical, err := filepath.Abs(canonical)
	return err == nil && filepath.Clean(path) == filepath.Clean(absCanonical)
}

// isFileExporter reports whether the file sink is selected.
//
// OTEL_TRACES_EXPORTER is the spec's own knob (its defined values include
// "otlp", "console" and "none"); "file" is the OTLP file-exporter convention.
// "console" is accepted as a synonym rather than writing to stdout directly,
// because a shell that prints spans into its own stdout corrupts every pipeline
// it is part of — the spool file is the honest reading of the intent.
func isFileExporter() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("OTEL_TRACES_EXPORTER"))) {
	case "file", "console":
		return true
	case "none":
		return false // explicit opt-out
	case "otlp", "grpc", "http", "http/protobuf":
		return false // caller wants the network exporter
	case "":
		// DEFAULT ON. Unset means "nobody configured anything", and the useful
		// behaviour there is to record — to a file, which needs no service, no
		// port and no setup.
		//
		// The old default was total no-op, on the reasoning that a shell should
		// not pay for telemetry it is not exporting. True of a network exporter,
		// which buys nothing when no collector is listening. Not true of a file:
		// the spool is an append on an already-open handle, bounded at 64 MiB,
		// and it is what makes a failure explicable AFTER the fact rather than
		// only while someone happened to be watching.
		//
		// That distinction is the whole lesson of this subsystem: every silent
		// failure found here — a QA poller dead for ten days, a red CI nobody
		// saw, a stack up for eight days with zero data — was invisible because
		// telemetry defaulted to off and somebody had to opt in first.
		//
		// Opt out with OTEL_TRACES_EXPORTER=none.
		return !hasOTLPEndpoint()
	}
	return false
}

// hasOTLPEndpoint reports whether a network collector was configured. When one
// is, it wins the unset-exporter case: an operator who set an endpoint asked
// for the network path, and quietly spooling to a file instead would be its own
// silent surprise.
func hasOTLPEndpoint() bool {
	return strings.TrimSpace(os.Getenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT")) != "" ||
		strings.TrimSpace(os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT")) != ""
}

func networkEndpoint(exporter string) string {
	if endpoint := strings.TrimSpace(os.Getenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT")); endpoint != "" {
		return endpoint
	}
	if endpoint := strings.TrimSpace(os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT")); endpoint != "" {
		return endpoint
	}
	switch exporter {
	case "otlp", "grpc", "http", "http/protobuf":
		if strings.EqualFold(strings.TrimSpace(os.Getenv("OTEL_EXPORTER_OTLP_PROTOCOL")), "grpc") || exporter == "grpc" {
			return "http://localhost:4317"
		}
		return "http://localhost:4318"
	default:
		return ""
	}
}

// maxSpoolBytes bounds the spool so telemetry cannot fill a disk when nothing
// ever imports it — the realistic long-running case, since importing requires
// someone to start the stack. 64 MiB holds on the order of a hundred thousand
// exec spans, far more than any debugging session needs, and is small enough
// that losing the oldest half is not a real loss.
const maxSpoolBytes = 64 << 20

// rotateIfLarge keeps the newest half of an oversized spool and drops the rest.
//
// Dropping the OLDEST is deliberate: when telemetry is being used to explain
// something that just happened, recent spans are the ones that matter. The
// alternative — refusing to write once full — would silently stop recording at
// exactly the moment a long session got interesting.
//
// Called with the lock held.
func (e *spoolExporter) rotateIfLarge() {
	if e.f == nil {
		return
	}
	fi, err := e.f.Stat()
	if err != nil || fi.Size() < maxSpoolBytes {
		return
	}
	data, err := os.ReadFile(e.path)
	if err != nil {
		return
	}
	// Cut at a line boundary so the survivor is still valid jsonline; a
	// half-record would make the whole import fail.
	keep := data[len(data)/2:]
	if i := indexByte(keep, '\n'); i >= 0 {
		keep = keep[i+1:]
	}
	tmp, err := os.CreateTemp(filepath.Dir(e.path), ".spans-*.rotating")
	if err != nil {
		return
	}
	tmpPath := tmp.Name()
	if _, err := tmp.Write(keep); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpPath)
		return
	}
	if err := tmp.Chmod(e.mode); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpPath)
		return
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpPath)
		return
	}
	if err := e.f.Close(); err != nil {
		_ = os.Remove(tmpPath)
		return
	}
	if err := os.Rename(tmpPath, e.path); err != nil {
		_ = os.Remove(tmpPath)
	}
	f, err := openPrivateSpool(e.path)
	if err != nil {
		e.f = nil // writes become no-ops rather than panicking
		return
	}
	e.f = f
}

func indexByte(b []byte, c byte) int {
	for i := range b {
		if b[i] == c {
			return i
		}
	}
	return -1
}
