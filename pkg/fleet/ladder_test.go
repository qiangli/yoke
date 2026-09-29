package fleet

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"testing/fstest"
)

// The fixtures below name no vendor: the ladder is pure mechanism, so its tests
// run on made-up tool/model names and made-up lines.

func line(f float64) *float64 { return &f }

// testLadder places every absolute line at 1500 in season 10.
func testLadder() Ladder {
	return Ladder{Season: 10, Lines: LadderLines{L3Code: line(1500), L4Manage: line(1500), L5Judge: line(1500)}}
}

var testModel = Model{Name: "m1", Version: "1"}

func certs(season int, names ...string) []Certificate {
	out := make([]Certificate, len(names))
	for i, n := range names {
		out[i] = Certificate{Name: n, Season: season, Model: testModel.Name, Version: testModel.Version}
	}
	return out
}

func rated(r, rd float64, events int) *DutyRating { return &DutyRating{R: r, RD: rd, Events: events} }

func entry(name string, ratings *DutyRatings, cs []Certificate) LadderEntry {
	return LadderEntry{Agent: Agent{Name: name, Tool: "t1", Model: testModel.Name, Ratings: ratings, Certificates: cs}, Model: testModel, Resolved: true}
}

// l3Entry holds every G1–G3 condition with an established code rating whose
// conservative bound (r − 2·RD) is r − 100.
func l3Entry(name string, code float64) LadderEntry {
	return entry(name, &DutyRatings{Code: rated(code, 50, 10)}, certs(10, CertL1, CertL2, CertL3, CertSteer))
}

func standingOf(t *testing.T, ss []Standing, name string) Standing {
	t.Helper()
	for _, s := range ss {
		if s.Agent == name {
			return s
		}
	}
	t.Fatalf("no standing for %s", name)
	return Standing{}
}

func hasCheck(s Standing, gate int, need string, ok bool) bool {
	for _, c := range s.Checks {
		if c.Gate == gate && strings.Contains(c.Need, need) && c.OK == ok {
			return true
		}
	}
	return false
}

// The ladder is CUMULATIVE: an agent holding every L4 condition but missing
// the L2 certificate is L1, not L4. A lapse below caps everything above.
func TestDerivedBandIsCappedByTheLowestFailingGate(t *testing.T) {
	cs := certs(10, CertL1, CertL3, CertSteer, CertManager, CertReview)
	a := entry("a", &DutyRatings{Code: rated(1800, 50, 20), Manage: rated(1800, 50, 20)}, cs)
	s := DeriveStandings([]LadderEntry{a}, testLadder())[0]
	if s.Derived != 1 {
		t.Fatalf("derived = %d, want 1 (the l2 certificate is missing)", s.Derived)
	}
	if got := s.MissingGates(); len(got) != 1 || got[0] != "G2" {
		t.Fatalf("missing gates = %v, want [G2]", got)
	}
	if s.Band != 1 || s.Source != BandDerived {
		t.Fatalf("effective = L%d %q, want L1 derived (no seed)", s.Band, s.Source)
	}
}

func TestG3NeedsCertificatesAndAConservativeCodeRatingOverTheLine(t *testing.T) {
	cases := []struct {
		name string
		e    LadderEntry
		want int
		miss string
	}{
		{"holds", l3Entry("a", 1700), 3, ""},
		// r 1590 − 2·50 = 1490 < 1500: the POINT rating clears the line, the
		// conservative bound does not.
		{"conservative-below-line", l3Entry("a", 1590), 2, "conservative code ≥ L3 line"},
		{"not-established", entry("a", &DutyRatings{Code: rated(1900, 50, EstablishedEvents-1)}, certs(10, CertL1, CertL2, CertL3, CertSteer)), 2, "conservative code"},
		{"unrated", entry("a", nil, certs(10, CertL1, CertL2, CertL3, CertSteer)), 2, "conservative code"},
		{"no-steer", entry("a", &DutyRatings{Code: rated(1900, 50, 20)}, certs(10, CertL1, CertL2, CertL3)), 2, "steer certificate"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := DeriveStandings([]LadderEntry{c.e}, testLadder())[0]
			if s.Derived != c.want {
				t.Fatalf("derived = %d, want %d; checks %v", s.Derived, c.want, s.Checks)
			}
			if c.miss != "" && !hasCheck(s, 3, c.miss, false) {
				t.Fatalf("want failed G3 check %q, got %v", c.miss, s.Missing())
			}
		})
	}
}

