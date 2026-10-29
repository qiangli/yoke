package src_test

// S525.A2 acceptance (red/green, local fixtures only — no network): the
// adversarial evidence table. Each row is a way a candidate, or something
// holding a record, could try to get a merge it has not earned; the last
// row is the genuine fix that must sail through. Everything runs against a
// real src store, real commits, a real isolated clone and a real sealed
// record — the gate is exercised, not mocked.

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	ygit "github.com/qiangli/yoke/git"
	"github.com/qiangli/yoke/pkg/shape/contain"
	"github.com/qiangli/yoke/pkg/shape/evidence"
	"github.com/qiangli/yoke/pkg/shape/verify"
	"github.com/qiangli/yoke/pkg/src"
)

const testPrincipal = "os:shaping-user"

// --- the fixture contract -------------------------------------------------

// baseApp declares two contracted functions. Both must be reached by the
// check set or the verdict fails, however green the checks look.
const baseApp = `@guard(effects: "read,write")
@require('true')
@ensure('test -f out.txt')
build() {
	printf 'built\n' > out.txt
}

@guard(effects: "read")
@ensure('true')
report() {
	cat out.txt
}
`

// baseCheck is the frozen check set. It dispatches contracted calls
// through the interpreter's run helper, which is what records them.
// tweakedApp is a real but contract-neutral change: enough to be a
// candidate, not enough to be the point of the row.
var tweakedApp = baseApp + "\n# a harmless tweak\n"

const baseCheck = `set -e
run build
test -f out.txt
run report
`

func baseFiles() map[string]string {
	return map[string]string{"app.bsh": baseApp, "checks/smoke.bsh": baseCheck}
}

// --- the stand-in interpreter --------------------------------------------

// interpreter stands in for the host's decorator-aware interpreter: it
// consumes the contract decorators the way the real one does, dispatches
// contracted calls through a recording helper, and runs the frozen check
// through the production TrustedRunner so pkg/shape/contain is really in
// the path. Recording is the interpreter's job, never the candidate's:
// that is what makes "it exited 0" different from "it ran the contract".
type interpreter struct{}

func (interpreter) RunCheck(ctx context.Context, c verify.CheckRun) (verify.Outcome, error) {
	// c.Script is <clone>/.shape/checks/<file>.
	clone := filepath.Dir(filepath.Dir(filepath.Dir(c.Script)))
	app, err := stageApp(clone)
	if err != nil {
		return verify.Outcome{}, err
	}
	prelude := "run() { printf '%s\\n' \"$1\" >> \"$BASHY_SHAPE_CALLLOG\"; \"$@\"; }\n" +
		". " + shQuote(app) + "\n. \"$1\"\n"
	return verify.TrustedRunner{
		Bin:     "sh",
		Args:    func(script string) []string { return []string{"-c", prelude, "sh", script} },
		Effects: []string{"read", "write"},
		Level:   contain.LevelProcess,
	}.RunCheck(ctx, c)
}

// stageApp writes a decorator-free copy of the clone's units for the stand-in
// interpreter to source. A real interpreter parses the decorators instead of
// stripping them; either way the contract lines are the interpreter's, not
// the shell's.
func stageApp(clone string) (string, error) {
	entries, err := os.ReadDir(clone)
	if err != nil {
		return "", err
	}
	var body strings.Builder
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".bsh" {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(clone, e.Name()))
		if err != nil {
			return "", err
		}
		for _, line := range strings.Split(string(raw), "\n") {
			if strings.HasPrefix(strings.TrimSpace(line), "@") {
				continue
			}
			body.WriteString(line + "\n")
		}
	}
	p := filepath.Join(clone, ".shape", "app.sh")
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return "", err
	}
	return p, os.WriteFile(p, []byte(body.String()), 0o644)
}

func shQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

// --- fixture plumbing -----------------------------------------------------

// shapeBase puts the base contract on `local`: the version the principal is
// running, and the only place the verifier reads checks from.
func shapeBase(t *testing.T, home string) (dir, baseSHA string) {
	t.Helper()
	if err := src.EnsureLocal(home, "yoke"); err != nil {
		t.Fatalf("EnsureLocal: %v", err)
	}
	dir = destOf(home)
	if _, err := ygit.Checkout(ygit.CheckoutOptions{RepoPath: dir, Branch: "local"}); err != nil {
		t.Fatalf("checkout local: %v", err)
	}
	for rel, body := range baseFiles() {
		writeWork(t, dir, rel, body)
	}
	return dir, commitWork(t, dir, "base contract")
}

