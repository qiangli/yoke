// Package stats is a unix filter over results JSONL: per-arm resolve rates
// with instance-clustered confidence intervals, paired differences between
// two arms over their shared instances, pass^k / pass@k, and cost per solve.
//
// It knows nothing about any benchmark: every field name is a flag, so it
// composes with bench runners, agent-bench results or foreign files. One
// record is one attempt: an instance, an arm (agent/config), an outcome and
// optionally a cost. Repeated attempts of an instance in one arm (K runs)
// are clustered by instance: the instance mean is the unit, which is what
// makes the standard error honest when K > 1.
//
// Sprint: #290; Story: #881; Story-ID: 85569cee686c (G0.2 slice of #287 V6)
package stats

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"sort"
	"strconv"
	"strings"
)

// Z95 is the two-sided 95% normal quantile used for every interval here.
const Z95 = 1.959963984540054

// Fields names the JSON fields a record is read from.
type Fields struct {
	Instance string // required: the unit clustered on (task id)
	Arm      string // the compared configuration; "" = one arm named "all"
	Outcome  string // required: bool, 0/1 number, or "resolved"/"pass"/"true" string
	Cost     string // optional: numeric cost of the attempt
}

// Attempt is one parsed record.
type Attempt struct {
	Instance string
	Arm      string
	Pass     bool
	Cost     float64
	HasCost  bool
}