// An unplaced line fails closed: no agent can hold a gate nobody has placed.
func TestUnplacedLineFailsClosed(t *testing.T) {
	l := testLadder()
	l.Lines.L3Code = nil
	s := DeriveStandings([]LadderEntry{l3Entry("a", 2000)}, l)[0]
	if s.Derived != 2 {
		t.Fatalf("derived = %d, want 2 with no L3 line placed", s.Derived)
	}
	var detail string
	for _, c := range s.Missing() {
		detail += c.String()
	}
	if !strings.Contains(detail, "L3 line not placed") {
		t.Fatalf("missing = %q, want it to say the L3 line is not placed", detail)
	}
}

// G4 is relative: conservative code must reach the MEDIAN code of the
// established L3s, so the gate tightens as the fleet improves.
func TestG4ComparesAgainstTheMedianOfEstablishedL3s(t *testing.T) {
	l4 := func(name string, code float64) LadderEntry {
		e := l3Entry(name, code)
		e.Agent.Ratings.Manage = rated(1700, 50, 10)
		e.Agent.Certificates = append(e.Agent.Certificates, certs(10, CertManager, CertReview)...)
		return e
	}
	// L3 conservative ratings 1550, 1700, 1600 → median 1600.
	fleet := []LadderEntry{
		l4("low", 1650),  // conservative 1550 < 1600
		l4("high", 1800), // conservative 1700 ≥ 1600
		l3Entry("mid", 1700),
	}
	ss := DeriveStandings(fleet, testLadder())
	if s := standingOf(t, ss, "high"); s.Derived != 4 {
		t.Fatalf("high derived = %d, want 4; missing %v", s.Derived, s.Missing())
	}
	low := standingOf(t, ss, "low")
	if low.Derived != 3 || !hasCheck(low, 4, "L3 median", false) {
		t.Fatalf("low derived = %d, want 3 failing the L3 median; missing %v", low.Derived, low.Missing())
	}

	// The same agent passes in a weaker fleet: the line is relative.
	weaker := []LadderEntry{l4("low", 1650), l3Entry("mid", 1600), l3Entry("mid2", 1600)}
	if s := standingOf(t, DeriveStandings(weaker, testLadder()), "low"); s.Derived != 4 {
		t.Fatalf("in a weaker fleet low derived = %d, want 4; missing %v", s.Derived, s.Missing())
	}
}

// An ephemeral clone carries a copy of its parent's record; counting it would
// let one agent vote twice on the median.
func TestEphemeralClonesDoNotMoveTheMedian(t *testing.T) {
	cand := l3Entry("cand", 1700)
	cand.Agent.Ratings.Manage = rated(1700, 50, 10)
	cand.Agent.Certificates = append(cand.Agent.Certificates, certs(10, CertManager, CertReview)...)
	clone1, clone2 := l3Entry("c1", 2000), l3Entry("c2", 2000)
	clone1.Agent.Ephemeral, clone2.Agent.Ephemeral = true, true
	ss := DeriveStandings([]LadderEntry{cand, l3Entry("peer", 1600), clone1, clone2}, testLadder())
	if s := standingOf(t, ss, "cand"); s.Derived != 4 {
		t.Fatalf("cand derived = %d, want 4 (clones must not raise the median); missing %v", s.Derived, s.Missing())
	}
}

