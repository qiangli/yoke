package weave

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/qiangli/yoke/pkg/capability"
	"github.com/qiangli/yoke/pkg/fleet/fleettest"
)

// pinCapabilityStore points the capability matrix at a scratch store and its
// seed at the test ring, the same way capability's own tests do.
func pinCapabilityStore(t *testing.T) {
	t.Helper()
	fleettest.Ring(t)
	t.Setenv("BASHY_CAPABILITY_DIR", t.TempDir())
}

func capabilityCell(t *testing.T, agent string, c capability.Capability) capability.Cell {
	t.Helper()
	m, err := capability.Load()
	if err != nil {
		t.Fatal(err)
	}
	row, ok := m.Agents[agent]
	if !ok {
		t.Fatalf("no matrix row for %s", agent)
	}
	cell, ok := row[c]
	if !ok {
		t.Fatalf("matrix row %s has no %s cell", agent, c)
	}
	return cell
}

// assertNoHostEvidence proves the matrix carries priors only — not one cell
// of one agent moved to a host source.
func assertNoHostEvidence(t *testing.T) {
	t.Helper()
	m, err := capability.Load()
	if err != nil {
		t.Fatal(err)
	}
	for agent, row := range m.Agents {
		for c, cell := range row {
			if cell.Source == capability.SourceHost || cell.Samples != 0 {
				t.Fatalf("host evidence recorded for %s/%s: %+v", agent, c, cell)
			}
		}
	}
}

func assertNoHostEvidenceFor(t *testing.T, c capability.Capability) {
	t.Helper()
	m, err := capability.Load()
	if err != nil {
		t.Fatal(err)
	}
	for agent, row := range m.Agents {
		if cell, ok := row[c]; ok && (cell.Source == capability.SourceHost || cell.Samples != 0) {
			t.Fatalf("%s evidence recorded for %s: %+v", c, agent, cell)
		}
	}
}

// THE FLEET-EVIDENCE-INVARIANT. A nil VerifyExit is the absence of evidence:
// the recorder must write NOTHING — not a pass, not a fail, not even an
// operability sample — no matter how finished the run otherwise looks.
func TestWeaveCapabilityNilVerifyExitRecordsNothing(t *testing.T) {
	pinFleet(t)
	pinCapabilityStore(t)
	it := &weaveItem{
		Tool:         "codex",
		CommitsAhead: 3,
		LaunchSpec:   &weaveLaunchSpec{Tool: "codex", Agent: "codex-gpt-5.5", Model: "gpt-5.5"},
	}
	weaveRecordCapability(it)
	assertNoHostEvidence(t)
}

// BASHY_NO_CAPABILITY_RECORD opts the host out entirely, even with perfect
// gate evidence on the table.
func TestWeaveCapabilityEnvOptOutRecordsNothing(t *testing.T) {
	pinFleet(t)
	pinCapabilityStore(t)
	t.Setenv("BASHY_NO_CAPABILITY_RECORD", "1")
	verify := 0
	it := &weaveItem{
		Tool:         "codex",
		CommitsAhead: 2,
		VerifyExit:   &verify,
		LaunchSpec:   &weaveLaunchSpec{Tool: "codex", Agent: "codex-gpt-5.5", Model: "gpt-5.5"},
	}
	weaveRecordCapability(it)
	assertNoHostEvidence(t)
}

// Attribution ladder, rung 1: the launch nickname resolves through the fleet
// catalog to the canonical tool:model row, and a clean gate lands a coding
// pass on it.
func TestWeaveCapabilityNicknameAttribution(t *testing.T) {
	pinFleet(t)
	pinCapabilityStore(t)
	prior := capabilityCell(t, "codex:gpt-5.5", capability.CapCoding)
	verify := 0
	it := &weaveItem{
		Tool:         "codex",
		CommitsAhead: 2,
		VerifyExit:   &verify,
		LaunchSpec:   &weaveLaunchSpec{Tool: "codex", Agent: "codex-gpt-5.5", Model: "gpt-5.5"},
	}
	weaveRecordCapability(it)
	cell := capabilityCell(t, "codex:gpt-5.5", capability.CapCoding)
	if cell.Source != capability.SourceHost || cell.Samples != 1 {
		t.Fatalf("coding cell is not one host sample: %+v", cell)
	}
	if cell.Quality <= prior.Quality {
		t.Fatalf("a gate pass must lift coding quality: prior %.4f, now %.4f", prior.Quality, cell.Quality)
	}
}

