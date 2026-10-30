package cdpactions

import (
	"encoding/json"
	"errors"
	"image"
	"image/color"
	"os"
	"strings"
	"testing"
)

// Short units for the S536.4 stitcher geometry and paint math. No
// browser: synthetic tiles feed the same paint functions StitchFullPage
// uses, so the row mapping (page strips skipping panel zones, panels
// painted over their expanded zones) is checked exactly.

// solidTile returns a w×h tile filled with bands of the given colors,
// one band per row-block of height bh.
func solidTile(w, h int, bands []color.RGBA, bh int) *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		c := bands[(y/bh)%len(bands)]
		for x := 0; x < w; x++ {
			img.Set(x, y, c)
		}
	}
	return img
}

func at(img *image.RGBA, x, y int) color.RGBA {
	r, g, b, _ := img.At(x, y).RGBA()
	return color.RGBA{uint8(r >> 8), uint8(g >> 8), uint8(b >> 8), 255}
}

func TestShotScaleFromGate(t *testing.T) {
	if s, err := shotScaleFrom(200, 100); err != nil || s != 2 {
		t.Fatalf("200/100 = %v, %v", s, err)
	}
	if s, err := shotScaleFrom(100, 100); err != nil || s != 1 {
		t.Fatalf("100/100 = %v, %v", s, err)
	}
	for _, tc := range [][2]int{{0, 100}, {100, 0}, {40, 100}, {500, 100}} {
		if _, err := shotScaleFrom(tc[0], tc[1]); err == nil {
			t.Fatalf("shotScaleFrom(%d, %d): expected an error", tc[0], tc[1])
		}
	}
}

func TestOutermostJobsDropsNested(t *testing.T) {
	outer := shotPanel{X: 0, Y: 100, W: 200, H: 100, SH: 300}
	inner := shotPanel{X: 10, Y: 120, W: 100, H: 40, SH: 200}
	jobs := outermostJobs([]shotPanel{outer, inner})
	if len(jobs) != 1 || jobs[0].panel != outer || jobs[0].cache != 0 {
		t.Fatalf("nested panel not dropped: %+v", jobs)
	}
	// Sibling panels both survive, keeping their cache indices.
	a := shotPanel{X: 0, Y: 0, W: 100, H: 50, SH: 150}
	b := shotPanel{X: 0, Y: 200, W: 100, H: 50, SH: 150}
	jobs = outermostJobs([]shotPanel{a, b})
	if len(jobs) != 2 || jobs[0].cache != 0 || jobs[1].cache != 1 {
		t.Fatalf("siblings disturbed: %+v", jobs)
	}
}

func TestPlanStitchGeometry(t *testing.T) {
	layout := shotLayout{VW: 100, VH: 100, DocW: 100, DocH: 250,
		Panels: []shotPanel{{X: 0, Y: 100, W: 100, H: 50, SH: 150}}}
	jobs := outermostJobs(layout.Panels)
	plan := planStitch(layout, jobs)
	// 3 page tiles (250/100) + 3 panel tiles (150/50).
	if plan.tiles != 6 {
		t.Fatalf("tiles = %d, want 6", plan.tiles)
	}
	if got := plan.height(1); got != 250+(150-50) {
		t.Fatalf("final height = %d, want 350", got)
	}
	// Rows above the panel map 1:1; rows below shift by the expansion.
	if plan.finalRow(50) != 50 || plan.finalRow(200) != 300 {
		t.Fatalf("finalRow maps %d and %d", plan.finalRow(50), plan.finalRow(200))
	}
	if !plan.inPanel(120) || plan.inPanel(50) || plan.inPanel(200) {
		t.Fatal("inPanel misclassifies rows")
	}
}

func TestPaintPanelSliceStacksTiles(t *testing.T) {
	red := color.RGBA{255, 0, 0, 255}
	green := color.RGBA{0, 255, 0, 255}
	p := shotPanel{X: 0, Y: 0, W: 10, H: 10, SH: 20}
	full := image.NewRGBA(image.Rect(0, 0, 10, 20))
	next, err := paintPanelSlice(full, solidTile(10, 10, []color.RGBA{red}, 10), 1, p, shotPos{X: 0, Y: 0, PST: 0}, 0)
	if err != nil || next != 10 {
		t.Fatalf("first slice: next=%d err=%v", next, err)
	}
	next, err = paintPanelSlice(full, solidTile(10, 10, []color.RGBA{green}, 10), 1, p, shotPos{X: 0, Y: 0, PST: 10}, 10)
	if err != nil || next != 20 {
		t.Fatalf("second slice: next=%d err=%v", next, err)
	}
	if at(full, 5, 5) != red || at(full, 5, 15) != green {
		t.Fatalf("slices misstacked: %+v / %+v", at(full, 5, 5), at(full, 5, 15))
	}
}