// candidatePR commits files on a topic branch off local and opens a PR.
func candidatePR(t *testing.T, home, branch string, files map[string]string) src.PR {
	t.Helper()
	if err := src.CreateBranch(home, "yoke", branch); err != nil {
		t.Fatalf("CreateBranch: %v", err)
	}
	dir := destOf(home)
	for rel, body := range files {
		writeWork(t, dir, rel, body)
	}
	commitWork(t, dir, "candidate "+branch)
	pr, err := src.OpenPR(home, "yoke", branch, "Candidate "+branch, "")
	if err != nil {
		t.Fatalf("OpenPR: %v", err)
	}
	return pr
}

// runVerifier verifies pr's head against baseSHA for principal.
func runVerifier(t *testing.T, home, dir, baseSHA string, pr src.PR, principal string) verify.Report {
	t.Helper()
	rep, err := verify.Run(context.Background(), verify.Options{
		Home: home, RepoDir: dir, BaseSHA: baseSHA, CandidateSHA: pr.HeadSHA,
		Principal: principal, Runner: interpreter{},
	})
	if err != nil {
		t.Fatalf("verify.Run: %v", err)
	}
	return rep
}

// --- the table ------------------------------------------------------------

func TestEvidenceGateAdversarialTable(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the stand-in interpreter is a POSIX shell")
	}

	for _, tc := range []struct {
		name string
		// files is the candidate's tree, merged over the base.
		files map[string]string
		// verifyAs is the principal to verify for; "" skips verification
		// entirely (no record is ever written).
		verifyAs string
		// tamper runs after verification and before the merge.
		tamper func(t *testing.T, home, dir, baseSHA string, pr src.PR) src.PR
		// mergeAs is the principal at merge time; "" means testPrincipal.
		mergeAs string
		// wantVerdictPass is what the verifier should have concluded.
		wantVerdictPass bool
		// wantMerge is whether the merge must be allowed.
		wantMerge bool
		// wantRefusal is a substring the refusal must name.
		wantRefusal string
	}{
		{
			// The contract is exercised and the checks pass: this is what
			// the whole apparatus exists to let through.
			name:            "a genuine fix is accepted and merges",
			files:           map[string]string{"app.bsh": strings.Replace(baseApp, "printf 'built\\n'", "printf 'built v2\\n'", 1)},
			verifyAs:        testPrincipal,
			wantVerdictPass: true,
			wantMerge:       true,
		},
		{
			// Exit 0 from inside the first contracted call. The check
			// reports success and the second contracted function is never
			// reached — only the accounting notices.
			name: "early exit leaves the contract unexercised",
			files: map[string]string{"app.bsh": strings.Replace(baseApp,
				"printf 'built\\n' > out.txt", "printf 'built\\n' > out.txt\n\texit 0", 1)},
			verifyAs:    testPrincipal,
			wantRefusal: "never called",
		},
		{
			// Contracted code the candidate adds without a check that
			// reaches it is unverified code, however green everything else is.
			name:        "a decorated function no check calls fails the verdict",
			files:       map[string]string{"app.bsh": baseApp + "\n@guard(effects: \"read\")\n@ensure('true')\naudit() {\n\techo audit\n}\n"},
			verifyAs:    testPrincipal,
			wantRefusal: "never called",
		},
		{
			// The candidate neuters the check and breaks the code. The
			// verifier runs the BASE bytes, so the neutering is a no-op
			// and the broken code is what gets measured.
			name: "editing a check does not weaken it",
			files: map[string]string{
				"app.bsh":          strings.Replace(baseApp, "printf 'built\\n' > out.txt", "true", 1),
				"checks/smoke.bsh": "exit 0\n",
			},
			verifyAs:    testPrincipal,
			wantRefusal: "check",
		},
		{
			// A check the candidate ADDS runs in addition to the frozen
			// set — and reports itself inapplicable, which is an unproven
			// check rather than a lenient one.
			name: "a skipped check never passes a verdict",
			files: map[string]string{
				"app.bsh":          tweakedApp,
				"checks/extra.bsh": "exit 77\n",
			},
			verifyAs:    testPrincipal,
			wantRefusal: "check",
		},
		{
			// Asking for an effect the base never allowed is refused
			// before any of the candidate's code runs.
			name:        "an effect cap escalation is refused",
			files:       map[string]string{"app.bsh": strings.Replace(baseApp, `@guard(effects: "read,write")`, `@guard(effects: "read,write,net")`, 1)},
			verifyAs:    testPrincipal,
			wantRefusal: "escalation",
		},
		{
			// Nobody verified it. Policy alone is not evidence.
			name:        "missing evidence blocks the merge",
			files:       map[string]string{"app.bsh": tweakedApp},
			verifyAs:    "",
			wantRefusal: "no record",
		},
		{
			// A failing verdict edited to say pass. The seal is over the
			// whole record, so the edit is visible and the record is
			// refused outright rather than read as a weaker pass.
			name: "an edited verdict is refused as forged",
			files: map[string]string{"app.bsh": strings.Replace(baseApp,
				"printf 'built\\n' > out.txt", "printf 'built\\n' > out.txt\n\texit 0", 1)},
			verifyAs: testPrincipal,
			tamper: func(t *testing.T, home, dir, baseSHA string, pr src.PR) src.PR {
				rewriteRecord(t, home, pr.HeadSHA, func(m map[string]any) {
					m["verdict"] = evidence.VerdictPass
					delete(m, "uncalled")
				})
				return pr
			},
			wantRefusal: "seal",
		},
		{
			// A record hand-written for a candidate nobody ever ran.
			name:     "an unsealed record is refused as forged",
			files:    map[string]string{"app.bsh": tweakedApp},
			verifyAs: "",
			tamper: func(t *testing.T, home, dir, baseSHA string, pr src.PR) src.PR {
				forgeRecord(t, home, evidence.Record{
					Schema: evidence.Schema, Verdict: evidence.VerdictPass,
					Binding: evidence.Binding{
						Candidate: pr.HeadSHA, Base: baseSHA,
						Verifier: verify.BuildID(), Principal: testPrincipal,
					},
				})
				return pr
			},
			wantRefusal: "seal",
		},
		{
			// Verified for one principal, merged by another. A verdict is
			// always for someone.
			name:            "a record bound to another principal is stale",
			files:           map[string]string{"app.bsh": tweakedApp},
			verifyAs:        "os:someone-else",
			wantVerdictPass: true,
			mergeAs:         testPrincipal,
			wantRefusal:     "principal",
		},
		{
			// Rebasing the candidate produces a different commit, and the
			// record is bound to the one that was actually run.
			name:            "evidence does not survive a rebase",
			files:           map[string]string{"app.bsh": tweakedApp},
			verifyAs:        testPrincipal,
			wantVerdictPass: true,
			tamper: func(t *testing.T, home, dir, baseSHA string, pr src.PR) src.PR {
				// Rewrite the branch tip: the same intent, a new commit.
				writeWork(t, dir, "app.bsh", tweakedApp+"\n# rebased\n")
				commitWork(t, dir, "rebased candidate")
				rebased, err := src.OpenPR(home, "yoke", pr.Head, "Rebased", "")
				if err != nil {
					t.Fatalf("OpenPR after rebase: %v", err)
				}
				if rebased.HeadSHA == pr.HeadSHA {
					t.Fatal("the rebase did not move the head")
				}
				return rebased
			},
			wantRefusal: "no record",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("BASHY_AGENT", "test-agent")
			t.Setenv("BASHY_PRINCIPAL", testPrincipal)
			_, newSHA := seedFixture(t, home)
			fetchAt(t, home, newSHA)
			dir, baseSHA := shapeBase(t, home)

			files := baseFiles()
			for rel, body := range tc.files {
				files[rel] = body
			}
			pr := candidatePR(t, home, "candidate", files)

			if tc.verifyAs != "" {
				rep := runVerifier(t, home, dir, baseSHA, pr, tc.verifyAs)
				if got := rep.Passed(); got != tc.wantVerdictPass {
					t.Fatalf("verdict pass = %v, want %v (%s; checks %+v; uncalled %v)",
						got, tc.wantVerdictPass, rep.Record.Reason, rep.Record.Checks, rep.Record.Uncalled)
				}
			}
			if tc.tamper != nil {
				pr = tc.tamper(t, home, dir, baseSHA, pr)
			}
			if tc.mergeAs != "" {
				t.Setenv("BASHY_PRINCIPAL", tc.mergeAs)
			}

			_, err := src.MergePR(home, pr.ID)
			if tc.wantMerge {
				if err != nil {
					t.Fatalf("MergePR = %v, want the merge allowed", err)
				}
				got, gerr := src.GetPR(home, pr.ID)
				if gerr != nil || got.State != "merged" {
					t.Fatalf("PR state = %+v (%v), want merged", got, gerr)
				}
				return
			}
			if err == nil {
				t.Fatal("MergePR succeeded, want a refusal")
			}
			if !strings.Contains(strings.ToLower(err.Error()), tc.wantRefusal) {
				t.Fatalf("MergePR = %v, want a refusal naming %q", err, tc.wantRefusal)
			}
			// A blocked merge changes nothing.
			got, gerr := src.GetPR(home, pr.ID)
			if gerr != nil || got.State != "open" {
				t.Fatalf("PR state = %+v (%v), want still open", got, gerr)
			}
		})
	}
}