// Attribution ladder, rung 2: no nickname, but the provider-side model id
// resolves to the canonical model — attributed to tool:canonicalModel.
func TestWeaveCapabilityLaunchModelAttribution(t *testing.T) {
	pinFleet(t)
	pinCapabilityStore(t)
	verify := 0
	it := &weaveItem{
		Tool:         "codex",
		CommitsAhead: 1,
		VerifyExit:   &verify,
		LaunchSpec:   &weaveLaunchSpec{Tool: "codex", Model: "gpt-5.5"},
	}
	weaveRecordCapability(it)
	cell := capabilityCell(t, "codex:gpt-5.5", capability.CapCoding)
	if cell.Source != capability.SourceHost || cell.Samples != 1 {
		t.Fatalf("model-only launch was not attributed to codex:gpt-5.5: %+v", cell)
	}
}

// A nickname the catalog does not know falls THROUGH to the model rung — it
// does not strand the run at operability-only.
func TestWeaveCapabilityUnknownNicknameFallsToModelRung(t *testing.T) {
	pinFleet(t)
	pinCapabilityStore(t)
	verify := 0
	it := &weaveItem{
		Tool:         "codex",
		CommitsAhead: 1,
		VerifyExit:   &verify,
		LaunchSpec:   &weaveLaunchSpec{Tool: "codex", Agent: "no-such-nick", Model: "gpt-5.5"},
	}
	weaveRecordCapability(it)
	cell := capabilityCell(t, "codex:gpt-5.5", capability.CapCoding)
	if cell.Source != capability.SourceHost || cell.Samples != 1 {
		t.Fatalf("unknown nick should fall through to the model rung: %+v", cell)
	}
}

// Attribution ladder, rung 3: neither nick nor model resolves. The run is
// real, so the TOOL's operability takes the sample — but with no canonical
// identity there is NO attribution and NO quality row.
func TestWeaveCapabilityUnattributedRecordsOperabilityOnly(t *testing.T) {
	pinFleet(t)
	pinCapabilityStore(t)
	opPrior := capabilityCell(t, "codex:gpt-5.5", capability.CapOperability)
	verify := 0
	it := &weaveItem{
		Tool:         "codex",
		CommitsAhead: 1,
		VerifyExit:   &verify,
		LaunchSpec:   &weaveLaunchSpec{Tool: "codex", Model: "gpt-5.7-future"},
	}
	weaveRecordCapability(it)
	op := capabilityCell(t, "codex:gpt-5.5", capability.CapOperability)
	if op.Source != capability.SourceHost || op.Samples != 1 {
		t.Fatalf("operability-only rung left no operability sample: %+v", op)
	}
	if op.Quality <= opPrior.Quality {
		t.Fatalf("a gate pass must lift operability: prior %.4f, now %.4f", opPrior.Quality, op.Quality)
	}
	// No attribution, no quality row — anywhere in the matrix.
	assertNoHostEvidenceFor(t, capability.CapCoding)
	assertNoHostEvidenceFor(t, capability.CapCodeReview)
	assertNoHostEvidenceFor(t, capability.CapIsolation)
}

