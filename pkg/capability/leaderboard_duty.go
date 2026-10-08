package capability

// `bashy leaderboard --duty` — the per-duty band ladder view (design
// docs/bashy-band-ladder-design.md sections 2, 5, 8, 13).
//
// The default leaderboard ranks gate pass rates from the run ledger and its
// bashy-leaderboard-v1 output is byte-frozen for the bashy app; this view
// ranks Glicko-2 duty ratings replayed from the ladder event store, so it is
// a NEW envelope (bashy-leaderboard-duty-v1), never a new column on the old
// one. The same honesty rules carry over: ordering is the conservative lower
// bound R−2·RD, no rank is claimed on an inseparable gap, an empty store is a
// state and not a failure, and the cost view informs routing only.

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"sort"
	"strconv"
	"strings"

	"github.com/qiangli/yoke/pkg/fleet"
	"github.com/qiangli/yoke/pkg/ladder"
	"github.com/qiangli/yoke/pkg/ladder/blame"
	"github.com/spf13/cobra"
)

// LeaderboardDutySchema is the --duty --json envelope version.
const LeaderboardDutySchema = "bashy-leaderboard-duty-v1"

// dutyCurrencyRequired is the coding stories an L4/L5 seat owes per season so
// its `code` rating stays current (design section 5, "Currency").
const dutyCurrencyRequired = 2

// DutyRow is one agent's row on one duty ladder.
type DutyRow struct {
	Rank int `json:"rank"`
	// Separable is false when this row's interval [R−2RD, R+2RD] overlaps
	// the row above: no strict rank is claimed on an inseparable gap
	// (design section 13); the row shares the rank of its group's top and
	// renders as "≈".
	Separable   bool    `json:"separable"`
	Agent       string  `json:"agent"`
	R           float64 `json:"r"`
	RD          float64 `json:"rd"`
	Lower       float64 `json:"lower"`
	Events      int     `json:"events"`
	Established bool    `json:"established"`
	Band        int     `json:"band"`
	Seed        int     `json:"seed"`
	Streak      int     `json:"streak"`
	Moved       bool    `json:"moved"`
	// Missing is the first two gate misses blocking band+1, then "...".
	Missing  []string `json:"missing,omitempty"`
	Currency string   `json:"currency,omitempty"`
	// Move is the change in R since the previous season, or "new".
	Move string `json:"move"`
}

// DutyBoard is the computed per-duty ladder (bashy-leaderboard-duty-v1).
type DutyBoard struct {
	SchemaVersion string               `json:"schema_version"`
	Season        int                  `json:"season"`
	Lines         ladder.Lines         `json:"lines"`
	Duties        map[string][]DutyRow `json:"duties"`
	H2H           []HeadToHeadRow      `json:"h2h"`
}

// HeadToHeadRow is one paired comparison from heat delivery events. AgentA
// sorts before AgentB, so WinsA and WinsB have a stable meaning.
type HeadToHeadRow struct {
	AgentA      string  `json:"agent_a"`
	AgentB      string  `json:"agent_b"`
	Shared      int     `json:"shared"`
	WinsA       int     `json:"wins_a"`
	WinsB       int     `json:"wins_b"`
	Ties        int     `json:"ties"`
	Discordant  int     `json:"discordant"`
	P           float64 `json:"p"`
	Inseparable bool    `json:"inseparable"`
}

