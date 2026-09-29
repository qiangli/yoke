package seedfit

import (
	"bytes"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// simRows builds a deterministic synthetic matrix: every model scored on
// every item with score = sigmoid(a*theta - d) plus a small fixed wobble.
func simRows(thetas map[string]float64, items map[string][2]float64, wobble float64) []Row {
	var models, names []string
	for m := range thetas {
		models = append(models, m)
	}
	for b := range items {
		names = append(names, b)
	}
	sort.Strings(models)
	sort.Strings(names)
	var rows []Row
	k := 0
	for _, m := range models {
		for _, b := range names {
			ad := items[b]
			p := sigmoid(ad[0]*thetas[m] - ad[1])
			p += wobble * math.Sin(float64(k)*1.7)
			k++
			rows = append(rows, Row{
				Vendor: "vendor-" + m[len(m)-1:], VendorCLI: "cli-x", Model: m,
				Area: "coding-debug-test", Benchmark: b, Metric: "%",
				Score:   fmt.Sprintf("%.2f", 100*p),
				Harness: "Terminus 2", SourceType: "official-board", Line: k + 1,
			})
		}
	}
	return rows
}

var simThetas = map[string]float64{
	"model-a": -1.6, "model-b": -1.1, "model-c": -0.6, "model-d": -0.2,
	"model-e": 0.2, "model-f": 0.6, "model-g": 1.1, "model-h": 1.6,
}

var simItems = map[string][2]float64{
	"bench-p": {1.5, 0.0}, "bench-q": {1.0, 0.5}, "bench-r": {2.0, -0.5},
	"bench-s": {1.2, 1.0}, "bench-t": {0.8, -1.0}, "bench-u": {1.6, 0.3},
}

func fitRows(t *testing.T, rows []Row) *Result {
	t.Helper()
	prep := Prepare(rows)
	res, err := Fit(prep)
	if err != nil {
		t.Fatalf("Fit: %v", err)
	}
	return res
}

func thetaOf(t *testing.T, r *Result, m string) float64 {
	t.Helper()
	for _, e := range r.Models {
		if e.Model == m {
			return e.Theta
		}
	}
	t.Fatalf("model %s not in result", m)
	return 0
}

func TestRecoversKnownOrdering(t *testing.T) {
	res := fitRows(t, simRows(simThetas, simItems, 0.01))
	if len(res.Models) != len(simThetas) {
		t.Fatalf("got %d models", len(res.Models))
	}
	// Models are sorted by theta descending; the truth is h > g > ... > a.
	want := []string{"model-h", "model-g", "model-f", "model-e", "model-d", "model-c", "model-b", "model-a"}
	for i, e := range res.Models {
		if e.Model != want[i] {
			t.Fatalf("rank %d: got %s want %s (all: %+v)", i, e.Model, want[i], res.Models)
		}
		if !(e.Lo < e.Theta && e.Theta < e.Hi) {
			t.Errorf("%s: interval [%v,%v] does not bracket %v", e.Model, e.Lo, e.Hi, e.Theta)
		}
		// Near-noiseless data: the interval must reflect it, not the
		// location/scale freedom that standardisation removes.
		if e.SE <= 0 || e.SE > 0.2 {
			t.Errorf("%s: se %.3f, want (0, 0.2]", e.Model, e.SE)
		}
		if e.N != len(simItems) {
			t.Errorf("%s: n=%d want %d", e.Model, e.N, len(simItems))
		}
	}
	// Identifiability: theta has mean 0, sd 1 over models.
	var s, ss float64
	for _, e := range res.Models {
		s += e.Theta
		ss += e.Theta * e.Theta
	}
	n := float64(len(res.Models))
	if math.Abs(s/n) > 1e-9 || math.Abs(math.Sqrt(ss/n-(s/n)*(s/n))-1) > 1e-9 {
		t.Errorf("theta not standardised: mean %v sd^2 %v", s/n, ss/n)
	}
}

func TestSkippedCellsDoNotShiftOthers(t *testing.T) {
	rows := simRows(simThetas, simItems, 0.01)
	full := fitRows(t, rows)
	// Knock out two of model-d's cells: they become unparseable and are
	// skipped, never imputed.
	var holed []Row
	knocked := 0
	for _, r := range rows {
		if r.Model == "model-d" && (r.Benchmark == "bench-p" || r.Benchmark == "bench-s") {
			r.Score = "n/a"
			knocked++
		}
		holed = append(holed, r)
	}
	part := fitRows(t, holed)
	if part.Skips[SkipUnparseable] != knocked {
		t.Fatalf("skip count %d want %d", part.Skips[SkipUnparseable], knocked)
	}
	for m := range simThetas {
		if m == "model-d" {
			continue
		}
		if d := math.Abs(thetaOf(t, full, m) - thetaOf(t, part, m)); d > 0.1 {
			t.Errorf("%s moved by %.3f after skipping another model's cells", m, d)
		}
	}
}

func TestSaturatedItemGetsLowerDiscrimination(t *testing.T) {
	items := map[string][2]float64{
		"bench-p": {1.5, 0.0}, "bench-q": {1.0, 0.5}, "bench-r": {2.0, -0.5},
		"bench-s": {1.2, 1.0},
	}
	rows := simRows(simThetas, items, 0.005)
	// A saturated suite: every model sits at 97-99 whatever its ability.
	k := 0
	var models []string
	for m := range simThetas {
		models = append(models, m)
	}
	sort.Strings(models)
	for i, m := range models {
		k++
		rows = append(rows, Row{Model: m, Vendor: "vendor-x", Area: "tool-use-agentic",
			Benchmark: "bench-sat", Metric: "%", Score: fmt.Sprintf("%.1f", 97+float64((i*5)%3)),
			Harness: "Terminus 2", SourceType: "official-board", Line: 1000 + k})
	}
	res := fitRows(t, rows)
	var aSat, aInf float64
	for _, it := range res.Items {
		switch it.Key {
		case "bench sat":
			aSat = it.A
		case "bench r":
			aInf = it.A
		}
	}
	if aSat == 0 || aInf == 0 {
		t.Fatalf("items missing: %+v", res.Items)
	}
	if !(aSat < aInf) {
		t.Errorf("saturated a=%.3f not below informative a=%.3f", aSat, aInf)
	}
}

func TestOrderConstraintOnlyWithinGeneration(t *testing.T) {
	r := &Result{Models: []ModelEstimate{
		{Model: "model-a", Vendor: "vendor-a", Theta: 0.2, SE: 0.3},
		{Model: "model-b", Vendor: "vendor-a", Theta: 0.8, SE: 0.3},
		{Model: "model-c", Vendor: "vendor-a", Theta: 1.5, SE: 0.3},
		{Model: "model-d", Vendor: "vendor-b", Theta: -0.5, SE: 0.3},
	}}
	chains, err := ReadOrder(strings.NewReader("vendor-a\tgen-1\tmodel-a>model-b\nvendor-a\tgen-2\tmodel-d>model-c\n"))
	if err != nil {
		t.Fatal(err)
	}
	adj := r.ApplyOrder(chains)
	a, b := thetaOf(t, r, "model-a"), thetaOf(t, r, "model-b")
	if a < b-1e-12 {
		t.Errorf("gen-1 order violated after projection: a=%v b=%v", a, b)
	}
	if math.Abs(a-0.5) > 1e-9 || math.Abs(b-0.5) > 1e-9 {
		t.Errorf("equal-weight PAV should pool to 0.5: a=%v b=%v", a, b)
	}
	// model-c is not in gen-1 and must not move; model-d belongs to
	// another vendor, so the gen-2 chain is not the data's to declare.
	if thetaOf(t, r, "model-c") != 1.5 || thetaOf(t, r, "model-d") != -0.5 {
		t.Errorf("models outside a valid chain moved: %+v", r.Models)
	}
	var moved []string
	for _, x := range adj {
		if x.Applied {
			moved = append(moved, x.Model)
		}
	}
	sort.Strings(moved)
	if strings.Join(moved, ",") != "model-a,model-b" {
		t.Errorf("adjusted = %v", moved)
	}
	if r.Models[0].Model != "model-c" {
		t.Errorf("models not re-sorted by theta: %+v", r.Models)
	}
}

func TestReadOrderFile(t *testing.T) {
	f, err := os.Open(filepath.Join("testdata", "order.tsv"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	chains, err := ReadOrder(f)
	if err != nil {
		t.Fatal(err)
	}
	if len(chains) != 2 || chains[1].Vendor != "vendor-b" || strings.Join(chains[0].Models, ">") != "model-a>model-b" {
		t.Errorf("chains = %+v", chains)
	}
	if _, err := ReadOrder(strings.NewReader("vendor-a\tgen-1\n")); err == nil {
		t.Error("short line accepted")
	}
}

func TestSkipReportCounts(t *testing.T) {
	f, err := os.Open(filepath.Join("testdata", "skips.tsv"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	rows, err := ReadMatrix(f)
	if err != nil {
		t.Fatal(err)
	}
	prep := Prepare(rows)
	want := map[string]int{
		SkipArea: 2, SkipNoModel: 1, SkipMissing: 1, SkipRatingScale: 1,
		SkipIndexNoRange: 1, SkipNotProportion: 1, SkipUnparseable: 1,
		SkipMultiValue: 1, SkipApproximate: 1, SkipValueRange: 1,
		SkipOutOfRange: 1, SkipMultiMetric: 1, SkipLowerBetter: 1,
		SkipSingleModel: 1,
	}
	for k, v := range want {
		if prep.Skips[k] != v {
			t.Errorf("skip %q = %d want %d", k, prep.Skips[k], v)
		}
	}
	total := 0
	for _, v := range prep.Skips {
		total += v
	}
	if total != 15 || len(prep.Obs) != 5 {
		t.Errorf("skipped %d kept %d, want 15/5 (%v)", total, len(prep.Obs), prep.Skips)
	}
	got := map[string]float64{}
	for _, o := range prep.Obs {
		got[o.Model+"|"+o.Item] = o.Y
	}
	for k, v := range map[string]float64{
		"model-a|bench x": 0.60, "model-b|bench x": 0.405, "model-c|bench x": 0.20,
		"model-c|bench y": 0.70, "model-a|bench y": 0.55,
	} {
		if math.Abs(got[k]-v) > 1e-12 {
			t.Errorf("%s = %v want %v", k, got[k], v)
		}
	}
}

func TestParseScore(t *testing.T) {
	cases := []struct {
		metric, score string
		want          float64
		reason        string
	}{
		{"%", "12.5", 0.125, ""},
		{"% resolved", "0.1", 0.005, ""}, // clamped low
		{"accuracy %", "99.9", 0.995, ""},
		{"score (0-1)", "0.42", 0.42, ""},
		{"resolution rate %", "57.9 (±3.8)", 0.579, ""},
		{"%", "13.13 ±2.52", 0.1313, ""},
		{"index %", "66", 0.66, ""},
		{"index", "66", 0, SkipIndexNoRange},
		{"Elo", "1500", 0, SkipRatingScale},
		{"Arena score (AutoEval early)", "1500", 0, SkipRatingScale},
		{"score", "40", 0, SkipNotProportion},
		{"%", "", 0, SkipMissing},
		{"%", "n/a (figure only)", 0, SkipUnparseable},
		{"%", "78.80 -> 57.32 -> 59.51", 0, SkipMultiValue},
		{"score (0-1)", "1.5", 0, SkipOutOfRange},
	}
	for _, c := range cases {
		got, reason := ParseScore(c.metric, c.score)
		if reason != c.reason || (reason == "" && math.Abs(got-c.want) > 1e-12) {
			t.Errorf("ParseScore(%q,%q) = %v,%q want %v,%q", c.metric, c.score, got, reason, c.want, c.reason)
		}
	}
}

func TestHarnessClass(t *testing.T) {
	clis := map[string]bool{"cli-a": true, "toolx": true}
	vendors := map[string]bool{"acme": true}
	cases := map[string]string{
		"":                                   HarnessUnstated,
		"unstated (mostly official boards)":  HarnessUnstated,
		"not stated (site says Terminus 2)":  HarnessUnstated,
		"none/API":                           HarnessNone,
		"direct API judge":                   HarnessNone,
		"vendor internal harness":            HarnessVendorInternal,
		"Acme internal harness":              HarnessVendorInternal,
		"best self-reported harness":         HarnessVendorInternal,
		"ToolX Code --bare":                  HarnessAgentCLI,
		"cli-a":                              HarnessAgentCLI,
		"native harness (ToolX) in internal": HarnessVendorInternal,
		"Terminus 2":                         HarnessIndependent,
		"mini-swe-agent":                     HarnessIndependent,
		"Scale standardized scaffold":        HarnessIndependent,
	}
	for raw, want := range cases {
		if got := HarnessClass(raw, clis, vendors); got != want {
			t.Errorf("HarnessClass(%q) = %q want %q", raw, got, want)
		}
	}
}

func TestItemKey(t *testing.T) {
	cases := [][3]string{
		{"FrontierCode v1.1", "Main, max effort", "frontiercode 1.1 main"},
		{"FrontierCode 1.1", "Main split", "frontiercode 1.1 main"},
		{"FrontierCode v1.1", "Extended, xhigh", "frontiercode 1.1 extended"},
		{"Terminal-Bench 4.0", "Vals, Max", "terminal bench 4.0"},
		{"MCP-Atlas", "overall", "mcp atlas"},
		{"tau3-bench Banking", "AA", "tau3 bench banking"},
		{"tau3-bench", "Banking high", "tau3 bench banking"},
		{"Toolathlon Verified", "Pass^3", "toolathlon verified pass^3"},
		{"SWE-bench (Verified)", "overall", "swe bench verified"},
		{"SWE-bench (Verified, Vals)", "", "swe bench verified vals"},
	}
	for _, c := range cases {
		if got := ItemKey(c[0], c[1]); got != c[2] {
			t.Errorf("ItemKey(%q,%q) = %q want %q", c[0], c[1], got, c[2])
		}
	}
}

func TestPlacementNeverAboveL3(t *testing.T) {
	l2, l3 := -0.5, 0.5
	cases := []struct {
		theta  float64
		l2, l3 *float64
		want   string
	}{
		{2.0, nil, nil, "unplaced"},
		{2.0, &l2, &l3, "L3 (L4 candidate)"},
		{0.0, &l2, &l3, "L2"},
		{-1.0, &l2, &l3, "L1"},
		{1.0, nil, &l3, "L3 (L4 candidate)"},
		{0.0, nil, &l3, "unplaced (below L3 line)"},
		{0.0, &l2, nil, "L2 or above"},
		{-1.0, &l2, nil, "L1"},
	}
	for _, c := range cases {
		got := Place(c.theta, c.l2, c.l3)
		if got != c.want {
			t.Errorf("Place(%v) = %q want %q", c.theta, got, c.want)
		}
		if strings.Contains(got, "L4") && got != "L3 (L4 candidate)" || strings.Contains(got, "L5") {
			t.Errorf("placement %q seats above L3", got)
		}
	}
}

func TestRatingMapping(t *testing.T) {
	if CodeRatingBase != 1500 || CodeRatingPerTheta != 200 {
		t.Fatalf("rating constants changed: %v %v", CodeRatingBase, CodeRatingPerTheta)
	}
	if r := CodeRating(1.25); r != 1750 {
		t.Errorf("CodeRating(1.25) = %v", r)
	}
	if rd := CodeRD(0.01); rd != RDMin {
		t.Errorf("tiny se -> %v want %v", rd, RDMin)
	}
	if rd := CodeRD(5); rd != RDMax {
		t.Errorf("huge se -> %v want %v", rd, RDMax)
	}
	if rd := CodeRD(0.5); math.Abs(rd-100) > 1e-9 {
		t.Errorf("se 0.5 -> %v want 100", rd)
	}
}

func writeSim(t *testing.T) string {
	t.Helper()
	var b strings.Builder
	b.WriteString(strings.Join(matrixHeader, "\t") + "\n")
	for _, r := range simRows(simThetas, simItems, 0.01) {
		fields := map[string]string{
			"vendor": r.Vendor, "vendor_cli": r.VendorCLI, "fleet_model": r.Model,
			"capability_area": r.Area, "benchmark": r.Benchmark, "metric": r.Metric,
			"score": r.Score, "agent_tool_or_harness": r.Harness, "source_type": r.SourceType,
		}
		var cols []string
		for _, h := range matrixHeader {
			cols = append(cols, fields[h])
		}
		b.WriteString(strings.Join(cols, "\t") + "\n")
	}
	p := filepath.Join(t.TempDir(), "sim.tsv")
	if err := os.WriteFile(p, []byte(b.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func runMain(args ...string) (int, string, string) {
	var out, errb bytes.Buffer
	code := Main(args, &out, &errb)
	return code, out.String(), errb.String()
}

func TestMainDeterministic(t *testing.T) {
	p := writeSim(t)
	for _, format := range []string{"md", "tsv"} {
		c1, o1, e1 := runMain("--matrix", p, "--format", format, "--l2-line", "-0.5", "--l3-line", "0.8")
		c2, o2, _ := runMain("--matrix", p, "--format", format, "--l2-line", "-0.5", "--l3-line", "0.8")
		if c1 != 0 || c2 != 0 {
			t.Fatalf("%s: exit %d/%d stderr %s", format, c1, c2, e1)
		}
		if o1 != o2 {
			t.Fatalf("%s: two runs differ", format)
		}
		if !strings.Contains(o1, "1500 + 200") {
			t.Errorf("%s: rating mapping not stated in output header", format)
		}
		if !strings.Contains(o1, "L3 (L4 candidate)") || strings.Contains(o1, "| L4 |") {
			t.Errorf("%s: placement column wrong", format)
		}
	}
	_, md, _ := runMain("--matrix", p)
	for _, want := range []string{"coding-debug-test", "tool-use-agentic", "## Items", "## Offsets", "## Skipped rows", "## Models", "unplaced"} {
		if !strings.Contains(md, want) {
			t.Errorf("markdown missing %q", want)
		}
	}
	if strings.Index(md, "model-h") > strings.Index(md, "model-a |") {
		t.Errorf("model table not sorted by theta desc")
	}
}

func TestMainOrderFile(t *testing.T) {
	p := writeSim(t)
	// Declare the reverse of the truth within one generation: e before h.
	o := filepath.Join(t.TempDir(), "order.tsv")
	os.WriteFile(o, []byte("vendor-e\tgen-1\tmodel-e>model-h\n"), 0o644)
	// model-e and model-h have different vendors in the sim, so the chain
	// is not the data's to declare: nothing may move.
	code, out, errs := runMain("--matrix", p, "--order", o, "--format", "md")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errs)
	}
	if !strings.Contains(out, "## Order constraints") || !strings.Contains(out, "vendor mismatch") {
		t.Errorf("order report missing vendor mismatch:\n%s", out)
	}
}

func TestMainUsageErrors(t *testing.T) {
	if c, _, _ := runMain(); c != 2 {
		t.Errorf("no --matrix: exit %d want 2", c)
	}
	if c, _, _ := runMain("--matrix", "x", "--format", "html"); c != 2 {
		t.Errorf("bad --format: exit %d want 2", c)
	}
	if c, _, _ := runMain("--matrix", "x", "--l2-line", "1", "--l3-line", "0"); c != 2 {
		t.Errorf("l2 above l3: exit %d want 2", c)
	}
	if c, _, _ := runMain("--matrix", filepath.Join(t.TempDir(), "missing.tsv")); c != 1 {
		t.Errorf("missing file: exit %d want 1", c)
	}
}