func TestPaintPageStripSkipsPanelRows(t *testing.T) {
	blue := color.RGBA{0, 0, 255, 255}
	yellow := color.RGBA{255, 255, 0, 255}
	layout := shotLayout{VW: 10, VH: 10, DocW: 10, DocH: 30}
	plan := planStitch(layout, outermostJobs([]shotPanel{{X: 0, Y: 10, W: 10, H: 10, SH: 20}}))
	final := image.NewRGBA(image.Rect(0, 0, plan.width(1), plan.height(1)))
	if plan.height(1) != 40 {
		t.Fatalf("final height = %d, want 40", plan.height(1))
	}
	// Window tile at scroll 0: rows 0-10 are blue; rows 10-20 (the panel
	// box) must be SKIPPED here — the panel paints them later.
	paintPageStrip(final, solidTile(10, 10, []color.RGBA{blue}, 10), 1, plan, layout, 0, 0, 10)
	tile2 := solidTile(10, 10, []color.RGBA{yellow}, 10)
	paintPageStrip(final, tile2, 1, plan, layout, 10, 10, 20)
	paintPageStrip(final, tile2, 1, plan, layout, 20, 20, 30)
	if at(final, 5, 5) != blue {
		t.Fatalf("page top wrong: %+v", at(final, 5, 5))
	}
	// Rows 10-20 are the panel's expanded zone: still empty (black).
	if at(final, 5, 15) != (color.RGBA{0, 0, 0, 255}) {
		t.Fatalf("panel zone painted by the page strip: %+v", at(final, 5, 15))
	}
	// Rows below the panel shift down by SH-H = 10.
	if at(final, 5, 35) != yellow {
		t.Fatalf("page bottom wrong: %+v", at(final, 5, 35))
	}
	// The panel paints its full content over rows 10-30.
	panel := solidTile(10, 20, []color.RGBA{{255, 0, 255, 255}}, 20)
	paintPanelFull(final, panel, 1, plan, shotPanel{X: 0, Y: 10, W: 10, H: 10, SH: 20})
	if at(final, 5, 15) != (color.RGBA{255, 0, 255, 255}) || at(final, 5, 29) != (color.RGBA{255, 0, 255, 255}) {
		t.Fatalf("panel content misplaced: %+v / %+v", at(final, 5, 15), at(final, 5, 29))
	}
	if at(final, 5, 5) != blue || at(final, 5, 35) != yellow {
		t.Fatal("panel paint clobbered page rows")
	}
}

func TestDevAndCSSRows(t *testing.T) {
	if dev(10, 1) != 10 || dev(10, 2) != 20 || dev(0, 2) != 0 {
		t.Fatal("dev mis-scales")
	}
	if cssRows(10, 1) != 10 || cssRows(0, 1) != 0 {
		t.Fatal("cssRows mis-converts")
	}
}

// Two sibling panels at NONZERO document rows: the tile budget, the
// final-image extent, and the row mapping must all account for both
// expansions (a panel's full content SH occupies SH rows where H stood).
func TestStitchPlanTwoPanelsNonzeroExtent(t *testing.T) {
	layout := shotLayout{VW: 100, VH: 100, DocW: 100, DocH: 400,
		Panels: []shotPanel{
			{X: 0, Y: 50, W: 100, H: 50, SH: 100},
			{X: 0, Y: 200, W: 100, H: 50, SH: 150},
		}}
	jobs := outermostJobs(layout.Panels)
	if len(jobs) != 2 {
		t.Fatalf("siblings disturbed: %+v", jobs)
	}
	plan := planStitch(layout, jobs)
	// 4 page tiles (400/100) + 2 first-panel tiles (100/50) + 3
	// second-panel tiles (150/50).
	if plan.tiles != 9 {
		t.Fatalf("tiles = %d, want 9", plan.tiles)
	}
	if got := plan.height(1); got != 400+(100-50)+(150-50) {
		t.Fatalf("final height = %d, want 550", got)
	}
	if plan.finalRow(0) != 0 || plan.finalRow(100) != 150 || plan.finalRow(300) != 450 {
		t.Fatalf("finalRow maps %d, %d, %d", plan.finalRow(0), plan.finalRow(100), plan.finalRow(300))
	}
	if !plan.inPanel(60) || !plan.inPanel(210) || plan.inPanel(0) || plan.inPanel(150) || plan.inPanel(300) {
		t.Fatal("inPanel misclassifies rows around nonzero panels")
	}
}

