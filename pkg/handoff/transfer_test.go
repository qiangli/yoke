// Copyright (c) 2026 qiangli
// See LICENSE for licensing information

package handoff

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"
)

// fixtureRecord builds a record with in-flight work of every kind: a modified
// tracked file, a staged addition (index detail, carried as change), an
// untracked file, and a too-large file that is named but not carried.
func fixtureRecord() *Record {
	now := time.Now().UTC()
	return &Record{
		ID:         "20261119T000000Z-1a2b3c4d",
		CreatedAt:  now,
		Project:    Project{Name: "yoke", Primary: "/w/yoke", Roots: []string{"/w/yoke"}},
		Continuity: "halfway through the retry gate",
		NextAction: "reconcile the deploy effect, then rerun go test",
		Blockers:   []string{"deploy outcome unknown after host reboot"},
		Work: WorkingState{
			Repo:    "/w/yoke",
			Branch:  "agent/weave-issue-87",
			BaseSHA: "d14e9e038fe01c5287de0d65311cdfac234ebc62",
			Diff: "diff --git a/pkg/handoff/transfer.go b/pkg/handoff/transfer.go\n" +
				"new file mode 100644\n" +
				"--- /dev/null\n" +
				"+++ b/pkg/handoff/transfer.go\n" +
				"@@ -0,0 +1 @@\n" +
				"+package handoff\n" +
				"diff --git a/pkg/handoff/record.go b/pkg/handoff/record.go\n" +
				"--- a/pkg/handoff/record.go\n" +
				"+++ b/pkg/handoff/record.go\n" +
				"@@ -1 +1 @@\n" +
				"-old\n" +
				"+new\n" +
				"diff --git a/pkg/handoff/old.go b/pkg/handoff/old.go\n" +
				"--- a/pkg/handoff/old.go\n" +
				"+++ /dev/null\n" +
				"@@ -1 +0,0 @@\n" +
				"-gone\n",
			Untracked: []UntrackedFile{
				{Path: "notes/scratch.md", Content: "recovery notes\n"},
				{Path: "big/core.dump", Content: "<<< NOT CARRIED: 99999999 bytes exceeds the 1048576-byte handoff cap. Fetch it from the origin host (/w/yoke). >>>"},
			},
		},
		Links:    Links{Sprint: 666, WeaveIssue: 87, Workspace: "/w/yoke"},
		Dispatch: Dispatch{Disposition: DispatchPark},
	}
}

// The transfer preserves candidate and source identity: the exact repo,
// branch, base SHA and carried-work digest a replacement must verify, plus
// the task pointers that locate the work without proving anything about it.
func TestSummarizePreservesCandidateIdentity(t *testing.T) {
	rec := fixtureRecord()
	tr := Summarize(rec)
	if !tr.Candidate.Complete() {
		t.Fatalf("candidate incomplete: %+v", tr.Candidate)
	}
	if tr.Candidate.HandoffID != rec.ID {
		t.Fatalf("handoff id = %q, want %q", tr.Candidate.HandoffID, rec.ID)
	}
	if tr.Candidate.WorkSHA != WorkDigest(rec.Work) {
		t.Fatal("work digest not bound to the carried candidate")
	}
	if tr.Candidate.SprintText == "" || tr.Candidate.WeaveIssue == 0 {
		t.Fatalf("task pointers missing: %+v", tr.Candidate)
	}
	id := tr.Candidate.Identity()
	for _, want := range []string{"/w/yoke", "d14e9e03", tr.Candidate.WorkSHA[:12]} {
		if !strings.Contains(id, want) {
			t.Fatalf("identity %q missing %q", id, want)
		}
	}
}

// The digest covers the whole candidate: a moved base, edited untracked
// content (including the too-large marker), or a different repo is a
// different candidate — evidence bound to one must not validate another.
func TestWorkDigestCoversBaseAndUntracked(t *testing.T) {
	base := WorkDigest(fixtureRecord().Work)
	if base == "" {
		t.Fatal("non-empty work must digest to a non-empty identity")
	}
	moved := fixtureRecord().Work
	moved.BaseSHA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	if WorkDigest(moved) == base {
		t.Fatal("changed base must change the candidate digest")
	}
	edited := fixtureRecord().Work
	edited.Untracked[0].Content = "different recovery notes\n"
	if WorkDigest(edited) == base {
		t.Fatal("changed untracked content must change the candidate digest")
	}
	relocated := fixtureRecord().Work
	relocated.Repo = "/elsewhere/yoke"
	if WorkDigest(relocated) == base {
		t.Fatal("changed repo must change the candidate digest")
	}
	if WorkDigest(fixtureRecord().Work) != base {
		t.Fatal("identical work must digest identically")
	}
	if WorkDigest(WorkingState{Repo: "/w/yoke"}) != "" {
		t.Fatal("work with nothing in flight must have no digest")
	}
}

