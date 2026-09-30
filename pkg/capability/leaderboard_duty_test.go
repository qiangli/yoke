package capability

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/qiangli/yoke/pkg/ladder"
)

// --- fixtures ---------------------------------------------------------------

func dutyStamp(season, i int) time.Time {
	return time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC).
		Add(time.Duration(season)*time.Hour + time.Duration(i)*time.Second)
}

// dutyDeliveries returns n rated coding deliveries for agent in season.
func dutyDeliveries(agent string, season, n int, outcome, cost float64) []ladder.Event {
	events := make([]ladder.Event, 0, n)
	for i := 0; i < n; i++ {
		events = append(events, ladder.Event{
			At:      dutyStamp(season, i),
			Season:  season,
			Kind:    ladder.EventKindDelivery,
			Agent:   agent,
			Story:   fmt.Sprintf("%s-s%d-%d", agent, season, i),
			Points:  3,
			Outcome: outcome,
			Cost:    cost,
		})
	}
	return events
}

func dutyCertEvents(agent string, season int, kinds ...ladder.CertKind) []ladder.Event {
	events := make([]ladder.Event, 0, len(kinds))
	for i, k := range kinds {
		events = append(events, ladder.Event{
			At:     dutyStamp(season, 1000+i),
			Season: season,
			Kind:   ladder.EventKindCert,
			Agent:  agent,
			Cert:   ladder.Certificate{Kind: k, Season: season},
		})
	}
	return events
}

func dutySeat(agent string, season, provisional int) ladder.Event {
	return ladder.Event{
		At:          dutyStamp(season, 2000),
		Season:      season,
		Kind:        ladder.EventKindSeat,
		Agent:       agent,
		Provisional: provisional,
	}
}

func dutyWriteEvents(t *testing.T, events []ladder.Event) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "events.jsonl")
	st, err := ladder.OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range events {
		if err := st.Append(e); err != nil {
			t.Fatalf("append %+v: %v", e, err)
		}
	}
	return path
}

func dutyRun(t *testing.T, args ...string) string {
	t.Helper()
	cmd := NewLeaderboardCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs(args)
	if err := cmd.Execute(); err != nil {
		t.Fatalf("leaderboard %v: %v\n%s", args, err, out.String())
	}
	return out.String()
}

func dutyRunBoard(t *testing.T, args ...string) DutyBoard {
	t.Helper()
	var board DutyBoard
	if err := json.Unmarshal([]byte(dutyRun(t, args...)), &board); err != nil {
		t.Fatalf("decoding duty board: %v", err)
	}
	return board
}

func dutyFindRow(t *testing.T, rows []DutyRow, agent string) DutyRow {
	t.Helper()
	for _, r := range rows {
		if r.Agent == agent {
			return r
		}
	}
	t.Fatalf("no row for %q in %+v", agent, rows)
	return DutyRow{}
}

func TestLeaderboardBandsTable(t *testing.T) {
	path := dutyWriteEvents(t, dutyDeliveries("agent-a", 1, 5, 1, 0))
	out := dutyRun(t, "bands", "--events", path)
	if !strings.Contains(out, "SEED") || !strings.Contains(out, "STREAK") || !strings.Contains(out, "agent-a") || !strings.Contains(out, "L2") {
		t.Fatal(out)
	}
}

// dutyLine returns the rendered line containing needle.
func dutyLine(t *testing.T, out, needle string) string {
	t.Helper()
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, needle) {
			return line
		}
	}
	t.Fatalf("no line containing %q in:\n%s", needle, out)
	return ""
}

// --- the frozen default view -----------------------------------------------

// Without --duty, the existing bashy-leaderboard-v1 view must not change by a
// byte: the bashy app /sprint/ tab renders it.
func TestLeaderboardDefaultViewUnchangedWithoutDutyFlag(t *testing.T) {
	t.Setenv("BASHY_CAPABILITY_DIR", t.TempDir())
	t.Setenv("BASHY_HOME", t.TempDir())
	for i := 0; i < 6; i++ {
		rec := gated("agent-a:m", i != 0)
		rec.At = fmt.Sprintf("2026-08-02T00:00:0%dZ", i)
		if err := Append(rec); err != nil {
			t.Fatal(err)
		}
	}

	got := dutyRun(t)

	recs, err := ReadLedger()
	if err != nil {
		t.Fatal(err)
	}
	m, _ := Load()
	board := Compute(recs, ComputeOptions{MinSamples: DefaultMinSamples, Matrix: m})
	var want bytes.Buffer
	if err := renderTable(&want, board); err != nil {
		t.Fatal(err)
	}
	if got != want.String() {
		t.Errorf("default view drifted from the existing renderer.\ngot:\n%s\nwant:\n%s", got, want.String())
	}
}