// ComputeDutyBoard replays the event ledger through the current season and
// builds one ladder per requested duty, using the local fleet catalog for seeds.
func ComputeDutyBoard(events []ladder.Event, season int, override *ladder.Lines, duties []ladder.Duty) DutyBoard {
	active := make([]ladder.Event, 0, len(events))
	for _, e := range events {
		if e.Season <= season {
			active = append(active, e)
		}
	}
	rep := ladder.Replay(events, season)
	prev := ladder.Replay(events, season-1)
	lines := dutyMergeLines(dutyComputeLines(rep, season), override)

	names := make([]string, 0, len(rep.Agents))
	for name := range rep.Agents {
		names = append(names, name)
	}
	sort.Strings(names)

	type agentView struct {
		band, seed, streak int
		missing            []string
		currency           string
	}
	views := make(map[string]agentView, len(names))
	for _, name := range names {
		rec := rep.Agents[name]
		seed := dutySeedBandForEvidence(name, active)
		state := ladder.CurrentBand(seed, active, name)
		band := state.Band
		_, misses := ladder.DeriveBand(dutyProfile(rec, 0), lines, season)
		v := agentView{band: band, seed: seed, streak: state.Streak}
		for i, m := range misses {
			if i == 2 {
				v.missing = append(v.missing, "...")
				break
			}
			v.missing = append(v.missing, m.Reason)
		}
		if band >= 4 {
			rated := rec.Standings[ladder.DutyCode].Events
			if p := prev.Agents[name]; p != nil {
				rated -= p.Standings[ladder.DutyCode].Events
			}
			rated = max(rated, 0)
			state := "ok"
			if rated < dutyCurrencyRequired {
				state = "DUE"
			}
			v.currency = fmt.Sprintf("%s %d/%d", state, rated, dutyCurrencyRequired)
		}
		views[name] = v
	}

	board := DutyBoard{
		SchemaVersion: LeaderboardDutySchema,
		Season:        season,
		Lines:         lines,
		Duties:        make(map[string][]DutyRow, len(duties)),
		H2H:           []HeadToHeadRow{},
	}
	for _, duty := range duties {
		rows := make([]DutyRow, 0, len(names))
		for _, name := range names {
			s := rep.Agents[name].Standings[duty]
			v := views[name]
			move := "new"
			if p := prev.Agents[name]; p != nil {
				move = fmt.Sprintf("%+.0f", s.R-p.Standings[duty].R)
			}
			rows = append(rows, DutyRow{
				Agent:       name,
				R:           s.R,
				RD:          s.RD,
				Lower:       s.Lower(),
				Events:      s.Events,
				Established: s.Established(),
				Band:        v.band,
				Seed:        v.seed,
				Streak:      v.streak,
				Moved:       v.band != v.seed,
				Missing:     v.missing,
				Currency:    v.currency,
				Move:        move,
			})
		}
		sort.SliceStable(rows, func(i, j int) bool {
			if rows[i].Lower != rows[j].Lower {
				return rows[i].Lower > rows[j].Lower
			}
			return rows[i].Agent < rows[j].Agent
		})
		dutyAssignRanks(rows)
		board.Duties[string(duty)] = rows
	}
	return board
}

func dutySeedBand(agent string) int {
	cat := fleet.New()
	if a, _, model, err := cat.Binding(agent); err == nil {
		if a.IsCascade() && a.Band > 0 {
			return a.Band
		}
		if model.Band > 0 {
			return model.Band
		}
	}
	return 1
}

func dutySeedBandForEvidence(identity string, events []ladder.Event) int {
	for _, e := range events {
		if e.RatingAgent() != identity {
			continue
		}
		if e.SelectedBinding != "" {
			return dutySeedBand(e.SelectedBinding)
		}
		if e.FamilyID != "" && e.Agent != "" {
			return dutySeedBand(e.Agent)
		}
	}
	return dutySeedBand(identity)
}

func newLeaderboardBandsCmd() *cobra.Command {
	var path string
	cmd := &cobra.Command{Use: "bands", Short: "show seeded and current agent bands", Args: cobra.NoArgs}
	cmd.Flags().StringVar(&path, "events", "", "ladder event store path")
	cmd.RunE = func(cmd *cobra.Command, _ []string) error {
		if path == "" {
			path = ladder.DefaultStorePath()
		}
		events, err := dutyReadEvents(path)
		if err != nil {
			return err
		}
		names := map[string][]string{}
		seeds := map[string]int{}
		for _, e := range events {
			if name := e.RatingAgent(); name != "" {
				names[name] = nil
			}
		}
		cat := fleet.New()
		if agents, _ := cat.Agents(); len(agents) > 0 {
			for _, a := range agents {
				if a.Ephemeral {
					continue
				}
				base := a
				seen := map[string]bool{a.Name: true}
				for base.ClonedFrom != "" {
					if seen[base.ClonedFrom] {
						break
					}
					seen[base.ClonedFrom] = true
					parent, ok := cat.Agent(base.ClonedFrom)
					if !ok {
						break
					}
					base = parent
				}
				key := base.MatrixKey()
				if key == ":" {
					continue
				}
				names[key] = append(names[key], a.Name)
				if _, ok := seeds[key]; !ok {
					seeds[key] = dutySeedBand(base.Name)
				}
			}
		}
		ordered := make([]string, 0, len(names))
		for name := range names {
			ordered = append(ordered, name)
		}
		sort.Strings(ordered)
		fmt.Fprintln(cmd.OutOrStdout(), "AGENT                    NAMES                    SEED CURRENT STREAK LAST MOVE")
		for _, key := range ordered {
			seed := seeds[key]
			if seed == 0 {
				seed = dutySeedBandForEvidence(key, events)
			}
			state := ladder.CurrentBand(seed, events, key)
			last := "—"
			if n := len(state.Moves); n > 0 {
				move := state.Moves[n-1]
				last = fmt.Sprintf("%s L%d→L%d", move.At.Format("2006-01-02"), move.From, move.To)
			}
			aliases := append([]string(nil), names[key]...)
			sort.Strings(aliases)
			if len(aliases) == 0 {
				aliases = []string{"—"}
			}
			fmt.Fprintf(cmd.OutOrStdout(), "%-24s %-24s L%-4d L%-6d %+6d %s\n", key, strings.Join(aliases, ","), state.Seed, state.Band, state.Streak, last)
		}
		return nil
	}
	return cmd
}

