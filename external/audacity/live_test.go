package audacity

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/qiangli/yoke/pkg/binmgr"
)

// writeLiveWAV writes a 0.5 s, 8 kHz, 16-bit mono 440 Hz sine with the
// stdlib only, so the live smoke never needs an Audacity-produced file as
// INPUT: every byte Audacity reads here was produced by this test.
func writeLiveWAV(t *testing.T, path string) {
	t.Helper()
	const (
		rate     = 8000
		seconds  = 0.5
		channels = 1
		bits     = 16
	)
	n := int(rate * seconds)
	var pcm bytes.Buffer
	for i := 0; i < n; i++ {
		v := math.Sin(2 * math.Pi * 440 * float64(i) / rate)
		_ = binary.Write(&pcm, binary.LittleEndian, int16(v*0.5*math.MaxInt16))
	}
	var w bytes.Buffer
	w.WriteString("RIFF")
	_ = binary.Write(&w, binary.LittleEndian, uint32(36+pcm.Len()))
	w.WriteString("WAVEfmt ")
	_ = binary.Write(&w, binary.LittleEndian, uint32(16))
	_ = binary.Write(&w, binary.LittleEndian, uint16(1)) // PCM
	_ = binary.Write(&w, binary.LittleEndian, uint16(channels))
	_ = binary.Write(&w, binary.LittleEndian, uint32(rate))
	_ = binary.Write(&w, binary.LittleEndian, uint32(rate*channels*bits/8))
	_ = binary.Write(&w, binary.LittleEndian, uint16(channels*bits/8))
	_ = binary.Write(&w, binary.LittleEndian, uint16(bits))
	w.WriteString("data")
	_ = binary.Write(&w, binary.LittleEndian, uint32(pcm.Len()))
	w.Write(pcm.Bytes())
	if err := os.WriteFile(path, w.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestLive is the end-to-end smoke against a REAL Audacity 3.x, through the
// real front door (NewCmd → Path → binmgr.Ensure → defaultPipeClient), on a
// platform with a committed pin. It is opt-in (YOKE_AUDACITY_LIVE=1)
// because it downloads the official ~109 MB AppImage and talks to a running
// same-user Audacity over its local mod-script-pipe; CI never runs it. The
// manager runs it only on a claimed novidesign/noviwin host (never Dragon),
// after starting the provisioned Audacity 3.7.3 by hand with
// Preferences → Modules → mod-script-pipe → Enabled (+ restart). The test
// never starts Audacity itself: the wrapper has no launch path by design.
//
// One typed operation is exercised — `info --input in.wav` — which drives
// the fixed `Import2` + `GetInfo: Type=Tracks Format=JSON` command pair in
// strict request/reply order over the FIFO session. With
// YOKE_AUDACITY_LIVE_DIR set, the fixture is written there (and kept) so an
// operator can inspect it; otherwise it lives in t.TempDir(). BASHY_BIN_CACHE
// should point at a throwaway directory.
//
// Windows is provision-only in this story: `Path` succeeds (pinned zip) but
// the pipe step fails closed, so on windows this test proves provisioning
// and the honest fail-closed error — nothing more.
func TestLive(t *testing.T) {
	if os.Getenv("YOKE_AUDACITY_LIVE") != "1" {
		t.Skip("set YOKE_AUDACITY_LIVE=1 to run the live Audacity smoke (downloads the pinned release; needs a running same-user Audacity with mod-script-pipe)")
	}
	plat := runtime.GOOS + "/" + runtime.GOARCH
	if _, ok := binmgr.PinnedSHA256(toolName, DefaultVersion, plat); !ok {
		t.Skipf("no committed pin for %s@%s on %s", toolName, DefaultVersion, plat)
	}
	dir := os.Getenv("YOKE_AUDACITY_LIVE_DIR")
	if dir == "" {
		dir = t.TempDir()
	} else if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	dir, err := filepath.Abs(dir)
	if err != nil {
		t.Fatal(err)
	}
	in := filepath.Join(dir, "in.wav")
	writeLiveWAV(t, in)
	cache, _ := binmgr.CacheDir()
	t.Logf("platform=%s version=%s cache=%s work=%s", plat, DefaultVersion, cache, dir)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()
	start := time.Now()
	exe, err := Path(ctx, DefaultVersion)
	if err != nil {
		t.Fatalf("Path: %v", err)
	}
	st, err := os.Stat(exe)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("provisioned %s (%d bytes) in %s", exe, st.Size(), time.Since(start).Round(time.Millisecond))

	t.Logf("$ audacity info --input %s --timeout 2m", in)
	start = time.Now()
	out, err := execute(t, defaultRunner, "info", "--input", in, "--timeout", "2m")
	t.Logf("elapsed=%s err=%v\n%s", time.Since(start).Round(time.Millisecond), err, strings.TrimSpace(out))
	if runtime.GOOS == "windows" {
		// Honest provision-only: the pipe step must fail closed, not pretend.
		if err == nil || !strings.Contains(err.Error(), "no running Audacity 3.x with mod-script-pipe") {
			t.Fatalf("windows must fail closed at the pipe step, got err=%v out=%q", err, out)
		}
		return
	}
	if err != nil {
		t.Fatal(err)
	}
	payload, ok := findResult(out)
	if !ok {
		t.Fatalf("no %q line in output: %q", resultMarker, out)
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(payload), &m); err != nil {
		t.Fatalf("result is not one JSON object: %v", err)
	}
	if m["schema_version"] != "yoke-audacity-v1" || m["op"] != "info" || m["input"] != in {
		t.Fatalf("info result: %v", m)
	}
	resp, _ := m["pipe_response"].(string)
	// Two commands, two replies, both OK — the request/reply session
	// aggregated every reply, and GetInfo returned its JSON track list.
	if n := strings.Count(resp, "BatchCommand finished: OK"); n != 2 {
		t.Fatalf("pipe_response has %d OK markers, want 2 (Import2 + GetInfo): %q", n, resp)
	}
	if !strings.Contains(resp, `"name"`) {
		t.Fatalf("GetInfo Tracks JSON missing from pipe_response: %q", resp)
	}
}
