package git

// A line diff written from the published Myers O(ND) algorithm ("An
// O(ND) Difference Algorithm and Its Variations", 1986) and git's
// documented behavior, then fitted to host git's output by black-box
// differential testing (linediff_oracle_test.go). No git/xdiff source
// was used: xdiff is LGPL and this package is MIT.

import (
	"strconv"
	"strings"
)

// diffSplitLines splits content into lines that keep their "\n"; a final
// line without one is kept as is (it differs from the same text + "\n").
func diffSplitLines(s string) []string {
	if s == "" {
		return nil
	}
	lines := strings.SplitAfter(s, "\n")
	if lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	return lines
}

// lineDiff marks which lines of a and b are changed in a shortest edit
// script, compacted the way git presents it.
type lineDiff struct {
	a, b   []int  // line ids
	ca, cb []bool // changed flags
	la, lb []string
	indent bool // use the indent heuristic when sliding
	// declined is set when the search gave up on a box too large for
	// the verified give-up rule; the result may differ from host git's.
	declined bool
}

func newLineDiff(a, b []string, indent bool) *lineDiff {
	ids := map[string]int{}
	id := func(s string) int {
		if v, ok := ids[s]; ok {
			return v
		}
		ids[s] = len(ids)
		return len(ids) - 1
	}
	d := &lineDiff{la: a, lb: b, indent: indent}
	d.a = make([]int, len(a))
	for i, s := range a {
		d.a[i] = id(s)
	}
	d.b = make([]int, len(b))
	for i, s := range b {
		d.b[i] = id(s)
	}
	d.ca = make([]bool, len(a))
	d.cb = make([]bool, len(b))
	d.compareDiscarding()
	d.compact(d.a, d.ca, d.la, d.cb)
	d.compact(d.b, d.cb, d.lb, d.ca)
	return d
}

// compare finds a shortest edit script for a[aLo:aHi] vs b[bLo:bHi] by
// divide and conquer on a middle snake.
func (d *lineDiff) compare(aLo, aHi, bLo, bHi int) {
	for aLo < aHi && bLo < bHi && d.a[aLo] == d.b[bLo] {
		aLo++
		bLo++
	}
	for aHi > aLo && bHi > bLo && d.a[aHi-1] == d.b[bHi-1] {
		aHi--
		bHi--
	}
	switch {
	case aLo == aHi:
		for j := bLo; j < bHi; j++ {
			d.cb[j] = true
		}
		return
	case bLo == bHi:
		for i := aLo; i < aHi; i++ {
			d.ca[i] = true
		}
		return
	}
	x, y, ox, oy := d.middleSnake(aLo, aHi, bLo, bHi)
	if (x == aLo && y == bLo) || (x == aHi && y == bHi) {
		x, y = ox, oy // a corner split makes no progress: use the snake's other end
	}
	d.compare(aLo, x, bLo, y)
	d.compare(x, aHi, y, bHi)
}

// middleSnake finds where a forward search from the box's top-left and
// a backward search from its bottom-right first overlap, and returns
// that snake's start and its far end (the caller splits at the start,
// or at the far end when the start is a corner of the box). Where the
// paper leaves choices open, host git's are used (fitted by the
// oracle): each pass scans its diagonals from high k to low, a forward
// step on equal reach is a deletion (and a backward one its mirror),
// and the forward pass of each round runs before the backward pass.
func (d *lineDiff) middleSnake(aLo, aHi, bLo, bHi int) (int, int, int, int) {
	n, m := aHi-aLo, bHi-bLo
	max := n + m
	delta := n - m
	off := max + abs(delta) + 2
	fwd := make([]int, 2*off+1) // furthest x on diagonal k = x - y (relative)
	bwd := make([]int, 2*off+1) // smallest x on diagonal k, searching back
	odd := delta%2 != 0
	fwd[off+1] = 0
	bwd[off+delta-1] = n
	for D := 0; D <= (max+1)/2; D++ {
		for k := D; k >= -D; k -= 2 {
			var x int
			if k == -D || (k != D && fwd[off+k-1] < fwd[off+k+1]) {
				x = fwd[off+k+1]
			} else {
				x = fwd[off+k-1] + 1
			}
			x0, y0 := x, x-k
			y := y0
			for x < n && y < m && d.a[aLo+x] == d.b[bLo+y] {
				x++
				y++
			}
			fwd[off+k] = x
			if odd && k >= delta-(D-1) && k <= delta+(D-1) && x >= bwd[off+k] {
				return aLo + x0, bLo + y0, aLo + x, bLo + y
			}
		}
		for k := delta + D; k >= delta-D; k -= 2 {
			var x int
			if k == delta+D || (k != delta-D && bwd[off+k-1] < bwd[off+k+1]) {
				x = bwd[off+k-1]
			} else {
				x = bwd[off+k+1] - 1
			}
			x0, y0 := x, x-k
			y := y0
			for x > 0 && y > 0 && d.a[aLo+x-1] == d.b[bLo+y-1] {
				x--
				y--
			}
			bwd[off+k] = x
			if !odd && k >= -D && k <= D && x <= fwd[off+k] {
				return aLo + x0, bLo + y0, aLo + x, bLo + y
			}
		}
		if D >= searchCostLimit {
			if n+m > giveUpVerifiedSize {
				d.declined = true
			}
			return d.giveUpSplit(D, n, m, delta, off, fwd, bwd, aLo, bLo)
		}
	}
	return aLo + n, bLo + m, aLo + n, bLo + m
}

