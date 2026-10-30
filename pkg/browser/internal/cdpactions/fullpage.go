package cdpactions

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/draw"
	"image/png"
	"sort"

	"github.com/chromedp/cdproto/page"
	"github.com/chromedp/cdproto/runtime"
	"github.com/chromedp/chromedp"

	"github.com/qiangli/yoke/pkg/browser/live/extension"
	"github.com/qiangli/yoke/pkg/browser/wire"
)

// Full-page capture with inner scroll areas stitched in.
//
// A capture-beyond-viewport shows an inner scroll panel only at its
// current scroll: every row below the fold is missing from the image.
// The stitcher tiles the page vertically, tiles each scrollable panel
// through its own full extent, and composites one image in which every
// panel appears at its full content height (rows below a panel shift
// down; the shift is a capture artifact, reported in the result meta).
//
// Positions are restored when the stitch finishes — and when it fails:
// the restore runs deferred, so a capture error never leaves the page
// (or its panels) scrolled. position:fixed elements are hidden while
// tiling (they would otherwise repeat on every tile) and shown again by
// the same restore. Nested panels stitch outermost-only: an inner panel
// is captured at its current scroll, like the legacy path.

// shotPanel is one inner scroll area, in document CSS pixels, with its
// saved scroll offsets.
type shotPanel struct {
	X  int `json:"x"`
	Y  int `json:"y"`
	W  int `json:"w"`
	H  int `json:"h"`
	SH int `json:"sh"`
	SW int `json:"sw"`
	ST int `json:"st"`
	SL int `json:"sl"`
}

// shotLayout is the page geometry the stitcher tiles over.
type shotLayout struct {
	DPR    float64     `json:"dpr"`
	VW     int         `json:"vw"`
	VH     int         `json:"vh"`
	DocW   int         `json:"docW"`
	DocH   int         `json:"docH"`
	X      int         `json:"x"`
	Y      int         `json:"y"`
	Panels []shotPanel `json:"panels"`
}

// shotJob is a panel to stitch plus its index in the page's element
// cache (the cache holds every panel found, including nested ones the
// stitch skips).
type shotJob struct {
	panel shotPanel
	cache int
}

// Bounds on the stitch so a pathological page costs an error, never a
// runaway: at most maxShotPanels panels, maxShotTiles captures, and a
// final image no taller than maxShotHeight device pixels.
const (
	maxShotPanels = 8
	maxShotTiles  = 48
	maxShotHeight = 16000
)

// The tiling geometry (layout, set, restore) is page.js
// (shotLayout/shotSet/shotRestore) — the same functions the live
// extension calls — evaluated here in one persistent isolated world.
// Per-call worlds would discard the panel cache between steps; this
// world lives for the stitch. Over CDP, Runtime.evaluate is not subject
// to the page's Content-Security-Policy, so a locked-down page still
// tiles.

// captureMetaStitched is the data document of a stitched capture.
func captureMetaStitched() string {
	b, _ := json.Marshal(map[string]any{"capture": "stitched_full_page"})
	return string(b)
}

// captureMetaFullPage is the data document of a legacy beyond-viewport
// capture (no panels, or the stitch fell back to it).
func captureMetaFullPage() string {
	b, _ := json.Marshal(map[string]any{"capture": "full_page"})
	return string(b)
}

// finishFullPageFallback answers a failed stitch with the legacy
// beyond-viewport capture instead of nothing: the stitch already
// restored the saved positions, so this captures the settled page. The
// meta names the fallback (and why), so it never reads as a stitch.
func finishFullPageFallback(ctx context.Context, a wire.Action, cause error) (*wire.Result, error) {
	var buf []byte
	if err := chromedp.Run(ctx, chromedp.FullScreenshot(&buf, 100)); err != nil {
		return &wire.Result{Error: err.Error()}, nil
	}
	b, _ := json.Marshal(map[string]any{"capture": "full_page", "stitched": false, "stitch_error": cause.Error()})
	return finishCapture(a, buf, string(b))
}