func TestG5NeedsEveryDutyAtTheL4MedianAndTheJudgeLine(t *testing.T) {
	top := l3Entry("top", 1800)
	top.Agent.Ratings.Manage = rated(1800, 50, 10)
	top.Agent.Ratings.Judge = rated(1700, 50, 10)
	top.Agent.Certificates = append(top.Agent.Certificates, certs(10, CertManager, CertReview, CertJudge, CertL5)...)
	if s := DeriveStandings([]LadderEntry{top}, testLadder())[0]; s.Derived != MaxBand {
		t.Fatalf("derived = %d, want %d; missing %v", s.Derived, MaxBand, s.Missing())
	}
	top.Agent.Ratings.Judge = rated(1550, 50, 10) // conservative 1450 < 1500
	s := DeriveStandings([]LadderEntry{top}, testLadder())[0]
	if s.Derived != 4 || !hasCheck(s, 5, "L5 line", false) {
		t.Fatalf("derived = %d, want 4 failing the L5 judge line; missing %v", s.Derived, s.Missing())
	}
}

func TestCertificateExpiresAfterThreeSeasons(t *testing.T) {
	e := entry("a", nil, certs(7, CertL1))
	for season, want := range map[int]int{7: 1, 9: 1, 10: 0, 0: 1} {
		l := Ladder{Season: season}
		if s := DeriveStandings([]LadderEntry{e}, l)[0]; s.Derived != want {
			t.Errorf("season %d: derived = %d, want %d (%v)", season, s.Derived, want, s.Certificates)
		}
	}
}

func TestModelChangeVoidsCertificates(t *testing.T) {
	cases := map[string]Model{
		"model":   {Name: "m2", Version: "1"},
		"version": {Name: testModel.Name, Version: "2"},
	}
	for name, m := range cases {
		t.Run(name, func(t *testing.T) {
			e := entry("a", nil, certs(10, CertL1))
			e.Model = m
			s := DeriveStandings([]LadderEntry{e}, testLadder())[0]
			if s.Derived != 0 || s.Certificates[0].Valid || !strings.HasPrefix(s.Certificates[0].Reason, "void") {
				t.Fatalf("derived %d, cert %+v; want the certificate void", s.Derived, s.Certificates[0])
			}
		})
	}
	// A certificate with no model recorded cannot be bound, so it counts for nothing.
	e := entry("a", nil, []Certificate{{Name: CertL1, Season: 10}})
	if s := DeriveStandings([]LadderEntry{e}, testLadder())[0]; s.Derived != 0 {
		t.Fatalf("an unbound certificate placed the agent at L%d", s.Derived)
	}
}

// A declared model peg is only a seed: it sets the band while evidence is
// missing, yields once the ratings its band gates on are established, and
// expires two seasons after it was set.
func TestDeclaredPegIsASeedThatYieldsAndExpires(t *testing.T) {
	m := testModel
	m.Band, m.BandSeason = 4, 9
	e := entry("a", nil, certs(10, CertL1))
	e.Model = m

	s := DeriveStandings([]LadderEntry{e}, testLadder())[0]
	if s.Band != 4 || s.Source != BandDeclared || s.Derived != 1 || !s.Seed.Active {
		t.Fatalf("seeded: band L%d %q derived L%d seed %+v; want L4 declared over derived L1", s.Band, s.Source, s.Derived, s.Seed)
	}
	// Missing gates run up to the seed's band, so the gap is visible.
	if got := strings.Join(s.MissingGates(), ","); got != "G2,G3,G4" {
		t.Fatalf("missing gates = %s, want G2,G3,G4", got)
	}

	// Established code and manage ratings: the declared prior yields.
	e.Agent.Ratings = &DutyRatings{Code: rated(1500, 50, 10), Manage: rated(1500, 50, 10)}
	s = DeriveStandings([]LadderEntry{e}, testLadder())[0]
	if s.Band != 1 || s.Source != BandDerived || s.Seed.Active {
		t.Fatalf("established: band L%d %q seed %+v; want derived L1", s.Band, s.Source, s.Seed)
	}

	// Expiry: seated season 9 holds through 10 and expires from 11.
	e.Agent.Ratings = nil
	l := testLadder()
	l.Season = 11
	s = DeriveStandings([]LadderEntry{e}, l)[0]
	if s.Seed.Active || !strings.HasPrefix(s.Seed.Reason, "expired") {
		t.Fatalf("season 11 seed = %+v, want expired", s.Seed)
	}
}

