package fleet

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// The band ladder (dhnt docs/bashy-band-ladder-design.md): bands are a strict,
// cumulative progression, and an agent's band is DERIVED — the highest n whose
// gates G1..Gn all hold — never stored. What IS stored is the evidence the
// gates read: three duty ratings, a set of certificates, and an optional seat
// (a peg that seeds the ladder until evidence replaces it).
//
// This file holds the mechanism only. Every number that places a line — the
// current season and the rating lines — is data in the fleet root's
// ladder.yaml, and no agent, model or vendor is named here: the anchors that
// place the lines are chosen by the owner, in data.

// Duty names. Each is one Glicko-2 rating on the agent.
const (
	DutyCode   = "code"
	DutyManage = "manage"
	DutyJudge  = "judge"
)

// Certificate names, one per suite in the design's section 3.
const (
	CertL1      = "l1"      // one-shot suite
	CertL2      = "l2"      // mini Terminal-Bench
	CertL3      = "l3"      // coding suite
	CertSteer   = "steer"   // the agent-bench steerability pack
	CertManager = "manager" // conductor tasks
	CertReview  = "review"  // seeded-defect diffs
	CertJudge   = "judge"   // judge calibration
	CertL5      = "l5"      // the agent-bench frontier pack
)

// SeatProvisional marks an owner-seated provisional band (the bootstrap L5s):
// it holds like an operator peg and must be confirmed by the gates in time.
const SeatProvisional = "provisional"

// BandDerived is the band source of a band read off the gates.
const BandDerived = "derived"

const (
	// EstablishedEvents is how many rated events make a duty rating
	// established. Gates read only established ratings.
	EstablishedEvents = 8
	// CertificateSeasons is a certificate's life: earned in season S, it is
	// valid through S+2 and expired from S+3. A model change voids it at once.
	CertificateSeasons = 3
	// SeatSeasons is a peg's life as a seed: seated in S, it holds through S+1
	// and expires from S+2 unless the gates confirm it first.
	SeatSeasons = 2
)

// DutyRating is one duty's Glicko-2 rating: r ± RD over Events rated events.
type DutyRating struct {
	R      float64 `yaml:"r" json:"r" doc:"Glicko-2 rating"`
	RD     float64 `yaml:"rd" json:"rd" doc:"Glicko-2 rating deviation"`
	Events int     `yaml:"events,omitempty" json:"events,omitempty" doc:"rated events in this duty"`
}

// Conservative is the lower bound r − 2·RD that every gate compares.
func (d DutyRating) Conservative() float64 { return d.R - 2*d.RD }

// Established reports whether enough events back the rating for a gate to read it.
func (d DutyRating) Established() bool { return d.Events >= EstablishedEvents }

// DutyRatings are an agent's three duty ratings. An absent duty is unrated.
type DutyRatings struct {
	Code   *DutyRating `yaml:"code,omitempty" json:"code,omitempty" doc:"coding duty rating"`
	Manage *DutyRating `yaml:"manage,omitempty" json:"manage,omitempty" doc:"sprint-management duty rating"`
	Judge  *DutyRating `yaml:"judge,omitempty" json:"judge,omitempty" doc:"judging duty rating"`
}

// Duty returns the named duty's rating, or nil when unrated.
func (r *DutyRatings) Duty(name string) *DutyRating {
	if r == nil {
		return nil
	}
	switch name {
	case DutyCode:
		return r.Code
	case DutyManage:
		return r.Manage
	case DutyJudge:
		return r.Judge
	}
	return nil
}

// Certificate records a passed certification suite. It is bound to the model
// it was earned on: a different model or model version voids it.
type Certificate struct {
	Name    string `yaml:"name" json:"name" doc:"certificate name (l1, l2, l3, steer, manager, review, judge, l5)"`
	Season  int    `yaml:"season" json:"season" doc:"season the certificate was earned"`
	Model   string `yaml:"model" json:"model" doc:"canonical model the certificate was earned on"`
	Version string `yaml:"version,omitempty" json:"version,omitempty" doc:"model version the certificate was earned on"`
}