// StitchFullPage captures the page and every inner scroll area tile by
// tile and composites one PNG. It reports stitched=false (with the page
// untouched) when there is nothing to stitch, so the caller falls back
// to the single capture. Any failure restores the saved positions first.
func StitchFullPage(ctx context.Context) (buf []byte, stitched bool, err error) {
	world, err := shotWorld(ctx)
	if err != nil {
		return nil, false, err
	}
	layout, err := shotLayoutOf(ctx, world)
	if err != nil {
		// Best effort: the layout hid fixed elements before failing to
		// answer, so restore from the live cache before reporting. The
		// restore itself is never the error — the layout failure is.
		_ = shotRestoreCacheOnly(ctx, world)
		return nil, false, err
	}
	if len(layout.Panels) == 0 {
		// Untouched page, legacy single capture. (The setup hid fixed
		// elements; restore before leaving so nothing is stranded.)
		_ = shotRestoreTo(ctx, world, layout)
		return nil, false, nil
	}
	jobs := outermostJobs(layout.Panels)
	restored := false
	restore := func() {
		if restored {
			return
		}
		restored = true
		_ = shotRestoreTo(ctx, world, layout)
	}
	defer restore()

	plan := planStitch(layout, jobs)
	if plan.tiles > maxShotTiles {
		return nil, false, fmt.Errorf("full_page: stitch needs %d captures, over the %d cap", plan.tiles, maxShotTiles)
	}
	scale, err := shotScale(ctx, world, layout)
	if err != nil {
		return nil, false, err
	}
	final := image.NewRGBA(image.Rect(0, 0, plan.width(scale), plan.height(scale)))
	if final.Bounds().Dy() > maxShotHeight {
		return nil, false, fmt.Errorf("full_page: stitched image is %dpx tall, over the %dpx cap", final.Bounds().Dy(), maxShotHeight)
	}
	tiles := 1 // the scale probe above captured one tile already
	tick := func() error {
		tiles++
		if tiles > maxShotTiles {
			return fmt.Errorf("full_page: stitch exceeded the %d-capture cap", maxShotTiles)
		}
		return nil
	}

	// 1. Every panel's full content, at its own scroll positions.
	fulls := make([]*image.RGBA, len(jobs))
	for i, job := range jobs {
		p := job.panel
		full := image.NewRGBA(image.Rect(0, 0, dev(p.W, scale), dev(p.SH, scale)))
		st := 0
		for st < p.SH {
			got, tile, err := shotPanelTile(ctx, world, layout, job, st)
			if err != nil {
				return nil, false, err
			}
			if err := tick(); err != nil {
				return nil, false, err
			}
			next, err := paintPanelSlice(full, tile, scale, p, got, st)
			if err != nil {
				return nil, false, err
			}
			if next <= st {
				return nil, false, fmt.Errorf("full_page: panel %d would not scroll (pinned at %d of %d)", i, st, p.SH)
			}
			st = next
		}
		fulls[i] = full
	}

	// 2. The page rows, skipping the rows panels will cover. The loop is
	// driven by ACTUAL scroll positions: a page that snaps is painted
	// where it really is, and one that will not advance fails loudly
	// instead of tiling one row forever.
	y := 0
	for y < layout.DocH {
		top := y
		if layout.DocH > layout.VH {
			top = min(y, layout.DocH-layout.VH)
		}
		got, tile, err := shotWindowTile(ctx, world, top)
		if err != nil {
			return nil, false, err
		}
		if err := tick(); err != nil {
			return nil, false, err
		}
		lo, hi := y, layout.DocH
		if got.Y > lo {
			lo = got.Y
		}
		if got.Y+layout.VH < hi {
			hi = got.Y + layout.VH
		}
		paintPageStrip(final, tile, scale, plan, layout, got.Y, lo, hi)
		if hi <= y {
			return nil, false, fmt.Errorf("full_page: page would not scroll (pinned at %d of %d)", y, layout.DocH)
		}
		y = hi
		if layout.DocH <= layout.VH {
			break
		}
	}

	// 3. The panels over their expanded zones.
	for i, job := range jobs {
		paintPanelFull(final, fulls[i], scale, plan, job.panel)
	}

	var out bytes.Buffer
	if err := png.Encode(&out, final); err != nil {
		return nil, false, err
	}
	restore()
	return out.Bytes(), true, nil
}