// --- the duty ladder --------------------------------------------------------

func TestDutyLadderRanksByConservativeLowerBound(t *testing.T) {
	t.Setenv("BASHY_HOME", t.TempDir())
	certs := []ladder.CertKind{ladder.CertL1, ladder.CertL2, ladder.CertL3, ladder.CertSteer}
	var events []ladder.Event
	events = append(events, dutyDeliveries("agent-strong", 1, 8, 1, 0)...)
	events = append(events, dutyDeliveries("agent-weak", 1, 8, 0.5, 0)...)
	events = append(events, dutyCertEvents("agent-strong", 1, certs...)...)
	events = append(events, dutyCertEvents("agent-weak", 1, certs...)...)
	path := dutyWriteEvents(t, events)

	board := dutyRunBoard(t, "--duty", "code", "--events", path, "--json")
	rows := board.Duties["code"]
	if len(rows) != 2 {
		t.Fatalf("got %d rows, want 2", len(rows))
	}
	if rows[0].Agent != "agent-strong" {
		t.Errorf("top row = %q, want agent-strong", rows[0].Agent)
	}
	if rows[0].Lower <= rows[1].Lower {
		t.Errorf("rows not ordered by lower bound: %v then %v", rows[0].Lower, rows[1].Lower)
	}
	if rows[0].Rank != 1 {
		t.Errorf("top rank = %d, want 1", rows[0].Rank)
	}
	strong := rows[0]
	if strong.Events != 8 || !strong.Established {
		t.Errorf("events/established = %d/%v, want 8/true", strong.Events, strong.Established)
	}
	if strong.Lower != strong.R-2*strong.RD {
		t.Errorf("lower = %v, want R-2RD = %v", strong.Lower, strong.R-2*strong.RD)
	}
}

// No rank is claimed on an inseparable gap: overlapping [R-2RD, R+2RD]
// intervals share the rank of the group's top and print as an approx marker.
func TestDutyRanksInseparableGaps(t *testing.T) {
	rows := []DutyRow{
		{Agent: "agent-a", R: 1600, RD: 50}, // [1500, 1700]
		{Agent: "agent-b", R: 1450, RD: 30}, // [1390, 1510] overlaps a
		{Agent: "agent-c", R: 1200, RD: 20}, // [1160, 1240] separable
		{Agent: "agent-d", R: 1190, RD: 20}, // [1150, 1230] overlaps c
	}
	for i := range rows {
		rows[i].Lower = rows[i].R - 2*rows[i].RD
	}
	dutyAssignRanks(rows)
	want := []struct {
		rank      int
		separable bool
	}{{1, true}, {1, false}, {3, true}, {3, false}}
	for i, w := range want {
		if rows[i].Rank != w.rank || rows[i].Separable != w.separable {
			t.Errorf("row %d (%s): rank/separable = %d/%v, want %d/%v",
				i, rows[i].Agent, rows[i].Rank, rows[i].Separable, w.rank, w.separable)
		}
	}
}

func TestDutyDerivedBandAndMissingReasons(t *testing.T) {
	t.Setenv("BASHY_HOME", t.TempDir())
	var events []ladder.Event
	events = append(events, dutyDeliveries("agent-a", 1, 8, 1, 0)...)
	events = append(events, dutyCertEvents("agent-a", 1, ladder.CertL1, ladder.CertL2)...)
	path := dutyWriteEvents(t, events)

	board := dutyRunBoard(t, "--duty", "code", "--events", path, "--json")
	row := dutyFindRow(t, board.Duties["code"], "agent-a")
	if row.Band != 2 {
		t.Errorf("band = %d, want 2 (no L3 or steer certificate)", row.Band)
	}
	// Gate 3 has three misses (l3 cert, steer cert, unfitted line): the view
	// shows the first two and elides the rest.
	if len(row.Missing) != 3 || row.Missing[2] != "..." {
		t.Fatalf("missing = %v, want first 2 reasons then \"...\"", row.Missing)
	}
	if !strings.Contains(row.Missing[0], "l3 certificate") {
		t.Errorf("missing[0] = %q, want the l3 certificate miss", row.Missing[0])
	}

	// The text view prints the lines header with unfitted zeros named.
	out := dutyRun(t, "--duty", "code", "--events", path)
	if !strings.Contains(out, "lines:") || !strings.Contains(out, "unfitted") {
		t.Errorf("lines header with unfitted markers absent from:\n%s", out)
	}
	if !strings.Contains(out, "L2") {
		t.Errorf("band cell absent from:\n%s", out)
	}
}