// Seat is an agent-level peg: an operator seating or a provisional seat. It
// seeds the ladder, never overrides it — it expires after SeatSeasons unless
// the derived band confirms it.
type Seat struct {
	Band   int    `yaml:"band" json:"band" doc:"seated band"`
	Source string `yaml:"source" json:"source" doc:"seat kind: operator or provisional"`
	Season int    `yaml:"season,omitempty" json:"season,omitempty" doc:"season the seat was granted"`
}

// Ladder is the fleet-wide data the gates compare against. It lives in
// <fleet root>/ladder.yaml. Season 0 means the season is unknown, and nothing
// expires by season; an unset line fails every gate that reads it.
type Ladder struct {
	Season int         `yaml:"season,omitempty" json:"season,omitempty"`
	Lines  LadderLines `yaml:"lines,omitempty" json:"lines,omitempty"`
}

// LadderLines are the absolute band lines, placed from anchors once a season.
// The relative parts of G4 and G5 (fleet medians) are computed, not stored.
type LadderLines struct {
	L3Code   *float64 `yaml:"l3_code,omitempty" json:"l3_code,omitempty"`
	L4Manage *float64 `yaml:"l4_manage,omitempty" json:"l4_manage,omitempty"`
	L5Judge  *float64 `yaml:"l5_judge,omitempty" json:"l5_judge,omitempty"`
}

// LadderFile is the ladder's file name under the fleet root.
const LadderFile = "ladder.yaml"

// LoadLadder reads <root>/ladder.yaml. A missing file is the empty ladder:
// season unknown, no lines placed.
func LoadLadder(root string) (Ladder, error) {
	var l Ladder
	data, err := os.ReadFile(filepath.Join(root, LadderFile))
	if errors.Is(err, fs.ErrNotExist) {
		return l, nil
	}
	if err != nil {
		return l, err
	}
	dec := yaml.NewDecoder(strings.NewReader(string(data)))
	dec.KnownFields(true)
	if err := dec.Decode(&l); err != nil && !errors.Is(err, io.EOF) {
		return l, fmt.Errorf("fleet: %s: %w", LadderFile, err)
	}
	return l, nil
}

// knownCerts is the closed set of certificate names the gates read.
var knownCerts = map[string]bool{
	CertL1: true, CertL2: true, CertL3: true, CertSteer: true,
	CertManager: true, CertReview: true, CertJudge: true, CertL5: true,
}

// ValidLadderEvidence rejects ladder evidence the gates could only misread:
// an unknown certificate name (it would be silently ignored), a seat outside
// the band range or of an unknown kind, and negative ratings or counts.
func ValidLadderEvidence(a Agent) error {
	for _, c := range a.Certificates {
		if !knownCerts[c.Name] {
			return fmt.Errorf("fleet: agent %s: unknown certificate %q (want one of %s)", a.Name, c.Name, strings.Join(sortedKeys(knownCerts), ", "))
		}
		if c.Season < 0 {
			return fmt.Errorf("fleet: agent %s: certificate %s has negative season %d", a.Name, c.Name, c.Season)
		}
	}
	if s := a.Seat; s != nil {
		if s.Band < 1 || s.Band > MaxBand {
			return fmt.Errorf("fleet: agent %s: seat band %d is out of range (1-%d)", a.Name, s.Band, MaxBand)
		}
		if s.Source != BandOperator && s.Source != SeatProvisional {
			return fmt.Errorf("fleet: agent %s: seat source %q must be %s or %s", a.Name, s.Source, BandOperator, SeatProvisional)
		}
		if s.Season < 0 {
			return fmt.Errorf("fleet: agent %s: seat has negative season %d", a.Name, s.Season)
		}
	}
	for _, duty := range []string{DutyCode, DutyManage, DutyJudge} {
		if d := a.Ratings.Duty(duty); d != nil && (d.RD < 0 || d.Events < 0) {
			return fmt.Errorf("fleet: agent %s: %s rating has a negative rd or event count", a.Name, duty)
		}
	}
	return nil
}

// Ladder reads the catalog's ladder file.
func (c *Catalog) Ladder() (Ladder, error) { return LoadLadder(c.Root()) }

