// Copyright (c) 2025 qiangli
// See LICENSE for licensing information

package steward

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"
)

// The CLI is the surface a cold successor actually touches, so these tests assert
// the two things that make it trustworthy: the JSON envelopes are stable enough to
// parse, and the human output TELLS THE TRUTH — especially when the truth is
// "nobody established this".

// cliRegistry is the canonical seat registry for a CLI test, derived from the store dir so
// that every cli() call within ONE test shares it and no two tests share anything.
//
// It has to be injected, and that is the feature working rather than a wrinkle to route
// around: the registry allows a scope exactly ONE store directory, every CLI test uses a
// fresh temp dir, and they all run as the same OS account on the same machine — so a
// shared registry would (correctly) refuse the second test in the process as a second seat.
func cliRegistry(dir string) string { return dir + ".registry" }

// cli runs `steward <args…>` against an isolated store and returns stdout.
func cli(t *testing.T, dir string, args ...string) (string, error) {
	t.Helper()
	// A stable identity, so the seat is held by a known name rather than whatever
	// agent-detection happens to infer on the machine running the tests.
	t.Setenv("BASHY_PRINCIPAL", "dhnt:agent/tester")
	t.Setenv("BASHY_EPISODE", "ep-test")
	// A machine identity for the platforms where the OS has none to give (a CI container
	// with no /etc/machine-id would otherwise fail closed on every test).
	//
	// It is a FALLBACK, not an override — see HostIDEnv. Where the OS does answer (macOS,
	// a real Linux box), this is IGNORED and the tests run under the real machine id, which
	// is exactly what the enforcement requires: a variable that could rename the machine
	// could mint a second seat on it. What the tests depend on is that the id is STABLE
	// within a run, and both paths give that.
	t.Setenv("BASHY_HOST_ID", "cli-test-machine")
	// `claim` exports the fencing token with a raw os.Setenv, which would otherwise
	// survive into the NEXT test in this process and hand it a tenure it never took.
	// Re-registering the variable at its CURRENT value snapshots it for cleanup without
	// clearing it, so a claim earlier in this same test still counts — which is the
	// actual UX being tested (claim exports the epoch; later commands inherit it).
	t.Setenv(EpochEnv, os.Getenv(EpochEnv))

	cmd := NewStewardCmd(WithRegistryRoot(cliRegistry(dir)))
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	// Stdin is pinned to a NON-TERMINAL, always. Left unset, cobra hands the command
	// os.Stdin, and whether that is a terminal depends on how the suite was launched —
	// so `go test` from a shell would take the attended path and the same test in CI
	// would take the unattended one. The unattended path is the one that matters here,
	// and it is the one an agent will actually be on.
	cmd.SetIn(strings.NewReader(""))
	cmd.SetArgs(append([]string{"--dir", dir}, args...))
	err := cmd.Execute()
	return out.String(), err
}

