package otelcli

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Spool import: move spans that were captured while nothing was running into
// the store, so they become queryable.
//
// The file sink (coreutils/pkg/telemetry) lets any process record telemetry
// with no collector up — that is what makes telemetry free to leave on. But a
// spool nobody ingests is just a file that grows: the data would be captured
// and still invisible, which is the same silent-gap failure the sink was built
// to remove, moved one step later. Import closes that loop.
//
// The spool is truncated only after the store returns success, so a failed
// import loses nothing and can simply be retried.

// SpoolPath mirrors telemetry.SpoolPath. It is duplicated rather than imported
// because external/otel is its own module and must not depend on the parent —
// the two must stay in step; change one and grep for the other.
func SpoolPath() string {
	if p := strings.TrimSpace(os.Getenv("BASHY_OTEL_SPOOL")); p != "" {
		return p
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return filepath.Join(os.TempDir(), "bashy-otel-spool.jsonl")
	}
	return filepath.Join(home, ".agents", "otel", "spool", "spans.jsonl")
}

// importSpool posts a jsonline spool to a running stack and truncates it on
// success. Returns the number of records imported.
//
// _stream_fields=service_name keeps one log stream per service, which is what
// makes `otel failed` groupable by service without a post-hoc join.
func importSpool(proxyBase, path string) (int, error) {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil // nothing spooled is a normal, quiet outcome
		}
		return 0, err
	}
	defer f.Close()

	info, err := f.Stat()
	if err != nil {
		return 0, err
	}
	if info.Size() == 0 {
		return 0, nil
	}

	body, err := io.ReadAll(f)
	if err != nil {
		return 0, err
	}
	lines := strings.Count(strings.TrimRight(string(body), "\n"), "\n") + 1

	url := strings.TrimRight(proxyBase, "/") +
		"/insert/jsonline?_stream_fields=service_name&_time_field=_time&_msg_field=_msg"
	req, err := http.NewRequest(http.MethodPost, url, strings.NewReader(string(body)))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", "application/stream+json")

	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return 0, fmt.Errorf("post spool: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 200))
		return 0, fmt.Errorf("store rejected spool: %s: %s",
			resp.Status, strings.TrimSpace(string(snippet)))
	}

	// Truncate only now. Losing spans to a partial import would be worse than
	// importing them twice, and re-import is harmless.
	if err := os.Truncate(path, 0); err != nil {
		return lines, fmt.Errorf("imported %d records but could not truncate %s: %w",
			lines, path, err)
	}
	return lines, nil
}