// GateCheck is one condition of one gate.
type GateCheck struct {
	Gate   int    `json:"gate"`
	Need   string `json:"need"`
	OK     bool   `json:"ok"`
	Detail string `json:"detail,omitempty"`
}

// String renders a check as "G3: steer certificate (void: model changed)".
func (g GateCheck) String() string {
	s := "G" + strconv.Itoa(g.Gate) + ": " + g.Need
	if g.Detail != "" {
		s += " (" + g.Detail + ")"
	}
	return s
}

// CertStatus is one recorded certificate judged against the current binding
// and season.
type CertStatus struct {
	Certificate
	Valid  bool   `json:"valid"`
	Reason string `json:"reason,omitempty"`
}

// SeedStatus is a peg judged as a seed.
type SeedStatus struct {
	Band   int    `json:"band"`
	Source string `json:"source"`
	Season int    `json:"season,omitempty"`
	// Active means the seed still sets the band (derived is lower and the seed
	// has neither expired nor yielded to established evidence).
	Active bool   `json:"active"`
	Reason string `json:"reason,omitempty"`
}

// Standing is one agent's place on the ladder.
type Standing struct {
	Agent string `json:"agent"`
	// Derived is the highest n whose gates G1..Gn all hold; 0 is unplaced.
	Derived int `json:"derived_band"`
	// Band and Source are the effective band: Derived, unless an active seed
	// holds the agent higher while the gates catch up.
	Band   int         `json:"band"`
	Source string      `json:"band_source,omitempty"`
	Seed   *SeedStatus `json:"seed,omitempty"`
	// Checks are every gate condition up to the target band (Derived+1, or the
	// seed's band when that is higher); Missing are the failed ones.
	Checks       []GateCheck  `json:"checks,omitempty"`
	Certificates []CertStatus `json:"certificates,omitempty"`
}

// Missing lists the failed gate conditions.
func (s Standing) Missing() []GateCheck {
	var out []GateCheck
	for _, c := range s.Checks {
		if !c.OK {
			out = append(out, c)
		}
	}
	return out
}

// MissingGates lists the gates (as "G3") with at least one failed condition.
func (s Standing) MissingGates() []string {
	var out []string
	seen := map[int]bool{}
	for _, c := range s.Missing() {
		if !seen[c.Gate] {
			seen[c.Gate] = true
			out = append(out, "G"+strconv.Itoa(c.Gate))
		}
	}
	return out
}

// LadderEntry is one agent with the model its binding resolves to. Resolved is
// false for a dangling binding: it holds no certificate, since none can be
// matched to a model.
type LadderEntry struct {
	Agent    Agent
	Model    Model
	Resolved bool
}

// certStatus judges one certificate against the entry's binding and the season.
func certStatus(c Certificate, e LadderEntry, season int) CertStatus {
	st := CertStatus{Certificate: c}
	switch {
	case !e.Resolved:
		st.Reason = "binding does not resolve"
	case c.Model == "":
		st.Reason = "no model recorded"
	case c.Model != e.Model.Name:
		st.Reason = "void: model changed from " + c.Model
	case c.Version != e.Model.Version:
		st.Reason = "void: model version changed from " + dashIfEmpty(c.Version)
	case season > 0 && c.Season <= 0:
		st.Reason = "no season recorded"
	case season > 0 && season-c.Season >= CertificateSeasons:
		st.Reason = fmt.Sprintf("expired: earned season %d, expired from season %d", c.Season, c.Season+CertificateSeasons)
	default:
		st.Valid = true
	}
	return st
}

type ladderEval struct {
	entry  LadderEntry
	certs  []CertStatus
	valid  map[string]bool
	checks map[int][]GateCheck
}

func (ev *ladderEval) holds(gate int) bool {
	for _, c := range ev.checks[gate] {
		if !c.OK {
			return false
		}
	}
	return true
}

func (ev *ladderEval) holdsThrough(n int) bool {
	for g := 1; g <= n; g++ {
		if !ev.holds(g) {
			return false
		}
	}
	return true
}