// A window tile captured at a NONZERO outer scroll paints its rows at
// their shifted final positions: rows below a panel move down by the
// panel's expansion, rows above stay 1:1.
func TestPaintPageStripFromNonzeroWindow(t *testing.T) {
	yellow := color.RGBA{255, 255, 0, 255}
	layout := shotLayout{VW: 10, VH: 10, DocW: 10, DocH: 30}
	plan := planStitch(layout, outermostJobs([]shotPanel{{X: 0, Y: 10, W: 10, H: 10, SH: 20}}))
	final := image.NewRGBA(image.Rect(0, 0, plan.width(1), plan.height(1)))
	// Window sits at scroll 20: document rows 20-30 land at final rows
	// 30-40 (shifted by SH-H = 10 past the panel ending at row 20).
	paintPageStrip(final, solidTile(10, 10, []color.RGBA{yellow}, 10), 1, plan, layout, 20, 20, 30)
	if at(final, 5, 35) != yellow {
		t.Fatalf("scrolled page bottom wrong: %+v", at(final, 5, 35))
	}
	// Nothing above the painted band may be touched by this tile.
	if at(final, 5, 5) != (color.RGBA{0, 0, 0, 255}) {
		t.Fatalf("scrolled tile leaked above its band: %+v", at(final, 5, 5))
	}
}

// A panel whose box starts at a NONZERO offset inside the window paints
// only its visible slice (clipped at the fold), and the stride advances
// by exactly the rows copied — the page, not the plan, drives progress.
func TestPaintPanelSliceClippedAtFold(t *testing.T) {
	red := color.RGBA{255, 0, 0, 255}
	p := shotPanel{X: 0, Y: 5, W: 10, H: 10, SH: 20}
	full := image.NewRGBA(image.Rect(0, 0, 10, 20))
	// Window at 0, tile 10x10: panel rows 5-10 are visible (5 device
	// rows), rows 10-15 wait for the next scroll.
	next, err := paintPanelSlice(full, solidTile(10, 10, []color.RGBA{red}, 10), 1, p, shotPos{X: 0, Y: 0, PST: 0}, 0)
	if err != nil || next != 5 {
		t.Fatalf("clipped slice: next=%d err=%v, want next=5", next, err)
	}
	if at(full, 5, 0) != red || at(full, 5, 4) != red {
		t.Fatalf("clipped rows misplaced: %+v / %+v", at(full, 5, 0), at(full, 5, 4))
	}
	if at(full, 5, 5) != (color.RGBA{0, 0, 0, 255}) {
		t.Fatalf("unseen row painted: %+v", at(full, 5, 5))
	}
}

// A panel nowhere in the window is a loud error, never a skipped tile:
// silently dropping it would report a full page missing panel content.
func TestPaintPanelSliceOffscreenIsAnError(t *testing.T) {
	p := shotPanel{X: 0, Y: 500, W: 10, H: 10, SH: 20}
	full := image.NewRGBA(image.Rect(0, 0, 10, 20))
	if _, err := paintPanelSlice(full, solidTile(10, 10, []color.RGBA{{255, 0, 0, 255}}, 10), 1, p, shotPos{X: 0, Y: 0, PST: 0}, 0); err == nil {
		t.Fatal("offscreen panel painted silently: expected an error")
	}
}

// A viewport capture that is not a PNG is a loud error: decoding
// garbage as pixels would composite a corrupt stitch as success.
func TestDecodeTileRejectsNonPNG(t *testing.T) {
	if _, err := decodeTile([]byte("not a png")); err == nil {
		t.Fatal("decodeTile accepted garbage: expected an error")
	}
	if _, err := decodeTile(nil); err == nil {
		t.Fatal("decodeTile accepted empty input: expected an error")
	}
}

