// Package seedfit fits one composite coding ability θ per model from the
// public benchmark matrix, the seed half of the band ladder (design of
// record: dhnt docs/bashy-band-ladder-design.md §4).
//
// The model is a 2-parameter item-response fit over reported proportions:
//
//	score(m,b) = sigmoid(a_b·θ_m − d_b + h_harness + s_source)
//
// fitted by regularised maximum likelihood with a quasi-binomial likelihood
// (each reported proportion is a fractional outcome; the dispersion φ is
// re-estimated from Pearson residuals, so the data decides how much one
// reported score is worth). The optimiser is Levenberg–Marquardt on the
// exact Hessian from a fixed start, so reruns are byte-identical.
//
// Missing cells are skipped, never imputed. Saturated items get a low a_b
// on their own. The harness and source offsets absorb the gap between
// self-reports and independent runs. θ is standardised (mean 0, sd 1 over
// models) and mapped to a seed Glicko `code` rating. Public data seats
// L1–L3 only.
package seedfit

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"math"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// Constants of the fit and of the seed rating. They are printed in the
// output header, so a change here is visible in every report.
const (
	// CodeRatingBase and CodeRatingPerTheta map θ to the seed Glicko code
	// rating: rating = 1500 + 200·θ.
	CodeRatingBase     = 1500.0
	CodeRatingPerTheta = 200.0
	// RDMin and RDMax clamp the seed rating deviation, RD = 200·se(θ).
	RDMin = 60.0
	RDMax = 350.0

	// ScoreClampLo and ScoreClampHi keep proportions off 0 and 1.
	ScoreClampLo = 0.005
	ScoreClampHi = 0.995

	// Z90 is the two-sided 90% normal quantile for the Laplace interval.
	Z90 = 1.6448536269514722

	// Prior standard deviations (weak Gaussian priors).
	PriorSDTheta      = 1.0 // θ_m ~ N(0, 1)
	PriorSDLogA       = 0.5 // log a_b ~ N(0, 0.5)
	PriorSDDifficulty = 3.0 // d_b ~ N(0, 3), only to keep one-sided items finite
	PriorSDOffset     = 0.5 // harness and source offsets ~ N(0, 0.5)

	// Dispersion: start value, bounds and re-estimation rounds.
	DispersionInit   = 0.02
	DispersionMin    = 1e-4
	DispersionMax    = 1.0
	DispersionRounds = 10

	// MinModelsPerItem: an item seen for fewer models carries no
	// information about θ (its d_b absorbs it) and is skipped.
	MinModelsPerItem = 2

	// Reference levels fixed at 0 for identifiability.
	ReferenceHarness = HarnessIndependent
	ReferenceSource  = "official-board"
)

// CapabilityAreas is the set of matrix capability_area values the fit keeps.
// The matrix has twelve areas; θ is a coding ability, so only the two whose
// benchmarks score coding or agentic tool use are kept:
//
//	coding-debug-test   kept  (SWE-bench family, Terminal-Bench, DeepSWE, …)
//	tool-use-agentic    kept  (MCP-Atlas, Toolathlon, τ-bench, …)
//	computer-use, orchestration-workflow, architect-design-plan,
//	judge-review-verify, research-browse, security, reasoning-knowledge,
//	math, multimodal, other                                  dropped
//
// Managing and judging are measured by the ladder itself, not seeded.
var CapabilityAreas = []string{"coding-debug-test", "tool-use-agentic"}

// Skip reasons, the keys of a skip report.
const (
	SkipArea          = "area-not-selected"
	SkipNoModel       = "no-fleet-model"
	SkipMissing       = "missing-score"
	SkipRatingScale   = "rating-scale"
	SkipIndexNoRange  = "index-no-range"
	SkipLowerBetter   = "lower-is-better"
	SkipMultiMetric   = "multi-metric"
	SkipNotProportion = "not-proportion"
	SkipUnparseable   = "unparseable"
	SkipMultiValue    = "multi-value"
	SkipApproximate   = "approximate"
	SkipValueRange    = "value-range"
	SkipOutOfRange    = "out-of-range"
	SkipSingleModel   = "item-single-model"
)

var skipDescriptions = map[string]string{
	SkipArea:          "capability area outside the kept set",
	SkipNoModel:       "row has no fleet_model",
	SkipMissing:       "score cell empty",
	SkipRatingScale:   "Elo / arena rating, not a proportion",
	SkipIndexNoRange:  "index without a stated range",
	SkipLowerBetter:   "lower-is-better metric",
	SkipMultiMetric:   "several metrics in one cell",
	SkipNotProportion: "unit is not a proportion or percentage",
	SkipUnparseable:   "score not a number",
	SkipMultiValue:    "several values in one cell",
	SkipApproximate:   "approximate value (~)",
	SkipValueRange:    "value range, not a point",
	SkipOutOfRange:    "value outside the stated scale",
	SkipSingleModel:   "item reported for fewer than 2 models",
}

// Harness classes. The mapping from the free-text agent_tool_or_harness
// column is by these rules, first match wins:
//
//	class           rule (on the lower-cased label)
//	none            word "none", starts with "direct", or "no agent" (bare API)
//	unstated        empty, "?", "standard", word "unstated", or "not stated"
//	vendor-internal word "internal", "vendor" or "native"; "self-reported";
//	                "best reported" (the model vendor's own scaffold)
//	agent-cli       contains a vendor_cli value from the matrix itself
//	                (a fleet agent CLI, whoever's model it runs)
//	vendor-internal contains a vendor value from the matrix (that vendor's
//	                own scaffold, e.g. "<vendor> scaffold")
//	independent     anything else: a named third-party harness or board
//	                scaffold (the reference level, fixed at 0)
//
// The agent-cli and vendor token lists are the matrix's own vendor_cli and
// vendor columns, so no product or vendor name is written here. The output prints the full raw → class table.
const (
	HarnessNone           = "none"
	HarnessUnstated       = "unstated"
	HarnessVendorInternal = "vendor-internal"
	HarnessAgentCLI       = "agent-cli"
	HarnessIndependent    = "independent"
)