// TestEvidenceApprovalIsTheOnlyOverride pins that the override exists, is
// explicit, and leaves a record — and that it never reaches a core path.
func TestEvidenceApprovalIsTheOnlyOverride(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the stand-in interpreter is a POSIX shell")
	}
	home := t.TempDir()
	t.Setenv("BASHY_AGENT", "test-agent")
	t.Setenv("BASHY_PRINCIPAL", testPrincipal)
	_, newSHA := seedFixture(t, home)
	fetchAt(t, home, newSHA)
	shapeBase(t, home)

	files := baseFiles()
	files["app.bsh"] = strings.Replace(baseApp, "printf 'built\\n'", "true # broken", 1)
	pr := candidatePR(t, home, "unverified", files)

	// Unverified, and an approval that covers only part of the change does
	// not waive anything.
	if _, err := src.WriteApproval(home, src.ApprovalRecord{
		Repo: "yoke", Paths: []string{"README.md"}, Approver: "op", Reason: "partial",
	}); err != nil {
		t.Fatal(err)
	}
	_, err := src.MergePR(home, pr.ID)
	if err == nil {
		t.Fatal("a partial approval waived verification")
	}
	if !src.EvidenceStale(err) {
		t.Fatalf("err = %v, want a stale/missing evidence refusal", err)
	}

	// An approval covering the whole change is the one override.
	if _, err := src.WriteApproval(home, src.ApprovalRecord{
		Repo: "yoke", Paths: []string{"app.bsh"}, Approver: "op", Reason: "reviewed in full",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := src.MergePR(home, pr.ID); err != nil {
		t.Fatalf("MergePR with a covering approval: %v", err)
	}
}

// --- record tampering helpers --------------------------------------------

func rewriteRecord(t *testing.T, home, candidate string, edit func(map[string]any)) {
	t.Helper()
	p := evidence.Path(home, candidate)
	raw, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("read record: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("parse record: %v", err)
	}
	edit(m)
	out, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, out, 0o600); err != nil {
		t.Fatal(err)
	}
}