// The coding sample is the full gate conjunction: verify exit, suite-gate
// exit, commits ahead, and no isolation violation. Every failing conjunct is
// a fail sample, never silently absent.
func TestWeaveCapabilityGateOutcomeConjunction(t *testing.T) {
	for _, tc := range []struct {
		name    string
		verify  int
		suite   *int
		commits int
		pass    bool
	}{
		{"all green", 0, nil, 2, true},
		{"suite gate passed", 0, ptr(0), 2, true},
		{"verify failed", 1, nil, 2, false},
		{"suite gate failed", 0, ptr(1), 2, false},
		{"no commits", 0, nil, 0, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pinFleet(t)
			pinCapabilityStore(t)
			prior := capabilityCell(t, "codex:gpt-5.5", capability.CapCoding)
			verify := tc.verify
			it := &weaveItem{
				Tool:          "codex",
				CommitsAhead:  tc.commits,
				VerifyExit:    &verify,
				SuiteGateExit: tc.suite,
				LaunchSpec:    &weaveLaunchSpec{Tool: "codex", Agent: "codex-gpt-5.5", Model: "gpt-5.5"},
			}
			weaveRecordCapability(it)
			cell := capabilityCell(t, "codex:gpt-5.5", capability.CapCoding)
			if cell.Source != capability.SourceHost || cell.Samples != 1 {
				t.Fatalf("expected exactly one host sample: %+v", cell)
			}
			if tc.pass && cell.Quality <= prior.Quality {
				t.Fatalf("pass should lift quality: prior %.4f, now %.4f", prior.Quality, cell.Quality)
			}
			if !tc.pass && cell.Quality >= prior.Quality {
				t.Fatalf("fail should lower quality: prior %.4f, now %.4f", prior.Quality, cell.Quality)
			}
		})
	}
}

// An isolation violation is its own named sample: the coding gate fails (the
// conjunction includes !IsolationViolated) AND the isolation cell takes a
// fail — the escape is the harness fact the router must see.
func TestWeaveCapabilityIsolationViolationRecordsIsolationFail(t *testing.T) {
	pinFleet(t)
	pinCapabilityStore(t)
	isoPrior := capabilityCell(t, "codex:gpt-5.5", capability.CapIsolation)
	codingPrior := capabilityCell(t, "codex:gpt-5.5", capability.CapCoding)
	verify := 0
	it := &weaveItem{
		Tool:              "codex",
		CommitsAhead:      2,
		VerifyExit:        &verify,
		IsolationViolated: true,
		EscapedPaths:      []string{"../live"},
		LaunchSpec:        &weaveLaunchSpec{Tool: "codex", Agent: "codex-gpt-5.5", Model: "gpt-5.5"},
	}
	weaveRecordCapability(it)
	iso := capabilityCell(t, "codex:gpt-5.5", capability.CapIsolation)
	if iso.Source != capability.SourceHost || iso.Samples != 1 {
		t.Fatalf("isolation violation left no isolation sample: %+v", iso)
	}
	if iso.Quality >= isoPrior.Quality {
		t.Fatalf("an isolation violation must lower isolation quality: prior %.4f, now %.4f", isoPrior.Quality, iso.Quality)
	}
	coding := capabilityCell(t, "codex:gpt-5.5", capability.CapCoding)
	if coding.Samples != 1 || coding.Quality >= codingPrior.Quality {
		t.Fatalf("the gate conjunction must read the violation as a coding fail: %+v (prior %.4f)", coding, codingPrior.Quality)
	}
}