// dutyAssignRanks numbers rows already sorted by Lower descending. Adjacent
// rows whose intervals [R−2RD, R+2RD] overlap are inseparable: the lower row
// takes the rank of its group's top and Separable=false.
func dutyAssignRanks(rows []DutyRow) {
	for i := range rows {
		switch {
		case i == 0:
			rows[i].Rank, rows[i].Separable = 1, true
		case rows[i].R+2*rows[i].RD >= rows[i-1].R-2*rows[i-1].RD:
			rows[i].Rank, rows[i].Separable = rows[i-1].Rank, false
		default:
			rows[i].Rank, rows[i].Separable = i+1, true
		}
	}
}

// dutyProfile adapts a replayed record for ladder.DeriveBand. The model
// version is taken from the newest certificate: certificates are stamped per
// model version, and the replayed record carries no other identity for it.
func dutyProfile(rec *ladder.AgentRecord, provisional int) ladder.Profile {
	return ladder.Profile{
		Standings:    rec.Standings,
		Certs:        rec.Certs,
		ModelVersion: dutyModelVersion(rec.Certs),
		Provisional:  provisional,
	}
}

func dutyModelVersion(certs []ladder.Certificate) string {
	if len(certs) == 0 {
		return ""
	}
	return certs[len(certs)-1].ModelVersion
}

// dutyComputeLines fits from evidence alone the two lines the design derives
// from the fleet (section 2): L3Code is the conservative lower bound of the
// LOWEST established agent holding a valid L3 certificate, and L4Code is the
// median code rating of established agents whose derived band is exactly 3 —
// computed in two passes, first with the L4/L5 lines at zero so only G1–G3
// decide who is an L3. The manage and judge lines stay 0 (unfitted, fail
// closed) until fitted values arrive via --lines; a line is NEVER inferred
// from a vendor name.
func dutyComputeLines(rep ladder.ReplayResult, season int) ladder.Lines {
	var lines ladder.Lines
	fitted := false
	for _, rec := range rep.Agents {
		s := rec.Standings[ladder.DutyCode]
		if !s.Established() {
			continue
		}
		mv := dutyModelVersion(rec.Certs)
		valid := false
		for _, c := range rec.Certs {
			if c.Kind == ladder.CertL3 && ladder.CertValid(c, mv, season) {
				valid = true
				break
			}
		}
		if !valid {
			continue
		}
		if !fitted || s.Lower() < lines.L3Code {
			lines.L3Code = s.Lower()
			fitted = true
		}
	}

	pass1 := ladder.Lines{L3Code: lines.L3Code}
	var l3s []ladder.DutyStanding
	for _, rec := range rep.Agents {
		if band, _ := ladder.DeriveBand(dutyProfile(rec, 0), pass1, season); band != 3 {
			continue
		}
		if s := rec.Standings[ladder.DutyCode]; s.Established() {
			l3s = append(l3s, s)
		}
	}
	lines.L4Code = ladder.MedianCode(l3s)
	return lines
}