// stitchPlan is the pure geometry of a stitch: capture-count estimate
// and the final image size, so tests can check the math without a
// browser.
type stitchPlan struct {
	tiles  int
	jobs   []shotJob
	layout shotLayout
}

// outermostJobs drops panels nested inside another panel: the outer
// tile already shows the inner at its current scroll, and painting both
// would misalign them. The survivors keep their cache indices.
func outermostJobs(panels []shotPanel) []shotJob {
	var out []shotJob
	for i, p := range panels {
		nested := false
		for j, q := range panels {
			if i == j {
				continue
			}
			if q.X <= p.X && q.Y <= p.Y && q.X+q.W >= p.X+p.W && q.Y+q.H >= p.Y+p.H &&
				(q.W*q.H > p.W*p.H || (q.W == p.W && q.H == p.H && j < i)) {
				nested = true
				break
			}
		}
		if !nested {
			out = append(out, shotJob{panel: p, cache: i})
		}
	}
	return out
}

// planStitch counts one tile per viewport of page height plus one per
// viewport of panel content height.
func planStitch(layout shotLayout, jobs []shotJob) stitchPlan {
	tiles := 0
	if layout.DocH > layout.VH {
		tiles += (layout.DocH + layout.VH - 1) / layout.VH
	} else {
		tiles++
	}
	for _, j := range jobs {
		step := min(j.panel.H, layout.VH)
		if step <= 0 {
			step = 1
		}
		tiles += (j.panel.SH + step - 1) / step
	}
	return stitchPlan{tiles: tiles, jobs: jobs, layout: layout}
}

// shiftAbove is how many extra rows panels ending above docRow add: a
// panel's full content (SH) occupies SH rows where its box (H) held H.
func (p stitchPlan) shiftAbove(docRow int) int {
	shift := 0
	for _, j := range p.jobs {
		q := j.panel
		if q.Y+q.H <= docRow {
			shift += q.SH - q.H
		}
	}
	return shift
}

// width/height are the final image dimensions in device pixels.
func (p stitchPlan) width(scale float64) int { return dev(p.layout.DocW, scale) }
func (p stitchPlan) height(scale float64) int {
	return dev(p.layout.DocH+p.shiftAbove(1<<30), scale)
}

// finalRow maps a document row outside any panel box to its final-image
// row. Rows inside a panel box are painted by paintPanelFull instead.
func (p stitchPlan) finalRow(docRow int) int { return docRow + p.shiftAbove(docRow) }

// inPanel reports whether docRow falls inside a panel's box.
func (p stitchPlan) inPanel(docRow int) bool {
	for _, j := range p.jobs {
		q := j.panel
		if docRow >= q.Y && docRow < q.Y+q.H {
			return true
		}
	}
	return false
}

func dev(css int, scale float64) int {
	n := int(float64(css)*scale + 0.5)
	if n < 1 && css > 0 {
		n = 1
	}
	return n
}

