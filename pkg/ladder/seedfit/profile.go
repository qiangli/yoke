package seedfit

import (
	"fmt"
	"io"
	"math"
	"sort"
	"strings"
)

// FloorGap is how far below an area's lowest fitted θ a model with no rows
// in that area is placed (on the standardised scale). Such a cell is flagged
// "must measure": no public evidence exists, so host evidence must decide.
const FloorGap = 0.5

// AreaFit is one capability area fitted on its own.
type AreaFit struct {
	Area    string
	Weight  float64
	HandsOn bool
	Res     *Result // nil when the area could not be fitted
	Err     string
	Floored map[string]int  // model → benchmark cells floored (missing.mode floor)
	NoRows  map[string]bool // model → no real (measured or inherited) row in the area
}

// FitPerArea fits each configured area separately.
func FitPerArea(rows []Row, cfg *Config) []AreaFit {
	vendor := map[string]string{}
	for _, r := range rows {
		if vendor[r.Model] == "" {
			vendor[r.Model] = r.Vendor
		}
	}
	var out []AreaFit
	for _, name := range cfg.AreaNames() {
		a := cfg.Areas[name]
		f := AreaFit{Area: name, Weight: a.Weight, HandsOn: a.HandsOn, Floored: map[string]int{}, NoRows: map[string]bool{}}
		prep := PrepareAreas(rows, []string{name})
		real := map[string]bool{}
		for _, o := range prep.Obs {
			real[o.Model] = true
		}
		for _, c := range cfg.Candidates {
			if !real[c] {
				f.NoRows[c] = true
			}
		}
		if cfg.Missing.Mode == "floor" {
			prep.Obs = floorMissing(prep.Obs, cfg.Candidates, vendor, cfg.Missing.Gap, cfg.Missing.MinCoverage, f.Floored)
		}
		res, err := Fit(prep)
		if err != nil {
			f.Err = err.Error()
		} else {
			f.Res = res
		}
		out = append(out, f)
	}
	return out
}

// floorMissing adds, for each item covering at least minCov of the candidates
// and each candidate without an observation on it, a cell at (item minimum −
// gap), kept at or above ScoreClampLo. Floor cells carry the reference harness
// and source so no offset absorbs them.
func floorMissing(obs []Obs, candidates []string, vendor map[string]string, gap, minCov float64, count map[string]int) []Obs {
	type item struct {
		min  float64
		have map[string]bool
	}
	items := map[string]*item{}
	var order []string
	for _, o := range obs {
		it := items[o.Item]
		if it == nil {
			it = &item{min: o.Y, have: map[string]bool{}}
			items[o.Item] = it
			order = append(order, o.Item)
		}
		it.min = math.Min(it.min, o.Y)
		it.have[o.Model] = true
	}
	out := append([]Obs(nil), obs...)
	for _, key := range order {
		it := items[key]
		covered := 0
		for _, c := range candidates {
			if it.have[c] {
				covered++
			}
		}
		if len(candidates) == 0 || float64(covered) < minCov*float64(len(candidates)) {
			continue
		}
		y := math.Max(ScoreClampLo, it.min-gap)
		for _, c := range candidates {
			if it.have[c] || vendor[c] == "" {
				continue
			}
			out = append(out, Obs{Model: c, Vendor: vendor[c], Item: key, Harness: ReferenceHarness,
				HarnessRaw: "floor", Source: ReferenceSource, Y: y})
			count[c]++
		}
	}
	return out
}

// AreaScore is one model's standing in one area.
type AreaScore struct {
	Theta, SE float64
	N         int
	Floor     bool // no rows in this area: placed FloorGap below the area minimum
}

// ProfileRow is one model's manager profile.
type ProfileRow struct {
	Model, Vendor string
	Areas         map[string]AreaScore
	Role, RoleSE  float64
	Weakest       string
	WeakestTheta  float64
	HandsOn       float64
	HandsOnSE     float64
	Manages       []string // workers whose hands-on θ ≤ this model's conservative hands-on (θ − Z90·se)
	MustMeasure   []string
	Inherited     int
	Floored       int // benchmark cells floored (missing.mode floor)
	Anchor        bool
	Verdict       string
}

// Profile is the per-area manager ranking.
type Profile struct {
	Fits          []AreaFit
	Rows          []ProfileRow
	Line          float64 // lowest anchor role score minus its 90% half-interval
	Anchors       []string
	MissingMode   string
	MissingGap    float64
	MissingMinCov float64
	Inheritances  []Inheritance
	MissingAnchor []string
}

