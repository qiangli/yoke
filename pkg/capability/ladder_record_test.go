package capability

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/qiangli/yoke/pkg/fleet"
	"github.com/qiangli/yoke/pkg/ladder"
)

// ladderRecordRun executes the leaderboard command in-process and returns
// whatever it printed plus the run error. It never execs any binary.
func ladderRecordRun(t *testing.T, args ...string) (string, error) {
	t.Helper()
	cmd := NewLeaderboardCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs(args)
	err := cmd.Execute()
	return out.String(), err
}

func ladderRecordMust(t *testing.T, args ...string) string {
	t.Helper()
	out, err := ladderRecordRun(t, args...)
	if err != nil {
		t.Fatalf("leaderboard %v: %v\n%s", args, err, out)
	}
	return out
}

func ladderRecordStore(t *testing.T) []ladder.Event {
	t.Helper()
	st, err := ladder.OpenStore(ladder.DefaultStorePath())
	if err != nil {
		t.Fatal(err)
	}
	events, err := st.Read()
	if err != nil {
		t.Fatal(err)
	}
	return events
}

func TestLadderRecordSeat(t *testing.T) {
	t.Setenv("BASHY_HOME", t.TempDir())
	t.Setenv("BASHY_AGENT", "agent-a")
	out := ladderRecordMust(t, "record", "seat",
		"--agent", "tool-a:model-a", "--band", "5",
		"--reason", "owner seats provisional", "--season", "7")
	events := ladderRecordStore(t)
	if len(events) != 1 {
		t.Fatalf("want 1 event, got %d", len(events))
	}
	e := events[0]
	if e.Kind != ladder.EventKindSeat {
		t.Errorf("kind = %q, want seat", e.Kind)
	}
	if e.Agent != "tool-a:model-a" {
		t.Errorf("agent = %q", e.Agent)
	}
	if e.Provisional != 5 {
		t.Errorf("provisional = %d, want 5", e.Provisional)
	}
	if e.Season != 7 {
		t.Errorf("season = %d, want 7", e.Season)
	}
	if e.Note != "owner seats provisional" {
		t.Errorf("note = %q", e.Note)
	}
	if e.ID == "" {
		t.Error("empty event id")
	}
	if !strings.Contains(out, e.ID) {
		t.Errorf("confirmation line %q does not carry event id %q", out, e.ID)
	}
	if e.Reviewer != "agent-a" {
		t.Errorf("reviewer = %q, want agent-a", e.Reviewer)
	}
	if e.At.IsZero() {
		t.Error("zero At timestamp")
	}
	rep := ladder.Replay(events, 7)
	rec := rep.Agents["tool-a:model-a"]
	if rec == nil {
		t.Fatal("Replay has no record for tool-a:model-a")
	}
	if rec.Provisional != 5 {
		t.Errorf("Replay provisional = %d, want 5", rec.Provisional)
	}
}

func TestLadderRecordSeatClears(t *testing.T) {
	t.Setenv("BASHY_HOME", t.TempDir())
	t.Setenv("BASHY_AGENT", "agent-a")
	ladderRecordMust(t, "record", "seat",
		"--agent", "tool-a:model-a", "--band", "5",
		"--reason", "seat it", "--season", "7")
	ladderRecordMust(t, "record", "seat",
		"--agent", "tool-a:model-a", "--band", "0",
		"--reason", "gates confirmed", "--season", "8")
	events := ladderRecordStore(t)
	if len(events) != 2 {
		t.Fatalf("want 2 events, got %d", len(events))
	}
	rep := ladder.Replay(events, 8)
	if got := rep.Agents["tool-a:model-a"].Provisional; got != 0 {
		t.Errorf("Replay provisional = %d, want 0 after clear", got)
	}
}