// The pair verdict accrues to the REVIEWER's code-review row — the coder's
// coding row takes the gate outcome, and never the twain.
func TestWeaveCapabilityPairVerdictRecordsReviewer(t *testing.T) {
	for _, tc := range []struct {
		name      string
		verdict   string
		wantMoved bool
		wantUp    bool
	}{
		{"pair pass", "pass", true, true},
		{"pair refuted", "refuted", true, false},
		{"harness error is not evidence", "harness-error", false, false},
		{"broken before is not evidence", "broken-before", false, false},
		{"no verdict is not evidence", "", false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pinFleet(t)
			pinCapabilityStore(t)
			reviewPrior := capabilityCell(t, "codex:gpt-5.5", capability.CapCodeReview)
			verify := 0
			it := &weaveItem{
				Tool:         "claude",
				CommitsAhead: 2,
				VerifyExit:   &verify,
				LaunchSpec:   &weaveLaunchSpec{Tool: "claude", Agent: "claude-opus4.8", Model: "opus4.8"},
				PairVerdict:  tc.verdict,
				ReviewAgent:  "codex:gpt-5.5",
			}
			weaveRecordCapability(it)
			// The coder's own row took the coding sample regardless.
			coding := capabilityCell(t, "claude:opus4.8", capability.CapCoding)
			if coding.Source != capability.SourceHost || coding.Samples != 1 {
				t.Fatalf("coder's coding sample missing: %+v", coding)
			}
			review := capabilityCell(t, "codex:gpt-5.5", capability.CapCodeReview)
			if !tc.wantMoved {
				if review.Source == capability.SourceHost || review.Samples != 0 {
					t.Fatalf("verdict %q must not move code-review: %+v", tc.verdict, review)
				}
				return
			}
			if review.Source != capability.SourceHost || review.Samples != 1 {
				t.Fatalf("verdict %q left no code-review sample: %+v", tc.verdict, review)
			}
			if tc.wantUp && review.Quality <= reviewPrior.Quality {
				t.Fatalf("pass verdict should lift review quality: prior %.4f, now %.4f", reviewPrior.Quality, review.Quality)
			}
			if !tc.wantUp && review.Quality >= reviewPrior.Quality {
				t.Fatalf("refuted verdict should lower review quality: prior %.4f, now %.4f", reviewPrior.Quality, review.Quality)
			}
			// The review verdict must not leak into the reviewer's CODING row.
			assertNoHostEvidenceFor(t, capability.CapIsolation)
		})
	}
}

// A reviewer the registry can no longer resolve has no attribution — the
// verdict is dropped, not guessed onto a neighboring row.
func TestWeaveCapabilityUnresolvableReviewerRecordsNoReview(t *testing.T) {
	pinFleet(t)
	pinCapabilityStore(t)
	verify := 0
	it := &weaveItem{
		Tool:         "claude",
		CommitsAhead: 2,
		VerifyExit:   &verify,
		LaunchSpec:   &weaveLaunchSpec{Tool: "claude", Agent: "claude-opus4.8", Model: "opus4.8"},
		PairVerdict:  "pass",
		ReviewAgent:  "ghost:nonexistent",
	}
	weaveRecordCapability(it)
	assertNoHostEvidenceFor(t, capability.CapCodeReview)
}

func ptr(i int) *int { return &i }

// THE SEAT MARKER. docs/role-capability-evidence-matrix.md §2 names this as the
// one thing missing before a conductor claim can be evidence: "attribute by what
// the run did, not by assuming what it was". A worker run's repeat ratio filed
// under orchestration is the misattribution that made gemini3.1's demotion
// `operator` rather than `measured`.
func TestWeaveLedgerRole(t *testing.T) {
	dir := t.TempDir()
	ws := filepath.Join(dir, "workspaces")
	if err := os.MkdirAll(ws, 0o755); err != nil {
		t.Fatal(err)
	}
	q := &weaveQueue{NextID: 4, Items: []*weaveItem{
		{ID: 1, Workspace: filepath.Join(ws, "issue-1")}, // conducted: has a child
		{ID: 2, Parent: 1, Workspace: filepath.Join(ws, "issue-2")},
		{ID: 3, Workspace: filepath.Join(ws, "issue-3")}, // solo
	}}
	if err := saveWeaveQueue(dir, q); err != nil {
		t.Fatal(err)
	}

	for _, c := range []struct {
		name string
		it   *weaveItem
		want string
	}{
		{"an item whose children exist conducted", q.Items[0], roleConductor},
		{"a dispatched child is a worker", q.Items[1], roleWorker},
		{"a solo run claims neither seat", q.Items[2], ""},
		{"no workspace means an unknown seat, never a guessed one",
			&weaveItem{ID: 9}, ""},
	} {
		if got := weaveLedgerRole(c.it); got != c.want {
			t.Errorf("%s: role = %q, want %q", c.name, got, c.want)
		}
	}
}
