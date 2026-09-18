package capability

import (
	"testing"

	"github.com/qiangli/yoke/pkg/fleet/fleettest"
)

// withTempStore fences the matrix onto a scratch dir and pins the registry
// the seed reads to the test ring, so the seeded rows are the ring's agents
// and not whatever the developer's own store holds.
func withTempStore(t *testing.T) {
	t.Helper()
	fleettest.Ring(t)
	t.Setenv("BASHY_CAPABILITY_DIR", t.TempDir())
}

func TestSeedAndLoad(t *testing.T) {
	withTempStore(t)
	m, err := Load() // first Load seeds priors
	if err != nil {
		t.Fatal(err)
	}
	if len(m.Agents) == 0 {
		t.Fatal("seed produced no agents")
	}
	if _, ok := m.Agents["opencode:kimi-k2.7-code"]; !ok {
		t.Fatalf("expected seeded agent opencode:kimi-k2.7-code; got %v", keys(m.Agents))
	}
	// Every seeded agent must have every canonical capability.
	for agent, row := range m.Agents {
		for _, c := range AllCaps() {
			if _, ok := row[c]; !ok {
				t.Fatalf("%s missing capability %s", agent, c)
			}
		}
	}
}

func TestFactorization(t *testing.T) {
	withTempStore(t)
	m, _ := Load()
	// Same model, different tool → close QUALITY column. The matrix factorizes:
	// quality is a property of the MODEL, harness is a property of the TOOL, and
	// the same model behind two different tools must not read as two different
	// models.
	//
	// This used to compare aider against opencode. aider was RETIRED from the fleet
	// (it has no file discovery, so it cannot be handed a task — see
	// baseline/tools/aider.yaml), which left it with no agent bindings and this
	// assertion reading 0.00 for an agent that no longer exists. The property is
	// unchanged; it is now checked against two tools that are actually staffed.
	yd := m.Agents["ycode:deepseek-v4-pro"][CapCoding].Quality
	od := m.Agents["opencode:deepseek-v4-pro"][CapCoding].Quality
	if diff := yd - od; diff > 0.06 || diff < -0.06 {
		t.Errorf("same-model coding quality should be close: ycode=%.2f opencode=%.2f", yd, od)
	}
	// Same tool, different model → HARNESS column (operability) identical.
	ok := m.Agents["opencode:kimi-k2.7-code"][CapOperability].Quality
	od2 := m.Agents["opencode:deepseek-v4-pro"][CapOperability].Quality
	if ok != od2 {
		t.Errorf("same-tool operability should match: %.2f vs %.2f", ok, od2)
	}
	// gemini should lead web-search among seeded agents.
	best := m.Best(CapWebSearch, false, ByQuality)
	if len(best) == 0 || ToolOf(best[0].Agent) != "agy" {
		t.Errorf("expected agy to lead web-search, got %v", best)
	}
}

func TestBestRoutableFilter(t *testing.T) {
	withTempStore(t)
	m, _ := Load()
	// With routableOnly, every returned agent's tool must be operable here.
	for _, r := range m.Best(CapCoding, true, ByQuality) {
		if ok, _ := Operable(ToolOf(r.Agent)); !ok {
			t.Errorf("non-routable agent %s returned under routableOnly", r.Agent)
		}
	}
}

func TestRecordMovesQualityAndPersists(t *testing.T) {
	withTempStore(t)
	NowRFC = func() string { return "2026-07-08T00:00:00Z" }
	m, _ := Load()
	before := m.Agents["opencode:kimi-k2.7-code"][CapCoding].Quality
	// Two failures should pull quality DOWN and flip source to host.
	if err := Record("opencode:kimi-k2.7-code", CapCoding, false, 1200, 50, NowRFC()); err != nil {
		t.Fatal(err)
	}
	if err := Record("opencode:kimi-k2.7-code", CapCoding, false, 0, 0, NowRFC()); err != nil {
		t.Fatal(err)
	}
	m2, _ := Load() // reload from disk → persistence check
	cell := m2.Agents["opencode:kimi-k2.7-code"][CapCoding]
	if cell.Source != SourceHost {
		t.Errorf("source should be host after Record, got %s", cell.Source)
	}
	if cell.Samples != 2 {
		t.Errorf("samples should be 2, got %d", cell.Samples)
	}
	if cell.Quality >= before {
		t.Errorf("two failures should lower quality: before=%.2f after=%.2f", before, cell.Quality)
	}
	if cell.LatencyMS != 1200 {
		t.Errorf("latency not recorded: %d", cell.LatencyMS)
	}
}

func TestParseCapability(t *testing.T) {
	for in, want := range map[string]Capability{
		"research": CapDeepResearch, "WEB": CapWebSearch, "code-review": CapCodeReview,
		"coding": CapCoding, "judge": CapCodeReview,
	} {
		if got, ok := ParseCapability(in); !ok || got != want {
			t.Errorf("ParseCapability(%q) = %q,%v want %q", in, got, ok, want)
		}
	}
	if _, ok := ParseCapability("nonsense"); ok {
		t.Error("expected unknown capability to fail")
	}
}

func keys(m map[string]map[Capability]Cell) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func TestCostSeededAndValueRanking(t *testing.T) {
	withTempStore(t)
	NowRFC = func() string { return "2026-07-09T00:00:00Z" }
	m, _ := Load()
	// cost is seeded per model tier: premium codex > commodity opencode.
	if m.Agents["codex:gpt-5.5"][CapCoding].CostMicro <= m.Agents["opencode:deepseek-v4-pro"][CapCoding].CostMicro {
		t.Errorf("premium model should cost more than commodity")
	}
	// codex leads coding by QUALITY (meeting correction).
	byQ := m.Best(CapCoding, false, ByQuality)
	if len(byQ) == 0 {
		t.Fatal("expected quality ranking to return seeded agents")
	}
	if ToolOf(byQ[0].Agent) != "codex" {
		t.Errorf("codex should lead coding by quality, got %s", byQ[0].Agent)
	}
	// After codex operability tanks, its VALUE should drop (reliability/rework +
	// dishwasher): a cheaper, reliable commodity agent should out-value it.
	_ = RecordOperability("codex", false)
	_ = RecordOperability("codex", false)
	m2, _ := Load()
	byV := m2.Best(CapCoding, false, ByValue)
	if len(byV) == 0 {
		t.Fatal("expected value ranking to return seeded agents")
	}
	if ToolOf(byV[0].Agent) == "codex" {
		t.Errorf("flaky+premium codex should not top coding by VALUE, got %s (val=%.2f)", byV[0].Agent, byV[0].Value)
	}
}

func TestRecordOperabilityAllToolRows(t *testing.T) {
	withTempStore(t)
	NowRFC = func() string { return "2026-07-09T00:00:00Z" }
	_ = RecordOperability("opencode", false)
	m, _ := Load()
	for _, agent := range m.AgentsForTool("opencode") {
		cell := m.Agents[agent][CapOperability]
		if cell.Source != SourceHost {
			t.Errorf("%s operability should be host-measured after RecordOperability", agent)
		}
	}
}
