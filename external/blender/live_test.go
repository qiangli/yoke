package blender

import (
	"context"
	"encoding/json"
	pngimage "image/png"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/qiangli/yoke/pkg/binmgr"
)

// liveUSDA is the smoke fixture: one unit cube, one camera and one distant
// (sun) light, hand-written USD ASCII so a Blender-produced .blend is never
// needed as an INPUT — the first .blend is produced by asset-import itself.
// USD is one of the admitted asset types and (unlike OBJ/STL/PLY) can carry a
// camera and a light; render needs a camera, and without a light a fresh,
// world-less scene renders solid black (verified on noviwin1 2026-10-18),
// which would make the lit-pixel check below meaningless.
const liveUSDA = `#usda 1.0
(
    defaultPrim = "World"
    metersPerUnit = 1
    upAxis = "Z"
)

def Xform "World"
{
    def Mesh "Cube"
    {
        float3[] extent = [(-1, -1, -1), (1, 1, 1)]
        int[] faceVertexCounts = [4, 4, 4, 4, 4, 4]
        int[] faceVertexIndices = [0, 1, 3, 2, 2, 3, 7, 6, 6, 7, 5, 4, 4, 5, 1, 0, 2, 6, 4, 0, 7, 3, 1, 5]
        point3f[] points = [(-1, -1, -1), (1, -1, -1), (-1, 1, -1), (1, 1, -1), (-1, -1, 1), (1, -1, 1), (-1, 1, 1), (1, 1, 1)]
    }

    def Camera "Camera"
    {
        float focalLength = 50
        float horizontalAperture = 36
        float verticalAperture = 24
        double3 xformOp:translate = (7, -7, 5)
        float3 xformOp:rotateXYZ = (63.6, 0, 45)
        uniform token[] xformOpOrder = ["xformOp:translate", "xformOp:rotateXYZ"]
    }

    def DistantLight "Sun"
    {
        float inputs:angle = 1
        float inputs:intensity = 3
        double3 xformOp:translate = (0, 0, 10)
        float3 xformOp:rotateXYZ = (40, 20, 30)
        uniform token[] xformOpOrder = ["xformOp:translate", "xformOp:rotateXYZ"]
    }
}
`