// Read parses JSONL attempts. Blank lines are skipped; a line that is not a
// JSON object, or lacks the instance or outcome field, is an error naming the
// line (a silently dropped row would change every number below).
func Read(r io.Reader, f Fields) ([]Attempt, error) {
	if f.Instance == "" || f.Outcome == "" {
		return nil, errors.New("stats: instance and outcome field names are required")
	}
	var out []Attempt
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 1024*1024), 64*1024*1024)
	line := 0
	for sc.Scan() {
		line++
		text := strings.TrimSpace(sc.Text())
		if text == "" {
			continue
		}
		var rec map[string]any
		if err := json.Unmarshal([]byte(text), &rec); err != nil {
			return nil, fmt.Errorf("stats: line %d: %w", line, err)
		}
		inst, ok := scalarString(rec[f.Instance])
		if !ok || inst == "" {
			return nil, fmt.Errorf("stats: line %d: missing instance field %q", line, f.Instance)
		}
		pass, ok := outcome(rec[f.Outcome])
		if !ok {
			return nil, fmt.Errorf("stats: line %d: outcome field %q is not a pass/fail value", line, f.Outcome)
		}
		arm := "all"
		if f.Arm != "" {
			a, ok := scalarString(rec[f.Arm])
			if !ok || a == "" {
				return nil, fmt.Errorf("stats: line %d: missing arm field %q", line, f.Arm)
			}
			arm = a
		}
		at := Attempt{Instance: inst, Arm: arm, Pass: pass}
		if f.Cost != "" {
			if v, present := rec[f.Cost]; present && v != nil {
				c, ok := number(v)
				if !ok {
					return nil, fmt.Errorf("stats: line %d: cost field %q is not a number", line, f.Cost)
				}
				at.Cost, at.HasCost = c, true
			}
		}
		out = append(out, at)
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

func scalarString(v any) (string, bool) {
	switch t := v.(type) {
	case string:
		return t, true
	case float64:
		return strconv.FormatFloat(t, 'f', -1, 64), true
	case bool:
		return strconv.FormatBool(t), true
	}
	return "", false
}

func number(v any) (float64, bool) {
	switch t := v.(type) {
	case float64:
		return t, true
	case string:
		f, err := strconv.ParseFloat(strings.TrimSpace(t), 64)
		return f, err == nil
	}
	return 0, false
}

func outcome(v any) (bool, bool) {
	switch t := v.(type) {
	case bool:
		return t, true
	case float64:
		if t == 0 || t == 1 {
			return t == 1, true
		}
	case string:
		switch strings.ToLower(strings.TrimSpace(t)) {
		case "true", "1", "pass", "passed", "resolved", "yes", "ok":
			return true, true
		case "false", "0", "fail", "failed", "unresolved", "no", "error":
			return false, true
		}
	}
	return false, false
}

// Interval is an estimate with its 95% confidence interval.
type Interval struct {
	Mean float64 `json:"mean"`
	SE   float64 `json:"se"`
	Low  float64 `json:"low"`
	High float64 `json:"high"`
}

func interval(mean, se float64) Interval {
	return Interval{Mean: mean, SE: se, Low: mean - Z95*se, High: mean + Z95*se}
}

// ArmSummary is one arm's resolve rate and cost.
type ArmSummary struct {
	Arm          string   `json:"arm"`
	Instances    int      `json:"instances"`
	Attempts     int      `json:"attempts"`
	Passes       int      `json:"passes"`
	Rate         Interval `json:"resolve_rate"`
	TotalCost    *float64 `json:"total_cost,omitempty"`
	CostPerSolve *float64 `json:"cost_per_solve,omitempty"`
}

type instanceStat struct {
	n, c int
}

func (s instanceStat) mean() float64 { return float64(s.c) / float64(s.n) }

// byArm groups attempts into arm → instance → counts.
func byArm(attempts []Attempt) map[string]map[string]*instanceStat {
	out := map[string]map[string]*instanceStat{}
	for _, a := range attempts {
		inst := out[a.Arm]
		if inst == nil {
			inst = map[string]*instanceStat{}
			out[a.Arm] = inst
		}
		s := inst[a.Instance]
		if s == nil {
			s = &instanceStat{}
			inst[a.Instance] = s
		}
		s.n++
		if a.Pass {
			s.c++
		}
	}
	return out
}

// meanSE is the mean of xs and its standard error (sample sd / sqrt(n));
// SE is 0 for fewer than two values.
func meanSE(xs []float64) (float64, float64) {
	if len(xs) == 0 {
		return 0, 0
	}
	sum := 0.0
	for _, x := range xs {
		sum += x
	}
	mean := sum / float64(len(xs))
	if len(xs) < 2 {
		return mean, 0
	}
	ss := 0.0
	for _, x := range xs {
		ss += (x - mean) * (x - mean)
	}
	return mean, math.Sqrt(ss/float64(len(xs)-1)) / math.Sqrt(float64(len(xs)))
}

// Summarize reports every arm, sorted by name. The rate is the mean of the
// per-instance pass fractions, its SE clustered by instance.
func Summarize(attempts []Attempt) []ArmSummary {
	groups := byArm(attempts)
	costs := map[string]float64{}
	hasCost := map[string]bool{}
	for _, a := range attempts {
		if a.HasCost {
			costs[a.Arm] += a.Cost
			hasCost[a.Arm] = true
		}
	}
	arms := make([]string, 0, len(groups))
	for arm := range groups {
		arms = append(arms, arm)
	}
	sort.Strings(arms)
	out := make([]ArmSummary, 0, len(arms))
	for _, arm := range arms {
		var means []float64
		s := ArmSummary{Arm: arm, Instances: len(groups[arm])}
		for _, st := range groups[arm] {
			means = append(means, st.mean())
			s.Attempts += st.n
			s.Passes += st.c
		}
		m, se := meanSE(means)
		s.Rate = interval(m, se)
		if hasCost[arm] {
			total := costs[arm]
			s.TotalCost = &total
			if s.Passes > 0 {
				per := total / float64(s.Passes)
				s.CostPerSolve = &per
			}
		}
		out = append(out, s)
	}
	return out
}

// Paired compares arm B against arm A over the instances both attempted:
// the per-instance difference of pass fractions (B − A), its clustered CI,
// the correlation of the two arms' instance means, and — for reference —
// the unpaired SE the same instances would give.
type Paired struct {
	A          string   `json:"a"`
	B          string   `json:"b"`
	Shared     int      `json:"shared_instances"`
	OnlyA      int      `json:"only_a"`
	OnlyB      int      `json:"only_b"`
	RateA      float64  `json:"rate_a"`
	RateB      float64  `json:"rate_b"`
	Diff       Interval `json:"difference"`
	Corr       *float64 `json:"correlation,omitempty"`
	UnpairedSE float64  `json:"unpaired_se"`
	Wins       int      `json:"b_better_instances"`
	Losses     int      `json:"b_worse_instances"`
}

// ComparePaired computes B − A over shared instances.
func ComparePaired(attempts []Attempt, a, b string) (Paired, error) {
	groups := byArm(attempts)
	ga, gb := groups[a], groups[b]
	if ga == nil {
		return Paired{}, fmt.Errorf("stats: no attempts for arm %q", a)
	}
	if gb == nil {
		return Paired{}, fmt.Errorf("stats: no attempts for arm %q", b)
	}
	p := Paired{A: a, B: b}
	var xa, xb, d []float64
	keys := make([]string, 0, len(ga))
	for inst := range ga {
		keys = append(keys, inst)
	}
	sort.Strings(keys)
	for _, inst := range keys {
		sb, ok := gb[inst]
		if !ok {
			p.OnlyA++
			continue
		}
		ma, mb := ga[inst].mean(), sb.mean()
		xa, xb, d = append(xa, ma), append(xb, mb), append(d, mb-ma)
		switch {
		case mb > ma:
			p.Wins++
		case mb < ma:
			p.Losses++
		}
	}
	for inst := range gb {
		if _, ok := ga[inst]; !ok {
			p.OnlyB++
		}
	}
	p.Shared = len(d)
	if p.Shared == 0 {
		return p, fmt.Errorf("stats: arms %q and %q share no instances", a, b)
	}
	ra, sea := meanSE(xa)
	rb, seb := meanSE(xb)
	p.RateA, p.RateB = ra, rb
	md, sed := meanSE(d)
	p.Diff = interval(md, sed)
	p.UnpairedSE = math.Sqrt(sea*sea + seb*seb)
	if c, ok := correlation(xa, xb); ok {
		p.Corr = &c
	}
	return p, nil
}

func correlation(x, y []float64) (float64, bool) {
	if len(x) < 2 {
		return 0, false
	}
	mx, _ := meanSE(x)
	my, _ := meanSE(y)
	var sxy, sxx, syy float64
	for i := range x {
		dx, dy := x[i]-mx, y[i]-my
		sxy += dx * dy
		sxx += dx * dx
		syy += dy * dy
	}
	if sxx == 0 || syy == 0 {
		return 0, false
	}
	return sxy / math.Sqrt(sxx*syy), true
}

// PassK is pass^k (all k of k sampled attempts pass; the tau-bench
// reliability metric) and pass@k (at least one passes), each the mean over
// instances of the unbiased estimate from that instance's n attempts and c
// passes: pass^k = C(c,k)/C(n,k), pass@k = 1 − C(n−c,k)/C(n,k). Instances
// with fewer than k attempts are excluded and counted.
type PassK struct {
	Arm       string  `json:"arm"`
	K         int     `json:"k"`
	Instances int     `json:"instances"`
	Excluded  int     `json:"excluded_fewer_than_k"`
	PassHatK  float64 `json:"pass_hat_k"`
	PassAtK   float64 `json:"pass_at_k"`
}

// ComputePassK reports every arm, sorted by name.
func ComputePassK(attempts []Attempt, k int) ([]PassK, error) {
	if k < 1 {
		return nil, errors.New("stats: k must be at least 1")
	}
	groups := byArm(attempts)
	arms := make([]string, 0, len(groups))
	for arm := range groups {
		arms = append(arms, arm)
	}
	sort.Strings(arms)
	var out []PassK
	for _, arm := range arms {
		r := PassK{Arm: arm, K: k}
		var hat, at float64
		for _, st := range groups[arm] {
			if st.n < k {
				r.Excluded++
				continue
			}
			r.Instances++
			hat += choose(st.c, k) / choose(st.n, k)
			at += 1 - choose(st.n-st.c, k)/choose(st.n, k)
		}
		if r.Instances > 0 {
			r.PassHatK = hat / float64(r.Instances)
			r.PassAtK = at / float64(r.Instances)
		}
		out = append(out, r)
	}
	return out, nil
}

// choose is the binomial coefficient as a float (0 when k > n).
func choose(n, k int) float64 {
	if k < 0 || k > n {
		return 0
	}
	r := 1.0
	for i := 1; i <= k; i++ {
		r *= float64(n-k+i) / float64(i)
	}
	return r
}