// dutyMergeLines lets an operator-fitted --lines file override the computed
// lines. Only positive values override: a zero in the file means "not
// fitted", same as everywhere else on the ladder.
func dutyMergeLines(computed ladder.Lines, override *ladder.Lines) ladder.Lines {
	if override == nil {
		return computed
	}
	merged := computed
	if override.L3Code > 0 {
		merged.L3Code = override.L3Code
	}
	if override.L4Code > 0 {
		merged.L4Code = override.L4Code
	}
	if override.L4Manage > 0 {
		merged.L4Manage = override.L4Manage
	}
	if override.L5Code > 0 {
		merged.L5Code = override.L5Code
	}
	if override.L5Manage > 0 {
		merged.L5Manage = override.L5Manage
	}
	if override.L5Judge > 0 {
		merged.L5Judge = override.L5Judge
	}
	return merged
}

// --- the command path -------------------------------------------------------

type dutyViewOptions struct {
	Duty   string // code | manage | judge | all ("" = all)
	Season int    // 0 = latest season in the store
	Events string // "" = ladder.DefaultStorePath()
	Lines  string // JSON file of ladder.Lines
	Cost   bool
	H2H    bool
	JSON   bool
}

func runDutyLeaderboard(w io.Writer, opts dutyViewOptions) error {
	if opts.H2H && opts.Duty == "" {
		opts.Duty = string(ladder.DutyCode)
	}
	duties, err := dutySelection(opts.Duty)
	if err != nil {
		return err
	}
	path := opts.Events
	if path == "" {
		path = ladder.DefaultStorePath()
	}
	events, err := dutyReadEvents(path)
	if err != nil {
		return fmt.Errorf("leaderboard: reading the ladder store: %w", err)
	}
	if len(events) == 0 {
		// Absence is not failure: a host that has rated nothing is a real,
		// reportable state, and nothing is ranked.
		fmt.Fprintf(w, "no rated events yet — nothing to rank. ladder store: %s\n", path)
		return nil
	}

	season := opts.Season
	if season <= 0 {
		season = 1
		for _, e := range events {
			if e.Season > season {
				season = e.Season
			}
		}
	}

	var override *ladder.Lines
	if opts.Lines != "" {
		data, err := os.ReadFile(opts.Lines)
		if err != nil {
			return fmt.Errorf("leaderboard: reading --lines: %w", err)
		}
		var l ladder.Lines
		if err := json.Unmarshal(data, &l); err != nil {
			return fmt.Errorf("leaderboard: parsing --lines: %w", err)
		}
		override = &l
	}

	if opts.Cost {
		return renderDutyCost(w, events, season, duties[0], path)
	}
	board := ComputeDutyBoard(events, season, override, duties)
	if opts.H2H {
		board.H2H = ComputeHeadToHead(events, season, duties[0])
	}
	if opts.JSON {
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		return enc.Encode(board)
	}
	return renderDutyBoard(w, board, path)
}

func dutySelection(s string) ([]ladder.Duty, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "", "all":
		return []ladder.Duty{ladder.DutyCode, ladder.DutyManage, ladder.DutyJudge}, nil
	case string(ladder.DutyCode):
		return []ladder.Duty{ladder.DutyCode}, nil
	case string(ladder.DutyManage):
		return []ladder.Duty{ladder.DutyManage}, nil
	case string(ladder.DutyJudge):
		return []ladder.Duty{ladder.DutyJudge}, nil
	default:
		return nil, fmt.Errorf("leaderboard: unknown duty %q (code, manage, judge, all)", s)
	}
}

// ComputeHeadToHead joins delivery events from the same heat into binary
// paired outcomes. A passing outcome wins over every non-passing outcome;
// matching binary outcomes are ties. Only one event per agent per heat is
// considered, so a malformed duplicate cannot manufacture a comparison.
func ComputeHeadToHead(events []ladder.Event, season int, duty ladder.Duty) []HeadToHeadRow {
	heats := make(map[string]map[string]ladder.Event)
	for _, e := range events {
		if e.Kind != ladder.EventKindDelivery || e.Duty != duty || e.Season < 1 || e.Season > season {
			continue
		}
		id, ok := heatID(e.Note)
		name := e.RatingAgent()
		if !ok || name == "" {
			continue
		}
		if heats[id] == nil {
			heats[id] = make(map[string]ladder.Event)
		}
		if _, exists := heats[id][name]; !exists {
			heats[id][name] = e
		}
	}

	type pair struct{ a, b string }
	rows := make(map[pair]*HeadToHeadRow)
	for _, entrants := range heats {
		names := make([]string, 0, len(entrants))
		for name := range entrants {
			names = append(names, name)
		}
		sort.Strings(names)
		for i := 0; i < len(names); i++ {
			for j := i + 1; j < len(names); j++ {
				key := pair{names[i], names[j]}
				r := rows[key]
				if r == nil {
					r = &HeadToHeadRow{AgentA: key.a, AgentB: key.b}
					rows[key] = r
				}
				r.Shared++
				aPass := entrants[key.a].Outcome == 1
				bPass := entrants[key.b].Outcome == 1
				switch {
				case aPass && !bPass:
					r.WinsA++
				case bPass && !aPass:
					r.WinsB++
				default:
					r.Ties++
				}
			}
		}
	}
	out := make([]HeadToHeadRow, 0, len(rows))
	for _, r := range rows {
		r.Discordant = r.WinsA + r.WinsB
		r.P = ExactMcNemar(r.WinsA, r.WinsB)
		r.Inseparable = r.Discordant < 6 || r.P >= 0.05
		out = append(out, *r)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].AgentA != out[j].AgentA {
			return out[i].AgentA < out[j].AgentA
		}
		return out[i].AgentB < out[j].AgentB
	})
	return out
}