// The restore invocation must carry the NONZERO saved page and panel
// offsets back to the page: a zeroed payload would land the page at
// 0,0 instead of where the stitch found it (the forced-failure path
// below restores from the live cache for the same reason — a zero
// layout argument would clobber the true saved offsets).
func TestShotRestoreCallKeepsSavedPositions(t *testing.T) {
	layout := shotLayout{X: 111, Y: 222,
		Panels: []shotPanel{{X: 0, Y: 50, W: 100, H: 50, SH: 100, ST: 33, SL: 7}}}
	var back shotLayout
	call := shotRestoreCall(layout)
	const prefix = "globalThis.__bashyPage.shotRestore("
	if len(call) <= len(prefix)+1 || call[:len(prefix)] != prefix || call[len(call)-1] != ')' {
		t.Fatalf("restore call misbuilt: %q", call)
	}
	if err := json.Unmarshal([]byte(call[len(prefix):len(call)-1]), &back); err != nil {
		t.Fatalf("restore payload is not JSON: %v (%q)", err, call)
	}
	if back.X != 111 || back.Y != 222 {
		t.Fatalf("page offsets lost: %+v", back)
	}
	if len(back.Panels) != 1 || back.Panels[0].ST != 33 || back.Panels[0].SL != 7 {
		t.Fatalf("panel offsets lost: %+v", back.Panels)
	}
}

// TestLayoutFailureRestoresFromLiveCache pins the forced-failure path:
// the layout call hides fixed elements BEFORE answering, so an
// undecodable layout answer must still restore — from the live cache,
// never from a zeroed layout (which would land the page at 0,0) — and
// on a cleanup context detached from the operation's cancellation (a
// restore issued on an expired ctx fails instantly and strands the page
// hidden). The stitch itself needs Chrome, so this pins the wiring the
// manager's remote gate exercises; the pixel/restoration behavior it
// protects is covered by the paint and payload tests above.
func TestLayoutFailureRestoresFromLiveCache(t *testing.T) {
	src, err := os.ReadFile("fullpage.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"clean, stop := cleanupCtx(ctx)",
		"_ = shotRestoreCacheOnly(clean, world)",
		`globalThis.__bashyPage.shotRestore()`,
	} {
		if !strings.Contains(string(src), want) {
			t.Fatalf("stitch failure path lost %q", want)
		}
	}
}

// The window stays pinned on the panel box while the panel's own
// scrollTop moves: on a tall outer page the row is the clamped box row,
// never the box plus the content offset (which would scroll the panel
// itself offscreen). A short page pins at 0.
func TestPanelWindowYClamped(t *testing.T) {
	tall := shotLayout{DocH: 3000, VH: 500}
	for _, tc := range []struct {
		y, want int
	}{
		{100, 100},   // box in reach: window sits on it
		{2000, 2000}, // deep box on a tall page: still its own row
		{2800, 2500}, // box past the last window: clamped to docH-vh
		{-50, 0},     // box above the fold: clamped to 0
		{0, 0},
	} {
		if got := panelWindowY(tall, tc.y); got != tc.want {
			t.Fatalf("panelWindowY(tall, %d) = %d, want %d", tc.y, got, tc.want)
		}
	}
	short := shotLayout{DocH: 400, VH: 500}
	if got := panelWindowY(short, 100); got != 0 {
		t.Fatalf("panelWindowY(short, 100) = %d, want 0", got)
	}
}

// A failed stitch is refused, never downgraded: panels known names the
// missing rows, panels unknown (or an unwrapped error) refuses on
// unproven completeness. Pure formatting — no browser.
func TestRefuseStitchFallback(t *testing.T) {
	known, _ := refuseStitchFallback(&stitchError{cause: errors.New("Target crashed"), panels: 2})
	if known.Success || known.Error == "" {
		t.Fatalf("refusal must arrive as a result error, got %+v", known)
	}
	for _, want := range []string{"full_page stitch failed", "Target crashed",
		"refusing the single-capture fallback", "2 inner panel(s) would lose rows below the fold"} {
		if !strings.Contains(known.Error, want) {
			t.Fatalf("refusal with panels misses %q: %q", want, known.Error)
		}
	}
	unknown, _ := refuseStitchFallback(&stitchError{cause: errors.New("no size"), panels: -1})
	if !strings.Contains(unknown.Error, "completeness unproven") {
		t.Fatalf("refusal without panels misses completeness: %q", unknown.Error)
	}
	if strings.Contains(unknown.Error, "would lose rows") {
		t.Fatalf("refusal without panels must not claim panel rows: %q", unknown.Error)
	}
	plain, _ := refuseStitchFallback(errors.New("boom"))
	if !strings.Contains(plain.Error, "full_page stitch failed (boom)") {
		t.Fatalf("plain error not refused as a stitch failure: %q", plain.Error)
	}
}