func forgeRecord(t *testing.T, home string, rec evidence.Record) {
	t.Helper()
	raw, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(evidence.Dir(home), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(evidence.Path(home, rec.Binding.Candidate), raw, 0o600); err != nil {
		t.Fatal(err)
	}
}

// TestVerifyPRProducesTheRecordTheGateReads closes the loop: the verb an
// operator runs is what mints the record the merge gate then accepts, and
// the same verb on a broken candidate mints a refusal instead.
func TestVerifyPRProducesTheRecordTheGateReads(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the stand-in interpreter is a POSIX shell")
	}
	home := t.TempDir()
	t.Setenv("BASHY_AGENT", "test-agent")
	t.Setenv("BASHY_PRINCIPAL", testPrincipal)
	_, newSHA := seedFixture(t, home)
	fetchAt(t, home, newSHA)
	shapeBase(t, home)

	good := baseFiles()
	good["app.bsh"] = tweakedApp
	pr := candidatePR(t, home, "verified", good)

	rec, err := src.VerifyPR(context.Background(), home, pr.ID, interpreter{})
	if err != nil {
		t.Fatalf("VerifyPR: %v", err)
	}
	if rec.Verdict != evidence.VerdictPass || rec.Binding.Principal != testPrincipal {
		t.Fatalf("record = %+v, want a pass for %s", rec, testPrincipal)
	}
	if _, err := src.MergePR(home, pr.ID); err != nil {
		t.Fatalf("MergePR after verify: %v", err)
	}

	// The same verb on a candidate that stops exercising the contract
	// records the refusal rather than nothing at all.
	bad := baseFiles()
	bad["app.bsh"] = strings.Replace(baseApp, "printf 'built\\n' > out.txt", "printf 'built\\n' > out.txt\n\texit 0", 1)
	badPR := candidatePR(t, home, "unverified", bad)
	rec, err = src.VerifyPR(context.Background(), home, badPR.ID, interpreter{})
	if err != nil {
		t.Fatalf("VerifyPR: %v", err)
	}
	if rec.Verdict != evidence.VerdictFail || len(rec.Uncalled) == 0 {
		t.Fatalf("record = %+v, want a fail naming the uncalled function", rec)
	}
	if _, err := src.MergePR(home, badPR.ID); err == nil {
		t.Fatal("a recorded failure let the merge through")
	}
}

// The verb is registered where an operator can reach it.
func TestPRVerbIsRegistered(t *testing.T) {
	root := src.NewSrcCmd()
	for _, path := range [][]string{{"pr", "verify"}, {"pr", "merge"}} {
		c, _, err := root.Find(path)
		if err != nil || c.Name() != path[len(path)-1] {
			t.Fatalf("src %s not registered (%v)", strings.Join(path, " "), err)
		}
	}
}