// Scope without re-derivation: every changed file appears exactly once with
// its kind, including the too-large file that travels by name only.
func TestChangedFilesOf(t *testing.T) {
	got := ChangedFilesOf(fixtureRecord().Work)
	want := map[string]string{
		"pkg/handoff/transfer.go": "added",
		"pkg/handoff/record.go":   "modified",
		"pkg/handoff/old.go":      "deleted",
		"notes/scratch.md":        "untracked",
		"big/core.dump":           "not-carried",
	}
	if len(got) != len(want) {
		t.Fatalf("changed = %v, want %d entries", got, len(want))
	}
	for _, f := range got {
		if want[f.Path] != f.Kind {
			t.Fatalf("%s kind = %q, want %q (all: %v)", f.Path, f.Kind, want[f.Path], got)
		}
	}
}

// Evidence binds to the candidate it ran against. Evidence from another tree
// stays visible but is honestly unbound; evidence without a timestamp is
// never fresh.
func TestEvidenceBindingAndFreshness(t *testing.T) {
	work := "abc123"
	bound := Evidence{Kind: "gate", Command: "go test ./...", At: time.Now().UTC(), TreeSHA: work}
	if !bound.Binds(work) {
		t.Fatal("evidence against this work must bind")
	}
	if bound.Binds("other") {
		t.Fatal("evidence against another candidate must not bind")
	}
	stale := Evidence{Kind: "gate", Command: "go test ./...", At: time.Now().Add(-2 * time.Hour), TreeSHA: work}
	if stale.Fresh(time.Hour) {
		t.Fatal("two-hour-old evidence is not fresh against a one-hour bar")
	}
	if (Evidence{}).Fresh(time.Hour) {
		t.Fatal("evidence without a timestamp is never fresh")
	}
}

// THE RETRY GATE. An uncertain external effect refuses the retry; reconciling
// it to an observed outcome opens the gate; an undeclared scope keeps it
// closed even with no effects at all — a task id is not effect proof.
func TestRetryGateUncertainEffects(t *testing.T) {
	tr := Summarize(fixtureRecord())
	tr.DeclareEffect("deploy staging", "helm upgrade ran before the host died")
	if err := tr.VerifyRetrySafe(); err == nil {
		t.Fatal("gate open with an uncertain effect")
	} else if !strings.Contains(err.Error(), "fx-1") {
		t.Fatalf("refusal must name the effect: %v", err)
	}
	if err := tr.ReconcileEffect("fx-1", EffectUncertain, "muse"); err == nil {
		t.Fatal("reconciling as uncertain must be refused")
	}
	if err := tr.ReconcileEffect("fx-9", EffectApplied, "muse"); err == nil {
		t.Fatal("reconciling an undeclared effect must be refused")
	}
	if err := tr.ReconcileEffect("fx-1", EffectAbsent, "muse"); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if err := tr.VerifyRetrySafe(); err != nil {
		t.Fatalf("gate closed after reconciliation: %v", err)
	}
	if tr.Effects[0].CheckedBy != "muse" || tr.Effects[0].CheckedAt.IsZero() {
		t.Fatalf("reconciliation must stamp who and when: %+v", tr.Effects[0])
	}
}

func TestRetryGateUnknownScopeStaysClosed(t *testing.T) {
	tr := Summarize(fixtureRecord())
	if err := tr.VerifyRetrySafe(); err == nil {
		t.Fatal("gate open with undeclared scope: a task id is not effect proof")
	}
	tr.NoExternalEffects = true
	if err := tr.VerifyRetrySafe(); err != nil {
		t.Fatalf("explicit no-external-effects must open the gate: %v", err)
	}
}