var (
	nonAlnum       = regexp.MustCompile(`[^a-z0-9]+`)
	nonAlnumHyphen = regexp.MustCompile(`[^a-z0-9-]+`)
	ratingWords    = regexp.MustCompile(`\b(elo|arena)\b`)
	pointScore     = regexp.MustCompile(`^([-+]?[0-9]+(?:\.[0-9]+)?)\s*(?:\(\s*±\s*[0-9.]+\s*\)|±\s*[0-9.]+)?$`)
	rangeScore     = regexp.MustCompile(`^[0-9]+(?:\.[0-9]+)?\s*[-–]\s*[0-9]+(?:\.[0-9]+)?$`)
	notAvailable   = regexp.MustCompile(`^n/?a\b`)
	versionPrefix  = regexp.MustCompile(`\bv([0-9])`)
	spaces         = regexp.MustCompile(`\s+`)
	itemTokenSplit = regexp.MustCompile(`[^a-z0-9@^.]+`)
)

// scaleSplits are variant words that change what is measured (a different
// split or subset, or pass@k with k>1). Every other variant word — effort,
// runner, task count, "overall", "public" — describes the run, not the
// scale, and is absorbed by the offsets, so the item key ignores it.
var scaleSplits = map[string]bool{
	"extended": true, "main": true, "hard": true, "full": true,
	"commercial": true, "private": true, "subset": true, "dominance": true,
	"retail": true, "airline": true, "telecom": true, "banking": true,
	"pass@3": true, "pass^3": true,
}

func sigmoid(x float64) float64 {
	if x >= 0 {
		return 1 / (1 + math.Exp(-x))
	}
	e := math.Exp(x)
	return e / (1 + e)
}

func softplus(x float64) float64 {
	if x > 0 {
		return x + math.Log1p(math.Exp(-x))
	}
	return math.Log1p(math.Exp(x))
}

// ParseScore turns a (metric, score) cell into a proportion in
// [ScoreClampLo, ScoreClampHi], or returns the skip reason.
func ParseScore(metric, score string) (float64, string) {
	s := strings.TrimSpace(score)
	if s == "" {
		return 0, SkipMissing
	}
	m := strings.ToLower(metric)
	switch {
	case ratingWords.MatchString(m):
		return 0, SkipRatingScale
	case strings.Contains(m, "lower better") || strings.Contains(m, "lower is better"):
		return 0, SkipLowerBetter
	case strings.Contains(m, ";"):
		return 0, SkipMultiMetric
	}
	var scale float64
	switch {
	case strings.Contains(m, "%"):
		scale = 100
	case strings.Contains(m, "0-1"):
		scale = 1
	case strings.Contains(m, "index"):
		return 0, SkipIndexNoRange
	default:
		return 0, SkipNotProportion
	}
	ls := strings.ToLower(s)
	sm := pointScore.FindStringSubmatch(s)
	if sm == nil {
		switch {
		case notAvailable.MatchString(ls):
			return 0, SkipUnparseable
		case strings.HasPrefix(ls, "~") || strings.HasPrefix(ls, "≈") || strings.HasPrefix(ls, "approx"):
			return 0, SkipApproximate
		case rangeScore.MatchString(ls):
			return 0, SkipValueRange
		case strings.Contains(ls, "->") || strings.Contains(ls, "/"):
			return 0, SkipMultiValue
		}
		return 0, SkipUnparseable
	}
	var v float64
	if _, err := fmt.Sscanf(sm[1], "%g", &v); err != nil {
		return 0, SkipUnparseable
	}
	if v < 0 || v > scale {
		return 0, SkipOutOfRange
	}
	v /= scale
	return math.Min(ScoreClampHi, math.Max(ScoreClampLo, v)), ""
}

// HarnessClass maps a raw agent_tool_or_harness label to its class (see the
// table on the Harness* constants). clis and vendors are the sets of
// lower-cased vendor_cli and vendor values found in the matrix.
func HarnessClass(raw string, clis, vendors map[string]bool) string {
	l := strings.ToLower(strings.TrimSpace(raw))
	words := map[string]bool{}
	for _, w := range nonAlnum.Split(l, -1) {
		words[w] = true
	}
	for _, w := range nonAlnumHyphen.Split(l, -1) {
		words[w] = true
	}
	switch {
	case words["none"] || strings.HasPrefix(l, "direct") || strings.Contains(l, "no agent"):
		return HarnessNone
	case l == "" || l == "?" || l == "standard" || words["unstated"] || strings.Contains(l, "not stated"):
		return HarnessUnstated
	case words["internal"] || words["vendor"] || words["native"] ||
		strings.Contains(l, "self-reported") || strings.Contains(l, "best reported"):
		return HarnessVendorInternal
	}
	for w := range words {
		if w != "" && clis[w] {
			return HarnessAgentCLI
		}
	}
	for w := range words {
		if w != "" && vendors[w] {
			return HarnessVendorInternal
		}
	}
	return HarnessIndependent
}

func normName(s string) string {
	s = strings.ToLower(s)
	s = strings.NewReplacer("-", " ", "_", " ", "(", " ", ")", " ", ",", " ").Replace(s)
	s = versionPrefix.ReplaceAllString(s, "$1")
	return strings.TrimSpace(spaces.ReplaceAllString(s, " "))
}

// ItemKey is the item identity: the normalised benchmark name plus any
// variant words that change the scale (scaleSplits), in sorted order.
func ItemKey(benchmark, variant string) string {
	key := normName(benchmark)
	have := map[string]bool{}
	for _, t := range itemTokenSplit.Split(key, -1) {
		have[t] = true
	}
	var extra []string
	for _, t := range itemTokenSplit.Split(strings.ToLower(variant), -1) {
		if scaleSplits[t] && !have[t] {
			have[t] = true
			extra = append(extra, t)
		}
	}
	sort.Strings(extra)
	if len(extra) > 0 {
		key += " " + strings.Join(extra, " ")
	}
	return key
}