// BuildProfile combines the area fits into role scores, weakest areas, the
// hands-on check and the anchor line.
func BuildProfile(fits []AreaFit, cfg *Config, inh []Inheritance) *Profile {
	p := &Profile{Fits: fits, Anchors: cfg.Anchors, Inheritances: inh, MissingMode: cfg.Missing.Mode, MissingGap: cfg.Missing.Gap, MissingMinCov: cfg.Missing.MinCoverage}
	inhCount := map[string]int{}
	for _, i := range inh {
		inhCount[i.Model]++
	}
	vendor := map[string]string{}
	var models []string
	seen := map[string]bool{}
	for _, f := range fits {
		if f.Res == nil {
			continue
		}
		for _, m := range f.Res.Models {
			vendor[m.Model] = m.Vendor
			if !seen[m.Model] {
				seen[m.Model] = true
				models = append(models, m.Model)
			}
		}
	}
	if len(cfg.Candidates) > 0 {
		keep := map[string]bool{}
		for _, c := range cfg.Candidates {
			keep[c] = true
		}
		for _, a := range cfg.Anchors {
			keep[a] = true
		}
		var kept []string
		for _, m := range models {
			if keep[m] {
				kept = append(kept, m)
			}
		}
		models = kept
	}
	anchor := map[string]bool{}
	for _, a := range cfg.Anchors {
		anchor[a] = true
		if !seen[a] {
			p.MissingAnchor = append(p.MissingAnchor, a)
		}
	}
	for _, m := range models {
		row := ProfileRow{Model: m, Vendor: vendor[m], Areas: map[string]AreaScore{}, Inherited: inhCount[m], Anchor: anchor[m]}
		var wsum, rsum, vsum, hsum, hw, hv float64
		row.WeakestTheta = math.Inf(1)
		for _, f := range fits {
			if f.Res == nil {
				continue
			}
			s, ok := areaScore(f.Res, m)
			if !ok {
				s = floorScore(f.Res)
			}
			if !ok || f.NoRows[m] {
				s.Floor = true
				row.MustMeasure = append(row.MustMeasure, f.Area)
			}
			row.Floored += f.Floored[m]
			row.Areas[f.Area] = s
			if f.Weight > 0 {
				wsum += f.Weight
				rsum += f.Weight * s.Theta
				vsum += f.Weight * f.Weight * s.SE * s.SE
				if s.Theta < row.WeakestTheta {
					row.WeakestTheta, row.Weakest = s.Theta, f.Area
				}
			}
			if f.HandsOn {
				w := f.Weight
				if w <= 0 {
					w = 1
				}
				hsum += w * s.Theta
				hw += w
				hv += w * w * s.SE * s.SE
			}
		}
		if wsum > 0 {
			row.Role = rsum / wsum
			row.RoleSE = math.Sqrt(vsum) / wsum
		}
		if hw > 0 {
			row.HandsOn = hsum / hw
			row.HandsOnSE = math.Sqrt(hv) / hw
		}
		p.Rows = append(p.Rows, row)
	}
	p.Line = math.Inf(1)
	anchors := 0
	for _, r := range p.Rows {
		if r.Anchor {
			p.Line = math.Min(p.Line, r.Role-Z90*r.RoleSE)
			anchors++
		}
	}
	if anchors == 0 {
		p.Line = math.NaN()
	}
	for i := range p.Rows {
		r := &p.Rows[i]
		// Hands-on is relative: a manager steps in for the workers it
		// manages, so it must dominate each of them (nobody manages uphill),
		// conservatively — as ladder.DominanceOK does for reviewers.
		for _, w := range p.Rows {
			if w.Model != r.Model && r.HandsOn-Z90*r.HandsOnSE >= w.HandsOn {
				r.Manages = append(r.Manages, w.Model)
			}
		}
		switch {
		case r.Anchor:
			r.Verdict = "anchor"
		case math.IsNaN(p.Line):
			r.Verdict = "no anchors"
		case r.Role >= p.Line:
			r.Verdict = "candidate"
		case r.Role+Z90*r.RoleSE >= p.Line:
			r.Verdict = "near"
		default:
			r.Verdict = "below line"
		}
	}
	sort.SliceStable(p.Rows, func(i, j int) bool {
		if p.Rows[i].Role != p.Rows[j].Role {
			return p.Rows[i].Role > p.Rows[j].Role
		}
		return p.Rows[i].Model < p.Rows[j].Model
	})
	return p
}

func areaScore(r *Result, m string) (AreaScore, bool) {
	for _, e := range r.Models {
		if e.Model == m {
			return AreaScore{Theta: e.Theta, SE: e.SE, N: e.N}, true
		}
	}
	return AreaScore{}, false
}

func floorScore(r *Result) AreaScore {
	lo, se := math.Inf(1), 0.0
	for _, e := range r.Models {
		lo = math.Min(lo, e.Theta)
		se = math.Max(se, e.SE)
	}
	return AreaScore{Theta: lo - FloorGap, SE: se, Floor: true}
}