func TestLadderRecordCert(t *testing.T) {
	t.Setenv("BASHY_HOME", t.TempDir())
	t.Setenv("BASHY_AGENT", "agent-a")
	out := ladderRecordMust(t, "record", "cert",
		"--agent", "tool-a:model-a", "--kind", "l3",
		"--model-version", "v9", "--evidence", "run-123",
		"--season", "7")
	events := ladderRecordStore(t)
	if len(events) != 1 {
		t.Fatalf("want 1 event, got %d", len(events))
	}
	e := events[0]
	if e.Kind != ladder.EventKindCert {
		t.Errorf("kind = %q, want cert", e.Kind)
	}
	if e.Cert != (ladder.Certificate{Kind: ladder.CertL3, ModelVersion: "v9", Season: 7}) {
		t.Errorf("cert = %+v", e.Cert)
	}
	if e.Season != 7 {
		t.Errorf("season = %d, want 7", e.Season)
	}
	if !strings.Contains(e.Note, "run-123") {
		t.Errorf("note %q does not carry the evidence ref", e.Note)
	}
	if !strings.Contains(out, e.ID) {
		t.Errorf("confirmation line %q does not carry event id %q", out, e.ID)
	}
	rep := ladder.Replay(events, 7)
	rec := rep.Agents["tool-a:model-a"]
	if rec == nil {
		t.Fatal("Replay has no record for tool-a:model-a")
	}
	if len(rec.Certs) != 1 || rec.Certs[0].Kind != ladder.CertL3 {
		t.Errorf("Replay certs = %+v, want one l3", rec.Certs)
	}
}

func TestLadderRecordCorrection(t *testing.T) {
	t.Setenv("BASHY_HOME", t.TempDir())
	t.Setenv("BASHY_AGENT", "agent-a")
	ladderRecordMust(t, "record", "seat",
		"--agent", "tool-a:model-a", "--band", "5",
		"--reason", "seat it", "--season", "7")
	bad := ladderRecordStore(t)[0]
	out := ladderRecordMust(t, "record", "correction",
		"--supersedes", bad.ID, "--reason", "wrong band",
		"--season", "7")
	events := ladderRecordStore(t)
	if len(events) != 2 {
		t.Fatalf("want 2 events, got %d", len(events))
	}
	e := events[1]
	if e.Kind != ladder.EventKindCorrection {
		t.Errorf("kind = %q, want correction", e.Kind)
	}
	if e.Supersedes != bad.ID {
		t.Errorf("supersedes = %q, want %q", e.Supersedes, bad.ID)
	}
	if e.Note != "wrong band" {
		t.Errorf("note = %q", e.Note)
	}
	if e.Season != 7 {
		t.Errorf("season = %d, want 7", e.Season)
	}
	if !strings.Contains(out, e.ID) {
		t.Errorf("confirmation line %q does not carry event id %q", out, e.ID)
	}
	rep := ladder.Replay(events, 7)
	if rec := rep.Agents["tool-a:model-a"]; rec != nil && rec.Provisional != 0 {
		t.Errorf("Replay provisional = %d, want the seat corrected away", rec.Provisional)
	}
}

func TestLadderRecordValidation(t *testing.T) {
	cases := []struct {
		name string
		args []string
	}{
		{"band too high", []string{"record", "seat", "--agent", "tool-a:model-a", "--band", "6", "--reason", "r", "--season", "7"}},
		{"band missing", []string{"record", "seat", "--agent", "tool-a:model-a", "--reason", "r", "--season", "7"}},
		{"reason missing", []string{"record", "seat", "--agent", "tool-a:model-a", "--band", "5", "--season", "7"}},
		{"season missing", []string{"record", "seat", "--agent", "tool-a:model-a", "--band", "5", "--reason", "r"}},
		{"agent no colon", []string{"record", "seat", "--agent", "model-a", "--band", "5", "--reason", "r", "--season", "7"}},
		{"agent two colons", []string{"record", "seat", "--agent", "a:b:c", "--band", "5", "--reason", "r", "--season", "7"}},
		{"agent slash", []string{"record", "seat", "--agent", "tool-a/mo:del", "--band", "5", "--reason", "r", "--season", "7"}},
		{"agent space", []string{"record", "seat", "--agent", "tool a:model-a", "--band", "5", "--reason", "r", "--season", "7"}},
		{"cert bad kind", []string{"record", "cert", "--agent", "tool-a:model-a", "--kind", "l9", "--model-version", "v9", "--evidence", "run-1", "--season", "7"}},
		{"cert no evidence", []string{"record", "cert", "--agent", "tool-a:model-a", "--kind", "l3", "--model-version", "v9", "--season", "7"}},
		{"cert no model version", []string{"record", "cert", "--agent", "tool-a:model-a", "--kind", "l3", "--evidence", "run-1", "--season", "7"}},
		{"cert bad agent", []string{"record", "cert", "--agent", "model-a", "--kind", "l3", "--model-version", "v9", "--evidence", "run-1", "--season", "7"}},
		{"correction no supersedes", []string{"record", "correction", "--reason", "r", "--season", "7"}},
		{"correction no reason", []string{"record", "correction", "--supersedes", "some-id", "--season", "7"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("BASHY_HOME", t.TempDir())
			t.Setenv("BASHY_AGENT", "agent-a")
			out, err := ladderRecordRun(t, tc.args...)
			if err == nil {
				t.Fatalf("want error, got success: %s", out)
			}
			if got := len(ladderRecordStore(t)); got != 0 {
				t.Errorf("validation failure still wrote %d events", got)
			}
		})
	}
}