// Obs is one usable observation.
type Obs struct {
	Model, Vendor, Item string
	Harness, HarnessRaw string
	Source              string
	Y                   float64
	Line                int
}

// Prepared is the matrix reduced to observations plus the skip report.
type Prepared struct {
	Obs   []Obs
	Skips map[string]int
}

// Prepare filters and normalises matrix rows into observations.
func Prepare(rows []Row) Prepared {
	areas := map[string]bool{}
	for _, a := range CapabilityAreas {
		areas[a] = true
	}
	clis, vendors := map[string]bool{}, map[string]bool{}
	for _, r := range rows {
		if c := strings.ToLower(strings.TrimSpace(r.VendorCLI)); c != "" {
			clis[c] = true
		}
		if v := strings.ToLower(strings.TrimSpace(r.Vendor)); v != "" {
			vendors[v] = true
		}
	}
	p := Prepared{Skips: map[string]int{}}
	var obs []Obs
	for _, r := range rows {
		if !areas[r.Area] {
			p.Skips[SkipArea]++
			continue
		}
		if r.Model == "" {
			p.Skips[SkipNoModel]++
			continue
		}
		y, reason := ParseScore(r.Metric, r.Score)
		if reason != "" {
			p.Skips[reason]++
			continue
		}
		src := strings.ToLower(strings.TrimSpace(r.SourceType))
		if src == "" {
			src = "unstated"
		}
		obs = append(obs, Obs{
			Model: r.Model, Vendor: r.Vendor, Item: ItemKey(r.Benchmark, r.Variant),
			Harness: HarnessClass(r.Harness, clis, vendors), HarnessRaw: r.Harness,
			Source: src, Y: y, Line: r.Line,
		})
	}
	models := map[string]map[string]bool{}
	for _, o := range obs {
		if models[o.Item] == nil {
			models[o.Item] = map[string]bool{}
		}
		models[o.Item][o.Model] = true
	}
	for _, o := range obs {
		if len(models[o.Item]) < MinModelsPerItem {
			p.Skips[SkipSingleModel]++
			continue
		}
		p.Obs = append(p.Obs, o)
	}
	return p
}

// ModelEstimate is one model's seed.
type ModelEstimate struct {
	Model, Vendor string
	Theta, SE     float64 // standardised θ and its Laplace standard error
	Lo, Hi        float64 // 90% interval
	N             int
	Rating, RD    float64
	Placement     string
	Adjusted      bool    // moved by an order constraint
	ThetaFit      float64 // θ before any order projection
}

// ItemEstimate is one item's fitted parameters (on the standardised θ scale).
type ItemEstimate struct {
	Key        string
	A, D       float64
	N, Models  int
	MeanScore  float64
	MinS, MaxS float64
}

// OffsetEstimate is one harness-class or source-type offset.
type OffsetEstimate struct {
	Kind, Level string
	Value       float64
	N           int
	Reference   bool
}

// HarnessMapping records how one raw harness label was classed.
type HarnessMapping struct {
	Raw, Class string
	N          int
}

// Adjustment reports one model of an order chain.
type Adjustment struct {
	Vendor, Generation, Model string
	From, To                  float64
	Applied                   bool
	Note                      string
}

// Result is the whole fit.
type Result struct {
	Models     []ModelEstimate
	Items      []ItemEstimate
	Offsets    []OffsetEstimate
	Harness    []HarnessMapping
	Skips      map[string]int
	Order      []Adjustment
	OrderGiven bool
	NObs       int
	Params     int
	Phi        float64
	Iterations int
	Converged  bool
}

// CodeRating maps θ to the seed Glicko code rating.
func CodeRating(theta float64) float64 { return CodeRatingBase + CodeRatingPerTheta*theta }

// CodeRD maps se(θ) to a seed rating deviation, clamped to [RDMin, RDMax].
func CodeRD(se float64) float64 {
	return math.Min(RDMax, math.Max(RDMin, CodeRatingPerTheta*se))
}

// problem is the indexed fit problem.
type problem struct {
	obs              []Obs
	models, items    []string
	harness, sources []string
	om, ob, oh, os   []int // per-obs indices into the level lists
	hFree, sFree     []int // level → parameter index, -1 for the reference
	nM, nB, P        int
}

func levels(obs []Obs, f func(Obs) string) ([]string, map[string]int) {
	set := map[string]bool{}
	for _, o := range obs {
		set[f(o)] = true
	}
	var out []string
	for k := range set {
		out = append(out, k)
	}
	sort.Strings(out)
	idx := map[string]int{}
	for i, k := range out {
		idx[k] = i
	}
	return out, idx
}

func newProblem(obs []Obs) *problem {
	p := &problem{obs: obs}
	var mi, bi, hi, si map[string]int
	p.models, mi = levels(obs, func(o Obs) string { return o.Model })
	p.items, bi = levels(obs, func(o Obs) string { return o.Item })
	p.harness, hi = levels(obs, func(o Obs) string { return o.Harness })
	p.sources, si = levels(obs, func(o Obs) string { return o.Source })
	p.nM, p.nB = len(p.models), len(p.items)
	p.P = p.nM + 2*p.nB
	assign := func(lv []string, ref string) []int {
		r := ref
		if _, ok := indexOf(lv, ref); !ok {
			r = lv[0]
		}
		free := make([]int, len(lv))
		for i, l := range lv {
			if l == r {
				free[i] = -1
				continue
			}
			free[i] = p.P
			p.P++
		}
		return free
	}
	p.hFree = assign(p.harness, ReferenceHarness)
	p.sFree = assign(p.sources, ReferenceSource)
	for _, o := range obs {
		p.om = append(p.om, mi[o.Model])
		p.ob = append(p.ob, bi[o.Item])
		p.oh = append(p.oh, hi[o.Harness])
		p.os = append(p.os, si[o.Source])
	}
	return p
}