// Reconciling steps wait for the gate; local steps do not. Done steps are
// never attemptable again.
func TestSafeToAttempt(t *testing.T) {
	tr := Summarize(fixtureRecord())
	reconciling := NextStep{Step: "rerun deploy", RequiresReconciliation: true}
	local := NextStep{Step: "reread the record"}
	if !tr.SafeToAttempt(local) {
		t.Fatal("local step must be attemptable with the gate closed")
	}
	if tr.SafeToAttempt(reconciling) {
		t.Fatal("reconciling step must wait while scope is undeclared")
	}
	tr.NoExternalEffects = true
	if !tr.SafeToAttempt(reconciling) {
		t.Fatal("reconciling step must be attemptable once the gate opens")
	}
	done := NextStep{Step: "done", Done: true}
	if tr.SafeToAttempt(done) {
		t.Fatal("done step must never be re-attempted")
	}
}

// Cancellation and pending approvals stay visible: a cancelled record lands
// in Cancellations and the render; a pending approval lands in Decisions with
// its reference, and only pending rows gate.
func TestCancellationAndApprovalsVisible(t *testing.T) {
	rec := fixtureRecord()
	now := time.Now().UTC()
	by := taker("muse", "ep-1")
	rec.CancelledAt, rec.CancelledBy = &now, &by
	tr := Summarize(rec)
	if len(tr.Cancellations) != 1 || tr.Cancellations[0].Status != DecisionCancelled {
		t.Fatalf("cancelled record must surface one cancellation: %+v", tr.Cancellations)
	}
	if rec.Status() != "cancelled" {
		t.Fatalf("record status = %q, want cancelled", rec.Status())
	}
	tr.AttachDecision(Decision{Kind: "approval", Summary: "push the staging image", Ref: "ask:9f2c", Status: DecisionPending})
	tr.AttachDecision(Decision{Kind: "approval", Summary: "old prompt", Ref: "ask:1", Status: DecisionDecided})
	if n := len(tr.PendingDecisions()); n != 2 { // the blocker plus the pending approval
		t.Fatalf("pending decisions = %d, want 2: %+v", n, tr.PendingDecisions())
	}
	rendered := Render(tr, rec)
	for _, want := range []string{"explicitly cancelled", "ask:9f2c", "GATE: CLOSED"} {
		if !strings.Contains(rendered, want) {
			t.Fatalf("render missing %q:\n%s", want, rendered)
		}
	}
}

// The transfer names the ledger claim that prevents concurrent ownership:
// the take still goes through Claim, and a second holder is refused there.
func TestTransferNamesTheOwnershipClaim(t *testing.T) {
	ledger(t)
	dir := t.TempDir()
	rec := fixtureRecord()
	if _, err := Save(dir, rec); err != nil {
		t.Fatal(err)
	}
	tr := Summarize(rec)
	want := string(ClaimRef(rec.ID).Kind) + ":" + ClaimRef(rec.ID).Name
	if tr.Owner.HandoffClaim != want {
		t.Fatalf("handoff claim = %q, want %q", tr.Owner.HandoffClaim, want)
	}
	a, b := taker("muse-a", "ep-a"), taker("muse-b", "ep-b")
	if _, err := Claim(context.Background(), dir, rec, a, nil); err != nil {
		t.Fatalf("first take: %v", err)
	}
	cur, _ := Load(dir + "/" + rec.ID + ".json")
	if _, err := Claim(context.Background(), dir, cur, b, nil); err == nil {
		t.Fatal("second holder took a held transfer")
	}
	held := Compose(dir, cur)
	if held.Owner.Holder == "" || !strings.Contains(Render(held, cur), held.Owner.Holder) {
		t.Fatalf("render must name the holder: %+v", held.Owner)
	}
}