// Operator seats are rating seeds: once the target band's duties are
// established, evidence can speak. Provisional seats retain their bootstrap
// hold until confirmed or expired.
func TestOperatorSeatYieldsToEstablishedRatings(t *testing.T) {
	e := l3Entry("a", 1700)
	e.Agent.Ratings.Manage = rated(1500, 50, 10)
	e.Agent.Seat = &Seat{Band: 4, Source: BandOperator, Season: 9}
	s := DeriveStandings([]LadderEntry{e}, testLadder())[0]
	if s.Band != s.Derived || s.Source != BandDerived || s.Seed.Active || s.Seed.Reason != "yielded to established ratings" {
		t.Fatalf("band L%d %q derived L%d seed %+v; want operator seed yielded to derived band", s.Band, s.Source, s.Derived, s.Seed)
	}
}

func TestProvisionalSeatHoldsUntilExpiryOrConfirmation(t *testing.T) {
	e := l3Entry("a", 1700)
	e.Agent.Ratings.Manage = rated(1500, 50, 10)
	e.Agent.Ratings.Judge = rated(1500, 50, 10)
	e.Agent.Seat = &Seat{Band: 5, Source: SeatProvisional, Season: 9}
	s := DeriveStandings([]LadderEntry{e}, testLadder())[0]
	if s.Band != 5 || s.Source != SeatProvisional || s.Derived != 3 || !s.Seed.Active {
		t.Fatalf("band L%d %q derived L%d seed %+v; want provisional L5 held over derived L3", s.Band, s.Source, s.Derived, s.Seed)
	}
	l := testLadder()
	l.Season = 11
	// Seated in 9, the seat expires from 11; season-10 certificates still
	// hold, so the agent drops to its derived L3.
	if s := DeriveStandings([]LadderEntry{e}, l)[0]; s.Band != 3 || s.Seed.Active {
		t.Fatalf("season 11: band L%d seed %+v; want the provisional seat expired to derived L3", s.Band, s.Seed)
	}
	// A provisional seat also clears when the gates confirm it.
	e = l3Entry("a", 1700)
	e.Agent.Seat = &Seat{Band: 3, Source: SeatProvisional, Season: 10}
	if s := DeriveStandings([]LadderEntry{e}, testLadder())[0]; s.Source != BandDerived || s.Seed.Active {
		t.Fatalf("confirmed provisional seat: source %q seed %+v; want derived", s.Source, s.Seed)
	}
}

// Without a season on the peg the clock cannot run; the seed is reported as
// unclocked rather than silently aged or silently trusted.
func TestUnclockedSeedIsReported(t *testing.T) {
	m := testModel
	m.Band = 3
	e := entry("a", nil, nil)
	e.Model = m
	s := DeriveStandings([]LadderEntry{e}, testLadder())[0]
	if !s.Seed.Active || !strings.Contains(s.Seed.Reason, "unclocked") {
		t.Fatalf("seed = %+v, want active and unclocked", s.Seed)
	}
}

// A cascade's served band is its contract, not a peg: it never seeds.
func TestCascadeBandIsNotASeed(t *testing.T) {
	m := testModel
	m.Band, m.BandSource = 4, BandCascade
	e := entry("a", nil, nil)
	e.Model = m
	if s := DeriveStandings([]LadderEntry{e}, testLadder())[0]; s.Seed != nil {
		t.Fatalf("cascade model band became a seed: %+v", s.Seed)
	}
}