func indexOf(xs []string, x string) (int, bool) {
	for i, v := range xs {
		if v == x {
			return i, true
		}
	}
	return -1, false
}

func (p *problem) theta(i int) int { return i }
func (p *problem) alpha(b int) int { return p.nM + b }
func (p *problem) diff(b int) int  { return p.nM + p.nB + b }

func (p *problem) eta(x []float64, i int) (eta, a, th float64) {
	b := p.ob[i]
	a = math.Exp(x[p.alpha(b)])
	th = x[p.theta(p.om[i])]
	eta = a*th - x[p.diff(b)]
	if j := p.hFree[p.oh[i]]; j >= 0 {
		eta += x[j]
	}
	if j := p.sFree[p.os[i]]; j >= 0 {
		eta += x[j]
	}
	return
}

// prior returns the prior variance of parameter j.
func (p *problem) priorVar(j int) float64 {
	switch {
	case j < p.nM:
		return PriorSDTheta * PriorSDTheta
	case j < p.nM+p.nB:
		return PriorSDLogA * PriorSDLogA
	case j < p.nM+2*p.nB:
		return PriorSDDifficulty * PriorSDDifficulty
	}
	return PriorSDOffset * PriorSDOffset
}

// objective returns the negative log posterior at x with observation weight
// w = 1/φ; with grad it also fills g and H (exact Hessian).
func (p *problem) objective(x []float64, w float64, g []float64, H [][]float64) float64 {
	f := 0.0
	for j := range x {
		v := p.priorVar(j)
		f += x[j] * x[j] / (2 * v)
		if g != nil {
			g[j] = x[j] / v
			for k := range H[j] {
				H[j][k] = 0
			}
			H[j][j] = 1 / v
		}
	}
	var idx [5]int
	var jac [5]float64
	for i, o := range p.obs {
		eta, a, th := p.eta(x, i)
		f += w * (softplus(eta) - o.Y*eta)
		if g == nil {
			continue
		}
		pr := sigmoid(eta)
		r := w * (pr - o.Y)
		v := w * pr * (1 - pr)
		b := p.ob[i]
		tm, ta := p.theta(p.om[i]), p.alpha(b)
		n := 0
		add := func(j int, d float64) {
			if j >= 0 {
				idx[n], jac[n] = j, d
				n++
			}
		}
		add(tm, a)
		add(ta, a*th)
		add(p.diff(b), -1)
		add(p.hFree[p.oh[i]], 1)
		add(p.sFree[p.os[i]], 1)
		for u := 0; u < n; u++ {
			g[idx[u]] += r * jac[u]
			for q := 0; q < n; q++ {
				H[idx[u]][idx[q]] += v * jac[u] * jac[q]
			}
		}
		H[tm][ta] += r * a
		H[ta][tm] += r * a
		H[ta][ta] += r * a * th
	}
	return f
}

// cholesky factors A (symmetric) in place into L; false if not positive definite.
func cholesky(A [][]float64) ([][]float64, bool) {
	n := len(A)
	L := make([][]float64, n)
	for i := range L {
		L[i] = make([]float64, n)
	}
	for i := 0; i < n; i++ {
		for j := 0; j <= i; j++ {
			s := A[i][j]
			for k := 0; k < j; k++ {
				s -= L[i][k] * L[j][k]
			}
			if i == j {
				if s <= 0 || math.IsNaN(s) {
					return nil, false
				}
				L[i][i] = math.Sqrt(s)
			} else {
				L[i][j] = s / L[j][j]
			}
		}
	}
	return L, true
}

func cholSolve(L [][]float64, b []float64) []float64 {
	n := len(b)
	y := make([]float64, n)
	for i := 0; i < n; i++ {
		s := b[i]
		for k := 0; k < i; k++ {
			s -= L[i][k] * y[k]
		}
		y[i] = s / L[i][i]
	}
	x := make([]float64, n)
	for i := n - 1; i >= 0; i-- {
		s := y[i]
		for k := i + 1; k < n; k++ {
			s -= L[k][i] * x[k]
		}
		x[i] = s / L[i][i]
	}
	return x
}

func newMatrix(n int) [][]float64 {
	m := make([][]float64, n)
	for i := range m {
		m[i] = make([]float64, n)
	}
	return m
}

// minimise runs Levenberg–Marquardt from x (updated in place).
func (p *problem) minimise(x []float64, w float64) (iters int, converged bool) {
	const (
		maxIter = 2000
		gradTol = 1e-9
	)
	n := len(x)
	g := make([]float64, n)
	H := newMatrix(n)
	A := newMatrix(n)
	xn := make([]float64, n)
	lambda := 1e-3
	f := p.objective(x, w, g, H)
	for iters = 0; iters < maxIter; iters++ {
		gmax := 0.0
		for _, v := range g {
			gmax = math.Max(gmax, math.Abs(v))
		}
		if gmax < gradTol*math.Max(1, w) {
			return iters, true
		}
		accepted := false
		for lambda < 1e12 {
			for i := range H {
				copy(A[i], H[i])
				A[i][i] += lambda * math.Max(1, math.Abs(H[i][i]))
			}
			L, ok := cholesky(A)
			if !ok {
				lambda *= 10
				continue
			}
			neg := make([]float64, n)
			for i := range g {
				neg[i] = -g[i]
			}
			step := cholSolve(L, neg)
			for i := range x {
				xn[i] = x[i] + step[i]
			}
			fn := p.objective(xn, w, nil, nil)
			if fn <= f {
				same := f-fn <= 1e-15*math.Max(1, math.Abs(f))
				copy(x, xn)
				f = p.objective(x, w, g, H)
				lambda = math.Max(lambda/10, 1e-12)
				accepted = true
				if same {
					return iters + 1, true
				}
				break
			}
			lambda *= 10
		}
		if !accepted {
			return iters, false
		}
	}
	return iters, false
}