// Agent death and host interruption lose nothing: the record and the
// annotation sidecar are files, written atomically, and Compose rebuilds the
// full transfer from disk — identity from the record, annotations overlaid.
func TestSidecarRoundTripAndCompose(t *testing.T) {
	dir := t.TempDir()
	rec := fixtureRecord()
	if _, err := Save(dir, rec); err != nil {
		t.Fatal(err)
	}
	side := &Transfer{HandoffID: rec.ID}
	side.AttachEvidence(Evidence{Kind: "gate", Command: "go test ./pkg/handoff/", TreeSHA: WorkDigest(rec.Work), By: "muse"})
	fx := side.DeclareEffect("notify channel", "post may have sent before the crash")
	side.AttachDecision(Decision{Kind: "approval", Summary: "re-send the notice", Ref: "ask:7", Status: DecisionPending})
	side.Readiness = &ReadinessRef{Binding: "muse:spark", Reason: "probe passed", CheckedAt: time.Now().UTC(), Source: "weave fleet --probe"}
	if _, err := SaveTransfer(dir, side); err != nil {
		t.Fatal(err)
	}
	loaded, err := LoadTransfer(dir, rec.ID)
	if err != nil {
		t.Fatal(err)
	}
	full := Overlay(Summarize(rec), loaded)
	if len(full.Evidence) != 1 || !full.Evidence[0].Binds(full.Candidate.WorkSHA) {
		t.Fatalf("evidence lost or unbound after round trip: %+v", full.Evidence)
	}
	if err := full.ReconcileEffect(fx.ID, EffectApplied, "muse"); err != nil {
		t.Fatal(err)
	}
	if err := full.VerifyRetrySafe(); err != nil {
		t.Fatalf("gate closed after full reconcile: %v", err)
	}
	if full.Readiness == nil || full.Readiness.Binding != "muse:spark" {
		t.Fatalf("readiness ref lost: %+v", full.Readiness)
	}
	// Identity still comes from the record, not the sidecar: a tampered
	// sidecar cannot re-identify the candidate.
	loaded.Candidate.Repo = "/evil/repo"
	full = Overlay(Summarize(rec), loaded)
	if full.Candidate.Repo != "/w/yoke" {
		t.Fatalf("sidecar re-identified the candidate: %q", full.Candidate.Repo)
	}
}

// A retired handoff is never taken — not by a direct Claim, not by a stale
// copy that was retired after it was read, and not when the stored copy was
// retired between the read and the take.
func TestClaimRefusesRetired(t *testing.T) {
	ledger(t)
	dir := t.TempDir()

	cancelled := fixtureRecord()
	now := time.Now().UTC()
	by := taker("human", "ep-h")
	cancelled.CancelledAt, cancelled.CancelledBy = &now, &by
	if _, err := Save(dir, cancelled); err != nil {
		t.Fatal(err)
	}
	if _, err := Claim(context.Background(), dir, cancelled, taker("muse", "ep-1"), nil); err == nil {
		t.Fatal("cancelled handoff was taken")
	} else if !strings.Contains(err.Error(), "cancelled") {
		t.Fatalf("refusal must say cancelled: %v", err)
	}

	superseded := fixtureRecord()
	superseded.ID = "20261119T000001Z-5e6f7a8b"
	superseded.SupersededAt = &now
	superseded.SupersededBy = "20261119T000002Z-9a8b7c6d"
	if _, err := Save(dir, superseded); err != nil {
		t.Fatal(err)
	}
	if _, err := Claim(context.Background(), dir, superseded, taker("muse", "ep-2"), nil); err == nil {
		t.Fatal("superseded handoff was taken")
	} else if !strings.Contains(err.Error(), "superseded") {
		t.Fatalf("refusal must say superseded: %v", err)
	}

	// Retired between read and take: the claim re-reads under the ledger.
	raced := fixtureRecord()
	raced.ID = "20261119T000003Z-1a2b3c4e"
	if _, err := Save(dir, raced); err != nil {
		t.Fatal(err)
	}
	retired := raced
	retired.CancelledAt, retired.CancelledBy = &now, &by
	if _, err := Save(dir, retired); err != nil {
		t.Fatal(err)
	}
	if _, err := Claim(context.Background(), dir, raced, taker("muse", "ep-3"), nil); err == nil {
		t.Fatal("handoff retired mid-take was taken")
	}
}