func TestLadderRecordList(t *testing.T) {
	t.Setenv("BASHY_HOME", t.TempDir())
	t.Setenv("BASHY_AGENT", "agent-a")
	ladderRecordMust(t, "record", "seat",
		"--agent", "tool-a:model-a", "--band", "5",
		"--reason", "seat a", "--season", "7")
	ladderRecordMust(t, "record", "cert",
		"--agent", "tool-b:model-b", "--kind", "l3",
		"--model-version", "v9", "--evidence", "run-1",
		"--season", "7")
	ladderRecordMust(t, "record", "seat",
		"--agent", "tool-a:model-a", "--band", "0",
		"--reason", "clear a", "--season", "8")

	out := ladderRecordMust(t, "record", "list")
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) != 3 {
		t.Fatalf("want 3 lines, got %d:\n%s", len(lines), out)
	}
	if !strings.Contains(lines[0], "tool-a:model-a") || !strings.Contains(lines[0], "seat") {
		t.Errorf("newest first: first line = %q", lines[0])
	}
	if !strings.Contains(lines[2], "seat") || !strings.Contains(lines[2], "tool-a:model-a") {
		t.Errorf("oldest last: last line = %q", lines[2])
	}

	out = ladderRecordMust(t, "record", "list", "--agent", "tool-b:model-b")
	if lines = strings.Split(strings.TrimSpace(out), "\n"); len(lines) != 1 {
		t.Fatalf("agent filter: want 1 line, got %d:\n%s", len(lines), out)
	}
	if !strings.Contains(lines[0], "tool-b:model-b") || !strings.Contains(lines[0], "cert") {
		t.Errorf("agent filter line = %q", lines[0])
	}

	out = ladderRecordMust(t, "record", "list", "--kind", "cert")
	if lines = strings.Split(strings.TrimSpace(out), "\n"); len(lines) != 1 {
		t.Fatalf("kind filter: want 1 line, got %d:\n%s", len(lines), out)
	}

	out = ladderRecordMust(t, "record", "list", "-n", "1")
	if lines = strings.Split(strings.TrimSpace(out), "\n"); len(lines) != 1 {
		t.Fatalf("-n 1: want 1 line, got %d:\n%s", len(lines), out)
	}
	if !strings.Contains(lines[0], "tool-a:model-a") {
		t.Errorf("-n 1 should keep the newest: %q", lines[0])
	}

	out = ladderRecordMust(t, "record", "list", "--json")
	var rows []map[string]any
	if err := json.Unmarshal([]byte(out), &rows); err != nil {
		t.Fatalf("list --json is not a JSON array: %v\n%s", err, out)
	}
	if len(rows) != 3 {
		t.Fatalf("list --json: want 3 rows, got %d", len(rows))
	}
	for _, k := range []string{"id", "at", "season", "kind", "agent", "summary"} {
		if _, ok := rows[0][k]; !ok {
			t.Errorf("list --json row missing key %q: %v", k, rows[0])
		}
	}
}

func TestLadderRecordReviewerFallback(t *testing.T) {
	t.Setenv("BASHY_HOME", t.TempDir())
	t.Setenv("BASHY_AGENT", "")
	ladderRecordMust(t, "record", "seat",
		"--agent", "tool-a:model-a", "--band", "5",
		"--reason", "seat it", "--season", "7")
	e := ladderRecordStore(t)[0]
	if e.Reviewer == "" {
		t.Error("empty reviewer without BASHY_AGENT")
	}
	if e.Reviewer == "agent-a" {
		t.Error("reviewer leaked the test's usual actor with BASHY_AGENT unset")
	}
}

