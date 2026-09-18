package otelcli

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Import ycode's per-session trace files into the store.
//
// ycode has always written rich telemetry — spans with parents, attributes and
// timings, one directory per session under ~/.agents/ycode/otel/instances/ —
// using the OTel stdouttrace encoding. Nothing ever read it. On this machine
// that is 738 sessions and 53 MB of traces that no query could reach: collected
// carefully, stored durably, and invisible.
//
// That is the same failure the spool sink was built to remove, in an older
// form. The data does not need to be produced; it needs to be reachable. This
// converts the format ycode already writes into the jsonline schema the store
// already ingests, so the history becomes queryable rather than being
// regenerated.
//
// UNLIKE the spool, these files are NOT truncated after import. The spool is a
// handoff buffer and truncating it is how it stays bounded; a session archive
// is a record, and deleting someone's history to mark a job done would be a
// bad trade. Progress is tracked with a marker file instead, so re-running is
// cheap and a partially imported directory resumes.

// ycodeSpan is the subset of the stdouttrace encoding we map. Fields we do not
// carry (TraceState, Remote, Events, Links) are dropped deliberately: they add
// bulk to every record and nothing queries them today.
type ycodeSpan struct {
	Name        string `json:"Name"`
	SpanContext struct {
		TraceID string `json:"TraceID"`
		SpanID  string `json:"SpanID"`
	} `json:"SpanContext"`
	Parent struct {
		SpanID string `json:"SpanID"`
	} `json:"Parent"`
	StartTime  time.Time `json:"StartTime"`
	EndTime    time.Time `json:"EndTime"`
	Attributes []struct {
		Key   string `json:"Key"`
		Value struct {
			Value any `json:"Value"`
		} `json:"Value"`
	} `json:"Attributes"`
	Status struct {
		Code        string `json:"Code"`
		Description string `json:"Description"`
	} `json:"Status"`
}

// toRecord converts one ycode span to the same flat jsonline shape the spool
// sink emits, so both sources land in one schema and a single LogsQL query
// spans them.
func (s ycodeSpan) toRecord(sessionID string) map[string]any {
	rec := map[string]any{
		"_time":        s.StartTime.UTC().Format(time.RFC3339Nano),
		"_msg":         s.Name,
		"trace_id":     s.SpanContext.TraceID,
		"span_id":      s.SpanContext.SpanID,
		"service_name": "ycode",
		"session_id":   sessionID,
	}
	if p := s.Parent.SpanID; p != "" && p != "0000000000000000" {
		rec["parent_span_id"] = p
	}
	if !s.EndTime.IsZero() && !s.StartTime.IsZero() {
		rec["duration_ms"] = s.EndTime.Sub(s.StartTime).Milliseconds()
	}
	if c := strings.TrimSpace(s.Status.Code); c != "" {
		rec["status"] = c
	}
	if d := strings.TrimSpace(s.Status.Description); d != "" {
		rec["status_message"] = d
	}
	for _, a := range s.Attributes {
		k := sanitizeField(a.Key)
		if k == "" {
			continue
		}
		// Never let an attribute clobber an identity field; a span whose
		// attribute is called "trace_id" would otherwise rewrite its own.
		if _, taken := rec[k]; taken {
			k = "attr_" + k
		}
		rec[k] = a.Value.Value
	}
	return rec
}

// sanitizeField mirrors the spool sink's rule: dots are field separators in
// LogsQL, so an attribute like "http.status" must not create a nested path.
func sanitizeField(k string) string {
	return strings.ReplaceAll(strings.TrimSpace(k), ".", "_")
}

// YcodeInstancesDir is where ycode persists its per-session telemetry.
// $OTEL_STORAGE_PATH overrides, matching ycode's own resolveOTELDataDir.
func YcodeInstancesDir() string {
	if v := strings.TrimSpace(os.Getenv("OTEL_STORAGE_PATH")); v != "" {
		return filepath.Join(v, "instances")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".agents", "ycode", "otel", "instances")
}

// importYcodeDir walks the session archive and imports every trace file that
// has changed since it was last imported. Returns records imported and files
// visited.
func importYcodeDir(proxyBase, dir string, logf func(string, ...any)) (records, files int, err error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, 0, nil // no ycode history is a normal, quiet outcome
		}
		return 0, 0, err
	}
	// Stable order so a run that is interrupted resumes predictably rather
	// than re-walking in whatever order the filesystem returns.
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })

	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		session := e.Name()
		tdir := filepath.Join(dir, session, "traces")
		tfiles, rerr := filepath.Glob(filepath.Join(tdir, "traces-*.jsonl"))
		if rerr != nil {
			continue
		}
		sort.Strings(tfiles)
		for _, tf := range tfiles {
			n, skipped, ierr := importYcodeFile(proxyBase, tf, session)
			if ierr != nil {
				// One unreadable session must not abort the rest; the whole
				// point is to recover an archive, and archives have bad files.
				logf("otel: ycode import: %s: %v", filepath.Base(tf), ierr)
				continue
			}
			if skipped {
				continue
			}
			files++
			records += n
		}
	}
	return records, files, nil
}

// importYcodeFile converts and posts one trace file. skipped is true when the
// marker shows it was already imported and unchanged since.
func importYcodeFile(proxyBase, path, session string) (n int, skipped bool, err error) {
	marker := path + ".imported"
	src, err := os.Stat(path)
	if err != nil {
		return 0, false, err
	}
	if m, merr := os.Stat(marker); merr == nil && !src.ModTime().After(m.ModTime()) {
		return 0, true, nil
	}

	f, err := os.Open(path)
	if err != nil {
		return 0, false, err
	}
	defer f.Close()
	raw, err := io.ReadAll(f)
	if err != nil {
		return 0, false, err
	}

	var buf strings.Builder
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var s ycodeSpan
		if jerr := json.Unmarshal([]byte(line), &s); jerr != nil {
			continue // a truncated tail line is expected in a live archive
		}
		if s.Name == "" || s.StartTime.IsZero() {
			continue
		}
		b, jerr := json.Marshal(s.toRecord(session))
		if jerr != nil {
			continue
		}
		buf.Write(b)
		buf.WriteByte('\n')
		n++
	}
	if n == 0 {
		return 0, true, nil
	}

	if err := postJSONLine(proxyBase, buf.String()); err != nil {
		return 0, false, err
	}
	// Marker only after the store accepted it, for the same reason the spool
	// truncates only on success: re-importing is harmless, losing is not.
	if werr := os.WriteFile(marker, []byte(time.Now().UTC().Format(time.RFC3339)+"\n"), 0o644); werr != nil {
		return n, false, fmt.Errorf("imported %d but could not mark %s: %w", n, filepath.Base(path), werr)
	}
	return n, false, nil
}

// postJSONLine ships a jsonline body to the store through the proxy.
func postJSONLine(proxyBase, body string) error {
	url := strings.TrimRight(proxyBase, "/") +
		"/insert/jsonline?_stream_fields=service_name&_time_field=_time&_msg_field=_msg"
	req, err := http.NewRequest(http.MethodPost, url, strings.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/stream+json")
	client := &http.Client{Timeout: 60 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("post: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 200))
		return fmt.Errorf("store rejected: %s: %s", resp.Status, strings.TrimSpace(string(snippet)))
	}
	return nil
}