func heatID(note string) (string, bool) {
	const prefix = "heat:"
	if !strings.HasPrefix(note, prefix) || len(note) == len(prefix) {
		return "", false
	}
	return note[len(prefix):], true
}

// ExactMcNemar is the two-sided exact binomial p-value for discordant pairs.
func ExactMcNemar(winsA, winsB int) float64 {
	n := winsA + winsB
	if n == 0 {
		return 1
	}
	m := min(winsA, winsB)
	ln2 := math.Ln2
	var tail float64
	for k := 0; k <= m; k++ {
		lnChoose, _ := math.Lgamma(float64(n + 1))
		lnK, _ := math.Lgamma(float64(k + 1))
		lnRest, _ := math.Lgamma(float64(n - k + 1))
		tail += math.Exp(lnChoose - lnK - lnRest - float64(n)*ln2)
	}
	return min(1, 2*tail)
}

func dutyReadEvents(path string) ([]ladder.Event, error) {
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		// Do not create the store just to look at it.
		return nil, nil
	}
	st, err := ladder.OpenStore(path)
	if err != nil {
		return nil, err
	}
	return st.Read()
}

func renderDutyBoard(w io.Writer, board DutyBoard, path string) error {
	fmt.Fprintf(w, "bashy leaderboard — duty ladder — %s\n", Provenance)
	fmt.Fprintf(w, "season %d · events %s\n", board.Season, path)
	fmt.Fprintf(w, "lines: %s\n", dutyLinesHeader(board.Lines))

	for _, duty := range []ladder.Duty{ladder.DutyCode, ladder.DutyManage, ladder.DutyJudge} {
		rows, ok := board.Duties[string(duty)]
		if !ok {
			continue
		}
		fmt.Fprintf(w, "\n%s (by conservative rating R−2RD)\n", strings.ToUpper(string(duty)))
		fmt.Fprintf(w, "  %-4s %-24s %6s %5s %6s %7s %-6s %-6s %-7s %-5s %-44s %-8s %s\n",
			"RANK", "AGENT", "R", "RD", "LOWER", "EVENTS", "BAND", "SEED", "STREAK", "MOVED", "MISSING", "CURRENCY", "MOVE")
		for _, r := range rows {
			rank := strconv.Itoa(r.Rank)
			if !r.Separable {
				rank = "≈"
			}
			events := strconv.Itoa(r.Events)
			if r.Established {
				events += "*"
			}
			fmt.Fprintf(w, "  %-4s %-24s %6.0f %5.0f %6.0f %7s %-6s %-6s %-7s %-5s %-44s %-8s %s\n",
				rank, r.Agent, r.R, r.RD, r.Lower, events, dutyBandCell(r), fmt.Sprintf("L%d", r.Seed), fmt.Sprintf("%+d", r.Streak), map[bool]string{true: "yes", false: "no"}[r.Moved],
				strings.Join(r.Missing, "; "), r.Currency, r.Move)
		}
	}
	if len(board.H2H) > 0 {
		fmt.Fprintln(w, "\nHEAD-TO-HEAD (paired heat outcomes)")
		fmt.Fprintln(w, "  AGENT A                  AGENT B                  SHARED  W-L-T   P")
		for _, r := range board.H2H {
			result := fmt.Sprintf("%d-%d-%d", r.WinsA, r.WinsB, r.Ties)
			label := fmt.Sprintf("%.4g", r.P)
			if r.Inseparable {
				label += " inseparable"
			}
			fmt.Fprintf(w, "  %-24s %-24s %6d  %-7s %s\n", r.AgentA, r.AgentB, r.Shared, result, label)
		}
	}
	fmt.Fprintf(w, "\n* established (>= %d rated events) · ≈ inseparable from the row above (overlapping rating intervals)\n",
		ladder.EstablishedEvents)
	return nil
}

