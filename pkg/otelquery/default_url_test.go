package otelquery

import (
	"fmt"
	"os"
	"testing"
)

// The test that was missing, and whose absence let a broken default ship.
//
// Every other test in this package injects an httptest server URL, so NOTHING exercised the
// default — and the default pointed at VictoriaMetrics' own port (8428) while every query in
// the package is written against the reverse proxy's path prefixes (/logs/, /traces/,
// /metrics/), which exist only on the proxy (31415).
//
// A mocked transport proves the CLIENT parses what the server sends. It cannot prove the
// client is TALKING TO THE RIGHT SERVER. Those are different claims, and only one of them was
// ever tested.
func TestDefaultBaseURLPointsAtTheProxyNotAStore(t *testing.T) {
	t.Setenv("BASHY_OTEL_QUERY_URL", "")
	os.Unsetenv("BASHY_OTEL_QUERY_URL")

	want := fmt.Sprintf("http://127.0.0.1:%d", defaultProxyPort)
	if got := NewClient("").BaseURL; got != want {
		t.Fatalf("default base URL = %q, want %q\n\n"+
			"Every query in this package uses the proxy's path prefixes (/logs/, /traces/, /metrics/). "+
			"Those exist ONLY on the `bashy otel` reverse proxy. Point this at an individual Victoria "+
			"store and every verb 404s — which is exactly what shipped.", got, want)
	}
}

// Keep the constant honest against the stack it must match.
func TestDefaultProxyPortMatchesTheStack(t *testing.T) {
	// external/otel/stack.DefaultProxyPort. It cannot be imported here because it
	// lives in a separate module, so the coupling is asserted by hand.
	const stackDefaultProxyPort = 31415
	if defaultProxyPort != stackDefaultProxyPort {
		t.Fatalf("defaultProxyPort = %d, but external/otel/stack.DefaultProxyPort = %d — "+
			"the query client is aimed at a port the stack does not listen on",
			defaultProxyPort, stackDefaultProxyPort)
	}
}