// mustCLI runs and fails the test on error.
func mustCLI(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, err := cli(t, dir, args...)
	if err != nil {
		t.Fatalf("steward %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return out
}

// apiStore opens the same store the CLI is pointed at, with a TRUSTED verifier.
//
// Tests need this because the CLI, run from a test, CANNOT ACQUIRE THE SEAT — and that
// is the control working, not an inconvenience to route around. The CLI's only root of
// trust is a typed confirmation at a real terminal (see ptyVerifier); a test has no
// terminal, so it is by definition an unattended caller, and an unattended acquisition is
// exactly what the package refuses. Every CI runner, cron job and headless agent is in
// the same position, which is the point.
//
// So the seat is seeded the way a HOST would do it: through the API, with a verifier the
// host injected. The CLI tests below then exercise the commands a steward actually runs
// once it holds the seat — which is nearly all of them.
func apiStore(t *testing.T, dir string) *Store {
	t.Helper()
	t.Setenv("BASHY_HOST_ID", "cli-test-machine")
	t.Setenv("BASHY_PRINCIPAL", "dhnt:agent/tester")
	t.Setenv("BASHY_EPISODE", "ep-test")
	s, err := Open(dir, WithVerifier(verified()), WithRegistryRoot(cliRegistry(dir)))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	return s
}

// seedSeat takes the seat through the API and exports the fencing epoch, so the CLI
// commands under test inherit the tenure exactly as they would after a real `claim`.
func seedSeat(t *testing.T, dir string) uint64 {
	t.Helper()
	s := apiStore(t, dir)
	who := Self()
	now := time.Now()

	g, err := s.Authorize(context.Background(), GrantRequest{
		Action: ActionClaim, Grantee: who, Actor: "qiangli", Reason: "test seat", Attended: true,
	}, now)
	if err != nil {
		t.Fatalf("Authorize(claim): %v", err)
	}
	v, err := s.Claim(context.Background(), who, SeatRequest{GrantID: g.ID, Attended: true}, now)
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}
	t.Setenv(EpochEnv, strconv.FormatUint(v.Authority.Epoch, 10))
	return v.Authority.Epoch
}

// seizeSeat takes the seat over through the API, bumping the epoch — the setup a test
// needs when what it is actually testing is what happens to the FENCED steward.
func seizeSeat(t *testing.T, dir string) uint64 {
	t.Helper()
	s := apiStore(t, dir)
	who := Self()
	now := time.Now()

	g, err := s.Authorize(context.Background(), GrantRequest{
		Action: ActionTakeover, Grantee: who, Actor: "qiangli", Reason: "recovery drill", Attended: true,
	}, now)
	if err != nil {
		t.Fatalf("Authorize(takeover): %v", err)
	}
	v, err := s.Takeover(context.Background(), who, SeatRequest{GrantID: g.ID, Attended: true}, now)
	if err != nil {
		t.Fatalf("Takeover: %v", err)
	}
	t.Setenv(EpochEnv, strconv.FormatUint(v.Authority.Epoch, 10))
	return v.Authority.Epoch
}

func TestCLIStatusOnAVacantSeat(t *testing.T) {
	dir := t.TempDir()
	out := mustCLI(t, dir, "status")
	if !strings.Contains(out, "VACANT") {
		t.Fatalf("status on a fresh host must say the seat is vacant:\n%s", out)
	}
	if !strings.Contains(out, "steward claim") {
		t.Fatalf("status must tell the reader how to take a vacant seat:\n%s", out)
	}
}

func TestCLIStatusShowsTheLiveHolder(t *testing.T) {
	dir := t.TempDir()
	seedSeat(t, dir)

	out := mustCLI(t, dir, "status")
	if !strings.Contains(out, "live") || !strings.Contains(out, "tester") {
		t.Fatalf("status must show the live holder:\n%s", out)
	}
	if !strings.Contains(out, "epoch 1") {
		t.Fatalf("the first acquisition must be epoch 1:\n%s", out)
	}
}

// EVERY AUTHORITY TRANSITION FAILS CLOSED WITH NO TERMINAL — and a test process is an
// unattended process, exactly like the cron job, the CI runner, and the runaway agent
// loop this is meant to stop.
//
// This is the single most important CLI behavior in the revision, so it is asserted for
// all three verbs. Note what it means for the test suite itself: the CLI tests below
// cannot take the seat and must seed it through a host-injected verifier (see apiStore).
// A control that the test suite could shrug off would be a control an agent could shrug
// off.
func TestCLIAuthorityTransitionsFailClosedWithNoTerminal(t *testing.T) {
	// Minting is refused outright: there is nobody here to confirm anything.
	for _, action := range []string{ActionClaim, ActionTakeover} {
		t.Run("authorize a "+action, func(t *testing.T) {
			dir := t.TempDir()
			_, err := cli(t, dir, "authorize", "--action", action, "--actor", "qiangli")
			if err == nil {
				t.Fatal("an unattended process must not be able to mint a capability")
			}
			if !strings.Contains(err.Error(), "no terminal is attached") {
				t.Fatalf("the refusal must say why: %v", err)
			}
			if _, err := os.Stat(filepath.Join(dir, "journal.jsonl")); err == nil {
				t.Fatal("a refused mint must write nothing")
			}
		})
	}

	// And SPENDING a genuine capability — one a host verifier really did establish — is
	// refused too, which is the sharper half. A capability is a BOUND on an authority, not
	// the source of one: the CLI re-asks its own root of trust at the moment authority
	// moves, and an unattended process cannot satisfy it. If it could, an agent would only
	// ever need to get a grant minted once.
	for _, action := range []string{ActionClaim, ActionTakeover} {
		t.Run("spend a real capability on a "+action, func(t *testing.T) {
			dir := t.TempDir()
			if action == ActionTakeover {
				seedSeat(t, dir)
			}
			s := apiStore(t, dir)
			g, err := s.Authorize(context.Background(), GrantRequest{
				Action: action, Grantee: Self(), Actor: "qiangli", Reason: "drill", Attended: true,
			}, time.Now())
			if err != nil {
				t.Fatalf("Authorize: %v", err)
			}

			_, err = cli(t, dir, action, "--grant", g.ID)
			if err == nil {
				t.Fatalf("an unattended %s must be refused even holding a valid capability", action)
			}
			if !strings.Contains(err.Error(), "no terminal is attached") {
				t.Fatalf("the refusal must come from the ROOT OF TRUST, not from the paperwork: %v", err)
			}
		})
	}
}

// THERE IS NO --yes. It existed, and it was the whole control handed back in one word: a
// flag that skips the confirmation is a flag every unattended agent on the machine will
// pass, and an operator "assertion" nobody asserted is not an assertion.
func TestCLIHasNoYesFlagToSkipConfirmation(t *testing.T) {
	dir := t.TempDir()
	out, err := cli(t, dir, "authorize", "--action", "claim", "--actor", "qiangli", "--yes")
	if err == nil {
		t.Fatal("--yes must not exist: a flag that skips the human is a flag an agent will pass")
	}
	if !strings.Contains(err.Error(), "unknown flag") {
		t.Fatalf("--yes must be rejected as an unknown flag, got: %v\n%s", err, out)
	}
}

// The --json envelope must be parseable and carry the schema version, or no other
// program can safely consume it.
func TestCLIStatusJSONIsStable(t *testing.T) {
	dir := t.TempDir()
	seedSeat(t, dir)
	mustCLI(t, dir, "record", "-m", "did a thing", "--workstream", "api",
		"--outcome", "success", "-e", "command:go test ./...")

	out := mustCLI(t, dir, "--json", "status")

	var env statusEnvelope
	if err := json.Unmarshal([]byte(out), &env); err != nil {
		t.Fatalf("status --json is not valid JSON: %v\n%s", err, out)
	}
	if env.SchemaVersion != SchemaVersion {
		t.Fatalf("schema_version = %q, want %q", env.SchemaVersion, SchemaVersion)
	}
	if env.Seat.Liveness != LivenessLive {
		t.Fatalf("seat.liveness = %q, want live", env.Seat.Liveness)
	}
	if env.Seat.Authority.Epoch != 1 {
		t.Fatalf("seat.authority.epoch = %d, want 1", env.Seat.Authority.Epoch)
	}
	if !env.Journal.Intact || env.Journal.Entries != 2 {
		t.Fatalf("journal = %+v, want 2 intact entries", env.Journal)
	}
	// An evidenced success is ASSERTED, never verified: `-e command:go test ./...`
	// records that somebody SAID they ran the tests. Only `steward verify` closes that
	// gap, and the JSON envelope must not blur it — a consumer that read this as
	// "verified" would be laundering a reference into a check.
	if len(env.Board.Workstreams) != 1 || env.Board.Workstreams[0].Confidence != ConfidenceAsserted {
		t.Fatalf("board = %+v; an evidenced success is asserted, not verified", env.Board.Workstreams)
	}
	if env.Board.Asserted != 1 {
		t.Fatalf("the board must COUNT the unchecked claims, got %d", env.Board.Asserted)
	}
}

// NOTHING THE CLI CAN TYPE REACHES VERIFIED — and the CLI says so to the operator's face.
//
// This is the honest replacement for a test that used to assert the opposite. The CLI
// injects no verification verifier (it has nothing that could go and check a claim), so on
// this surface `verify --result success` RECORDS the check and promotes nothing. Every
// route an agent would reach for is exercised here, and all of them land on asserted:
// prose, a real digest over real bytes, and an arbitrary digest over bytes that never
// existed.
func TestCLIVerifyRecordsButCannotPromote(t *testing.T) {
	dir := t.TempDir()
	seedSeat(t, dir)
	mustCLI(t, dir, "record", "-m", "shipped the migration", "--workstream", "api",
		"--outcome", "success", "-e", "commit:de6485c")

	// seq 1 is the claim, seq 2 the effect.
	digest := digestOf([]byte("ok 1 - all tests passed"))
	out := mustCLI(t, dir, "verify", "--seq", "2", "--result", "success",
		"--method", "re-ran the suite on a clean checkout", "-e", "file:/tmp/test.log#"+digest)
	if !strings.Contains(out, "verification recorded") {
		t.Fatalf("the check is worth recording — the log is not the thing being defended:\n%s", out)
	}
	// And it must SAY it promoted nothing, here, where the operator is looking. A command
	// that prints "verification recorded" and stops invites exactly the belief the package
	// exists to refuse.
	if !strings.Contains(out, "NOT PROMOTED") || !strings.Contains(out, "ASSERTED") {
		t.Fatalf("verify must tell the operator what it was worth, rather than leaving them to "+
			"discover it from a board they may never read:\n%s", out)
	}

	var env statusEnvelope
	if err := json.Unmarshal([]byte(mustCLI(t, dir, "--json", "status")), &env); err != nil {
		t.Fatal(err)
	}
	if got := env.Board.Workstreams[0].Confidence; got != ConfidenceAsserted {
		t.Fatalf("a digest is INTEGRITY, not a check: it proves some bytes did not change and says nothing about "+
			"whether anybody looked. Got %q, want asserted", got)
	}

	// An ARBITRARY digest — the version an agent would actually reach for, since nothing
	// rehashes the evidence. Also asserted.
	mustCLI(t, dir, "verify", "--seq", "2", "--result", "success",
		"--method", "the suite passed, see the log",
		"-e", "file:/tmp/never-existed.log#sha256:"+strings.Repeat("f", 64))
	if err := json.Unmarshal([]byte(mustCLI(t, dir, "--json", "status")), &env); err != nil {
		t.Fatal(err)
	}
	if got := env.Board.Workstreams[0].Confidence; got != ConfidenceAsserted {
		t.Fatalf("thirty-two bytes an agent typed must not promote a claim, got %q", got)
	}

	// A verification with no --method is the same trust-me claim it replaces.
	_, err := cli(t, dir, "verify", "--seq", "2", "--result", "success")
	if err == nil || !strings.Contains(err.Error(), "method") {
		t.Fatalf("verify must demand HOW it checked, got %v", err)
	}

	// Prose alone: recorded, and still asserted.
	mustCLI(t, dir, "verify", "--seq", "2", "--result", "success",
		"--method", "I definitely checked it, trust me")
	if err := json.Unmarshal([]byte(mustCLI(t, dir, "--json", "status")), &env); err != nil {
		t.Fatal(err)
	}
	if got := env.Board.Workstreams[0].Confidence; got != ConfidenceAsserted {
		t.Fatalf("a sentence must not promote a claim, got %q", got)
	}
}

// …and when the HOST injects something that CAN check a claim, the very same command
// promotes it. The enforcement point is the only thing that changed; the CLI did not.
func TestCLIVerifyPromotesWhenTheHostInjectsAVerifier(t *testing.T) {
	dir := t.TempDir()
	seedSeat(t, dir)
	mustCLI(t, dir, "record", "-m", "shipped the migration", "--workstream", "api",
		"--outcome", "success", "-e", "commit:de6485c")

	// A host mounting `steward` with a real verification verifier — a CI adapter, a signing
	// service — is the ONLY difference between this test and the one above.
	run := func(args ...string) string {
		t.Helper()
		cmd := NewStewardCmd(WithRegistryRoot(cliRegistry(dir)), WithVerificationVerifier(sealing()))
		var out bytes.Buffer
		cmd.SetOut(&out)
		cmd.SetErr(&out)
		cmd.SetIn(strings.NewReader(""))
		cmd.SetArgs(append([]string{"--dir", dir}, args...))
		if err := cmd.Execute(); err != nil {
			t.Fatalf("steward %s: %v\n%s", strings.Join(args, " "), err, out.String())
		}
		return out.String()
	}

	out := run("verify", "--seq", "2", "--result", "success", "--method", "asked the CI system")
	if strings.Contains(out, "NOT PROMOTED") {
		t.Fatalf("a trusted verifier vouched for this one:\n%s", out)
	}

	var env statusEnvelope
	if err := json.Unmarshal([]byte(run("--json", "status")), &env); err != nil {
		t.Fatal(err)
	}
	if got := env.Board.Workstreams[0].Confidence; got != ConfidenceVerified {
		t.Fatalf("a seal a trusted verifier issued and still recognizes IS what verified means, got %q", got)
	}
}

// THE ONE THAT MATTERS. An agent records "done ✅" with nothing to point at, and the
// CLI must tell it — to its face — that the board will not believe it.
func TestCLIRecordWarnsWhenSuccessHasNoEvidence(t *testing.T) {
	dir := t.TempDir()
	seedSeat(t, dir)

	out := mustCLI(t, dir, "record", "-m", "shipped it, all green", "--workstream", "api", "--outcome", "success")
	if !strings.Contains(out, "NO EVIDENCE") {
		t.Fatalf("recording an unevidenced success must warn the agent that it will not project as one:\n%s", out)
	}
	if !strings.Contains(out, "unknown") {
		t.Fatalf("the warning must name what the board WILL show:\n%s", out)
	}

	// And the board agrees.
	board := mustCLI(t, dir, "board")
	if !strings.Contains(board, "unknown") {
		t.Fatalf("the board must show the unevidenced claim as unknown:\n%s", board)
	}
	if !strings.Contains(board, "NOBODY ESTABLISHED") {
		t.Fatalf("the board must lead with the honest headline:\n%s", board)
	}
}

func TestCLILogDegradedSurfacesOnlyTheUnproven(t *testing.T) {
	dir := t.TempDir()
	seedSeat(t, dir)
	mustCLI(t, dir, "record", "-m", "proven work", "--workstream", "api",
		"--outcome", "success", "-e", "commit:de6485c")
	mustCLI(t, dir, "record", "-m", "trust me bro", "--workstream", "db", "--outcome", "success")

	out := mustCLI(t, dir, "log", "--degraded")
	if strings.Contains(out, "proven work") {
		t.Fatalf("--degraded leaked an evidenced entry:\n%s", out)
	}
	if !strings.Contains(out, "trust me bro") {
		t.Fatalf("--degraded must surface the unproven claim:\n%s", out)
	}

	// --json must be a parseable array of entries.
	jsonOut := mustCLI(t, dir, "--json", "log", "--degraded")
	var entries []Entry
	if err := json.Unmarshal([]byte(jsonOut), &entries); err != nil {
		t.Fatalf("log --json is not valid JSON: %v\n%s", err, jsonOut)
	}
	if len(entries) != 1 || entries[0].Workstream != "db" {
		t.Fatalf("log --degraded --json = %+v, want exactly the db entry", entries)
	}
}

func TestCLIDecideRequiresARationale(t *testing.T) {
	dir := t.TempDir()
	seedSeat(t, dir)

	_, err := cli(t, dir, "decide", "-m", "drop v1")
	if err == nil {
		t.Fatal("a decision with no rationale must be refused: a successor can replay every effect and still never recover WHY")
	}
	if !strings.Contains(err.Error(), "rationale") {
		t.Fatalf("the refusal must name the missing flag: %v", err)
	}

	out := mustCLI(t, dir, "decide", "-m", "drop v1", "--rationale", "no callers in 90d", "--workstream", "api")
	if !strings.Contains(out, "decision recorded") {
		t.Fatalf("decide:\n%s", out)
	}

	conv := mustCLI(t, dir, "conversation")
	if !strings.Contains(conv, "DECISION") || !strings.Contains(conv, "no callers in 90d") {
		t.Fatalf("conversation must show the decision and its rationale:\n%s", conv)
	}
}

// The takeover the CLI CAN reach is the refusal, and the refusal is the feature.
//
// The previous revision's version of this test used `--yes` to mint an assertion nobody
// asserted, then leaned on a receipt file to get an unattended seizure through — which is
// precisely the hole: an agent can write a file and hash it, so the "control" on the
// unattended path was "the agent must write a file first". Both halves are gone. What an
// unattended CLI can do with a takeover now is be told no.
func TestCLITakeoverIsRefusedWithoutACapability(t *testing.T) {
	dir := t.TempDir()
	seedSeat(t, dir)

	_, err := cli(t, dir, "takeover")
	if err == nil {
		t.Fatal("a takeover presenting no authorization must be refused — otherwise the capability is decoration")
	}
	if !strings.Contains(err.Error(), "no authorization was presented") {
		t.Fatalf("the refusal must say what was missing: %v", err)
	}

	// Even holding a genuine capability, minted through a host verifier, the unattended
	// CLI cannot spend it: the CLI's own root of trust is a typed terminal confirmation,
	// and there is no terminal. A capability is a BOUND on an authority, never the source
	// of one.
	s := apiStore(t, dir)
	g, err := s.Authorize(context.Background(), GrantRequest{
		Action: ActionTakeover, Grantee: Self(), Actor: "qiangli", Reason: "looks stuck", Attended: true,
	}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := cli(t, dir, "takeover", "--grant", g.ID); err == nil {
		t.Fatal("an unattended CLI must not be able to seize the seat even with a valid grant in hand")
	}

	// The seat never moved.
	out := mustCLI(t, dir, "status")
	if !strings.Contains(out, "epoch 1") {
		t.Fatalf("no refused takeover may move the seat:\n%s", out)
	}
}

// The journal — not the grant file — is what makes a capability single-use. Spending it
// removes the file, so a file-based check would be flimsy: put the bytes back and a
// file-based check hands the seat over again.
func TestCLIGrantsListsARestoredGrantAsAlreadyUsed(t *testing.T) {
	dir := t.TempDir()
	seedSeat(t, dir)

	s := apiStore(t, dir)
	g, err := s.Authorize(context.Background(), GrantRequest{
		Action: ActionTakeover, Grantee: Self(), Actor: "qiangli", Reason: "wedged", Attended: true,
	}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	grantFile := filepath.Join(dir, "grants", g.ID+".json")
	backup, err := os.ReadFile(grantFile)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Takeover(context.Background(), Self(),
		SeatRequest{GrantID: g.ID, Attended: true}, time.Now()); err != nil {
		t.Fatalf("Takeover: %v", err)
	}

	// Restore the spent capability, exactly as a backup — or an attacker — would.
	if err := os.WriteFile(grantFile, backup, 0o600); err != nil {
		t.Fatal(err)
	}
	out := mustCLI(t, dir, "grants")
	if !strings.Contains(out, "already used") {
		t.Fatalf("a restored, spent capability must still read as used — the hash chain is what refuses it:\n%s", out)
	}

	hist := mustCLI(t, dir, "history")
	if !strings.Contains(hist, "qiangli") {
		t.Fatalf("history must record who authorized the seizure:\n%s", hist)
	}
}

// A steward that captured its epoch and presents it after being taken over must be
// FENCED from the command line, not merely told it is a stranger. Without --epoch the
// CLI could never reach ErrFenced at all — the most important error in the system
// would be unreachable from the shell.
func TestCLIRecordWithAStaleEpochIsFenced(t *testing.T) {
	dir := t.TempDir()
	seedSeat(t, dir) // epoch 1 — the token a long-running steward would capture

	// A human authorizes recovery; the seat moves to epoch 2. (Same principal here —
	// which is the sharpest version of the test: even the SAME agent, presenting a
	// superseded token, must be refused. Being yourself is not a credential.)
	seizeSeat(t, dir)

	_, err := cli(t, dir, "record", "-m", "…and then I deployed it",
		"--workstream", "api", "--outcome", "success", "-e", "command:kubectl apply", "--epoch", "1")
	if err == nil {
		t.Fatal("a write presenting a superseded epoch must be rejected")
	}
	var fenced *ErrFenced
	if !errors.As(err, &fenced) {
		t.Fatalf("want ErrFenced, got %T: %v", err, err)
	}
	if fenced.Presented != 1 || fenced.Current != 2 {
		t.Fatalf("fencing error = presented %d, current %d; want 1 and 2", fenced.Presented, fenced.Current)
	}
	if !strings.Contains(err.Error(), "steward log") {
		t.Fatalf("the fencing error must tell the zombie to re-read the journal: %v", err)
	}

	// Nothing of the fenced write reached the journal.
	log := mustCLI(t, dir, "log")
	if strings.Contains(log, "and then I deployed it") {
		t.Fatalf("a fenced write landed in the journal:\n%s", log)
	}
}

func TestCLICheckpointAndVerify(t *testing.T) {
	dir := t.TempDir()
	seedSeat(t, dir)
	mustCLI(t, dir, "record", "-m", "real work", "--workstream", "api",
		"--outcome", "success", "-e", "commit:abc1234")

	out := mustCLI(t, dir, "--json", "checkpoint", "--note", "before the risky bit")
	var ck Checkpoint
	if err := json.Unmarshal([]byte(out), &ck); err != nil {
		t.Fatalf("checkpoint --json: %v\n%s", err, out)
	}
	if ck.ID == "" || ck.JournalDigest == "" || ck.Board.Digest == "" {
		t.Fatalf("a checkpoint must carry its receipt: %+v", ck)
	}

	verify := mustCLI(t, dir, "checkpoint", "--verify", ck.ID)
	if !strings.Contains(verify, "REPRODUCIBLE") {
		t.Fatalf("a fresh checkpoint must verify:\n%s", verify)
	}

	list := mustCLI(t, dir, "checkpoint", "--list")
	if !strings.Contains(list, ck.ID) {
		t.Fatalf("checkpoint --list must show it:\n%s", list)
	}
}

func TestCLIReconcileReportsDegradedAndCanRecordIt(t *testing.T) {
	dir := t.TempDir()
	seedSeat(t, dir)
	mustCLI(t, dir, "record", "-m", "shipped, honest", "--workstream", "api", "--outcome", "success")

	out := mustCLI(t, dir, "reconcile")
	if !strings.Contains(out, "DEGRADED") {
		t.Fatalf("reconcile must report a degraded verdict for an unevidenced claim:\n%s", out)
	}
	if !strings.Contains(out, "UNPROVEN") {
		t.Fatalf("reconcile must list the unproven claims by name:\n%s", out)
	}

	// --json carries the same verdict, machine-readably.
	jsonOut := mustCLI(t, dir, "--json", "reconcile")
	var r Reconciliation
	if err := json.Unmarshal([]byte(jsonOut), &r); err != nil {
		t.Fatalf("reconcile --json: %v\n%s", err, jsonOut)
	}
	if r.Health != HealthDegraded || len(r.Unproven) != 1 {
		t.Fatalf("reconcile --json = health %q, %d unproven; want degraded/1", r.Health, len(r.Unproven))
	}

	// --record makes the finding permanent.
	mustCLI(t, dir, "reconcile", "--record")
	log := mustCLI(t, dir, "log", "--kind", "reconcile")
	if !strings.Contains(log, "reconciled") {
		t.Fatalf("--record must append the reconciliation to the journal:\n%s", log)
	}
}

// A non-holder gets the REPORT even though it cannot write it. Refusing to print the
// truth because you lack the seat would be the one genuinely unhelpful failure mode:
// reconcile is what you run BEFORE you have the seat.
func TestCLIReconcileStillReportsWithoutTheSeat(t *testing.T) {
	dir := t.TempDir()
	// Nobody has ever claimed. Reconcile must still work.
	out := mustCLI(t, dir, "reconcile")
	if !strings.Contains(out, "VACANT") {
		t.Fatalf("reconcile on a vacant host must say so:\n%s", out)
	}

	// With --record and no seat, the report is still printed and the failure to
	// write it is explained rather than swallowed.
	out = mustCLI(t, dir, "reconcile", "--record")
	if !strings.Contains(out, "VACANT") {
		t.Fatalf("--record must not suppress the report when the write fails:\n%s", out)
	}
	if !strings.Contains(out, "NOT written") {
		t.Fatalf("the reader must be told the report was not journaled, and why:\n%s", out)
	}
}

func TestCLIWorkstreamOpenAndClose(t *testing.T) {
	dir := t.TempDir()
	seedSeat(t, dir)
	mustCLI(t, dir, "workstream", "open", "api", "--title", "the API migration")

	out := mustCLI(t, dir, "workstream", "close", "api", "-m", "all done", "--outcome", "success")
	if !strings.Contains(out, "NO EVIDENCE") {
		t.Fatalf("closing with an unevidenced success must warn — closed is not the same fact as verified done:\n%s", out)
	}

	board := mustCLI(t, dir, "board", "api")
	if !strings.Contains(board, "the API migration") {
		t.Fatalf("board <name> must show the title:\n%s", board)
	}
	if !strings.Contains(board, "closed") || !strings.Contains(board, "unknown") {
		t.Fatalf("the workstream must be closed AND unknown:\n%s", board)
	}
	if !strings.Contains(board, "UNPROVEN") {
		t.Fatalf("the detail view must name the unproven claim:\n%s", board)
	}
}

// A transcript is stored, and the CLI says plainly that nothing depends on it.
func TestCLITranscriptIsMarkedNonAuthoritative(t *testing.T) {
	dir := t.TempDir()
	seedSeat(t, dir)

	cmd := NewStewardCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetIn(strings.NewReader("human: do the thing\nagent: ok\n"))
	cmd.SetArgs([]string{"--dir", dir, "transcript", "-m", "the conversation", "--workstream", "api"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("transcript: %v\n%s", err, out.String())
	}
	if !strings.Contains(out.String(), "Non-authoritative") {
		t.Fatalf("the CLI must say a transcript is non-authoritative:\n%s", out.String())
	}

	// It does not appear on the board — transcripts derive nothing.
	board := mustCLI(t, dir, "board")
	if strings.Contains(board, "the conversation") {
		t.Fatalf("a transcript reached the board; nothing may derive from a non-authoritative artifact:\n%s", board)
	}
}

// `take` is an alias for `claim` — the issue asks for either spelling, so both work.
func TestCLITakeIsAnAliasForClaim(t *testing.T) {
	dir := t.TempDir()
	// The alias resolves to the same command — including the same refusal, which is all an
	// unattended test can reach. `take --help` proves the routing without needing a seat.
	out := mustCLI(t, dir, "take", "--help")
	if !strings.Contains(out, "claim takes the seat in exactly two situations") {
		t.Fatalf("`take` must be an alias for `claim`:\n%s", out)
	}
	if !strings.Contains(out, "--grant") {
		t.Fatalf("claim takes a capability, and its help must say so:\n%s", out)
	}
}

// THE HELP IS AN AUTHORITY SURFACE. It once showed
//
//	eval "$(bashy steward claim --intent 'on call' --export)"
//
// as THE way onto the seat. Pasted, it fails — no grant was presented — but the failure is
// the smaller half of the damage: the example teaches a reader that an empty chair belongs
// to whoever sits in it, which is the exact belief the grant requirement exists to correct,
// and it teaches it in the one place a cold successor looks first. So the rule is stronger
// than "the examples work": NO advertised claim, anywhere in the command tree, may be
// spelled without the capability it spends.
func TestCLIHelpNeverAdvertisesAClaimWithoutAGrant(t *testing.T) {
	root := NewStewardCmd()

	var walk func(*cobra.Command)
	claims := 0
	walk = func(c *cobra.Command) {
		for _, src := range []struct{ where, text string }{
			{"Example", c.Example},
			{"Long", c.Long},
		} {
			for _, line := range invocations(src.text) {
				if !strings.Contains(line, "steward claim") && !strings.Contains(line, "steward take ") {
					continue
				}
				claims++
				if !strings.Contains(line, "--grant") {
					t.Errorf("`%s` %s advertises a claim that presents no capability — it cannot work, "+
						"and it says claiming is free:\n  %s", c.CommandPath(), src.where, line)
				}
			}
		}
		for _, sub := range c.Commands() {
			walk(sub)
		}
	}
	walk(root)
	if claims == 0 {
		t.Fatal("the scan found no advertised claim at all — the help changed shape and this test " +
			"is now passing by looking at nothing")
	}

	// The sequence has to be complete, not merely grant-shaped: the reader must be told
	// where the id comes from. A `claim --grant <id>` with no `authorize --action claim`
	// above it is a command with an unexplained operand.
	for _, c := range []*cobra.Command{root, findCmd(t, root, "claim")} {
		if !strings.Contains(c.Example, "authorize --action claim") {
			t.Errorf("`%s` Example must show where the grant comes from:\n%s", c.CommandPath(), c.Example)
		}
	}
	// And the export half — the reason the example exists — is still there.
	if !strings.Contains(root.Example, "--export") {
		t.Errorf("the root example must still show the epoch being exported:\n%s", root.Example)
	}

	// Finally: the help matches the enforcement. The form the examples DON'T show is the
	// form the store refuses — so the advertised grant is a requirement, not a decoration.
	dir := t.TempDir()
	_, err := cli(t, dir, "claim", "--intent", "on call")
	if err == nil {
		t.Fatal("a claim presenting no authorization must be refused, or the examples are advertising ceremony")
	}
	if !strings.Contains(err.Error(), "no authorization was presented") {
		t.Fatalf("the refusal must say what was missing: %v", err)
	}
}

// invocations returns the lines of a help block that read as a PASTEABLE COMMAND rather
// than prose: an indented `steward …` snippet, optionally wrapped in the shell forms the
// examples use (`eval "$(…)"`, `x=$(…)`, a `bashy ` prefix). Prose that merely NAMES a
// command — "`USER=someone-else bashy steward claim` used to be a different seat" — is not
// advertising it, and is deliberately not matched.
func invocations(help string) []string {
	var out []string
	for _, raw := range strings.Split(help, "\n") {
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		t := line
		for _, p := range []string{"$ ", "eval \"$(", "eval $("} {
			t = strings.TrimPrefix(t, p)
		}
		// grant=$(bashy steward authorize …)
		if i := strings.Index(t, "=$("); i > 0 && !strings.ContainsAny(t[:i], " \t") {
			t = t[i+len("=$("):]
		}
		t = strings.TrimPrefix(t, "bashy ")
		if strings.HasPrefix(t, "steward ") {
			out = append(out, line)
		}
	}
	return out
}

func findCmd(t *testing.T, root *cobra.Command, name string) *cobra.Command {
	t.Helper()
	for _, c := range root.Commands() {
		if c.Name() == name {
			return c
		}
	}
	t.Fatalf("no `%s` command", name)
	return nil
}

func TestCLIReleaseSaysItCapturedNoWork(t *testing.T) {
	dir := t.TempDir()
	seedSeat(t, dir)

	out := mustCLI(t, dir, "release", "--note", "stopping for the day")
	if !strings.Contains(out, "handoff") {
		t.Fatalf("release must point at the verb that DOES move work, or the two stay conflated:\n%s", out)
	}

	status := mustCLI(t, dir, "status")
	if !strings.Contains(status, "VACANT") {
		t.Fatalf("the seat must be vacant after release:\n%s", status)
	}
}

// A torn journal must be reported by every read verb, not silently truncated —
// printing a partial history with no warning is the one dishonest thing this could do.
func TestCLIWarnsAboutACorruptTail(t *testing.T) {
	dir := t.TempDir()
	seedSeat(t, dir)
	mustCLI(t, dir, "record", "-m", "real work", "--workstream", "api",
		"--outcome", "success", "-e", "commit:abc")

	s, err := Open(dir, WithRegistryRoot(cliRegistry(dir)))
	if err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(s.journalPath(), os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	f.WriteString(`{"seq":3,"trunc`)
	f.Close()

	out := mustCLI(t, dir, "log")
	if !strings.Contains(out, "WARNING") {
		t.Fatalf("log must warn that its output is a valid PREFIX, not the whole story:\n%s", out)
	}
	if !strings.Contains(out, "real work") {
		t.Fatalf("a torn tail must not hide the valid history before it:\n%s", out)
	}

	// A write refuses until the tail is explicitly repaired.
	_, err = cli(t, dir, "record", "-m", "carrying on", "--outcome", "success", "-e", "commit:def")
	if err == nil {
		t.Fatal("appending onto a corrupt tail must be refused")
	}

	// --plan changes nothing, and says exactly what it WOULD discard, so an operator can
	// see with their own eyes that it is a torn fragment and not a record.
	out = mustCLI(t, dir, "repair", "--plan")
	if !strings.Contains(out, "repairable:     true") {
		t.Fatalf("a torn final append is repairable, and the plan must say so:\n%s", out)
	}
	if !strings.Contains(out, "trunc") {
		t.Fatalf("the plan must show the bytes it would discard:\n%s", out)
	}
	if !strings.Contains(mustCLI(t, dir, "log"), "WARNING") {
		t.Fatal("--plan must change nothing")
	}

	// …and the repair is explicit, and truncates only the unreadable bytes.
	out = mustCLI(t, dir, "repair")
	if !strings.Contains(out, "No valid entry was removed") {
		t.Fatalf("the repair must reassure that it cost only the torn tail:\n%s", out)
	}
	if !strings.Contains(out, "quarantined") {
		t.Fatalf("the repair must say where the discarded bytes went:\n%s", out)
	}

	out = mustCLI(t, dir, "log")
	if strings.Contains(out, "WARNING") {
		t.Fatalf("the journal must be clean after a repair:\n%s", out)
	}
	if !strings.Contains(out, "real work") {
		t.Fatalf("the repair destroyed valid history:\n%s", out)
	}
	// The repair itself is on the record, as a DEGRADED entry. A journal that quietly
	// healed itself is indistinguishable from one somebody edited.
	if !strings.Contains(mustCLI(t, dir, "log", "--kind", "repair"), "quarantined") {
		t.Fatal("the repair must leave a receipt in the journal")
	}
}