// shotWorld opens one persistent isolated world for the stitch: the
// panel cache (page.js globalThis.__bashyShot) survives across the
// layout/set/restore calls, which per-call worlds would discard.
func shotWorld(ctx context.Context) (runtime.ExecutionContextID, error) {
	var world runtime.ExecutionContextID
	err := chromedp.Run(ctx, chromedp.ActionFunc(func(ctx context.Context) error {
		tree, err := page.GetFrameTree().Do(ctx)
		if err != nil {
			return err
		}
		world, err = page.CreateIsolatedWorld(tree.Frame.ID).WithWorldName("bashy-shot").Do(ctx)
		if err != nil {
			return err
		}
		if _, exc, err := runtime.Evaluate(extension.PageJS).WithContextID(world).Do(ctx); err != nil || exc != nil {
			if err == nil {
				err = errors.New(exc.Text)
			}
			return fmt.Errorf("full_page: load page core: %w", err)
		}
		return nil
	}))
	if err != nil {
		return 0, err
	}
	return world, nil
}

// worldEval calls one page.js shot function in the stitch world and
// decodes its by-value answer into out.
func worldEval(ctx context.Context, world runtime.ExecutionContextID, call string, out any) error {
	return chromedp.Run(ctx, chromedp.ActionFunc(func(ctx context.Context) error {
		res, exc, err := runtime.Evaluate(call).WithContextID(world).WithAwaitPromise(true).WithReturnByValue(true).Do(ctx)
		if err != nil {
			return err
		}
		if exc != nil {
			return errors.New("full_page: " + exc.Text)
		}
		if out == nil {
			return nil
		}
		if err := json.Unmarshal(res.Value, out); err != nil {
			return fmt.Errorf("full_page: unreadable page answer (%s)", string(res.Value))
		}
		return nil
	}))
}

func shotLayoutOf(ctx context.Context, world runtime.ExecutionContextID) (shotLayout, error) {
	var layout shotLayout
	if err := worldEval(ctx, world, "globalThis.__bashyPage.shotLayout()", &layout); err != nil {
		return shotLayout{}, err
	}
	if layout.VW <= 0 || layout.VH <= 0 || layout.DocW <= 0 || layout.DocH <= 0 {
		return shotLayout{}, fmt.Errorf("full_page: page reported no size")
	}
	if layout.DPR <= 0 {
		layout.DPR = 1
	}
	return layout, nil
}

func shotRestoreTo(ctx context.Context, world runtime.ExecutionContextID, layout shotLayout) error {
	return worldEval(ctx, world, shotRestoreCall(layout), nil)
}

// shotRestoreCall builds the restore invocation for a saved layout,
// separate so tests can pin the payload without a browser: the restore
// must carry the NONZERO saved page/panel offsets back, or the page
// lands at 0,0 instead of where the stitch found it.
func shotRestoreCall(layout shotLayout) string {
	saved, _ := json.Marshal(layout)
	return "globalThis.__bashyPage.shotRestore(" + string(saved) + ")"
}

// shotRestoreCacheOnly restores from the page's live stitch cache: the
// layout call hides fixed elements BEFORE answering, so a layout answer
// that never arrives (undecodable, or the world died mid-layout) still
// leaves the page hidden — and the saved positions it would have
// carried are exactly what the cache already holds. Calling shotRestore
// with no argument restores those live values instead of clobbering
// them with zeros (which a zero shotLayout argument would do: the page
// restores scroll 0,0 and loses the true saved offsets).
func shotRestoreCacheOnly(ctx context.Context, world runtime.ExecutionContextID) error {
	return worldEval(ctx, world, "globalThis.__bashyPage.shotRestore()", nil)
}

type shotPos struct {
	X   int `json:"x"`
	Y   int `json:"y"`
	PST int `json:"pst"`
}

func shotSetTo(ctx context.Context, world runtime.ExecutionContextID, x, y, cache, pt int) (shotPos, error) {
	arg, _ := json.Marshal(map[string]any{"x": x, "y": y, "p": cache, "pt": pt})
	var pos shotPos
	if err := worldEval(ctx, world, "globalThis.__bashyPage.shotSet("+string(arg)+")", &pos); err != nil {
		return shotPos{}, err
	}
	return pos, nil
}