func TestDutyProvisionalSeatIgnored(t *testing.T) {
	t.Setenv("BASHY_HOME", t.TempDir())
	events := []ladder.Event{dutySeat("agent-a", 1, 5)}
	path := dutyWriteEvents(t, events)

	board := dutyRunBoard(t, "--duty", "code", "--events", path, "--json")
	row := dutyFindRow(t, board.Duties["code"], "agent-a")
	if row.Band != 1 || row.Seed != 1 || row.Moved {
		t.Errorf("band/seed/moved = %d/%d/%v, want 1/1/false", row.Band, row.Seed, row.Moved)
	}
	out := dutyRun(t, "--duty", "code", "--events", path)
	if strings.Contains(out, "prov") {
		t.Errorf("provisional display remains:\n%s", out)
	}
}

// A provisional seat no longer changes the current band or currency duty.
func TestDutyCurrencyIgnoresProvisionalSeat(t *testing.T) {
	t.Setenv("BASHY_HOME", t.TempDir())
	var events []ladder.Event
	events = append(events, dutyDeliveries("agent-idle", 1, 8, 1, 0)...)
	events = append(events, dutySeat("agent-idle", 1, 4))
	events = append(events, dutyDeliveries("agent-current", 2, 2, 1, 0)...)
	events = append(events, dutySeat("agent-current", 1, 4))
	path := dutyWriteEvents(t, events)

	board := dutyRunBoard(t, "--duty", "code", "--events", path, "--json")
	rows := board.Duties["code"]
	if board.Season != 2 {
		t.Fatalf("season = %d, want the store maximum 2", board.Season)
	}
	if got := dutyFindRow(t, rows, "agent-idle").Currency; got != "" {
		t.Errorf("idle seat currency = %q, want empty", got)
	}
	if got := dutyFindRow(t, rows, "agent-current").Currency; got != "" {
		t.Errorf("current seat currency = %q, want empty", got)
	}
}

func TestDutyMoveAgainstPreviousSeason(t *testing.T) {
	t.Setenv("BASHY_HOME", t.TempDir())
	var events []ladder.Event
	events = append(events, dutyDeliveries("agent-riser", 1, 4, 1, 0)...)
	events = append(events, dutyDeliveries("agent-riser", 2, 4, 1, 0)...)
	events = append(events, dutyDeliveries("agent-new", 2, 1, 1, 0)...)
	path := dutyWriteEvents(t, events)

	board := dutyRunBoard(t, "--duty", "code", "--events", path, "--json")
	rows := board.Duties["code"]
	riser := dutyFindRow(t, rows, "agent-riser")
	if !strings.HasPrefix(riser.Move, "+") || riser.Move == "+0" {
		t.Errorf("riser move = %q, want a positive delta", riser.Move)
	}
	if got := dutyFindRow(t, rows, "agent-new").Move; got != "new" {
		t.Errorf("first-season move = %q, want \"new\"", got)
	}
}

// Absence is not failure: an empty store names itself and exits 0.
func TestDutyEmptyStoreIsAStateNotAFailure(t *testing.T) {
	t.Setenv("BASHY_HOME", t.TempDir())
	path := filepath.Join(t.TempDir(), "never-written.jsonl")
	out := dutyRun(t, "--duty", "code", "--events", path)
	if !strings.Contains(out, "no rated events") || !strings.Contains(out, path) {
		t.Errorf("empty-store message must say there is nothing yet and where the store is; got:\n%s", out)
	}
}

func TestDutyCostViewSkipsZeroCost(t *testing.T) {
	t.Setenv("BASHY_HOME", t.TempDir())
	var events []ladder.Event
	events = append(events, dutyDeliveries("agent-priced", 1, 4, 1, 2.5)...)
	events = append(events, dutyDeliveries("agent-free", 1, 4, 1, 0)...)
	path := dutyWriteEvents(t, events)

	out := dutyRun(t, "--duty", "code", "--cost", "--events", path)
	if !strings.Contains(out, "informational — routing only, never promotes") {
		t.Errorf("cost view header missing from:\n%s", out)
	}
	if !strings.Contains(dutyLine(t, out, "agent-free"), "no cost recorded") {
		t.Errorf("zero-cost agent must read \"no cost recorded\"; got:\n%s", out)
	}
	if strings.Contains(dutyLine(t, out, "agent-priced"), "no cost recorded") {
		t.Errorf("priced agent wrongly marked unpriced:\n%s", out)
	}
}