func (ev *ladderEval) cert(gate int, name string) {
	c := GateCheck{Gate: gate, Need: name + " certificate", OK: ev.valid[name]}
	if !c.OK {
		c.Detail = "missing"
		for _, st := range ev.certs {
			if st.Name == name {
				c.Detail = st.Reason
			}
		}
	}
	ev.checks[gate] = append(ev.checks[gate], c)
}

// rating adds a condition "conservative <duty> ≥ <what>" against bar; a nil
// bar is a line or median nobody has placed yet, and fails closed.
func (ev *ladderEval) rating(gate int, duty, what string, bar *float64) {
	c := GateCheck{Gate: gate, Need: "conservative " + duty + " ≥ " + what}
	d := ev.entry.Agent.Ratings.Duty(duty)
	switch {
	case d == nil:
		c.Detail = duty + " unrated"
	case !d.Established():
		c.Detail = fmt.Sprintf("%s not established: %d/%d events", duty, d.Events, EstablishedEvents)
	case bar == nil:
		c.Detail = what + " not placed"
	default:
		c.OK = d.Conservative() >= *bar
		rel := "≥"
		if !c.OK {
			rel = "<"
		}
		c.Detail = fmt.Sprintf("%s %s %s", fmtRating(d.Conservative()), rel, fmtRating(*bar))
	}
	ev.checks[gate] = append(ev.checks[gate], c)
}

func fmtRating(f float64) string { return strconv.FormatFloat(f, 'f', -1, 64) }

// median of the CONSERVATIVE ratings of one duty over the evals holding gates
// G1..Gn, excluding ephemeral clones (a clone's copied record is not a second
// player). Conservative against conservative keeps the comparison like for
// like: against point ratings an agent exactly at the median — or alone in its
// band — would fail by 2·RD and the gate could never be first held.
func median(evs []*ladderEval, n int, duty string) *float64 {
	var rs []float64
	for _, ev := range evs {
		if ev.entry.Agent.Ephemeral || !ev.holdsThrough(n) {
			continue
		}
		if d := ev.entry.Agent.Ratings.Duty(duty); d != nil && d.Established() {
			rs = append(rs, d.Conservative())
		}
	}
	if len(rs) == 0 {
		return nil
	}
	sort.Float64s(rs)
	m := rs[len(rs)/2]
	if len(rs)%2 == 0 {
		m = (rs[len(rs)/2-1] + rs[len(rs)/2]) / 2
	}
	return &m
}

// DeriveStandings places every entry on the ladder. It needs the whole fleet
// at once because G4 and G5 compare against the medians of the bands below.
func DeriveStandings(entries []LadderEntry, l Ladder) []Standing {
	evs := make([]*ladderEval, len(entries))
	for i, e := range entries {
		ev := &ladderEval{entry: e, valid: map[string]bool{}, checks: map[int][]GateCheck{}}
		for _, c := range e.Agent.Certificates {
			st := certStatus(c, e, l.Season)
			ev.certs = append(ev.certs, st)
			if st.Valid {
				ev.valid[c.Name] = true
			}
		}
		// The absolute part of the ladder: G1–G3.
		ev.cert(1, CertL1)
		ev.cert(2, CertL2)
		ev.cert(3, CertL3)
		ev.cert(3, CertSteer)
		ev.rating(3, DutyCode, "L3 line", l.Lines.L3Code)
		evs[i] = ev
	}
	// G4 is relative to the established L3s, G5 to the established L4s, so
	// each tier's median is taken only after the tier below is settled.
	l3Code := median(evs, 3, DutyCode)
	for _, ev := range evs {
		ev.cert(4, CertManager)
		ev.cert(4, CertReview)
		ev.rating(4, DutyCode, "L3 median", l3Code)
		ev.rating(4, DutyManage, "L4 line", l.Lines.L4Manage)
	}
	l4Code, l4Manage := median(evs, 4, DutyCode), median(evs, 4, DutyManage)
	for _, ev := range evs {
		ev.cert(5, CertJudge)
		ev.cert(5, CertL5)
		ev.rating(5, DutyCode, "L4 median", l4Code)
		ev.rating(5, DutyManage, "L4 median", l4Manage)
		ev.rating(5, DutyJudge, "L5 line", l.Lines.L5Judge)
	}

	out := make([]Standing, len(evs))
	for i, ev := range evs {
		s := Standing{Agent: ev.entry.Agent.Name, Certificates: ev.certs}
		for n := 1; n <= MaxBand && ev.holds(n); n++ {
			s.Derived = n
		}
		s.Band, s.Source = s.Derived, BandDerived
		if s.Derived == 0 {
			s.Source = ""
		}
		target := s.Derived + 1
		if seed := seedOf(ev.entry); seed != nil {
			judgeSeed(seed, ev, s.Derived, l.Season)
			s.Seed = seed
			if seed.Active {
				s.Band, s.Source = seed.Band, seed.Source
			}
			if seed.Band > target {
				target = seed.Band
			}
		}
		if target > MaxBand {
			target = MaxBand
		}
		for g := 1; g <= target; g++ {
			s.Checks = append(s.Checks, ev.checks[g]...)
		}
		out[i] = s
	}
	return out
}