// searchCostLimit bounds the search like host git's default (non
// --minimal) mode: once a middle-snake search has run this many rounds
// without the passes meeting, it stops and splits at the furthest point
// either pass has reached (giveUpSplit). Established black-box on
// boxes of up to giveUpVerifiedSize lines (both sides together); beyond
// that host git splits differently, so a give-up there marks the diff
// declined and callers defer to host git.
const (
	searchCostLimit    = 256
	giveUpVerifiedSize = 64000
)

// giveUpSplit picks the split for a search that hit searchCostLimit: the
// forward point with the largest x+y (the highest diagonal on ties) or
// the backward point with the smallest x+y (likewise), whichever got
// further from its corner; ties go to the backward point.
func (d *lineDiff) giveUpSplit(D, n, m, delta, off int, fwd, bwd []int, aLo, bLo int) (int, int, int, int) {
	fx, fy, fbest := 0, 0, -1
	for k := D; k >= -D; k -= 2 {
		x := min(fwd[off+k], n)
		if y := x - k; y >= 0 && y <= m && x+y > fbest {
			fx, fy, fbest = x, y, x+y
		}
	}
	bx, by, bbest := n, m, n+m+1
	for k := delta + D; k >= delta-D; k -= 2 {
		x := max(bwd[off+k], 0)
		if y := x - k; y >= 0 && y <= m && x+y < bbest {
			bx, by, bbest = x, y, x+y
		}
	}
	if fbest > n+m-bbest {
		return aLo + fx, bLo + fy, aLo + fx, bLo + fy
	}
	return aLo + bx, bLo + by, aLo + bx, bLo + by
}

// compact slides each group of changed lines in x (ids xs, flags cx)
// over equal neighbors: groups that can merge do, a group that can line
// up with a change in the other file does, and otherwise the indent
// heuristic (when on) or the lowest position decides.
func (d *lineDiff) compact(xs []int, cx []bool, xl []string, co []bool) {
	n := len(xs)
	// Positions of the other file's unchanged lines: the u-th unchanged
	// line of x pairs with the u-th unchanged line of the other file.
	var oUnchanged []int
	for i, c := range co {
		if !c {
			oUnchanged = append(oUnchanged, i)
		}
	}
	otherChangedBefore := func(u int) bool {
		k := len(co)
		if u < len(oUnchanged) {
			k = oUnchanged[u]
		}
		return k > 0 && co[k-1]
	}
	unchangedBefore := func(p int) int {
		u := 0
		for i := 0; i < p; i++ {
			if !cx[i] {
				u++
			}
		}
		return u
	}

	start := 0
	for {
		for start < n && !cx[start] {
			start++
		}
		if start >= n {
			return
		}
		end := start
		for end < n && cx[end] {
			end++
		}
		var earliestEnd, endMatchingOther int
		for {
			size := end - start
			for start > 0 && xs[start-1] == xs[end-1] {
				start--
				end--
				cx[start] = true
				cx[end] = false
				for start > 0 && cx[start-1] {
					start--
				}
			}
			earliestEnd = end
			endMatchingOther = -1
			if otherChangedBefore(unchangedBefore(end)) {
				endMatchingOther = end
			}
			for end < n && xs[start] == xs[end] {
				cx[start] = false
				cx[end] = true
				start++
				end++
				for end < n && cx[end] {
					end++
				}
				if otherChangedBefore(unchangedBefore(end)) {
					endMatchingOther = end
				}
			}
			if end-start == size {
				break
			}
		}
		target := end
		switch {
		case end == earliestEnd:
		case endMatchingOther != -1:
			target = endMatchingOther
		case d.indent:
			target = bestSliderEnd(xl, cx, start, end, earliestEnd)
		}
		for end > target {
			start--
			end--
			cx[start] = true
			cx[end] = false
		}
		start = end
	}
}

