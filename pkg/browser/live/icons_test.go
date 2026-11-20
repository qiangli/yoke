package live

import (
	"bytes"
	"encoding/xml"
	"flag"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// The extension's icons are renderings of ONE source, extension/icons/shape.svg
// (the Shape mark: the Flow path twice, the second turned half way round, white
// on the blue-violet square). This file is the rasterizer and the pin:
//
//	go test ./pkg/browser/live -run TestExtensionIconsRenderShapeSVG -update-icons
//
// rewrites icons/icon-{16,32,48,128}.png from the SVG; without the flag the test
// fails when a committed PNG is not that rendering. No image tool is involved,
// so the icons can be regenerated on any machine that can run the tests.
//
// It is a rasterizer for THIS file, not for SVG: a rect and absolute M/C/Q/L/Z
// paths with a solid fill and an optional rotate(180 cx cy). Anything else in
// the source is an error, never a guess.

var updateIcons = flag.Bool("update-icons", false, "rewrite extension/icons/icon-*.png from icons/shape.svg")

var iconSizes = []int{16, 32, 48, 128}

const (
	iconSVG = "extension/icons/shape.svg"
	// iconSub is the supersampling per pixel edge: coverage is counted on an
	// iconSub x iconSub grid, in integers.
	iconSub = 8
	// curveSteps flattens each curve segment into this many chords.
	curveSteps = 48
)

type pt struct{ x, y float64 }

// iconShape is one filled outline in viewBox units, painted in order.
type iconShape struct {
	fill  color.NRGBA
	polys [][]pt
}

func TestExtensionIconsRenderShapeSVG(t *testing.T) {
	src, err := os.ReadFile(iconSVG)
	if err != nil {
		t.Fatal(err)
	}
	box, shapes, err := parseIconSVG(src)
	if err != nil {
		t.Fatalf("%s: %v", iconSVG, err)
	}
	for _, size := range iconSizes {
		want := renderIcon(box, shapes, size)
		path := filepath.Join("extension", "icons", fmt.Sprintf("icon-%d.png", size))
		if *updateIcons {
			var buf bytes.Buffer
			enc := png.Encoder{CompressionLevel: png.BestCompression}
			if err := enc.Encode(&buf, want); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil {
				t.Fatal(err)
			}
			t.Logf("wrote %s (%d bytes)", path, buf.Len())
			continue
		}
		f, err := os.Open(path)
		if err != nil {
			t.Fatal(err)
		}
		got, err := png.Decode(f)
		f.Close()
		if err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		if got.Bounds() != want.Bounds() {
			t.Fatalf("%s is %v, want %v", path, got.Bounds(), want.Bounds())
		}
		// Coverage is integer, but an edge crossing is floating point, and a
		// platform that fuses multiply-add may move one sample across an edge.
		// One sample is 255/64 of a channel; allow two.
		const slack = 8
		for y := 0; y < size; y++ {
			for x := 0; x < size; x++ {
				g := color.NRGBAModel.Convert(got.At(x, y)).(color.NRGBA)
				w := want.NRGBAAt(x, y)
				if absDiff(g.R, w.R) > slack || absDiff(g.G, w.G) > slack || absDiff(g.B, w.B) > slack || absDiff(g.A, w.A) > slack {
					t.Fatalf("%s pixel (%d,%d) is %v, the rendering of %s is %v; regenerate with -update-icons",
						path, x, y, g, iconSVG, w)
				}
			}
		}
	}
}

// TestExtensionIconIsTheShapeMark pins what the rendering must show, so a
// source edited into something else is caught and not just re-rendered: an
// opaque blue-violet square (a transparent icon disappears on a dark toolbar)
// with the white mark through its centre, the same picture turned half round.
func TestExtensionIconIsTheShapeMark(t *testing.T) {
	src, err := os.ReadFile(iconSVG)
	if err != nil {
		t.Fatal(err)
	}
	box, shapes, err := parseIconSVG(src)
	if err != nil {
		t.Fatal(err)
	}
	if len(shapes) != 3 {
		t.Fatalf("%d shapes, want the square and the Flow path twice", len(shapes))
	}
	const n = 128
	img := renderIcon(box, shapes, n)
	violet := color.NRGBA{0x6D, 0x4A, 0xFF, 0xFF}
	white := color.NRGBA{0xFF, 0xFF, 0xFF, 0xFF}
	for _, c := range [][2]int{{0, 0}, {n - 1, 0}, {0, n - 1}, {n - 1, n - 1}} {
		if got := img.NRGBAAt(c[0], c[1]); got != violet {
			t.Errorf("corner %v is %v, want the opaque square %v", c, got, violet)
		}
	}
	whites := 0
	for y := 0; y < n; y++ {
		for x := 0; x < n; x++ {
			p := img.NRGBAAt(x, y)
			if p.A != 0xFF {
				t.Fatalf("pixel (%d,%d) is not opaque: %v", x, y, p)
			}
			// An edge pixel may round differently from its mirror; a different
			// picture would not be within a few levels.
			if q := img.NRGBAAt(n-1-x, n-1-y); absDiff(p.R, q.R) > 8 || absDiff(p.G, q.G) > 8 || absDiff(p.B, q.B) > 8 {
				t.Fatalf("pixel (%d,%d) %v differs from its half-turn %v", x, y, p, q)
			}
			if p == white {
				whites++
			}
		}
	}
	// Each Flow stroke covers a little under a fifth of the square.
	if share := whites * 100 / (n * n); share < 25 || share > 45 {
		t.Errorf("the white mark covers %d%% of the icon, want about a third", share)
	}
	// No voice dot: the centre, between the two strokes, stays the background.
	if got := img.NRGBAAt(n/2, n/2); got != violet {
		t.Errorf("centre is %v, want the background (no dot in the mouth)", got)
	}
}

func absDiff(a, b uint8) int {
	if a > b {
		return int(a - b)
	}
	return int(b - a)
}

// parseIconSVG reads the viewBox side and the shapes, in paint order.
func parseIconSVG(src []byte) (float64, []iconShape, error) {
	dec := xml.NewDecoder(bytes.NewReader(src))
	var box float64
	var shapes []iconShape
	for {
		tok, err := dec.Token()
		if err != nil {
			break
		}
		el, ok := tok.(xml.StartElement)
		if !ok {
			continue
		}
		attr := map[string]string{}
		for _, a := range el.Attr {
			attr[a.Name.Local] = a.Value
		}
		take := func(k string) string {
			v := attr[k]
			delete(attr, k)
			return v
		}
		switch el.Name.Local {
		case "svg":
			vb := strings.Fields(take("viewBox"))
			if len(vb) != 4 || vb[0] != "0" || vb[1] != "0" || vb[2] != vb[3] {
				return 0, nil, fmt.Errorf("viewBox %q: want a square at the origin", vb)
			}
			if box, err = strconv.ParseFloat(vb[2], 64); err != nil {
				return 0, nil, err
			}
			continue // width/height/xmlns do not change the picture
		case "rect":
			fill, err := parseFill(take("fill"))
			if err != nil {
				return 0, nil, err
			}
			var v [4]float64 // x y width height
			for i, k := range []string{"x", "y", "width", "height"} {
				if s := take(k); s != "" {
					if v[i], err = strconv.ParseFloat(s, 64); err != nil {
						return 0, nil, fmt.Errorf("rect %s: %v", k, err)
					}
				}
			}
			shapes = append(shapes, iconShape{fill: fill, polys: [][]pt{{
				{v[0], v[1]}, {v[0] + v[2], v[1]}, {v[0] + v[2], v[1] + v[3]}, {v[0], v[1] + v[3]},
			}}})
		case "path":
			fill, err := parseFill(take("fill"))
			if err != nil {
				return 0, nil, err
			}
			polys, err := flattenPath(take("d"))
			if err != nil {
				return 0, nil, err
			}
			if tr := take("transform"); tr != "" {
				var a, cx, cy float64
				if _, err := fmt.Sscanf(tr, "rotate(%g %g %g)", &a, &cx, &cy); err != nil || a != 180 {
					return 0, nil, fmt.Errorf("transform %q: only rotate(180 cx cy) is rendered", tr)
				}
				// A half turn about (cx,cy) is a point reflection: exact.
				for _, poly := range polys {
					for i, p := range poly {
						poly[i] = pt{2*cx - p.x, 2*cy - p.y}
					}
				}
			}
			shapes = append(shapes, iconShape{fill: fill, polys: polys})
		default:
			return 0, nil, fmt.Errorf("element <%s> is not rendered", el.Name.Local)
		}
		if len(attr) > 0 {
			return 0, nil, fmt.Errorf("<%s>: attributes %v are not rendered", el.Name.Local, attr)
		}
	}
	if box == 0 || len(shapes) == 0 {
		return 0, nil, fmt.Errorf("no viewBox or nothing to draw")
	}
	return box, shapes, nil
}

func parseFill(s string) (color.NRGBA, error) {
	if len(s) != 7 || s[0] != '#' {
		return color.NRGBA{}, fmt.Errorf("fill %q: want #RRGGBB", s)
	}
	v, err := strconv.ParseUint(s[1:], 16, 32)
	if err != nil {
		return color.NRGBA{}, fmt.Errorf("fill %q: %v", s, err)
	}
	return color.NRGBA{uint8(v >> 16), uint8(v >> 8), uint8(v), 0xFF}, nil
}

// flattenPath turns absolute M/C/Q/L/Z path data into closed polygons.
func flattenPath(d string) ([][]pt, error) {
	// Separate the command letters from the numbers they are written against.
	var spaced strings.Builder
	for _, r := range d {
		switch {
		case r == ',':
			spaced.WriteByte(' ')
		case (r >= 'A' && r <= 'Z') || (r >= 'a' && r <= 'z'):
			spaced.WriteByte(' ')
			spaced.WriteRune(r)
			spaced.WriteByte(' ')
		default:
			spaced.WriteRune(r)
		}
	}
	toks := strings.Fields(spaced.String())
	var polys [][]pt
	var cur []pt
	i := 0
	nums := func(n int) ([]pt, error) {
		out := make([]pt, n)
		for k := range out {
			if i+1 >= len(toks) {
				return nil, fmt.Errorf("path data ends inside a command")
			}
			x, err1 := strconv.ParseFloat(toks[i], 64)
			y, err2 := strconv.ParseFloat(toks[i+1], 64)
			if err1 != nil || err2 != nil {
				return nil, fmt.Errorf("path data: %q %q is not a point", toks[i], toks[i+1])
			}
			out[k] = pt{x, y}
			i += 2
		}
		return out, nil
	}
	for i < len(toks) {
		cmd := toks[i]
		i++
		switch cmd {
		case "M":
			p, err := nums(1)
			if err != nil {
				return nil, err
			}
			if len(cur) > 0 {
				polys = append(polys, cur)
			}
			cur = []pt{p[0]}
		case "L":
			p, err := nums(1)
			if err != nil {
				return nil, err
			}
			cur = append(cur, p[0])
		case "C":
			p, err := nums(3)
			if err != nil {
				return nil, err
			}
			if len(cur) == 0 {
				return nil, fmt.Errorf("path data: C before M")
			}
			a := cur[len(cur)-1]
			for s := 1; s <= curveSteps; s++ {
				t := float64(s) / curveSteps
				u := 1 - t
				cur = append(cur, pt{
					u*u*u*a.x + 3*u*u*t*p[0].x + 3*u*t*t*p[1].x + t*t*t*p[2].x,
					u*u*u*a.y + 3*u*u*t*p[0].y + 3*u*t*t*p[1].y + t*t*t*p[2].y,
				})
			}
		case "Q":
			p, err := nums(2)
			if err != nil {
				return nil, err
			}
			if len(cur) == 0 {
				return nil, fmt.Errorf("path data: Q before M")
			}
			a := cur[len(cur)-1]
			for s := 1; s <= curveSteps; s++ {
				t := float64(s) / curveSteps
				u := 1 - t
				cur = append(cur, pt{
					u*u*a.x + 2*u*t*p[0].x + t*t*p[1].x,
					u*u*a.y + 2*u*t*p[0].y + t*t*p[1].y,
				})
			}
		case "Z":
			if len(cur) > 0 {
				polys = append(polys, cur)
				cur = nil
			}
		default:
			return nil, fmt.Errorf("path command %q is not rendered (absolute M C Q L Z only)", cmd)
		}
	}
	if len(cur) > 0 {
		polys = append(polys, cur)
	}
	if len(polys) == 0 {
		return nil, fmt.Errorf("empty path")
	}
	return polys, nil
}

// renderIcon paints the shapes into a size x size image. Each shape's
// coverage of a pixel is counted on the sample grid (nonzero winding) and
// composited over what is already there.
func renderIcon(box float64, shapes []iconShape, size int) *image.NRGBA {
	img := image.NewNRGBA(image.Rect(0, 0, size, size))
	n := size * iconSub
	scale := float64(n) / box
	const full = iconSub * iconSub
	for _, sh := range shapes {
		cover := make([]int, size*size)
		type crossing struct {
			x   float64
			dir int
		}
		for sy := 0; sy < n; sy++ {
			y := (float64(sy) + 0.5) / scale
			var xs []crossing
			for _, poly := range sh.polys {
				for k := range poly {
					a, b := poly[k], poly[(k+1)%len(poly)]
					if (a.y <= y) == (b.y <= y) {
						continue
					}
					dir := 1
					if a.y > b.y {
						dir = -1
					}
					xs = append(xs, crossing{a.x + (y-a.y)/(b.y-a.y)*(b.x-a.x), dir})
				}
			}
			sort.Slice(xs, func(i, j int) bool { return xs[i].x < xs[j].x })
			wind := 0
			for k := 0; k+1 < len(xs); k++ {
				wind += xs[k].dir
				if wind == 0 {
					continue
				}
				// Samples whose centre lies in (from, to].
				from := sampleAt(xs[k].x*scale, n)
				to := sampleAt(xs[k+1].x*scale, n)
				for sx := from; sx < to; sx++ {
					cover[(sy/iconSub)*size+sx/iconSub]++
				}
			}
		}
		for i, c := range cover {
			if c == 0 {
				continue
			}
			x, y := i%size, i/size
			dst := img.NRGBAAt(x, y)
			// Source over destination, straight alpha, in integers.
			// The weights are alpha times the sample count.
			sw := c * 255
			dw := int(dst.A) * (full - c)
			tot := sw + dw
			mix := func(s, d uint8) uint8 {
				return uint8((int(s)*sw + int(d)*dw + tot/2) / tot)
			}
			img.SetNRGBA(x, y, color.NRGBA{mix(sh.fill.R, dst.R), mix(sh.fill.G, dst.G), mix(sh.fill.B, dst.B), uint8((tot + full/2) / full)})
		}
	}
	return img
}

// sampleAt is the index of the first sample whose centre is right of x (in
// sample units), clamped to the row.
func sampleAt(x float64, n int) int {
	i := int(x + 0.5)
	if x+0.5 < 0 {
		i = 0
	}
	if i > n {
		i = n
	}
	return i
}