// The retry gate lives in the real pickup path: `resume --claim` refuses
// while an effect is uncertain, warns (but proceeds) on undeclared scope,
// and succeeds once the effect is reconciled.
func TestResumeClaimEnforcesEffectGate(t *testing.T) {
	ledger(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("BASHY_HOME", home)
	t.Setenv("BASHY_ROOM_DIR", t.TempDir())
	dir := t.TempDir()
	t.Setenv("BASHY_HANDOFF_DIR", dir)
	repo := newRepo(t)
	prev, _ := os.Getwd()
	if err := os.Chdir(repo); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(prev) })

	h := NewHandoffCmd()
	var out bytes.Buffer
	h.SetOut(&out)
	h.SetErr(&out)
	h.SetArgs([]string{"-m", "half-deployed work"})
	if err := h.Execute(); err != nil {
		t.Fatalf("handoff: %v\n%s", out.String(), err)
	}
	recs, err := List(dir)
	if err != nil || len(recs) != 1 {
		t.Fatalf("records = %v, %v", recs, err)
	}
	id := recs[0].ID

	// Declare an uncertain effect through the agent CLI: the pickup must
	// refuse and name it. The CLI re-saves the sidecar AND the record's
	// declaration together, so the pair stays a whole export.
	declare := NewResumeCmd()
	declare.SetOut(&out)
	declare.SetErr(&out)
	declare.SetArgs([]string{"--declare-effect", "push staging image", "--detail", "push ran before the host died", id})
	if err := declare.Execute(); err != nil {
		t.Fatalf("declare: %v\n%s", out.String(), err)
	}
	r := NewResumeCmd()
	out.Reset()
	r.SetOut(&out)
	r.SetErr(&out)
	r.SetArgs([]string{"--claim", id})
	if err := r.Execute(); err == nil {
		t.Fatal("--claim succeeded with an uncertain effect")
	} else if !strings.Contains(err.Error(), "uncertain") {
		t.Fatalf("refusal must name the uncertain effect: %v", err)
	}
	if cur, _ := Load(dir + "/" + id + ".json"); cur.ResumedAt != nil {
		t.Fatal("refused pickup still stamped a claim")
	}

	// Reconcile the effect to an observed outcome: the pickup proceeds.
	reconcile := NewResumeCmd()
	out.Reset()
	reconcile.SetOut(&out)
	reconcile.SetErr(&out)
	reconcile.SetArgs([]string{"--reconcile-effect", "fx-1", "--as", "absent", id})
	if err := reconcile.Execute(); err != nil {
		t.Fatalf("reconcile: %v\n%s", out.String(), err)
	}
	r2 := NewResumeCmd()
	out.Reset()
	r2.SetOut(&out)
	r2.SetErr(&out)
	r2.SetArgs([]string{"--claim", id})
	if err := r2.Execute(); err != nil {
		t.Fatalf("--claim after reconcile: %v\n%s", out.String(), err)
	}
}

// Pruning a retired record removes its sidecar too: annotations must never
// outlive the record they describe.
func TestPruneRemovesSidecar(t *testing.T) {
	dir := t.TempDir()
	rec := fixtureRecord()
	now := time.Now().UTC()
	by := taker("human", "ep-h")
	rec.CancelledAt, rec.CancelledBy = &now, &by
	if _, err := Save(dir, rec); err != nil {
		t.Fatal(err)
	}
	side := &Transfer{HandoffID: rec.ID}
	side.DeclareEffect("post notice", "may have sent")
	if _, err := SaveTransfer(dir, side); err != nil {
		t.Fatal(err)
	}
	n, err := Prune(dir)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("pruned = %d, want 1", n)
	}
	if _, err := LoadTransfer(dir, rec.ID); err == nil {
		t.Fatal("sidecar survived its record's prune")
	}
}

// A record that arrived by path (scp/mesh) composes the sidecar travelling
// beside it — not the local store, which never saw this handoff.
func TestComposeFindsSiblingSidecar(t *testing.T) {
	dir := t.TempDir()
	rec := fixtureRecord()
	recPath, err := Save(dir, rec)
	if err != nil {
		t.Fatal(err)
	}
	side := &Transfer{HandoffID: rec.ID}
	side.AttachEvidence(Evidence{Kind: "gate", Command: "go test ./...", TreeSHA: WorkDigest(rec.Work), By: "muse"})
	if _, err := SaveTransfer(dir, side); err != nil {
		t.Fatal(err)
	}
	arrived, err := Load(recPath)
	if err != nil {
		t.Fatal(err)
	}
	full := Compose(dir, arrived)
	if len(full.Evidence) != 1 || !full.Evidence[0].Binds(full.Candidate.WorkSHA) {
		t.Fatalf("sibling sidecar not composed: %+v", full.Evidence)
	}
}

// A missing sidecar is a normal state (older handoff, annotations never
// added): Compose falls back to the record summary with the gate closed.
func TestComposeWithoutSidecar(t *testing.T) {
	dir := t.TempDir()
	rec := fixtureRecord()
	full := Compose(dir, rec)
	if full.HandoffID != rec.ID || len(full.Changed) == 0 {
		t.Fatalf("compose without sidecar lost the record: %+v", full.Candidate)
	}
	if err := full.VerifyRetrySafe(); err == nil {
		t.Fatal("gate must be closed when no scope was ever declared")
	}
}