func (p *problem) pearson(x []float64) float64 {
	s := 0.0
	for i, o := range p.obs {
		eta, _, _ := p.eta(x, i)
		pr := sigmoid(eta)
		s += (o.Y - pr) * (o.Y - pr) / (pr * (1 - pr))
	}
	return s
}

// Fit fits θ, item and offset parameters to the prepared observations.
func Fit(prep Prepared) (*Result, error) {
	if len(prep.Obs) == 0 {
		return nil, errors.New("seedfit: no usable observations")
	}
	p := newProblem(prep.Obs)
	if p.nM < 2 {
		return nil, errors.New("seedfit: need at least 2 models")
	}
	// Fixed start: θ=0, a=1, d_b = −logit(mean score of b), offsets 0.
	x := make([]float64, p.P)
	sum := make([]float64, p.nB)
	cnt := make([]float64, p.nB)
	for i, o := range p.obs {
		sum[p.ob[i]] += o.Y
		cnt[p.ob[i]]++
	}
	for b := range sum {
		m := sum[b] / cnt[b]
		x[p.diff(b)] = -math.Log(m / (1 - m))
	}
	N := float64(len(p.obs))
	dof := math.Max(N-float64(p.P), math.Max(1, N/4))
	phi := DispersionInit
	iters := 0
	for round := 0; round < DispersionRounds; round++ {
		n, _ := p.minimise(x, 1/phi)
		iters += n
		next := math.Min(DispersionMax, math.Max(DispersionMin, p.pearson(x)/dof))
		done := math.Abs(next-phi) <= 1e-4*phi
		phi = next
		if done {
			break
		}
	}
	n, conv := p.minimise(x, 1/phi)
	iters += n
	w := 1 / phi

	// Standardise θ: mean 0, sd 1 over models.
	mu, ss := 0.0, 0.0
	for m := 0; m < p.nM; m++ {
		mu += x[m]
	}
	mu /= float64(p.nM)
	for m := 0; m < p.nM; m++ {
		ss += (x[m] - mu) * (x[m] - mu)
	}
	sd := math.Sqrt(ss / float64(p.nM))
	if sd < 1e-12 {
		sd = 1
	}
	se := p.standardisedSE(x, w, mu, sd)

	res := &Result{
		Skips: prep.Skips, NObs: len(p.obs), Params: p.P,
		Phi: phi, Iterations: iters, Converged: conv,
	}
	nObs := make([]int, p.nM)
	vendors := make([]map[string]int, p.nM)
	for i, o := range p.obs {
		m := p.om[i]
		nObs[m]++
		if vendors[m] == nil {
			vendors[m] = map[string]int{}
		}
		vendors[m][o.Vendor]++
	}
	for m, name := range p.models {
		th := (x[m] - mu) / sd
		res.Models = append(res.Models, ModelEstimate{
			Model: name, Vendor: mostCommon(vendors[m]),
			Theta: th, ThetaFit: th, SE: se[m], Lo: th - Z90*se[m], Hi: th + Z90*se[m],
			N: nObs[m], Rating: CodeRating(th), RD: CodeRD(se[m]),
		})
	}
	sortModels(res.Models)

	itemModels := make([]map[string]bool, p.nB)
	itemN := make([]int, p.nB)
	minS, maxS := make([]float64, p.nB), make([]float64, p.nB)
	for i, o := range p.obs {
		b := p.ob[i]
		if itemModels[b] == nil {
			itemModels[b] = map[string]bool{}
			minS[b], maxS[b] = o.Y, o.Y
		}
		itemModels[b][o.Model] = true
		itemN[b]++
		minS[b], maxS[b] = math.Min(minS[b], o.Y), math.Max(maxS[b], o.Y)
	}
	for b, key := range p.items {
		a := math.Exp(x[p.alpha(b)])
		res.Items = append(res.Items, ItemEstimate{
			Key: key, A: a * sd, D: x[p.diff(b)] - a*mu,
			N: itemN[b], Models: len(itemModels[b]), MeanScore: sum[b] / cnt[b],
			MinS: minS[b], MaxS: maxS[b],
		})
	}
	sort.SliceStable(res.Items, func(i, j int) bool {
		if res.Items[i].A != res.Items[j].A {
			return res.Items[i].A > res.Items[j].A
		}
		return res.Items[i].Key < res.Items[j].Key
	})

	hN, sN := map[string]int{}, map[string]int{}
	raw := map[[2]string]int{}
	for _, o := range p.obs {
		hN[o.Harness]++
		sN[o.Source]++
		raw[[2]string{o.HarnessRaw, o.Harness}]++
	}
	for i, l := range p.harness {
		o := OffsetEstimate{Kind: "harness", Level: l, N: hN[l], Reference: p.hFree[i] < 0}
		if !o.Reference {
			o.Value = x[p.hFree[i]]
		}
		res.Offsets = append(res.Offsets, o)
	}
	for i, l := range p.sources {
		o := OffsetEstimate{Kind: "source", Level: l, N: sN[l], Reference: p.sFree[i] < 0}
		if !o.Reference {
			o.Value = x[p.sFree[i]]
		}
		res.Offsets = append(res.Offsets, o)
	}
	for k, n := range raw {
		res.Harness = append(res.Harness, HarnessMapping{Raw: k[0], Class: k[1], N: n})
	}
	sort.Slice(res.Harness, func(i, j int) bool {
		a, b := res.Harness[i], res.Harness[j]
		if a.Class != b.Class {
			return a.Class < b.Class
		}
		if a.N != b.N {
			return a.N > b.N
		}
		return a.Raw < b.Raw
	})
	return res, nil
}