// unified renders a and b's changes as unified diff hunks with ctx lines
// of context, the way git prints them (without file headers).
func (d *lineDiff) unified(ctx int, funcs func(hunkStart int) string) string {
	type change struct{ i0, i1, j0, j1 int }
	var changes []change
	i, j := 0, 0
	for i < len(d.ca) || j < len(d.cb) {
		if (i < len(d.ca) && d.ca[i]) || (j < len(d.cb) && d.cb[j]) {
			c := change{i0: i, j0: j}
			for i < len(d.ca) && d.ca[i] {
				i++
			}
			for j < len(d.cb) && d.cb[j] {
				j++
			}
			c.i1, c.j1 = i, j
			changes = append(changes, c)
			continue
		}
		i++
		j++
	}
	var b strings.Builder
	for k := 0; k < len(changes); {
		first := changes[k]
		last := k
		for last+1 < len(changes) && changes[last+1].i0-changes[last].i1 <= 2*ctx {
			last++
		}
		lastC := changes[last]
		s1 := max(first.i0-ctx, 0)
		s2 := max(first.j0-ctx, 0)
		e1 := min(lastC.i1+ctx, len(d.ca))
		e2 := min(lastC.j1+ctx, len(d.cb))
		b.WriteString("@@ -" + hunkRange(s1, e1-s1) + " +" + hunkRange(s2, e2-s2) + " @@")
		if funcs != nil {
			if f := funcs(s1); f != "" {
				b.WriteString(" " + f)
			}
		}
		b.WriteByte('\n')
		ai, bj := s1, s2
		for c := k; c <= last; c++ {
			ch := changes[c]
			for ai < ch.i0 {
				writeDiffLine(&b, ' ', d.la[ai])
				ai++
				bj++
			}
			for ; ai < ch.i1; ai++ {
				writeDiffLine(&b, '-', d.la[ai])
			}
			for ; bj < ch.j1; bj++ {
				writeDiffLine(&b, '+', d.lb[bj])
			}
		}
		for ai < e1 {
			writeDiffLine(&b, ' ', d.la[ai])
			ai++
		}
		k = last + 1
	}
	return b.String()
}

func hunkRange(start, count int) string {
	if count == 1 {
		return strconv.Itoa(start + 1)
	}
	if count == 0 {
		return strconv.Itoa(start) + ",0"
	}
	return strconv.Itoa(start+1) + "," + strconv.Itoa(count)
}

func writeDiffLine(b *strings.Builder, mark byte, line string) {
	b.WriteByte(mark)
	b.WriteString(line)
	if !strings.HasSuffix(line, "\n") {
		b.WriteString("\n\\ No newline at end of file\n")
	}
}

// lineIndent is the column of the first non-blank character (tabs to
// the next multiple of 8), or -1 for a blank line.
func lineIndent(s string) int {
	col := 0
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case ' ':
			col++
		case '\t':
			col += 8 - col%8
		case '\n', '\r', '\f', '\v':
		default:
			return col
		}
	}
	return -1
}

// The slider model. Where a group of changed lines can sit at several
// equivalent positions, host git (with its default indent heuristic)
// picks one by looking at the lines around each candidate's two edges.
// This model was established black-box against host git
// (linediff_oracle_test.go) and is consistent with every observed
// decision in the fitting corpus:
//
//   - only positions within size+1 lines of the lowest one are tried;
//   - they are scanned top to bottom, a candidate replacing the best so
//     far when it compares <= 0 (ties go to the lower position);
//   - two candidates compare by indentSignWeight times the sign of the
//     difference of their summed edge indents, plus the difference of
//     their summed edge penalties;
//   - an edge's indent is that of the line after the split, of the next
//     non-blank line when that line is blank, and -1 when only blank
//     lines or the end of file follow;
//   - an edge's penalty is a weighted sum of the features
//     sliderEdgeFeatures reports: the blank runs around the split, and
//     whether the indent rises, holds or falls into the split line (a
//     fall further split by whether the following line goes back in).
const indentSignWeight = 446

var sliderPenaltyWeights = map[string]int{
	"down:b":          101,
	"down:b:dedent":   50,
	"down:b:outdent":  51,
	"down:nb":         134,
	"down:nb:dedent":  65,
	"down:nb:outdent": 69,
	"eof":             24,
	"post=0":          13,
	"post=1":          3,
	"post=2":          -5,
	"post=3":          -11,
	"postBlankN":      -39,
	"pre=0":           9,
	"pre=1":           2,
	"pre=2":           -2,
	"pre=3":           -9,
	"preBlankN":       -88,
	"preNone":         30,
	"same:b":          27,
	"same:nb":         22,
	"sof":             3,
	"totBlankN":       -127,
	"up:b":            97,
	"up:nb":           -7,
}