// The sidecar is a travelling artifact like the record: a vault value in an
// annotation must not survive persistence.
func TestSaveTransferRedactsVaultValues(t *testing.T) {
	t.Setenv("BASHY_HOME", t.TempDir())
	t.Setenv("BASHY_ROOM_DIR", t.TempDir())
	handoffVaultFixture(t, "DEPLOY_TOKEN", "sycamore-lantern-42")
	dir := t.TempDir()
	tr := Summarize(fixtureRecord())
	tr.AttachEvidence(Evidence{Kind: "gate", Command: "deploy", Output: "token sycamore-lantern-42 leaked", By: "muse"})
	path, err := SaveTransfer(dir, tr)
	if err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "sycamore-lantern-42") {
		t.Fatalf("vault value persisted to the transfer sidecar:\n%s", b)
	}
}

// `resume --transfer` is the replacement's read path: it prints the composed
// transfer and changes nothing — no stamp, no apply, still unclaimed after.
func TestResumeTransferIsReadOnly(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("BASHY_HOME", home)
	t.Setenv("BASHY_ROOM_DIR", t.TempDir())
	dir := t.TempDir()
	t.Setenv("BASHY_HANDOFF_DIR", dir)
	repo := newRepo(t)
	prev, _ := os.Getwd()
	if err := os.Chdir(repo); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(prev) })

	h := NewHandoffCmd()
	var out bytes.Buffer
	h.SetOut(&out)
	h.SetErr(&out)
	h.SetArgs([]string{"-m", "parking the transfer work", "--next", "reconcile, then rerun tests"})
	if err := h.Execute(); err != nil {
		t.Fatalf("handoff: %v\n%s", out.String(), err)
	}

	r := NewResumeCmd()
	out.Reset()
	r.SetOut(&out)
	r.SetErr(&out)
	r.SetArgs([]string{"--transfer"})
	if err := r.Execute(); err != nil {
		t.Fatalf("resume --transfer: %v\n%s", out.String(), err)
	}
	rendered := out.String()
	for _, want := range []string{
		"# Recovery transfer",
		"## Candidate (exact source identity)",
		"## Changed files",
		"## Verification evidence",
		"## Pending decisions and approvals",
		"## Next safe steps",
		"## External effects and retry gate",
		"## Ownership",
		"reconcile, then rerun tests",
	} {
		if !strings.Contains(rendered, want) {
			t.Fatalf("transfer render missing %q:\n%s", want, rendered)
		}
	}
	// Read-only: the record is still unclaimed.
	recs, err := List(dir)
	if err != nil || len(recs) != 1 {
		t.Fatalf("records = %v, %v", recs, err)
	}
	if recs[0].ResumedAt != nil {
		t.Fatal("--transfer stamped a claim: the read path must not take the seat")
	}

	// --transfer --json emits the transfer envelope, not the record.
	r2 := NewResumeCmd()
	out.Reset()
	r2.SetOut(&out)
	r2.SetErr(&out)
	r2.SetArgs([]string{"--transfer", "--json"})
	if err := r2.Execute(); err != nil {
		t.Fatalf("resume --transfer --json: %v\n%s", out.String(), err)
	}
	var tr Transfer
	if err := json.Unmarshal(out.Bytes(), &tr); err != nil {
		t.Fatalf("transfer JSON: %v\n%s", out.String(), err)
	}
	if tr.SchemaVersion != TransferSchemaVersion || tr.HandoffID == "" {
		t.Fatalf("transfer envelope incomplete: %+v", tr)
	}
}

// Readiness staleness: a missing finding is stale, a fresh one is not, and
// the render says so either way — the replacement never assumes readiness.
func TestReadinessStale(t *testing.T) {
	var missing *ReadinessRef
	if !missing.Stale(time.Hour) {
		t.Fatal("missing readiness must read as stale")
	}
	fresh := &ReadinessRef{Binding: "muse:spark", CheckedAt: time.Now().UTC()}
	if fresh.Stale(time.Hour) {
		t.Fatal("fresh readiness must not read as stale")
	}
	old := &ReadinessRef{Binding: "muse:spark", CheckedAt: time.Now().Add(-2 * time.Hour)}
	if !old.Stale(time.Hour) {
		t.Fatal("two-hour-old readiness must read as stale")
	}
	rendered := Render(Summarize(fixtureRecord()), fixtureRecord())
	if !strings.Contains(rendered, "NO readiness finding") {
		t.Fatalf("render must warn on missing readiness:\n%s", rendered)
	}
}