// shotScale measures the device-pixel scale from one real tile: the
// renderer's answer beats the DPR the page reports.
func shotScale(ctx context.Context, world runtime.ExecutionContextID, layout shotLayout) (float64, error) {
	if _, err := shotSetTo(ctx, world, 0, 0, -1, 0); err != nil {
		return 0, err
	}
	var tile []byte
	if err := chromedp.Run(ctx, chromedp.CaptureScreenshot(&tile)); err != nil {
		return 0, err
	}
	img, err := png.Decode(bytes.NewReader(tile))
	if err != nil {
		return 0, fmt.Errorf("full_page: tile is not a PNG: %v", err)
	}
	return shotScaleFrom(img.Bounds().Dx(), layout.VW)
}

// shotScaleFrom converts a tile width to a scale. The plausibility gate
// stays separate so tests can check it without a browser.
func shotScaleFrom(tileW, vw int) (float64, error) {
	if tileW <= 0 || vw <= 0 {
		return 0, fmt.Errorf("full_page: tile has no size")
	}
	scale := float64(tileW) / float64(vw)
	if scale < 0.5 || scale > 4 {
		return 0, fmt.Errorf("full_page: implausible tile scale %g", scale)
	}
	return scale, nil
}

// shotWindowTile scrolls the window to top and captures one viewport,
// reporting where the window really landed.
func shotWindowTile(ctx context.Context, world runtime.ExecutionContextID, top int) (shotPos, *image.RGBA, error) {
	pos, err := shotSetTo(ctx, world, 0, top, -1, 0)
	if err != nil {
		return shotPos{}, nil, err
	}
	tile, err := shotTile(ctx)
	if err != nil {
		return shotPos{}, nil, err
	}
	return pos, tile, nil
}

// shotPanelTile pins the window on job's box and sets the panel to st,
// then captures one viewport. The window must NOT follow st: the panel
// box is fixed in the document, so scrolling the window by the content
// offset pushes the box itself out of view. The paint math uses only
// the ACTUAL positions that come back in pos.
func shotPanelTile(ctx context.Context, world runtime.ExecutionContextID, layout shotLayout, job shotJob, st int) (shotPos, *image.RGBA, error) {
	sy := 0
	if layout.DocH > layout.VH {
		if max := layout.DocH - layout.VH; job.panel.Y > max {
			sy = max
		} else if job.panel.Y > 0 {
			sy = job.panel.Y
		}
	}
	pos, err := shotSetTo(ctx, world, 0, sy, job.cache, st)
	if err != nil {
		return shotPos{}, nil, err
	}
	tile, err := shotTile(ctx)
	if err != nil {
		return shotPos{}, nil, err
	}
	return pos, tile, nil
}

func shotTile(ctx context.Context) (*image.RGBA, error) {
	var raw []byte
	if err := chromedp.Run(ctx, chromedp.CaptureScreenshot(&raw)); err != nil {
		return nil, err
	}
	return decodeTile(raw)
}

// decodeTile decodes one viewport PNG into RGBA. Separate so tests can
// feed synthetic tiles into the paint math without a browser.
func decodeTile(raw []byte) (*image.RGBA, error) {
	img, err := png.Decode(bytes.NewReader(raw))
	if err != nil {
		return nil, fmt.Errorf("full_page: tile is not a PNG: %v", err)
	}
	rgba, ok := img.(*image.RGBA)
	if !ok {
		rgba = image.NewRGBA(img.Bounds())
		draw.Draw(rgba, img.Bounds(), img, image.Point{}, draw.Src)
	}
	return rgba, nil
}