func dutyBandCell(r DutyRow) string {
	return fmt.Sprintf("L%d", r.Band)
}

func dutyLinesHeader(l ladder.Lines) string {
	cell := func(v float64) string {
		if v <= 0 {
			return "unfitted"
		}
		return strconv.FormatFloat(v, 'f', 0, 64)
	}
	return fmt.Sprintf("L3Code=%s L4Code=%s L4Manage=%s L5Code=%s L5Manage=%s L5Judge=%s",
		cell(l.L3Code), cell(l.L4Code), cell(l.L4Manage), cell(l.L5Code), cell(l.L5Manage), cell(l.L5Judge))
}

// renderDutyCost is the rating-per-dollar view (design section 13, "Cost
// view"): it informs routing only and never promotes. Agents with no
// recorded cost are listed unscored — an unmetered agent is not a free one.
func renderDutyCost(w io.Writer, events []ladder.Event, season int, duty ladder.Duty, path string) error {
	rep := ladder.Replay(events, season)
	costs := dutyRatedCost(events, season)

	fmt.Fprintln(w, "bashy leaderboard — cost — informational — routing only, never promotes")
	fmt.Fprintf(w, "%s\n", Provenance)
	fmt.Fprintf(w, "duty %s · season %d · events %s\n\n", duty, season, path)

	names := make([]string, 0, len(rep.Agents))
	for name := range rep.Agents {
		names = append(names, name)
	}
	sort.Strings(names)

	type costRow struct {
		agent            string
		r, cost, perUnit float64
	}
	var rows []costRow
	var unpriced []string
	for _, name := range names {
		s := rep.Agents[name].Standings[duty]
		c := costs[name]
		if c <= 0 {
			unpriced = append(unpriced, name)
			continue
		}
		rows = append(rows, costRow{agent: name, r: s.R, cost: c, perUnit: (s.R - 1000) / c})
	}
	sort.SliceStable(rows, func(i, j int) bool {
		if rows[i].perUnit != rows[j].perUnit {
			return rows[i].perUnit > rows[j].perUnit
		}
		return rows[i].agent < rows[j].agent
	})

	fmt.Fprintf(w, "  %-24s %6s %10s %10s\n", "AGENT", "R", "COST", "R/$")
	for _, r := range rows {
		fmt.Fprintf(w, "  %-24s %6.0f %10.2f %10.1f\n", r.agent, r.r, r.cost, r.perUnit)
	}
	for _, name := range unpriced {
		fmt.Fprintf(w, "  %-24s no cost recorded\n", name)
	}
	return nil
}

// RatedCost returns each agent's recorded cost over its rated events.
func RatedCost(events []ladder.Event, season int) map[string]float64 {
	return dutyRatedCost(events, season)
}

// dutyRatedCost sums each agent's recorded Cost over its RATED events through
// the given season, honouring corrections. Unrated events (a delivery with an
// unclassified failure, a cert, a seat) carry no rating and so no cost here.
func dutyRatedCost(events []ladder.Event, season int) map[string]float64 {
	dropped := make(map[string]bool)
	for _, e := range events {
		if e.Kind == ladder.EventKindCorrection && e.Season <= season && e.Supersedes != "" {
			dropped[e.Supersedes] = true
		}
	}
	costs := make(map[string]float64)
	for _, e := range events {
		if e.Season < 1 || e.Season > season || dropped[e.ID] || e.Agent == "" || e.Cost <= 0 {
			continue
		}
		switch e.Kind {
		case ladder.EventKindDelivery:
			if e.Outcome == 1 || e.Outcome == 0.5 || (e.Outcome == 0 && blame.Rates(e.Blame)) {
				costs[e.Agent] += e.Cost
			}
		case ladder.EventKindManage, ladder.EventKindEstimate:
			costs[e.Agent] += e.Cost
		}
	}
	return costs
}
