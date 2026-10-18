package gimp

import (
	"context"
	"encoding/json"
	"image"
	"image/color"
	pngimage "image/png"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/qiangli/yoke/pkg/binmgr"
)

// livePNG is a 64x48 gradient written with the stdlib encoder, so the live
// smoke never needs a GIMP-produced image as an INPUT — every output file
// below is produced by GIMP itself.
func writeLivePNG(t *testing.T, path string) {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 64, 48))
	for y := 0; y < 48; y++ {
		for x := 0; x < 64; x++ {
			img.Set(x, y, color.RGBA{uint8(x * 4), uint8(y * 5), 128, 255})
		}
	}
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := pngimage.Encode(f, img); err != nil {
		t.Fatal(err)
	}
}

// TestLive is the end-to-end smoke against a REAL GIMP, through the real
// front door (NewCmd → Path → binmgr.Ensure → runCommand), on a platform
// with a committed pin. It is opt-in (YOKE_GIMP_LIVE=1) because it
// downloads the ~240 MB official AppImage and spawns GIMP; CI never runs
// it. With YOKE_GIMP_LIVE_DIR set, fixtures and outputs are written there
// (and kept) so an operator can inspect them; otherwise they live in
// t.TempDir(). BASHY_BIN_CACHE should point at a throwaway directory.
//
// Sequence (each step is one short-lived foreground batch child):
//
//	export   in.png → flat.png       (format conversion, no pixel change)
//	convert  in.png → gray.png       (mode gray)
//	edit     in.png → small.png      (scale 32x24)
//
// NOT RUN in this story: no Linux host with a committed pin was in scope,
// so this test is compile-verified only. The first live run confirms (and
// fixes, if needed) the fixed program bodies — especially the convert/edit
// method calls — and records the evidence in EVIDENCE.md.
func TestLive(t *testing.T) {
	if os.Getenv("YOKE_GIMP_LIVE") != "1" {
		t.Skip("set YOKE_GIMP_LIVE=1 to run the live GIMP smoke (downloads the pinned release)")
	}
	plat := runtime.GOOS + "/" + runtime.GOARCH
	if _, ok := binmgr.PinnedSHA256(toolName, DefaultVersion, plat); !ok {
		t.Skipf("no committed pin for %s@%s on %s", toolName, DefaultVersion, plat)
	}
	dir := os.Getenv("YOKE_GIMP_LIVE_DIR")
	if dir == "" {
		dir = t.TempDir()
	} else if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	dir, err := filepath.Abs(dir)
	if err != nil {
		t.Fatal(err)
	}
	in := filepath.Join(dir, "in.png")
	flat := filepath.Join(dir, "flat.png")
	gray := filepath.Join(dir, "gray.png")
	small := filepath.Join(dir, "small.png")
	writeLivePNG(t, in)
	cache, _ := binmgr.CacheDir()
	t.Logf("platform=%s version=%s cache=%s work=%s", plat, DefaultVersion, cache, dir)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()
	start := time.Now()
	exe, err := Path(ctx, "")
	if err != nil {
		t.Fatalf("Path: %v", err)
	}
	st, err := os.Stat(exe)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("provisioned %s (%d bytes) in %s", exe, st.Size(), time.Since(start).Round(time.Millisecond))

	run := func(args ...string) map[string]any {
		t.Helper()
		t.Logf("$ gimp %s", strings.Join(args, " "))
		start := time.Now()
		out, err := execute(t, defaultRunner, args...)
		t.Logf("elapsed=%s err=%v\n%s", time.Since(start).Round(time.Millisecond), err, strings.TrimSpace(out))
		if err != nil {
			t.Fatal(err)
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &m); err != nil {
			t.Fatalf("result is not one JSON object: %v", err)
		}
		return m
	}
	const timeout = "15m"

	exp := run("export", "--input", in, "--output", flat, "--timeout", timeout)
	if exp["schema_version"] != "yoke-gimp-export-v1" {
		t.Fatalf("export result: %v", exp)
	}
	if n, _ := exp["output_bytes"].(float64); n <= 0 {
		t.Fatalf("export output_bytes = %v", exp["output_bytes"])
	}

	conv := run("convert", "--input", in, "--output", gray, "--mode", "gray", "--timeout", timeout)
	if conv["schema_version"] != "yoke-gimp-convert-v1" {
		t.Fatalf("convert result: %v", conv)
	}

	ed := run("edit", "--input", in, "--output", small, "--scale", "32x24", "--timeout", timeout)
	if ed["schema_version"] != "yoke-gimp-edit-v1" {
		t.Fatalf("edit result: %v", ed)
	}

	// The scaled output must decode (stdlib, independent of GIMP's own
	// reading) at exactly the requested dimensions.
	f, err := os.Open(small)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	decoded, err := pngimage.Decode(f)
	if err != nil {
		t.Fatalf("decode scaled png: %v", err)
	}
	if b := decoded.Bounds(); b.Dx() != 32 || b.Dy() != 24 {
		t.Fatalf("scaled frame is %dx%d, want 32x24", b.Dx(), b.Dy())
	}
}
