package ladder

import (
	"testing"
)

func panelTestPool() []PanelMember {
	return []PanelMember{
		{Agent: "agent-a", Vendor: "vendor-a", Judge: DutyStanding{R: 1700, RD: 50, Events: 20}, Manage: DutyStanding{R: 1700, RD: 50, Events: 20}},
		{Agent: "agent-b", Vendor: "vendor-b", Judge: DutyStanding{R: 1650, RD: 50, Events: 20}, Manage: DutyStanding{R: 1650, RD: 50, Events: 20}},
		{Agent: "agent-c", Vendor: "vendor-c", Judge: DutyStanding{R: 1600, RD: 50, Events: 20}, Manage: DutyStanding{R: 1600, RD: 50, Events: 20}},
		{Agent: "agent-d", Vendor: "vendor-a", Judge: DutyStanding{R: 1550, RD: 50, Events: 20}, Manage: DutyStanding{R: 1550, RD: 50, Events: 20}},
		{Agent: "agent-e", Vendor: "vendor-b", Judge: DutyStanding{R: 1500, RD: 50, Events: 20}, Manage: DutyStanding{R: 1500, RD: 50, Events: 20}},
		{Agent: "agent-f", Vendor: "vendor-c", Judge: DutyStanding{R: 1450, RD: 50, Events: 20}, Manage: DutyStanding{R: 1450, RD: 50, Events: 20}},
	}
}

func TestPanelSize(t *testing.T) {
	cases := []struct {
		name string
		c    PanelCase
		want int
	}{
		{"low-stakes", PanelCase{ID: "c1", Kind: "low-stakes", Points: 1}, 1},
		{"low-stakes 2pt", PanelCase{ID: "c2", Kind: "low-stakes", Points: 2}, 1},
		{"design", PanelCase{ID: "c3", Kind: "design", Points: 1}, 3},
		{"merge small", PanelCase{ID: "c4", Kind: "merge", Points: 2}, 1},
		{"merge 5pt", PanelCase{ID: "c5", Kind: "merge", Points: 5}, 3},
		{"merge 8pt", PanelCase{ID: "c6", Kind: "merge", Points: 8}, 3},
		{"low-confidence single", PanelCase{ID: "c7", Kind: "low-stakes", Points: 1, LowConfidence: true}, 3},
		{"dispute", PanelCase{ID: "c8", Kind: "dispute", Points: 1}, 5},
		{"escalated", PanelCase{ID: "c9", Kind: "escalated", Points: 8}, 5},
	}
	for _, tc := range cases {
		if got := PanelSize(tc.c); got != tc.want {
			t.Errorf("%s: PanelSize = %d, want %d", tc.name, got, tc.want)
		}
		if got := PanelSize(tc.c); got%2 == 0 {
			t.Errorf("%s: PanelSize = %d, want odd", tc.name, got)
		}
	}
}