func TestDutyJSONSchema(t *testing.T) {
	t.Setenv("BASHY_HOME", t.TempDir())
	var events []ladder.Event
	events = append(events, dutyDeliveries("agent-a", 1, 2, 1, 0)...)
	path := dutyWriteEvents(t, events)

	linesPath := filepath.Join(t.TempDir(), "lines.json")
	if err := os.WriteFile(linesPath, []byte(`{"L4Manage": 1200}`), 0o600); err != nil {
		t.Fatal(err)
	}

	out := dutyRun(t, "--duty", "all", "--events", path, "--lines", linesPath, "--json")
	var raw map[string]json.RawMessage
	if err := json.Unmarshal([]byte(out), &raw); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, out)
	}
	for _, key := range []string{"schema_version", "season", "lines", "duties"} {
		if _, ok := raw[key]; !ok {
			t.Errorf("envelope missing %q; got keys %v", key, out)
		}
	}
	var board DutyBoard
	if err := json.Unmarshal([]byte(out), &board); err != nil {
		t.Fatal(err)
	}
	if board.SchemaVersion != LeaderboardDutySchema {
		t.Errorf("schema_version = %q, want %q", board.SchemaVersion, LeaderboardDutySchema)
	}
	for _, duty := range []string{"code", "manage", "judge"} {
		if _, ok := board.Duties[duty]; !ok {
			t.Errorf("duties missing %q", duty)
		}
	}
	if board.Lines.L4Manage != 1200 {
		t.Errorf("--lines override lost: L4Manage = %v, want 1200", board.Lines.L4Manage)
	}
	var duties map[string][]map[string]json.RawMessage
	if err := json.Unmarshal(raw["duties"], &duties); err != nil {
		t.Fatal(err)
	}
	row := duties["code"][0]
	for _, key := range []string{"rank", "separable", "agent", "r", "rd", "lower", "events", "established", "band", "move"} {
		if _, ok := row[key]; !ok {
			t.Errorf("row missing field %q; got %v", key, row)
		}
	}
}

func TestHeadToHeadPairsHeatsAndMcNemar(t *testing.T) {
	events := []ladder.Event{
		{Season: 1, Kind: ladder.EventKindDelivery, Agent: "agent-a", Duty: ladder.DutyCode, Points: 3, Outcome: 1, Note: "heat:one"},
		{Season: 1, Kind: ladder.EventKindDelivery, Agent: "agent-b", Duty: ladder.DutyCode, Points: 3, Outcome: 0, Note: "heat:one"},
		{Season: 1, Kind: ladder.EventKindDelivery, Agent: "agent-a", Duty: ladder.DutyCode, Points: 3, Outcome: 0, Note: "heat:two"},
		{Season: 1, Kind: ladder.EventKindDelivery, Agent: "agent-b", Duty: ladder.DutyCode, Points: 3, Outcome: 1, Note: "heat:two"},
		{Season: 1, Kind: ladder.EventKindDelivery, Agent: "agent-a", Duty: ladder.DutyCode, Points: 3, Outcome: 1, Note: "heat:three"},
		{Season: 1, Kind: ladder.EventKindDelivery, Agent: "agent-b", Duty: ladder.DutyCode, Points: 3, Outcome: 1, Note: "heat:three"},
	}
	rows := ComputeHeadToHead(events, 1, ladder.DutyCode)
	if len(rows) != 1 {
		t.Fatalf("rows = %d, want 1", len(rows))
	}
	r := rows[0]
	if r.Shared != 3 || r.WinsA != 1 || r.WinsB != 1 || r.Ties != 1 || r.Discordant != 2 || r.P != 1 || !r.Inseparable {
		t.Fatalf("row = %+v", r)
	}
}

func TestHeadToHeadExactMcNemar(t *testing.T) {
	if got := ExactMcNemar(8, 2); got < 0.109374 || got > 0.109376 {
		t.Fatalf("p = %g, want 0.109375", got)
	}
}

func TestHeadToHeadJSONAndInseparableText(t *testing.T) {
	t.Setenv("BASHY_HOME", t.TempDir())
	events := []ladder.Event{
		{Season: 1, Kind: ladder.EventKindDelivery, Agent: "agent-a", Duty: ladder.DutyCode, Points: 3, Outcome: 1, Note: "heat:one"},
		{Season: 1, Kind: ladder.EventKindDelivery, Agent: "agent-b", Duty: ladder.DutyCode, Points: 3, Outcome: 0, Note: "heat:one"},
	}
	path := dutyWriteEvents(t, events)
	board := dutyRunBoard(t, "--h2h", "--events", path, "--json")
	if len(board.H2H) != 1 || !board.H2H[0].Inseparable {
		t.Fatalf("h2h = %+v, want one inseparable pair", board.H2H)
	}
	out := dutyRun(t, "--h2h", "--events", path)
	if !strings.Contains(out, "inseparable") {
		t.Fatalf("text must label inseparable pair:\n%s", out)
	}
}