// edgeFacts are the observations around a split between lines[s-1] and
// lines[s].
type edgeFacts struct {
	sof, eof              bool
	at                    int // indent of lines[s]; -1 when blank or at end of file
	preBlank, preIndent   int // blank lines just before s; nearest non-blank indent before
	postBlank, postIndent int // blank lines just after lines[s]; nearest non-blank indent after s
}

func factsAt(lines []string, s int) edgeFacts {
	f := edgeFacts{sof: s == 0, eof: s >= len(lines), at: -1, preIndent: -1, postIndent: -1}
	if s < len(lines) {
		f.at = lineIndent(lines[s])
	}
	for i := s - 1; i >= 0; i-- {
		if ind := lineIndent(lines[i]); ind >= 0 {
			f.preIndent = ind
			break
		}
		f.preBlank++
	}
	for i := s + 1; i < len(lines); i++ {
		if ind := lineIndent(lines[i]); ind >= 0 {
			f.postIndent = ind
			break
		}
		f.postBlank++
	}
	return f
}

// edgeIndent is an edge's contribution to a candidate's indent sum.
func edgeIndent(f edgeFacts) int {
	switch {
	case f.eof:
		return -1
	case f.at >= 0:
		return f.at
	case f.postIndent >= 0:
		return f.postIndent
	}
	return -1
}

// sliderEdgeFeatures names the categorical features of an edge.
func sliderEdgeFeatures(f edgeFacts, add func(string, int)) {
	if f.sof {
		add("sof", 1)
	}
	if f.eof {
		add("eof", 1)
	}
	post := 0
	if f.at < 0 && !f.eof {
		post = 1 + f.postBlank
	}
	tot := f.preBlank + post
	add("preBlankN", f.preBlank)
	add("postBlankN", post)
	add("totBlankN", tot)
	add("pre="+strconv.Itoa(min(f.preBlank, 3)), 1)
	add("post="+strconv.Itoa(min(post, 3)), 1)
	eff := f.at
	if eff < 0 {
		eff = f.postIndent
	}
	if eff < 0 {
		return
	}
	if f.preIndent < 0 {
		add("preNone", 1)
		return
	}
	any := "nb"
	if tot > 0 {
		any = "b"
	}
	switch {
	case eff > f.preIndent:
		add("up:"+any, 1)
	case eff == f.preIndent:
		add("same:"+any, 1)
	default:
		// A dedent into the split line is weighed by whether the line
		// after it goes back in (an outdent inside a block) or not.
		add("down:"+any, 1)
		if f.at >= 0 && f.postIndent > eff {
			add("down:"+any+":outdent", 1)
		} else {
			add("down:"+any+":dedent", 1)
		}
	}
}

type sliderScore struct{ indent, penalty int }

func sliderCandidate(lines []string, top, bottom int) sliderScore {
	var sc sliderScore
	for _, s := range []int{top, bottom} {
		f := factsAt(lines, s)
		sc.indent += edgeIndent(f)
		sliderEdgeFeatures(f, func(k string, v int) { sc.penalty += sliderPenaltyWeights[k] * v })
	}
	return sc
}

func compareSlider(a, b sliderScore) int {
	sign := 0
	switch {
	case a.indent > b.indent:
		sign = 1
	case a.indent < b.indent:
		sign = -1
	}
	return indentSignWeight*sign + a.penalty - b.penalty
}

// bestSliderEnd picks where a slidable group of changed lines in x
// should end, between earliestEnd and end (see the slider model above).
func bestSliderEnd(lines []string, cx []bool, start, end, earliestEnd int) int {
	size := end - start
	lo := max(earliestEnd, end-size-1)
	best := lo
	bestScore := sliderCandidate(lines, lo-size, lo)
	for e := lo + 1; e <= end; e++ {
		sc := sliderCandidate(lines, e-size, e)
		if compareSlider(sc, bestScore) <= 0 {
			best, bestScore = e, sc
		}
	}
	return best
}