// seedOf returns the entry's peg: its own seat, else its model's band. A
// cascade's served band is its contract, not a peg, and never seeds.
func seedOf(e LadderEntry) *SeedStatus {
	if s := e.Agent.Seat; s != nil && s.Band > 0 {
		src := s.Source
		if src == "" {
			src = BandOperator
		}
		return &SeedStatus{Band: s.Band, Source: src, Season: s.Season}
	}
	if e.Resolved && e.Model.Band > 0 && e.Model.BandSource != BandCascade {
		return &SeedStatus{Band: e.Model.Band, Source: effectiveBandSource(e.Model.Band, e.Model.BandSource), Season: e.Model.BandSeason}
	}
	return nil
}

// judgeSeed decides whether a peg still sets the band.
//
// A seed stops at the first of: the gates confirm it (derived ≥ seed), it
// expires (SeatSeasons after it was set), or — for a declared prior only —
// the ratings its band gates on are all established, so evidence can speak.
// An operator or provisional seat holds until confirmed or expired: the owner
// seated it on purpose and gave the gates a fixed time to agree.
func judgeSeed(seed *SeedStatus, ev *ladderEval, derived, season int) {
	switch {
	case derived >= seed.Band:
		seed.Reason = "confirmed by the gates"
	case season > 0 && seed.Season > 0 && season-seed.Season >= SeatSeasons:
		seed.Reason = fmt.Sprintf("expired: seated season %d, expired from season %d", seed.Season, seed.Season+SeatSeasons)
	case seed.Source == BandDeclared && dutiesEstablished(ev.entry.Agent.Ratings, seed.Band):
		seed.Reason = "yielded to established ratings"
	default:
		seed.Active = true
		if seed.Season <= 0 {
			seed.Reason = "unclocked: no season recorded"
		} else {
			seed.Reason = fmt.Sprintf("holds through season %d", seed.Season+SeatSeasons-1)
		}
	}
}

// dutiesEstablished reports whether every duty rating band n gates on is
// established. Bands below L3 gate on certificates alone, so there is nothing
// for a declared L1/L2 prior to yield to but the certificates themselves.
func dutiesEstablished(r *DutyRatings, band int) bool {
	var duties []string
	switch {
	case band >= 5:
		duties = []string{DutyCode, DutyManage, DutyJudge}
	case band == 4:
		duties = []string{DutyCode, DutyManage}
	case band == 3:
		duties = []string{DutyCode}
	default:
		return false
	}
	for _, d := range duties {
		if rt := r.Duty(d); rt == nil || !rt.Established() {
			return false
		}
	}
	return true
}

// Standings places every agent in the catalog on the ladder, keyed by
// canonical agent name.
func (c *Catalog) Standings(l Ladder) (map[string]Standing, []error) {
	agents, errs := c.Agents()
	entries := make([]LadderEntry, 0, len(agents))
	for _, a := range agents {
		e := LadderEntry{Agent: a}
		if _, _, m, err := c.Binding(a.Name); err == nil {
			e.Model, e.Resolved = m, true
		}
		entries = append(entries, e)
	}
	out := make(map[string]Standing, len(entries))
	for _, s := range DeriveStandings(entries, l) {
		out[s.Agent] = s
	}
	return out, errs
}
