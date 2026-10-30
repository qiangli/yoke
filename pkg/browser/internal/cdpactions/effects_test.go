package cdpactions

import (
	"image"
	"image/color"
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