// standardisedSE is the Laplace standard error of each standardised θ.
// The θ block C of the inverse Hessian still carries the location/scale
// freedom that only the θ prior pins; the standardisation removes it, so C is
// propagated through θ'_m = (θ_m − μ)/σ by the delta method:
// ∂θ'_m/∂θ_k = (δ_mk − 1/M − z_m·z_k/M)/σ. If the Hessian is not positive
// definite, it falls back to the conditional se 1/sqrt(H_mm)/σ.
func (p *problem) standardisedSE(x []float64, w, mu, sd float64) []float64 {
	g := make([]float64, p.P)
	H := newMatrix(p.P)
	p.objective(x, w, g, H)
	M := p.nM
	se := make([]float64, M)
	L, ok := cholesky(H)
	if !ok {
		for m := range se {
			se[m] = 1 / math.Sqrt(math.Max(H[m][m], 1e-12)) / sd
		}
		return se
	}
	C := newMatrix(M)
	for k := 0; k < M; k++ {
		e := make([]float64, p.P)
		e[k] = 1
		col := cholSolve(L, e)
		for m := 0; m < M; m++ {
			C[m][k] = col[m]
		}
	}
	z := make([]float64, M)
	for m := range z {
		z[m] = (x[m] - mu) / sd
	}
	J := make([]float64, M)
	for m := 0; m < M; m++ {
		for k := 0; k < M; k++ {
			J[k] = -1/float64(M) - z[m]*z[k]/float64(M)
			if k == m {
				J[k]++
			}
			J[k] /= sd
		}
		v := 0.0
		for k := 0; k < M; k++ {
			for l := 0; l < M; l++ {
				v += J[k] * C[k][l] * J[l]
			}
		}
		se[m] = math.Sqrt(math.Max(v, 0))
	}
	return se
}

func mostCommon(m map[string]int) string {
	best, bn := "", -1
	for k, n := range m {
		if n > bn || (n == bn && k < best) {
			best, bn = k, n
		}
	}
	return best
}

func sortModels(ms []ModelEstimate) {
	sort.SliceStable(ms, func(i, j int) bool {
		if ms[i].Theta != ms[j].Theta {
			return ms[i].Theta > ms[j].Theta
		}
		return ms[i].Model < ms[j].Model
	})
}

// ApplyOrder projects θ onto each declared within-generation order by
// weighted pool-adjacent-violators (weights 1/se²). A chain constrains only
// its own members; a member whose matrix vendor differs from the chain's
// vendor, or that is not in the fit, is left out and reported.
func (r *Result) ApplyOrder(chains []OrderChain) []Adjustment {
	r.OrderGiven = true
	pos := map[string]int{}
	for i, m := range r.Models {
		pos[m.Model] = i
	}
	var out []Adjustment
	for _, c := range chains {
		var members []int
		for _, name := range c.Models {
			adj := Adjustment{Vendor: c.Vendor, Generation: c.Generation, Model: name}
			i, ok := pos[name]
			switch {
			case !ok:
				adj.Note = "not in fit"
			case r.Models[i].Vendor != c.Vendor:
				adj.Note = fmt.Sprintf("vendor mismatch (matrix vendor %s)", r.Models[i].Vendor)
				adj.From, adj.To = r.Models[i].Theta, r.Models[i].Theta
			default:
				members = append(members, i)
				adj.From = r.Models[i].Theta
			}
			out = append(out, adj)
		}
		vals := make([]float64, len(members))
		ws := make([]float64, len(members))
		for k, i := range members {
			vals[k] = r.Models[i].Theta
			ws[k] = 1
			if se := r.Models[i].SE; se > 0 {
				ws[k] = 1 / (se * se)
			}
		}
		proj := pavNonIncreasing(vals, ws)
		k := 0
		for j := len(out) - len(c.Models); j < len(out); j++ {
			if out[j].Note != "" {
				continue
			}
			i := members[k]
			to := proj[k]
			k++
			out[j].To = to
			if math.Abs(to-r.Models[i].Theta) > 1e-12 {
				m := &r.Models[i]
				d := to - m.Theta
				m.Theta, m.Lo, m.Hi = to, m.Lo+d, m.Hi+d
				m.Rating = CodeRating(to)
				m.Adjusted = true
				out[j].Applied = true
			}
		}
	}
	sortModels(r.Models)
	r.Order = out
	return out
}

// pavNonIncreasing returns the weighted least-squares projection of v onto
// non-increasing sequences.
func pavNonIncreasing(v, w []float64) []float64 {
	type block struct {
		sum, w float64
		n      int
	}
	var bs []block
	for i := range v {
		bs = append(bs, block{v[i] * w[i], w[i], 1})
		for len(bs) >= 2 {
			a, b := bs[len(bs)-2], bs[len(bs)-1]
			if a.sum/a.w >= b.sum/b.w {
				break
			}
			bs = append(bs[:len(bs)-2], block{a.sum + b.sum, a.w + b.w, a.n + b.n})
		}
	}
	out := make([]float64, 0, len(v))
	for _, b := range bs {
		for k := 0; k < b.n; k++ {
			out = append(out, b.sum/b.w)
		}
	}
	return out
}

// Place seats a θ from public data. Lines are θ values; nil means unset.
// Public data seats L1–L3 only: a model above the L3 line is seated L3 and
// flagged as an L4 candidate, never L4 or L5.
func Place(theta float64, l2, l3 *float64) string {
	switch {
	case l3 != nil && theta > *l3:
		return "L3 (L4 candidate)"
	case l2 != nil && theta < *l2:
		return "L1"
	case l2 != nil && l3 != nil:
		return "L2"
	case l2 != nil:
		return "L2 or above"
	case l3 != nil:
		return "unplaced (below L3 line)"
	}
	return "unplaced"
}