// funcNamer returns host git's default hunk-header function context for
// a hunk whose first shown pre-image line is s1: the nearest line above
// it, searching no further than the previous hunk's start, that
// defFuncName accepts; a hunk with no match reuses the previous one's.
func funcNamer(old []string) func(s1 int) string {
	limit, fn := -1, ""
	return func(s1 int) string {
		for ln := s1 - 1; ln > limit && ln >= 0 && ln < len(old); ln-- {
			if f, ok := defFuncName(strings.TrimSuffix(old[ln], "\n")); ok {
				fn = f
				break
			}
		}
		limit = s1 - 1
		return fn
	}
}

func abs(x int) int {
	if x < 0 {
		return -x
	}
	return x
}

// compareDiscarding runs the search only over the lines it keeps, then
// maps the result back onto the full files. Established black-box
// against host git (linediff_oracle_test.go):
//
//   - a line with no equal in the other file is changed in every
//     alignment and is dropped;
//   - a line with at least frequentLimit(len(own file)) equals in the
//     other file is "frequent"; a frequent line is dropped too when,
//     scanning outward from it through dropped-unique and frequent lines
//     up to the nearest ordinary line (at most scanLimit lines each way),
//     unique lines lie on both sides
//     and number at least 3 x (frequent lines met, itself included) + 4;
//   - everything else is kept.
//
// Dropping such lines changes which shortest script the search meets
// first, and lets a frequent line inside a changed region stay changed.
func (d *lineDiff) compareDiscarding() {
	cntA, cntB := map[int]int{}, map[int]int{}
	for _, v := range d.a {
		cntA[v]++
	}
	for _, v := range d.b {
		cntB[v]++
	}
	// The common prefix and suffix match outright; classification and its
	// scans see only the middle (their edges act as file edges), while
	// counts and limits come from the whole files.
	lo := 0
	for lo < len(d.a) && lo < len(d.b) && d.a[lo] == d.b[lo] {
		lo++
	}
	hiA, hiB := len(d.a), len(d.b)
	for hiA > lo && hiB > lo && d.a[hiA-1] == d.b[hiB-1] {
		hiA--
		hiB--
	}
	ka := keptLines(d.a[lo:hiA], cntB, frequentLimit(len(d.a)), d.ca[lo:hiA])
	kb := keptLines(d.b[lo:hiB], cntA, frequentLimit(len(d.b)), d.cb[lo:hiB])
	sub := &lineDiff{a: make([]int, len(ka)), b: make([]int, len(kb))}
	for i, p := range ka {
		sub.a[i] = d.a[lo+p]
	}
	for j, p := range kb {
		sub.b[j] = d.b[lo+p]
	}
	sub.ca = make([]bool, len(ka))
	sub.cb = make([]bool, len(kb))
	sub.compare(0, len(ka), 0, len(kb))
	d.declined = sub.declined
	for i, c := range sub.ca {
		if c {
			d.ca[lo+ka[i]] = true
		}
	}
	for j, c := range sub.cb {
		if c {
			d.cb[lo+kb[j]] = true
		}
	}
}

// frequentLimit is the match count at which a line of a file with n
// lines counts as frequent: 2^(floor(log4 n)+1).
func frequentLimit(n int) int {
	lim := 1
	for ; n > 0; n >>= 2 {
		lim <<= 1
	}
	return lim
}

// scanDropped walks from i in direction step over unique (1) and
// frequent (2) lines until an ordinary one, the trimmed edge (the caller
// has removed the common prefix and suffix) or scanLimit lines, and
// returns the unique and frequent lines met.
func scanDropped(class []int, i, step int) (uniq, freq int) {
	for j, n := i+step, 0; j >= 0 && j < len(class) && class[j] != 0 && n < scanLimit; j, n = j+step, n+1 {
		if class[j] == 1 {
			uniq++
		} else {
			freq++
		}
	}
	return uniq, freq
}

// scanLimit caps each scanDropped walk; host git looks no further.
const scanLimit = 100

// keptLines classifies xs against the other file's counts, marks the
// dropped lines changed, and returns the positions it keeps.
func keptLines(xs []int, other map[int]int, limit int, cx []bool) []int {
	const (
		keep = iota
		unique
		frequent
	)
	class := make([]int, len(xs))
	for i, v := range xs {
		switch n := other[v]; {
		case n == 0:
			class[i] = unique
		case n >= limit:
			class[i] = frequent
		}
	}
	var kept []int
	for i, c := range class {
		drop := c == unique
		if c == frequent {
			u0, f0 := scanDropped(class, i, -1)
			u1, f1 := scanDropped(class, i, +1)
			drop = u0 > 0 && u1 > 0 && u0+u1 >= 3*(1+f0+f1)+4
		}
		if drop {
			cx[i] = true
		} else {
			kept = append(kept, i)
		}
	}
	return kept
}