// TestLive is the end-to-end smoke against a REAL Blender, through the real
// front door (NewCmd → Path → binmgr.Ensure → runCommand), on a platform with
// a committed pin. It is opt-in (YOKE_BLENDER_LIVE=1) because it downloads
// the ~380 MB official archive and renders a frame; CI never runs it. With
// YOKE_BLENDER_LIVE_DIR set, fixtures and outputs are written there (and kept)
// so an operator can inspect them; otherwise they live in t.TempDir().
// BASHY_BIN_CACHE should point at a throwaway directory for the same reason.
//
// Sequence (each step is one short-lived foreground `blender --background`):
//
//	asset-import  cube.usda → cube.blend   (fresh, empty scene)
//	scene-inspect cube.blend
//	render        cube.blend → frame.png   (frame 1, scene's own engine)
//	output-inspect frame.png
func TestLive(t *testing.T) {
	if os.Getenv("YOKE_BLENDER_LIVE") != "1" {
		t.Skip("set YOKE_BLENDER_LIVE=1 to run the live Blender smoke (downloads the pinned release)")
	}
	plat := runtime.GOOS + "/" + runtime.GOARCH
	if _, ok := binmgr.PinnedSHA256(toolName, DefaultVersion, plat); !ok {
		t.Skipf("no committed pin for %s@%s on %s", toolName, DefaultVersion, plat)
	}
	dir := os.Getenv("YOKE_BLENDER_LIVE_DIR")
	if dir == "" {
		dir = t.TempDir()
	} else if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	dir, err := filepath.Abs(dir)
	if err != nil {
		t.Fatal(err)
	}
	asset := filepath.Join(dir, "cube.usda")
	blend := filepath.Join(dir, "cube.blend")
	png := filepath.Join(dir, "frame.png")
	if err := os.WriteFile(asset, []byte(liveUSDA), 0o644); err != nil {
		t.Fatal(err)
	}
	cache, _ := binmgr.CacheDir()
	t.Logf("platform=%s version=%s cache=%s work=%s", plat, DefaultVersion, cache, dir)

	// Provision through the real path once, so the download/verify step is
	// visible on its own in the log (Run would do the same implicitly).
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
		t.Logf("$ blender %s", strings.Join(args, " "))
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

	imp := run("asset-import", "--asset", asset, "--output", blend, "--timeout", timeout)
	if imp["schema_version"] != "yoke-blender-import-v1" || imp["fresh_scene"] != true {
		t.Fatalf("import result: %v", imp)
	}
	if n, _ := imp["object_count"].(float64); n < 3 {
		t.Fatalf("expected the cube, the camera and the light, got object_count=%v", imp["object_count"])
	}
	if st, err := os.Stat(blend); err != nil || st.Size() == 0 {
		t.Fatalf("output .blend: %v %v", st, err)
	}

	scene := run("scene-inspect", "--input", blend)
	if scene["schema_version"] != "yoke-blender-scene-v1" {
		t.Fatalf("scene result: %v", scene)
	}
	types := map[string]bool{}
	for _, o := range scene["objects"].([]any) {
		types[o.(map[string]any)["type"].(string)] = true
	}
	if !types["MESH"] || !types["CAMERA"] || !types["LIGHT"] {
		t.Fatalf("scene objects lack MESH+CAMERA+LIGHT: %v", scene["objects"])
	}
	if v, _ := scene["blender"].(string); !strings.HasPrefix(v, DefaultVersion) {
		t.Fatalf("blender version %q, want %s", v, DefaultVersion)
	}

	rend := run("render", "--input", blend, "--output", png, "--frame", "1", "--timeout", timeout)
	if rend["schema_version"] != "yoke-blender-render-v1" || rend["camera"] == nil {
		t.Fatalf("render result: %v", rend)
	}
	if st, err := os.Stat(png); err != nil || st.Size() == 0 {
		t.Fatalf("output .png: %v %v", st, err)
	}

	img := run("output-inspect", "--input", png)
	if img["schema_version"] != "yoke-blender-output-v1" || img["has_data"] != true {
		t.Fatalf("output result: %v", img)
	}
	rx, _ := rend["resolution_x"].(float64)
	ry, _ := rend["resolution_y"].(float64)
	pct, _ := rend["resolution_percentage"].(float64)
	w, _ := img["width"].(float64)
	h, _ := img["height"].(float64)
	if w != rx*pct/100 || h != ry*pct/100 {
		t.Fatalf("rendered %vx%v, scene says %vx%v @%v%%", w, h, rx, ry, pct)
	}

	// The frame must show something: decode it here (stdlib, independent of
	// Blender's own reading above) and require a visible share of lit pixels.
	// A world-less scene has a black background; the sun-lit cube faces are
	// what must be there.
	f, err := os.Open(png)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	decoded, err := pngimage.Decode(f)
	if err != nil {
		t.Fatalf("decode rendered png: %v", err)
	}
	b := decoded.Bounds()
	lit, total := 0, b.Dx()*b.Dy()
	for y := b.Min.Y; y < b.Max.Y; y++ {
		for x := b.Min.X; x < b.Max.X; x++ {
			r, g, bl, _ := decoded.At(x, y).RGBA()
			if r>>8 >= 32 || g>>8 >= 32 || bl>>8 >= 32 {
				lit++
			}
		}
	}
	t.Logf("frame %dx%d: %d of %d pixels lit (%.2f%%)", b.Dx(), b.Dy(), lit, total, 100*float64(lit)/float64(total))
	if lit < total/200 {
		t.Fatalf("rendered frame is (near) black: %d of %d pixels lit", lit, total)
	}
}