// WriteProfileMarkdown renders the profile for a human (bashy viewer input).
func (p *Profile) WriteProfileMarkdown(w io.Writer) {
	pf := func(format string, a ...any) { fmt.Fprintf(w, format, a...) }
	pf("# Manager profile — per-area ability (θ)\n\n")
	pf("Each area is fitted on its own (%s); θ is standardised within the area over the models it covers. ", methodLine)
	pf("Role = weight-averaged θ; weakest = lowest weighted area; hands-on = θ over the hands-on areas. ")
	pf("Line = lowest anchor role − %s·se. Hands-on is relative: a model *can manage* a worker when its hands-on θ − %s·se ≥ the worker's hands-on θ (nobody manages uphill). ", fmtF(Z90, 3), fmtF(Z90, 3))
	if p.MissingMode == "floor" {
		pf("Missing cells: on a benchmark covering at least %s of the candidates, a candidate without a row gets (lowest observed − %s), kept above zero (*floored* column); on sparser boards missing cells are skipped. A model with no real row in an area is flagged *must measure* (ᶠ). ", fmtF(p.MissingMinCov, 2), fmtF(p.MissingGap, 2))
	} else {
		pf("A model with no rows in an area sits %s below that area's lowest model and is flagged *must measure* (ᶠ). ", fmtF(FloorGap, 1))
	}
	pf("Inherited rows come from the same line's predecessor.\n\n")
	pf("| anchor line (role) | anchors |\n|---|---|\n| %s | %s |\n\n", fmtF(p.Line, 2), strings.Join(p.Anchors, ", "))
	if len(p.MissingAnchor) > 0 {
		pf("**Anchors with no rows:** %s\n\n", strings.Join(p.MissingAnchor, ", "))
	}
	var areas []AreaFit
	for _, f := range p.Fits {
		if f.Res != nil {
			areas = append(areas, f)
		}
	}
	pf("## Ranking\n\n| model | role ± se | hands-on ± se | can manage | weakest | verdict | must measure | inherited | floored |\n|---|---|---|---|---|---|---|---|---|\n")
	for _, r := range p.Rows {
		pf("| %s | %s ± %s | %s ± %s | %d: %s | %s (%s) | %s | %s | %d | %d |\n", r.Model, fmtF(r.Role, 2), fmtF(r.RoleSE, 2),
			fmtF(r.HandsOn, 2), fmtF(r.HandsOnSE, 2), len(r.Manages), strings.Join(r.Manages, ", "),
			r.Weakest, fmtF(r.WeakestTheta, 2), r.Verdict, strings.Join(r.MustMeasure, ", "), r.Inherited, r.Floored)
	}
	pf("\n## Area matrix (θ)\n\n| model |")
	for _, f := range areas {
		pf(" %s (w %s) |", f.Area, fmtF(f.Weight, 2))
	}
	pf("\n|---|")
	for range areas {
		pf("---|")
	}
	pf("\n")
	for _, r := range p.Rows {
		pf("| %s |", r.Model)
		for _, f := range areas {
			s := r.Areas[f.Area]
			mark := ""
			if s.Floor {
				mark = "ᶠ"
			}
			pf(" %s%s |", fmtF(s.Theta, 2), mark)
		}
		pf("\n")
	}
	pf("\n## Area fits\n\n| area | weight | hands-on | models | items | observations | converged | skipped rows | note |\n|---|---|---|---|---|---|---|---|---|\n")
	for _, f := range p.Fits {
		if f.Res == nil {
			pf("| %s | %s | %v | – | – | – | – | – | **not fitted:** %s |\n", f.Area, fmtF(f.Weight, 2), f.HandsOn, f.Err)
			continue
		}
		var sk []string
		for _, k := range sortedSkips(f.Res.Skips) {
			if k != SkipArea {
				sk = append(sk, fmt.Sprintf("%s=%d", k, f.Res.Skips[k]))
			}
		}
		pf("| %s | %s | %v | %d | %d | %d | %v | %s | |\n", f.Area, fmtF(f.Weight, 2), f.HandsOn,
			len(f.Res.Models), len(f.Res.Items), f.Res.NObs, f.Res.Converged, strings.Join(sk, " "))
	}
	if len(p.Inheritances) > 0 {
		pf("\n## Inherited rows\n\n| model | from | benchmark | variant |\n|---|---|---|---|\n")
		for _, i := range p.Inheritances {
			pf("| %s | %s | %s | %s |\n", i.Model, i.From, mdCell(i.Benchmark), mdCell(i.Variant))
		}
	}
}

// WriteProfileTSV renders the profile as one row per model.
func (p *Profile) WriteProfileTSV(w io.Writer) {
	var areas []string
	for _, f := range p.Fits {
		if f.Res != nil {
			areas = append(areas, f.Area)
		}
	}
	fmt.Fprintf(w, "# line=%s anchors=%s\n", fmtF(p.Line, 4), strings.Join(p.Anchors, ","))
	fmt.Fprintf(w, "model\trole\trole_se\thands_on\thands_on_se\tmanages\tweakest\tverdict\tmust_measure\tinherited\tfloored\t%s\n", strings.Join(areas, "\t"))
	for _, r := range p.Rows {
		var cells []string
		for _, a := range areas {
			cells = append(cells, fmtF(r.Areas[a].Theta, 4))
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%d\t%d\t%s\n", r.Model, fmtF(r.Role, 4), fmtF(r.RoleSE, 4),
			fmtF(r.HandsOn, 4), fmtF(r.HandsOnSE, 4), strings.Join(r.Manages, ","), r.Weakest, r.Verdict, strings.Join(r.MustMeasure, ","), r.Inherited, r.Floored, strings.Join(cells, "\t"))
	}
}