func TestLoadLadder(t *testing.T) {
	root := t.TempDir()
	if l, err := LoadLadder(root); err != nil || l.Season != 0 || l.Lines.L3Code != nil {
		t.Fatalf("missing file: %+v %v, want the empty ladder", l, err)
	}
	write := func(body string) {
		if err := os.WriteFile(filepath.Join(root, LadderFile), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("season: 12\nlines:\n  l3_code: 1450\n")
	l, err := LoadLadder(root)
	if err != nil || l.Season != 12 || l.Lines.L3Code == nil || *l.Lines.L3Code != 1450 || l.Lines.L4Manage != nil {
		t.Fatalf("got %+v %v", l, err)
	}
	// A misspelled line would otherwise read as "not placed" forever.
	write("lines:\n  l3code: 1450\n")
	if _, err := LoadLadder(root); err == nil {
		t.Fatal("an unknown ladder key must fail loudly")
	}
}

func TestSaveAgentRejectsMalformedLadderEvidence(t *testing.T) {
	c := bareStore(t)
	bad := []Agent{
		{Name: "a", Tool: "t1", Model: "m1", Certificates: []Certificate{{Name: "l9", Season: 1, Model: "m1"}}},
		{Name: "a", Tool: "t1", Model: "m1", Seat: &Seat{Band: MaxBand + 1, Source: BandOperator}},
		{Name: "a", Tool: "t1", Model: "m1", Seat: &Seat{Band: 3, Source: "vibes"}},
		{Name: "a", Tool: "t1", Model: "m1", Ratings: &DutyRatings{Code: &DutyRating{R: 1500, RD: -1}}},
	}
	for i, a := range bad {
		if err := c.SaveAgent(a); err == nil {
			t.Errorf("case %d: SaveAgent accepted %+v", i, a)
		}
	}
	good := Agent{Name: "a", Tool: "t1", Model: "m1", Seat: &Seat{Band: 5, Source: SeatProvisional, Season: 1},
		Ratings: &DutyRatings{Code: rated(1500, 350, 0)}, Certificates: certs(1, CertL1)}
	if err := c.SaveAgent(good); err != nil {
		t.Fatalf("SaveAgent(valid evidence): %v", err)
	}
}

// ladderCatalog is a scratch fleet with one resolving agent and a ladder file.
func ladderCatalog(t *testing.T) (string, []Option) {
	t.Helper()
	root := t.TempDir()
	opts := []Option{WithRoot(root), WithBaselineFS(fstest.MapFS{})}
	c := New(opts...)
	if err := c.SaveTool(Tool{Name: "t1", Kind: ToolKindCLI, CLI: ToolCLI{Binary: "t1", Launch: ToolLaunch{Exec: "t1 --model {model} {prompt}"}}}); err != nil {
		t.Fatal(err)
	}
	if err := c.SaveModel(Model{Name: "m1", Version: "1", Band: 4, BandSeason: 10}); err != nil {
		t.Fatal(err)
	}
	if err := c.SaveAgent(Agent{Name: "a1", Tool: "t1", Model: "m1"}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, LadderFile), []byte("season: 10\nlines:\n  l3_code: 1500\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return root, opts
}

// The evidence rides the generic --set path mechanism: no ladder-specific
// verb is needed to record a rating or a certificate, and the YAML stays the
// one source of truth.
func TestLadderEvidenceIsSettableByPath(t *testing.T) {
	_, opts := ladderCatalog(t)
	for _, set := range [][]string{
		{"ratings.code.r=1700", "ratings.code.rd=50", "ratings.code.events=9"},
		{"certificates.name=l1.season=10", "certificates.name=l1.model=m1", "certificates.name=l1.version=1"},
		{"certificates.name=l2.season=10", "certificates.name=l2.model=m1", "certificates.name=l2.version=1"},
	} {
		args := []string{"set", "a1"}
		for _, s := range set {
			args = append(args, "--set", s)
		}
		if out, err := runCmd(t, NewAgentsCmd(opts...), args...); err != nil {
			t.Fatalf("%v: %v\n%s", args, err, out)
		}
	}
	a, ok := New(opts...).Agent("a1")
	if !ok || a.Ratings == nil || a.Ratings.Code.R != 1700 || len(a.Certificates) != 2 {
		t.Fatalf("agent after --set = %+v", a)
	}
	// The stored record never carries a band of its own.
	if a.Band != 0 {
		t.Fatalf("a derived band was stored: %d", a.Band)
	}
}

func TestAgentListShowsDerivedBandAndMissingGates(t *testing.T) {
	_, opts := ladderCatalog(t)
	if _, err := runCmd(t, NewAgentsCmd(opts...), "set", "a1", "--set", "certificates.name=l1.season=10", "--set", "certificates.name=l1.model=m1", "--set", "certificates.name=l1.version=1"); err != nil {
		t.Fatal(err)
	}
	out, err := runCmd(t, NewAgentsCmd(opts...), "list", "--custom", "--json")
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	var rows []agentRow
	if err := json.Unmarshal([]byte(out), &rows); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if len(rows) != 1 {
		t.Fatalf("rows = %+v", rows)
	}
	r := rows[0]
	if r.DerivedBand != 1 || r.Band != 4 || r.BandSource != BandDeclared || strings.Join(r.MissingGates, ",") != "G2,G3,G4" {
		t.Fatalf("row = %+v; want effective L4 declared seed, derived L1, missing G2,G3,G4", r)
	}

	table, err := runCmd(t, NewAgentsCmd(opts...), "list", "--custom")
	if err != nil {
		t.Fatal(err)
	}
	if !regexp.MustCompile(`(?m)^NAME\s+NICK\s+BAND\s+DERIVED\s+MISSING\s`).MatchString(table) ||
		!regexp.MustCompile(`(?m)^a1\s+\S+\s+L4~\s+L1\s+G2,G3,G4\s`).MatchString(table) {
		t.Fatalf("table lacks the derived band and missing gates:\n%s", table)
	}

	show, err := runCmd(t, NewAgentsCmd(opts...), "show", "a1")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"band:    L4~, derived L1",
		"seed:    L4 declared, active (holds through season 11)",
		"G2: l2 certificate (missing)",
		"G3: conservative code ≥ L3 line (code unrated)",
		"G4: conservative manage ≥ L4 line (manage unrated)",
		"cert:    l1      season 10 on m1 1: valid",
	} {
		if !strings.Contains(show, want) {
			t.Errorf("show lacks %q:\n%s", want, show)
		}
	}
}

// Band filters read the effective band, so a derived band routes exactly like
// a peg did.
func TestAgentListBandFilterReadsTheEffectiveBand(t *testing.T) {
	_, opts := ladderCatalog(t)
	out, err := runCmd(t, NewAgentsCmd(opts...), "list", "--custom", "--json", "--min-band", "4")
	if err != nil || !strings.Contains(out, `"a1"`) {
		t.Fatalf("--min-band 4 dropped the L4-seeded agent: %v\n%s", err, out)
	}
	out, err = runCmd(t, NewAgentsCmd(opts...), "list", "--custom", "--json", "--min-band", "5")
	if err != nil || strings.Contains(out, `"a1"`) {
		t.Fatalf("--min-band 5 kept an L4 agent: %v\n%s", err, out)
	}
}

// The ladder is mechanism; the anchors that place its lines are the owner's
// data. No vendor, tool or model name may appear in its Go source.
func TestLadderSourceHasNoVendorKnowledge(t *testing.T) {
	src, err := os.ReadFile("ladder.go")
	if err != nil {
		t.Fatal(err)
	}
	vendor := regexp.MustCompile(`(?i)\b(claude|anthropic|opus|sonnet|fable|haiku|codex|openai|gpt|gemini|google|agy|glm|zhipu|deepseek|qwen|kimi|genie|ycode|ollama)\b`)
	if m := vendor.FindString(string(src)); m != "" {
		t.Fatalf("ladder.go names %q: lines and anchors belong in data, not Go", m)
	}
}