func TestLadderRecordIDsUnique(t *testing.T) {
	t.Setenv("BASHY_HOME", t.TempDir())
	t.Setenv("BASHY_AGENT", "agent-a")
	for i := 0; i < 5; i++ {
		ladderRecordMust(t, "record", "seat",
			"--agent", "tool-a:model-a", "--band", "5",
			"--reason", "seat it", "--season", "7")
	}
	events := ladderRecordStore(t)
	seen := map[string]bool{}
	for _, e := range events {
		if e.ID == "" {
			t.Fatal("empty event id")
		}
		if seen[e.ID] {
			t.Fatalf("duplicate event id %q", e.ID)
		}
		seen[e.ID] = true
	}
}

// ladderSeedFleet pins newCatalog to a scratch fleet: model-x is bound under
// two tools (plus a clone that must not be seeded), model-q has no seedfit row,
// and the seedfit TSV names model-y, which no agent is bound to.
func ladderSeedFleet(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	cat := fleet.New(fleet.WithRoot(root), fleet.WithBaselineFS(fstest.MapFS{}))
	for _, tool := range []string{"tool-a", "tool-b", "tool-c"} {
		if err := cat.SaveTool(fleet.Tool{Name: tool}); err != nil {
			t.Fatal(err)
		}
	}
	for _, m := range []fleet.Model{
		{Name: "model-x1", Aliases: []string{"model-x"}},
		{Name: "model-q"},
	} {
		if err := cat.SaveModel(m); err != nil {
			t.Fatal(err)
		}
	}
	for _, a := range []fleet.Agent{
		{Name: "a-x", Tool: "tool-a", Model: "model-x1"},
		{Name: "a-x-nick", Tool: "tool-a", Model: "model-x1"},
		{Name: "b-x", Tool: "tool-b", Model: "model-x1"},
		{Name: "c-q", Tool: "tool-c", Model: "model-q"},
		{Name: "a-x-2", Tool: "tool-a", Model: "model-x1", ClonedFrom: "a-x", ClonedAt: "2026-09-01T00:00:00Z"},
	} {
		if err := cat.SaveAgent(a); err != nil {
			t.Fatal(err)
		}
	}
	prev := newCatalog
	newCatalog = func() *fleet.Catalog {
		return fleet.New(fleet.WithRoot(root), fleet.WithBaselineFS(fstest.MapFS{}))
	}
	t.Cleanup(func() { newCatalog = prev })

	tsv := "# method: test\n# skipped: \n" +
		"model\tvendor\ttheta\tlo90\thi90\tn\tcode_rating\trd\tplacement\tadjusted\n" +
		"model-x\tvendor-a\t0.500\t0.100\t0.900\t12\t1700\t90\tL3\tfalse\n" +
		"model-y\tvendor-b\t-0.200\t-0.600\t0.200\t4\t1420\t210\tL2\tfalse\n"
	path := filepath.Join(t.TempDir(), "seedfit.tsv")
	if err := os.WriteFile(path, []byte(tsv), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func ladderSeedAgents(events []ladder.Event) []string {
	var got []string
	for _, e := range events {
		got = append(got, e.Agent)
	}
	sort.Strings(got)
	return got
}

func TestLadderRecordSeedImportMapsModelsToAgents(t *testing.T) {
	t.Setenv("BASHY_HOME", t.TempDir())
	tsv := ladderSeedFleet(t)
	out := ladderRecordMust(t, "record", "seed", "--from-seedfit", tsv)
	events := ladderRecordStore(t)
	// Two tools bind model-x (by alias in the TSV); the nickname collapses
	// onto its binding and the clone is skipped.
	if got := ladderSeedAgents(events); strings.Join(got, ",") != "tool-a:model-x1,tool-b:model-x1" {
		t.Fatalf("seeded agents = %v\n%s", got, out)
	}
	for _, e := range events {
		if e.Kind != ladder.EventKindSeed || e.Duty != ladder.DutyCode || e.SeedR != 1700 || e.SeedRD != 90 || e.Season != 1 {
			t.Errorf("seed event = %+v", e)
		}
		if e.ID == "" || !strings.Contains(out, e.ID) {
			t.Errorf("summary does not carry event id %q:\n%s", e.ID, out)
		}
	}
	for _, want := range []string{"agents seeded: 2", "seedfit models with no fleet agent: model-y", "fleet agents with no seedfit row: tool-c:model-q"} {
		if !strings.Contains(out, want) {
			t.Errorf("summary missing %q:\n%s", want, out)
		}
	}
	// The imported seed is the agent's starting code rating.
	res := ladder.Replay(events, 1)
	if r := res.Agents["tool-a:model-x1"].Standings[ladder.DutyCode].R; r < 1699 || r > 1701 {
		t.Errorf("replayed seed R = %v, want ~1700", r)
	}
}

func TestLadderRecordSeedToolFilters(t *testing.T) {
	t.Setenv("BASHY_HOME", t.TempDir())
	tsv := ladderSeedFleet(t)
	ladderRecordMust(t, "record", "seed", "--from-seedfit", tsv, "--tool", "tool-b")
	if got := ladderSeedAgents(ladderRecordStore(t)); strings.Join(got, ",") != "tool-b:model-x1" {
		t.Fatalf("--tool seeded %v", got)
	}

	t.Setenv("BASHY_HOME", t.TempDir())
	ladderRecordMust(t, "record", "seed", "--from-seedfit", tsv, "--tool-map", "model-x=tool-a,model-x=tool-b")
	if got := ladderSeedAgents(ladderRecordStore(t)); strings.Join(got, ",") != "tool-a:model-x1,tool-b:model-x1" {
		t.Fatalf("--tool-map seeded %v", got)
	}

	t.Setenv("BASHY_HOME", t.TempDir())
	ladderRecordMust(t, "record", "seed", "--from-seedfit", tsv, "--tool-map", "model-x=tool-a")
	if got := ladderSeedAgents(ladderRecordStore(t)); strings.Join(got, ",") != "tool-a:model-x1" {
		t.Fatalf("--tool-map seeded %v", got)
	}
	if _, err := ladderRecordRun(t, "record", "seed", "--from-seedfit", tsv, "--tool-map", "model-x"); err == nil {
		t.Error("malformed --tool-map accepted")
	}
}

func TestLadderRecordSeedDryRunWritesNothing(t *testing.T) {
	home := t.TempDir()
	t.Setenv("BASHY_HOME", home)
	tsv := ladderSeedFleet(t)
	out := ladderRecordMust(t, "record", "seed", "--from-seedfit", tsv, "--dry-run")
	if !strings.Contains(out, "tool-a:model-x1") || !strings.Contains(out, "dry run") {
		t.Errorf("dry run output:\n%s", out)
	}
	if _, err := os.Stat(ladder.DefaultStorePath()); !os.IsNotExist(err) {
		t.Fatalf("dry run touched the store: %v", err)
	}
}

func TestLadderRecordSeedManual(t *testing.T) {
	t.Setenv("BASHY_HOME", t.TempDir())
	out := ladderRecordMust(t, "record", "seed", "--agent", "tool-a:model-a", "--duty", "judge",
		"--r", "1600", "--rd", "120", "--reason", "owner prior")
	events := ladderRecordStore(t)
	if len(events) != 1 {
		t.Fatalf("want 1 event, got %d", len(events))
	}
	e := events[0]
	if e.Kind != ladder.EventKindSeed || e.Agent != "tool-a:model-a" || e.Duty != ladder.DutyJudge ||
		e.SeedR != 1600 || e.SeedRD != 120 || e.Note != "owner prior" {
		t.Fatalf("event = %+v", e)
	}
	if !strings.Contains(out, e.ID) {
		t.Errorf("confirmation %q lacks id %q", out, e.ID)
	}
	for _, bad := range [][]string{
		{"--agent", "tool-a:model-a", "--duty", "judge", "--r", "1600"},                          // no reason
		{"--agent", "tool-a:model-a", "--duty", "sing", "--r", "1600", "--reason", "x"},          // bad duty
		{"--agent", "model-a", "--r", "1600", "--reason", "x"},                                   // not tool:model
		{"--agent", "tool-a:model-a", "--reason", "x"},                                           // no rating
		{"--agent", "tool-a:model-a", "--r", "1600", "--reason", "x", "--from-seedfit", "f.tsv"}, // both modes
		{}, // neither
	} {
		if _, err := ladderRecordRun(t, append([]string{"record", "seed"}, bad...)...); err == nil {
			t.Errorf("accepted %v", bad)
		}
	}
}
