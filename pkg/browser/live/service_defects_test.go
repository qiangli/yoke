package live

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/qiangli/yoke/pkg/browser/wire"
)

// TestLiveLoggingIsCapturable pins the fix for a log line that escaped
// BOTH stdout and stderr. It went to the process's os.Stderr, which is
// not the stream a bashy builtin is handed, so
//
//	browser --mode live --json tabs list >/tmp/o 2>/tmp/e
//
// left /tmp/e empty and printed on the terminal anyway. Nothing in
// this package may write outside a sink the caller installed.
func TestLiveLoggingIsCapturable(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	orig := os.Stderr
	os.Stderr = w
	defer func() { os.Stderr = orig }()

	// Default sink: every one of these must be silent.
	logInfo("live: hub already owned by another process", "port", 58082)
	logWarn("live: something", "error", "x")

	_ = w.Close()
	var leaked bytes.Buffer
	_, _ = leaked.ReadFrom(r)
	os.Stderr = orig
	if leaked.Len() != 0 {
		t.Fatalf("live logged %d bytes to the process stderr with no sink installed: %q",
			leaked.Len(), leaked.String())
	}

	// An explicit sink receives them.
	var buf bytes.Buffer
	SetLogger(NewWriterLogger(&buf))
	defer SetLogger(nil)
	logInfo("live: listening for extension", "addr", "127.0.0.1:1")
	if !strings.Contains(buf.String(), "listening for extension") {
		t.Fatalf("installed sink received nothing: %q", buf.String())
	}
}

// TestFinishScreenshotWritesTheFile pins the fix for `screenshot
// /tmp/x.png` in live mode: the extension always returns raw base64
// and documents that the Go side applies SavePath/MaxBytes — but live
// never did, so the path was discarded, no file was written, the exit
// code was 0, and 223 KB of base64 went to stdout.
func TestFinishScreenshotWritesTheFile(t *testing.T) {
	png := []byte("\x89PNG\r\n\x1a\nnot-really-a-png-but-bytes-are-bytes")
	dir := t.TempDir()
	want := filepath.Join(dir, "nested", "shot.png")

	res := &wire.Result{Success: true, Image: base64.StdEncoding.EncodeToString(png)}
	if err := finishScreenshot(res, wire.Action{Type: wire.ActionScreenshot, SavePath: want}); err != nil {
		t.Fatal(err)
	}
	if res.Path != want {
		t.Fatalf("Path=%q want %q", res.Path, want)
	}
	if res.Image != "" {
		t.Fatal("image must be dropped once it is on disk, not returned twice")
	}
	got, err := os.ReadFile(want)
	if err != nil {
		t.Fatalf("no file written: %v", err)
	}
	if !bytes.Equal(got, png) {
		t.Fatalf("file contents differ: %q", got)
	}

	// MaxBytes spills to a temp file rather than returning a huge blob.
	big := &wire.Result{Success: true, Image: base64.StdEncoding.EncodeToString(png)}
	if err := finishScreenshot(big, wire.Action{Type: wire.ActionScreenshot, MaxBytes: 4}); err != nil {
		t.Fatal(err)
	}
	if big.Path == "" || big.Image != "" {
		t.Fatalf("MaxBytes did not spill: %#v", big)
	}
	_ = os.Remove(big.Path)

	// Below the cap and with no path, the image stays inline.
	inline := &wire.Result{Success: true, Image: "aGk="}
	if err := finishScreenshot(inline, wire.Action{Type: wire.ActionScreenshot}); err != nil {
		t.Fatal(err)
	}
	if inline.Image != "aGk=" || inline.Path != "" {
		t.Fatalf("inline capture disturbed: %#v", inline)
	}
}

// TestActionToParamsCarriesTheNewKnobs guards the silent-drop class:
// a flag the CLI accepts but the transport discards is exactly how
// `screenshot <path>` came to exit 0 having done nothing.
func TestActionToParamsCarriesTheNewKnobs(t *testing.T) {
	cases := []struct {
		action wire.Action
		method string
		want   map[string]any
	}{
		{
			wire.Action{Type: wire.ActionScreenshot, FullPage: true, SettleMs: 400},
			"screenshot",
			map[string]any{"full_page": true, "settle_ms": 400},
		},
		{
			wire.Action{Type: wire.ActionClick, ElementID: 36},
			"click",
			map[string]any{"element_id": 36},
		},
		{
			wire.Action{Type: wire.ActionTabs, TabAction: "switch", MatchURL: "localhost:5478"},
			"tabs",
			map[string]any{"action": "switch", "match_url": "localhost:5478"},
		},
		{
			wire.Action{Type: wire.ActionExtract, IncludeHidden: true},
			"extract",
			map[string]any{"include_hidden": true},
		},
		{
			wire.Action{Type: wire.ActionDispatchEvent, Event: "toggle", Detail: `{"a":1}`, Selector: "document"},
			"dispatch_event",
			map[string]any{"event": "toggle", "detail": `{"a":1}`, "selector": "document"},
		},
	}
	for _, tc := range cases {
		method, params, err := actionToParams(tc.action)
		if err != nil {
			t.Fatalf("%s: %v", tc.action.Type, err)
		}
		if method != tc.method {
			t.Fatalf("%s: method=%q want %q", tc.action.Type, method, tc.method)
		}
		for k, want := range tc.want {
			if got := params[k]; got != want {
				t.Fatalf("%s: params[%q]=%v want %v", tc.action.Type, k, got, want)
			}
		}
	}
}

// TestLiveStatusNeverDeniesTheMode: with no hub, status must report an
// unreachable live mode AND the remedy — never "mode is not supported",
// which routed a caller to a weaker tool while live mode worked.
func TestLiveStatusNeverDeniesTheMode(t *testing.T) {
	st := LiveStatus(context.Background(), 1)
	if st.Mode != "live" {
		t.Fatalf("mode=%q", st.Mode)
	}
	if st.Reachable || st.HubUp {
		t.Fatalf("expected an unreachable status: %#v", st)
	}
	if strings.Contains(st.Message, "not supported") {
		t.Fatalf("status denies the mode: %q", st.Message)
	}
	if !strings.Contains(st.Message, "browser hub") {
		t.Fatalf("status names no remedy: %q", st.Message)
	}
}

// TestLiveStatusReadsAConnectedHub covers the reachable branch against
// a stand-in hub, including the stale-extension verdict.
func TestLiveStatusReadsAConnectedHub(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok")) })
	mux.HandleFunc("/connected", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"connected": true, "version": "0.6.6", "methods_count": 20, "stale": true,
		})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	port := srv.Listener.Addr().(*net.TCPAddr).Port

	st := LiveStatus(context.Background(), port)
	if !st.HubUp || !st.Connected {
		t.Fatalf("hub not seen: %#v", st)
	}
	if st.Reachable {
		t.Fatal("a stale extension is not reachable — it would silently capture the wrong tab")
	}
	if !strings.Contains(st.Message, LiveExtensionMinVersion) {
		t.Fatalf("stale message does not name the required version: %q", st.Message)
	}
}
