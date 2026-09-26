package stats

import (
	"bytes"
	"encoding/json"
	"math"
	"strings"
	"testing"
)

// Fixture: arms A and B, K=2 attempts per instance, i5 attempted by B only;
// A carries a cost of 0.5 per attempt. Reference values were computed
// independently (Python statistics/math.comb).
const fixture = `
{"instance_id":"i1","agent":"A","resolved":true,"cost":0.5}
{"instance_id":"i1","agent":"A","resolved":true,"cost":0.5}
{"instance_id":"i2","agent":"A","resolved":true,"cost":0.5}
{"instance_id":"i2","agent":"A","resolved":false,"cost":0.5}
{"instance_id":"i3","agent":"A","resolved":false,"cost":0.5}
{"instance_id":"i3","agent":"A","resolved":0,"cost":0.5}
{"instance_id":"i4","agent":"A","resolved":"fail","cost":0.5}
{"instance_id":"i4","agent":"A","resolved":false,"cost":0.5}
{"instance_id":"i1","agent":"B","resolved":true}
{"instance_id":"i1","agent":"B","resolved":1}
{"instance_id":"i2","agent":"B","resolved":"resolved"}
{"instance_id":"i2","agent":"B","resolved":true}

{"instance_id":"i3","agent":"B","resolved":true}
{"instance_id":"i3","agent":"B","resolved":false}
{"instance_id":"i4","agent":"B","resolved":false}
{"instance_id":"i4","agent":"B","resolved":false}
{"instance_id":"i5","agent":"B","resolved":true}
`

func near(t *testing.T, name string, got, want float64) {
	t.Helper()
	if math.Abs(got-want) > 1e-9 {
		t.Errorf("%s = %.12f, want %.12f", name, got, want)
	}
}

func readFixture(t *testing.T) []Attempt {
	t.Helper()
	a, err := Read(strings.NewReader(fixture), Fields{Instance: "instance_id", Arm: "agent", Outcome: "resolved", Cost: "cost"})
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func TestSummarizeMatchesReference(t *testing.T) {
	rows := Summarize(readFixture(t))
	if len(rows) != 2 || rows[0].Arm != "A" || rows[1].Arm != "B" {
		t.Fatalf("arms = %+v", rows)
	}
	a, b := rows[0], rows[1]
	if a.Instances != 4 || a.Attempts != 8 || a.Passes != 3 || b.Instances != 5 || b.Attempts != 9 || b.Passes != 6 {
		t.Fatalf("counts a=%+v b=%+v", a, b)
	}
	near(t, "A rate", a.Rate.Mean, 0.375)
	near(t, "A se", a.Rate.SE, 0.23935677693908453)
	near(t, "A low", a.Rate.Low, -0.09413066225619304)
	near(t, "B rate", b.Rate.Mean, 0.7)
	near(t, "B se", b.Rate.SE, 0.2)
	near(t, "B high", b.Rate.High, 1.0919927969080108)
	if a.TotalCost == nil || a.CostPerSolve == nil {
		t.Fatal("A cost missing")
	}
	near(t, "A total cost", *a.TotalCost, 4)
	near(t, "A cost per solve", *a.CostPerSolve, 4.0/3)
	if b.TotalCost != nil {
		t.Fatalf("B has no cost field, got %v", *b.TotalCost)
	}
}

func TestPairedMatchesReferenceAndBeatsUnpaired(t *testing.T) {
	p, err := ComparePaired(readFixture(t), "A", "B")
	if err != nil {
		t.Fatal(err)
	}
	if p.Shared != 4 || p.OnlyA != 0 || p.OnlyB != 1 || p.Wins != 2 || p.Losses != 0 {
		t.Fatalf("paired counts %+v", p)
	}
	near(t, "rate A", p.RateA, 0.375)
	near(t, "rate B (shared)", p.RateB, 0.625)
	near(t, "diff", p.Diff.Mean, 0.25)
	near(t, "diff se", p.Diff.SE, 0.14433756729740643)
	near(t, "diff low", p.Diff.Low, -0.03289643351904292)
	near(t, "diff high", p.Diff.High, 0.5328964335190429)
	near(t, "unpaired se", p.UnpairedSE, 0.338501600193165)
	if p.Corr == nil {
		t.Fatal("correlation missing")
	}
	near(t, "corr", *p.Corr, 0.8181818181818182)
	if !(p.Diff.SE < p.UnpairedSE) {
		t.Fatalf("paired SE %.3f should be narrower than unpaired %.3f on correlated arms", p.Diff.SE, p.UnpairedSE)
	}
	if _, err := ComparePaired(readFixture(t), "A", "C"); err == nil {
		t.Fatal("unknown arm must be an error")
	}
}

func TestPassKMatchesReference(t *testing.T) {
	rows, err := ComputePassK(readFixture(t), 2)
	if err != nil {
		t.Fatal(err)
	}
	near(t, "A pass^2", rows[0].PassHatK, 0.25)
	near(t, "A pass@2", rows[0].PassAtK, 0.5)
	near(t, "B pass^2", rows[1].PassHatK, 0.5)
	near(t, "B pass@2", rows[1].PassAtK, 0.75)
	if rows[1].Excluded != 1 || rows[1].Instances != 4 {
		t.Fatalf("B instances/excluded = %d/%d, want 4/1", rows[1].Instances, rows[1].Excluded)
	}
	// k=1: pass^1 == pass@1 == the resolve rate.
	one, _ := ComputePassK(readFixture(t), 1)
	near(t, "A pass^1", one[0].PassHatK, 0.375)
	near(t, "A pass@1", one[0].PassAtK, 0.375)
}

func TestReadRefusesBadRows(t *testing.T) {
	f := Fields{Instance: "instance_id", Arm: "agent", Outcome: "resolved"}
	for name, input := range map[string]string{
		"not json":        "{nope}\n",
		"missing outcome": `{"instance_id":"i1","agent":"A"}` + "\n",
		"bad outcome":     `{"instance_id":"i1","agent":"A","resolved":0.5}` + "\n",
		"missing arm":     `{"instance_id":"i1","resolved":true}` + "\n",
	} {
		if _, err := Read(strings.NewReader(input), f); err == nil {
			t.Errorf("%s: want an error", name)
		}
	}
}

func TestCommandJSONEnvelope(t *testing.T) {
	cmd := NewCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetIn(strings.NewReader(fixture))
	cmd.SetArgs([]string{"paired", "--arm", "agent", "--a", "A", "--b", "B", "--json"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	var env struct {
		Schema string `json:"schema_version"`
		Paired Paired `json:"paired"`
	}
	if err := json.Unmarshal(out.Bytes(), &env); err != nil || env.Schema != "bashy-stats-paired-v1" || env.Paired.Shared != 4 {
		t.Fatalf("envelope %q err=%v", out.String(), err)
	}
}