// Place fills every model's placement.
func (r *Result) Place(l2, l3 *float64) {
	for i := range r.Models {
		r.Models[i].Placement = Place(r.Models[i].Theta, l2, l3)
	}
}

// fmtF formats a number with fixed decimals and no negative zero.
func fmtF(v float64, dec int) string {
	s := strconv.FormatFloat(v, 'f', dec, 64)
	if strings.Trim(s, "-0.") == "" {
		s = strings.TrimPrefix(s, "-")
	}
	return s
}

func mdCell(s string) string {
	if s == "" {
		return "—"
	}
	return strings.ReplaceAll(s, "|", `\|`)
}

func sortedSkips(sk map[string]int) []string {
	var keys []string
	for k, n := range sk {
		if n > 0 {
			keys = append(keys, k)
		}
	}
	sort.Slice(keys, func(i, j int) bool {
		if sk[keys[i]] != sk[keys[j]] {
			return sk[keys[i]] > sk[keys[j]]
		}
		return keys[i] < keys[j]
	})
	return keys
}

const methodLine = "score(m,b) = sigmoid(a_b·θ_m − d_b + h_harness + s_source), regularised quasi-binomial maximum likelihood (dispersion φ from Pearson residuals), Levenberg–Marquardt on the exact Hessian from a fixed start"

func lineText(v *float64) string {
	if v == nil {
		return "unset"
	}
	return fmtF(*v, 3)
}

// WriteMarkdown renders the full report.
func (r *Result) WriteMarkdown(w io.Writer, l2, l3 *float64) {
	pf := func(format string, a ...any) { fmt.Fprintf(w, format, a...) }
	pf("# Seed fit — composite coding ability (θ)\n\n")
	pf("Method: %s.\n\n", methodLine)
	pf("## Constants\n\n| name | value |\n|---|---|\n")
	pf("| seed code rating | rating = %s + %s·θ |\n", fmtF(CodeRatingBase, 0), fmtF(CodeRatingPerTheta, 0))
	pf("| seed RD | %s·se(θ), clamped [%s, %s] |\n", fmtF(CodeRatingPerTheta, 0), fmtF(RDMin, 0), fmtF(RDMax, 0))
	pf("| interval | 90%% Laplace: θ ± %s·se; se from the inverse Hessian, carried through the standardisation (delta method) |\n", fmtF(Z90, 3))
	pf("| priors | θ ~ N(0,%s), log a_b ~ N(0,%s), d_b ~ N(0,%s), offsets ~ N(0,%s) |\n",
		fmtF(PriorSDTheta, 1), fmtF(PriorSDLogA, 1), fmtF(PriorSDDifficulty, 1), fmtF(PriorSDOffset, 1))
	pf("| score clamp | [%s, %s]; %% scores divided by 100 |\n", fmtF(ScoreClampLo, 3), fmtF(ScoreClampHi, 3))
	pf("| identifiability | θ standardised to mean 0, sd 1 over models; reference levels fixed at 0: harness `%s`, source `%s` |\n", ReferenceHarness, ReferenceSource)
	pf("| capability areas kept | %s |\n", strings.Join(CapabilityAreas, ", "))
	pf("| min models per item | %d |\n", MinModelsPerItem)
	pf("| placement lines (θ) | L2 %s, L3 %s; public data seats L1–L3 only |\n", lineText(l2), lineText(l3))
	pf("| observations / models / items / params | %d / %d / %d / %d |\n", r.NObs, len(r.Models), len(r.Items), r.Params)
	pf("| dispersion φ | %s (one reported score ≈ %s binomial trials) |\n", fmtF(r.Phi, 5), fmtF(1/r.Phi, 1))
	pf("| optimiser | %d Levenberg–Marquardt steps over all dispersion rounds, converged %v |\n\n", r.Iterations, r.Converged)

	pf("## Harness classes\n\n| raw agent_tool_or_harness | class | n |\n|---|---|---|\n")
	for _, h := range r.Harness {
		pf("| %s | %s | %d |\n", mdCell(h.Raw), h.Class, h.N)
	}
	pf("\n## Items\n\nSorted by discrimination a_b (θ-standardised scale).\n\n| item | n | models | mean score | min–max | a_b | d_b |\n|---|---|---|---|---|---|---|\n")
	for _, it := range r.Items {
		pf("| %s | %d | %d | %s | %s–%s | %s | %s |\n", mdCell(it.Key), it.N, it.Models,
			fmtF(it.MeanScore, 3), fmtF(it.MinS, 3), fmtF(it.MaxS, 3), fmtF(it.A, 3), fmtF(it.D, 3))
	}
	pf("\n## Offsets\n\n| kind | level | offset (logit) | n |\n|---|---|---|---|\n")
	for _, o := range r.Offsets {
		v := fmtF(o.Value, 3)
		if o.Reference {
			v += " (reference)"
		}
		pf("| %s | %s | %s | %d |\n", o.Kind, o.Level, v, o.N)
	}
	pf("\n## Skipped rows\n\n| reason | meaning | rows |\n|---|---|---|\n")
	total := 0
	for _, k := range sortedSkips(r.Skips) {
		pf("| %s | %s | %d |\n", k, skipDescriptions[k], r.Skips[k])
		total += r.Skips[k]
	}
	pf("| total | | %d |\n", total)
	if r.OrderGiven {
		pf("\n## Order constraints\n\nWithin-generation order projected by weighted pool-adjacent-violators.\n\n| vendor | generation | model | θ before | θ after | adjusted | note |\n|---|---|---|---|---|---|---|\n")
		for _, a := range r.Order {
			before, after := "—", "—"
			if a.Note != "not in fit" {
				before, after = fmtF(a.From, 3), fmtF(a.To, 3)
			}
			pf("| %s | %s | %s | %s | %s | %v | %s |\n", mdCell(a.Vendor), mdCell(a.Generation), mdCell(a.Model), before, after, a.Applied, mdCell(a.Note))
		}
	}
	pf("\n## Models\n\nSorted by θ, highest first. rating = %s + %s·θ.\n\n| rank | model | vendor | θ | 90%% lo | 90%% hi | n | code rating | RD | placement |\n|---|---|---|---|---|---|---|---|---|---|\n",
		fmtF(CodeRatingBase, 0), fmtF(CodeRatingPerTheta, 0))
	for i, m := range r.Models {
		th := fmtF(m.Theta, 3)
		if m.Adjusted {
			th += "*"
		}
		pf("| %d | %s | %s | %s | %s | %s | %d | %s | %s | %s |\n", i+1, mdCell(m.Model), mdCell(m.Vendor), th,
			fmtF(m.Lo, 3), fmtF(m.Hi, 3), m.N, fmtF(m.Rating, 0), fmtF(m.RD, 0), m.Placement)
	}
	if r.OrderGiven {
		pf("\n`*` θ moved by an order constraint.\n")
	}
}