func TestPanelDrawSingleExcludesAuthorVendor(t *testing.T) {
	pool := panelTestPool()
	c := PanelCase{ID: "c1", Kind: "low-stakes", Points: 1, AuthorVendor: "vendor-a"}
	got, err := PanelDraw(c, pool, 42)
	if err != nil {
		t.Fatalf("PanelDraw: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("len = %d, want 1", len(got))
	}
	if got[0].Vendor == "vendor-a" {
		t.Errorf("single judge shares author vendor: %+v", got[0])
	}
}

func TestPanelDrawThreeVendors(t *testing.T) {
	pool := panelTestPool()
	c := PanelCase{ID: "c3", Kind: "design", Points: 3, AuthorVendor: "vendor-a"}
	got, err := PanelDraw(c, pool, 7)
	if err != nil {
		t.Fatalf("PanelDraw: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("len = %d, want 3", len(got))
	}
	vendors := map[string]bool{}
	for _, m := range got {
		vendors[m.Vendor] = true
		if m.Vendor == "vendor-a" {
			t.Errorf("author vendor should be excluded when enough members remain: %+v", m)
		}
	}
	if len(vendors) < 2 {
		t.Errorf("want >= 2 vendors, got %v", vendors)
	}
	// Sorted by judge conservative rating, descending.
	for i := 1; i < len(got); i++ {
		if got[i].Judge.Lower() > got[i-1].Judge.Lower() {
			t.Errorf("not sorted by conservative rating: %+v", got)
			break
		}
	}
}

func TestPanelDrawIncludesAuthorVendorWhenScarce(t *testing.T) {
	pool := []PanelMember{
		{Agent: "agent-a", Vendor: "vendor-a", Judge: DutyStanding{R: 1700, RD: 50, Events: 20}},
		{Agent: "agent-b", Vendor: "vendor-a", Judge: DutyStanding{R: 1650, RD: 50, Events: 20}},
		{Agent: "agent-c", Vendor: "vendor-b", Judge: DutyStanding{R: 1600, RD: 50, Events: 20}},
	}
	c := PanelCase{ID: "c3", Kind: "design", Points: 3, AuthorVendor: "vendor-a"}
	got, err := PanelDraw(c, pool, 7)
	if err != nil {
		t.Fatalf("PanelDraw: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("len = %d, want 3", len(got))
	}
	vendors := map[string]bool{}
	for _, m := range got {
		vendors[m.Vendor] = true
	}
	if len(vendors) < 2 {
		t.Errorf("want >= 2 vendors even when author vendor included, got %v", vendors)
	}
}

func TestPanelDrawDominanceSkip(t *testing.T) {
	pool := []PanelMember{
		{Agent: "agent-a", Vendor: "vendor-a", Judge: DutyStanding{R: 1700, RD: 50, Events: 20}, Manage: DutyStanding{R: 1400, RD: 50, Events: 20}},
		{Agent: "agent-b", Vendor: "vendor-b", Judge: DutyStanding{R: 1650, RD: 50, Events: 20}, Manage: DutyStanding{R: 1800, RD: 50, Events: 20}},
		{Agent: "agent-c", Vendor: "vendor-c", Judge: DutyStanding{R: 1600, RD: 50, Events: 20}, Manage: DutyStanding{R: 1800, RD: 50, Events: 20}},
		{Agent: "agent-d", Vendor: "vendor-d", Judge: DutyStanding{R: 1550, RD: 50, Events: 20}, Manage: DutyStanding{R: 1800, RD: 50, Events: 20}},
	}
	// Manager verdict: judged manager at R 1600; agent-a manages 1400-100=1300 < 1600.
	c := PanelCase{ID: "c1", Kind: "low-stakes", Points: 1, AuthorVendor: "vendor-z", JudgedManage: DutyStanding{R: 1600, RD: 60, Events: 20}}
	got, err := PanelDraw(c, pool, 1)
	if err != nil {
		t.Fatalf("PanelDraw: %v", err)
	}
	if got[0].Agent == "agent-a" {
		t.Errorf("ineligible judge by dominance was drawn: %+v", got[0])
	}
}

func TestPanelDrawNotEnough(t *testing.T) {
	pool := []PanelMember{
		{Agent: "agent-a", Vendor: "vendor-a", Judge: DutyStanding{R: 1700, RD: 50, Events: 20}},
	}
	c := PanelCase{ID: "c8", Kind: "dispute", Points: 1, AuthorVendor: "vendor-z"}
	if _, err := PanelDraw(c, pool, 1); err == nil {
		t.Error("want error when not enough eligible members, got nil")
	}
	// Single judge sharing the only vendor must also fail, never shrink.
	solo := PanelCase{ID: "c1", Kind: "low-stakes", Points: 1, AuthorVendor: "vendor-a"}
	if _, err := PanelDraw(solo, pool, 1); err == nil {
		t.Error("want error when single judge would share author vendor, got nil")
	}
}

func TestPanelDrawDeterministic(t *testing.T) {
	pool := panelTestPool()
	c := PanelCase{ID: "c3", Kind: "design", Points: 3, AuthorVendor: "vendor-a"}
	first, err := PanelDraw(c, pool, 99)
	if err != nil {
		t.Fatalf("PanelDraw: %v", err)
	}
	for i := 0; i < 10; i++ {
		again, err := PanelDraw(c, pool, 99)
		if err != nil {
			t.Fatalf("PanelDraw: %v", err)
		}
		if len(again) != len(first) {
			t.Fatalf("nondeterministic length")
		}
		for j := range first {
			if again[j].Agent != first[j].Agent {
				t.Fatalf("nondeterministic draw: %+v vs %+v", first, again)
			}
		}
	}
}

func TestPanelVerdictMajority(t *testing.T) {
	votes := []Vote{
		{Agent: "agent-a", Verdict: "accept", Confidence: 0.9},
		{Agent: "agent-b", Verdict: "accept", Confidence: 0.8},
		{Agent: "agent-c", Verdict: "reject", Confidence: 0.7},
	}
	r := PanelVerdict(votes)
	if r.Verdict != "accept" {
		t.Errorf("Verdict = %q, want accept", r.Verdict)
	}
	if r.ToOwner {
		t.Errorf("ToOwner = true, want false for confident majority")
	}
	if r.Tally["accept"] != 2 || r.Tally["reject"] != 1 {
		t.Errorf("Tally = %v", r.Tally)
	}
}

func TestPanelVerdictThreeWaySplit(t *testing.T) {
	votes := []Vote{
		{Agent: "agent-a", Verdict: "accept", Confidence: 0.9},
		{Agent: "agent-b", Verdict: "reject", Confidence: 0.9},
		{Agent: "agent-c", Verdict: "rework", Confidence: 0.9},
	}
	r := PanelVerdict(votes)
	if !r.ToOwner {
		t.Errorf("ToOwner = false, want true for 1-1-1 split")
	}
	if r.Verdict != "" {
		t.Errorf("Verdict = %q, want empty on split", r.Verdict)
	}
}

func TestPanelVerdictLowConfidenceSplit(t *testing.T) {
	votes := []Vote{
		{Agent: "agent-a", Verdict: "accept", Confidence: 0.4},
		{Agent: "agent-b", Verdict: "accept", Confidence: 0.3},
		{Agent: "agent-c", Verdict: "reject", Confidence: 0.9},
	}
	r := PanelVerdict(votes)
	if !r.ToOwner {
		t.Errorf("ToOwner = false, want true for low-confidence 2-1 split")
	}
	// Confident 2-1 stands.
	votes[0].Confidence, votes[1].Confidence = 0.8, 0.7
	r = PanelVerdict(votes)
	if r.ToOwner || r.Verdict != "accept" {
		t.Errorf("confident 2-1 should stand: %+v", r)
	}
}

func TestPanelVerdictSingle(t *testing.T) {
	r := PanelVerdict([]Vote{{Agent: "agent-a", Verdict: "accept", Confidence: 0.9}})
	if r.Verdict != "accept" || r.ToOwner {
		t.Errorf("confident single should stand: %+v", r)
	}
	r = PanelVerdict([]Vote{{Agent: "agent-a", Verdict: "accept", Confidence: 0.2}})
	if !r.ToOwner {
		t.Errorf("low-confidence single should go to owner: %+v", r)
	}
	if r.Reason != "escalate to 3" {
		t.Errorf("Reason = %q, want %q", r.Reason, "escalate to 3")
	}
}

func TestIsPlantedRate(t *testing.T) {
	if PlantedRate != 0.10 {
		t.Errorf("PlantedRate = %v, want 0.10", PlantedRate)
	}
	const n = 10000
	hits := 0
	for i := 0; i < n; i++ {
		id := string(rune('a'+i%26)) + string(rune('0'+(i/26)%10)) + "-" + string(rune(i%256)) + "-" + string(rune((i*7919)%1000))
		if IsPlanted(id, 12345) {
			hits++
		}
	}
	rate := float64(hits) / n
	if rate < 0.07 || rate > 0.13 {
		t.Errorf("planted rate = %v (%d/%d), want ~0.10", rate, hits, n)
	}
	// Deterministic.
	if IsPlanted("case-1", 99) != IsPlanted("case-1", 99) {
		t.Error("IsPlanted not deterministic")
	}
}

func TestPanelStripIdentity(t *testing.T) {
	got := panelStripIdentity("agent-a wrote this and agent-b reviewed", []string{"agent-a", "agent-b"})
	want := "author wrote this and author reviewed"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}
