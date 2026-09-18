package node

import (
	"bufio"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

// TestPinnedSHA_Offline confirms the embedded pins parse and cover this
// platform's default-version archive — no network. A broken embed (empty/typo'd
// file) fails here, offline.
func TestPinnedSHA_Offline(t *testing.T) {
	if strings.TrimSpace(embeddedSums) == "" {
		t.Fatal("embeddedSums is empty — the //go:embed pin is missing")
	}
	n := 0
	sc := bufio.NewScanner(strings.NewReader(embeddedSums))
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) != 2 || len(f[0]) != 64 {
			t.Fatalf("malformed pin line: %q", sc.Text())
		}
		if !strings.HasPrefix(f[1], "node-v"+DefaultVersion+"-") {
			t.Fatalf("pin for a non-default version: %q (default %s)", f[1], DefaultVersion)
		}
		n++
	}
	if n < 4 {
		t.Fatalf("expected pins for the major platforms, got %d", n)
	}
}

// TestEmbeddedSumsMatchUpstream is the PRE-RELEASE supply-chain gate: it
// re-downloads nodejs.org's official SHASUMS256 for the default version and
// fails if any embedded pin drifts. A drift means either a legitimate version
// change (update the embed deliberately) or upstream tampering (do NOT ship).
// Network-gated: skipped under -short.
func TestEmbeddedSumsMatchUpstream(t *testing.T) {
	if testing.Short() {
		t.Skip("network gate; run without -short before a release")
	}
	req, _ := http.NewRequest(http.MethodGet, "https://nodejs.org/dist/v"+DefaultVersion+"/SHASUMS256.txt", nil)
	resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		t.Skipf("offline: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("upstream SHASUMS256 HTTP %d", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	upstream := map[string]string{}
	sc := bufio.NewScanner(strings.NewReader(string(body)))
	for sc.Scan() {
		if f := strings.Fields(sc.Text()); len(f) == 2 {
			upstream[f[1]] = f[0]
		}
	}
	sc = bufio.NewScanner(strings.NewReader(embeddedSums))
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) != 2 {
			continue
		}
		if up := upstream[f[1]]; up != f[0] {
			t.Fatalf("SUPPLY-CHAIN DRIFT for %s: embedded %s, upstream %s — do NOT release until reconciled", f[1], f[0], up)
		}
	}
}