// WriteTSV renders the model table as TSV, preceded by '#' comment lines
// carrying the method and constants.
func (r *Result) WriteTSV(w io.Writer, l2, l3 *float64) {
	pf := func(format string, a ...any) { fmt.Fprintf(w, format, a...) }
	pf("# method: %s\n", methodLine)
	pf("# rating = %s + %s*theta; rd = %s*se clamped [%s,%s]; interval 90%% = theta ± %s*se\n",
		fmtF(CodeRatingBase, 0), fmtF(CodeRatingPerTheta, 0), fmtF(CodeRatingPerTheta, 0), fmtF(RDMin, 0), fmtF(RDMax, 0), fmtF(Z90, 3))
	pf("# areas: %s; lines: l2=%s l3=%s; obs=%d items=%d phi=%s converged=%v\n",
		strings.Join(CapabilityAreas, ","), lineText(l2), lineText(l3), r.NObs, len(r.Items), fmtF(r.Phi, 5), r.Converged)
	var sk []string
	for _, k := range sortedSkips(r.Skips) {
		sk = append(sk, fmt.Sprintf("%s=%d", k, r.Skips[k]))
	}
	pf("# skipped: %s\n", strings.Join(sk, " "))
	pf("model\tvendor\ttheta\tlo90\thi90\tn\tcode_rating\trd\tplacement\tadjusted\n")
	for _, m := range r.Models {
		pf("%s\t%s\t%s\t%s\t%s\t%d\t%s\t%s\t%s\t%v\n", m.Model, m.Vendor, fmtF(m.Theta, 3),
			fmtF(m.Lo, 3), fmtF(m.Hi, 3), m.N, fmtF(m.Rating, 0), fmtF(m.RD, 0), m.Placement, m.Adjusted)
	}
}

func parseLine(s string) (*float64, error) {
	if s == "" {
		return nil, nil
	}
	v, err := strconv.ParseFloat(s, 64)
	if err != nil || math.IsNaN(v) || math.IsInf(v, 0) {
		return nil, fmt.Errorf("not a number: %q", s)
	}
	return &v, nil
}

// Main is the yoke-seedfit command: it reads the matrix, fits, applies any
// order file, places and renders. Exit codes: 0 ok, 1 runtime error,
// 2 usage error.
func Main(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("yoke-seedfit", flag.ContinueOnError)
	fs.SetOutput(stderr)
	matrix := fs.String("matrix", "", "benchmark matrix TSV (required)")
	orderPath := fs.String("order", "", "optional order file: vendor<TAB>generation<TAB>m1>m2>...")
	l2s := fs.String("l2-line", "", "θ of the L2 line (below it: L1)")
	l3s := fs.String("l3-line", "", "θ of the L3 line (above it: L3, L4 candidate)")
	format := fs.String("format", "md", "output format: md or tsv")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(stderr, "yoke-seedfit: unexpected argument %q\n", fs.Arg(0))
		return 2
	}
	if *matrix == "" {
		fmt.Fprintln(stderr, "yoke-seedfit: --matrix is required")
		fs.Usage()
		return 2
	}
	if *format != "md" && *format != "tsv" {
		fmt.Fprintf(stderr, "yoke-seedfit: --format must be md or tsv, got %q\n", *format)
		return 2
	}
	l2, err := parseLine(*l2s)
	if err != nil {
		fmt.Fprintf(stderr, "yoke-seedfit: --l2-line: %v\n", err)
		return 2
	}
	l3, err := parseLine(*l3s)
	if err != nil {
		fmt.Fprintf(stderr, "yoke-seedfit: --l3-line: %v\n", err)
		return 2
	}
	if l2 != nil && l3 != nil && *l2 >= *l3 {
		fmt.Fprintln(stderr, "yoke-seedfit: --l2-line must be below --l3-line")
		return 2
	}
	f, err := os.Open(*matrix)
	if err != nil {
		fmt.Fprintf(stderr, "yoke-seedfit: %v\n", err)
		return 1
	}
	rows, err := ReadMatrix(f)
	f.Close()
	if err != nil {
		fmt.Fprintf(stderr, "yoke-seedfit: %v\n", err)
		return 1
	}
	res, err := Fit(Prepare(rows))
	if err != nil {
		fmt.Fprintf(stderr, "yoke-seedfit: %v\n", err)
		return 1
	}
	if *orderPath != "" {
		of, err := os.Open(*orderPath)
		if err != nil {
			fmt.Fprintf(stderr, "yoke-seedfit: %v\n", err)
			return 1
		}
		chains, err := ReadOrder(of)
		of.Close()
		if err != nil {
			fmt.Fprintf(stderr, "yoke-seedfit: %v\n", err)
			return 1
		}
		res.ApplyOrder(chains)
	}
	res.Place(l2, l3)
	if *format == "tsv" {
		res.WriteTSV(stdout, l2, l3)
	} else {
		res.WriteMarkdown(stdout, l2, l3)
	}
	return 0
}