// paintPanelSlice copies the visible slice of panel p from a viewport
// tile into its full-content image, and returns the content row the next
// slice must start at. The page — not the plan — decides the stride: st
// advances by what this tile actually showed, so a snapped scroll can
// neither skip rows nor tile one row forever.
func paintPanelSlice(full, tile *image.RGBA, scale float64, p shotPanel, got shotPos, st int) (int, error) {
	vw := tile.Bounds().Dx()
	vh := tile.Bounds().Dy()
	vx := dev(p.X-got.X, scale)
	vy := dev(p.Y-got.Y, scale)
	pw := dev(p.W, scale)
	ph := dev(p.H, scale)
	// Visible box of the panel in this tile, in device pixels.
	x0 := max(vx, 0)
	x1 := min(vx+pw, vw)
	y0 := max(vy, 0)
	y1 := min(vy+ph, vh)
	if x1 <= x0 || y1 <= y0 {
		return st, fmt.Errorf("full_page: panel at %d,%d is not visible at window %d,%d", p.X, p.Y, got.X, got.Y)
	}
	// Tile device row y0 shows content device row pstDev + (y0 - vy).
	pstDev := dev(got.PST, scale)
	base := pstDev + (y0 - vy)
	if base < 0 {
		// The page clamped above the request: copy what is shown.
		y0 -= base
		base = 0
		if y1 <= y0 {
			return st, nil
		}
	}
	h := y1 - y0
	fullH := full.Bounds().Dy()
	if base+h > fullH {
		h = fullH - base
		if h <= 0 {
			return st, nil
		}
	}
	draw.Draw(full, image.Rect(0, base, x1-x0, base+h), tile, image.Pt(x0, y0), draw.Src)
	// The next slice starts after the last content row copied, in CSS px.
	return cssRows(base+h, scale), nil
}

// cssRows converts a device-row count back to CSS rows (rounding up, so
// a stride never stands still on a fractional scale).
func cssRows(devRows int, scale float64) int {
	n := int(float64(devRows)/scale + 0.5)
	if n < 1 && devRows > 0 {
		n = 1
	}
	return n
}

// paintPageStrip draws the document rows [lo, hi) of one window tile
// into the final image, skipping the rows panels cover (paintPanelFull
// draws those over their expanded zones). actualY is where the window
// really is; rows are copied from where they really are.
func paintPageStrip(final, tile *image.RGBA, scale float64, plan stitchPlan, layout shotLayout, actualY, lo, hi int) {
	w := min(dev(layout.DocW, scale), tile.Bounds().Dx())
	bounds := []int{lo, hi}
	for _, j := range plan.jobs {
		q := j.panel
		if q.Y+q.H > lo && q.Y < hi {
			bounds = append(bounds, max(q.Y, lo), min(q.Y+q.H, hi))
		}
	}
	sort.Ints(bounds)
	for i := 0; i+1 < len(bounds); i++ {
		a, b := bounds[i], bounds[i+1]
		if a >= b {
			continue
		}
		if plan.inPanel((a + b) / 2) {
			continue
		}
		srcY0 := dev(a-actualY, scale)
		srcY1 := dev(b-actualY, scale)
		if srcY1 <= 0 || srcY0 >= tile.Bounds().Dy() {
			continue
		}
		skip := 0
		if srcY0 < 0 {
			skip = -srcY0
			srcY0 = 0
		}
		srcH := srcY1 - srcY0
		if srcY0+srcH > tile.Bounds().Dy() {
			srcH = tile.Bounds().Dy() - srcY0
		}
		if srcH <= 0 {
			continue
		}
		// The shift is constant across a run outside panels, so final
		// rows track document rows 1:1 past the segment start.
		dstY := dev(plan.finalRow(a), scale) + skip
		draw.Draw(final, image.Rect(0, dstY, w, dstY+srcH), tile, image.Pt(0, srcY0), draw.Src)
	}
}

// paintPanelFull draws a panel's stitched content over its expanded
// zone: full content height where its box stood.
func paintPanelFull(final, full *image.RGBA, scale float64, plan stitchPlan, p shotPanel) {
	dstY := dev(plan.finalRow(p.Y), scale)
	dst := image.Rect(dev(p.X, scale), dstY, dev(p.X, scale)+full.Bounds().Dx(), dstY+full.Bounds().Dy())
	draw.Draw(final, dst, full, image.Point{}, draw.Src)
}
