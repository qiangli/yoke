// Copyright (c) 2025 qiangli
// See LICENSE for licensing information

package steward

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/qiangli/yoke/pkg/principal"
)

// Every test here is named for the CLAIM it defends, and each one guards a specific
// way a continuity subsystem rots: a second steward appears, a returning zombie
// interleaves its writes, a crash erases the history, a seizure of authority leaves no
// trace — or, the quietest and worst, an agent's "done ✅" gets laundered into a fact.

// ─── helpers ──────────────────────────────────────────────────────────────────

// testVerifier is an injected root of trust, which is the only kind there is.
//
// Tests get one because the store FAILS CLOSED without one: no verifier, no authority
// transition, ever. That is the fail-closed default (see ErrNoVerifier), and a test
// helper that quietly worked around it would be testing a system nobody ships.
//
// It records what it was asked, so a test can assert that the transition really did put
// the capability in front of the root of trust rather than deciding on its own.
type testVerifier struct {
	name    string
	grade   Grade
	approve bool
	err     error
	calls   []Capability
}

func (v *testVerifier) Name() string {
	if v.name == "" {
		return "test-verifier"
	}
	return v.name
}

func (v *testVerifier) VerifyCapability(_ context.Context, c Capability) (Attestation, error) {
	v.calls = append(v.calls, c)
	if v.err != nil {
		return Attestation{}, v.err
	}
	return Attestation{
		Channel:  "test",
		Grade:    v.grade,
		Approved: v.approve,
		Why:      "test verifier",
	}, nil
}

// verified is a verifier that establishes authority OUTSIDE the store — the grade a host
// with a real human channel (bashy meet, an approval service) would return, and the only
// grade that authorizes an unattended transition.
func verified() *testVerifier { return &testVerifier{grade: GradeVerified, approve: true} }

// auditOnly is the grade the CLI's typed-terminal confirmation returns: deliberate and
// attended, and NOT proof a human was present.
func auditOnly() *testVerifier { return &testVerifier{grade: GradeAudit, approve: true} }

// testScope pins the seat identity so tests do not depend on the machine they run on —
// and, more to the point, so the ISOLATION properties can be exercised: two machines
// sharing a hostname, one machine under two accounts. None of that is reachable by
// setting an environment variable any more, which was the whole point of removing them.
func testScope(id string) ScopeProvider {
	return StaticScope(Scope{
		ID: id, Machine: "machine:" + id, Account: "account:" + id, Host: "test-host", Source: "test",
	})
}

// testHome is the throwaway home the suite runs in. Tests that need to know where a
// default-rooted registry landed read it from here.
var testHome string

// TestMain redirects the two things that fall back to a home directory — the default store
// dir ($HOME, see defaultDirFor) and the CANONICAL SEAT REGISTRY (the OS account's home, see
// defaultRegistryRoot) — into a throwaway directory for the whole package.
//
// Note what it does NOT do, because that is the point of the fix this backstop follows. It
// used to pin $HOME and consider the registry handled, since the registry root was
// os.UserHomeDir — i.e. $HOME. It is not any more: the root now comes from the OS ACCOUNT
// (the passwd record for the real uid; the token's profile directory on windows), which no
// amount of Setenv touches, because an agent that could relocate the registry by exporting a
// variable could always find it empty and mint a second seat. So the suite injects the
// resolver directly (accountHomeFn) — an in-process hook, the same kind of trusted seam as
// WithRegistryRoot — and TestDefaultRegistryRootIgnoresAmbientHome pins that the REAL
// resolver ignores the environment entirely.
//
// This remains a BACKSTOP, not a convenience: individual tests pass WithRegistryRoot where
// they need stores isolated from each other WITHIN one test. The floor underneath makes a
// missing option a contained bug rather than a binding written into the developer's real
// ~/.bashy — which, being a singleton, would then refuse the next test that tried.
func TestMain(m *testing.M) {
	home, err := os.MkdirTemp("", "steward-test-home-")
	if err != nil {
		panic(err)
	}
	testHome = home
	os.Setenv("HOME", home)        // unix: the default STORE dir only
	os.Setenv("USERPROFILE", home) // windows: likewise
	accountHomeFn = func() (string, string, error) { return home, "account:test", nil }
	code := m.Run()
	os.RemoveAll(home)
	os.Exit(code)
}

// ─── the trusted verification verifier, faked honestly ────────────────────────
//
// sealingVerifier is what a HOST supplies: something that can actually go and look, and
// that seals its verdict with a token only it can produce and only it can recognize.
//
// The token is an HMAC over the CANONICAL CLAIM, keyed by a secret the "agent" (the test's
// caller) does not have. That is not decoration — it is the property the whole design rests
// on, and it is what makes the adversarial tests below mean something:
//
//   - a caller cannot MINT one (it has no key), so a hand-written Seal fails RecheckSeal;
//   - a token cannot be MOVED to another claim (the claim is inside the MAC), so lifting a
//     real seal off one verification and pasting it onto another fails too.
//
// A real one asks a CI system or checks a signature. The shape is the same: an answer the
// agent's filesystem cannot fabricate.
type sealingVerifier struct {
	name   string
	key    string
	admits map[uint64]bool // which target seqs it will vouch for; nil ⇒ all
	refute bool            // it went and looked, and the claim is FALSE
	err    error           // it could not establish the claim either way
	calls  []VerificationClaim
}

func (v *sealingVerifier) Name() string {
	if v.name == "" {
		return "test-seal-verifier"
	}
	return v.name
}

// canonical is the exact byte string the token commits to. Everything that identifies the
// claim goes in; nothing that does not.
func (v *sealingVerifier) canonical(c VerificationClaim) string {
	return fmt.Sprintf("%s\x00%s\x00%d\x00%d\x00%s\x00%s",
		c.Workstream, c.Actor.Name, c.Epoch, c.TargetSeq, c.TargetHash, c.Result)
}

func (v *sealingVerifier) mac(c VerificationClaim) string {
	key := v.key
	if key == "" {
		key = "test-key"
	}
	m := hmac.New(sha256.New, []byte(key))
	m.Write([]byte(v.canonical(c)))
	return hex.EncodeToString(m.Sum(nil))
}

func (v *sealingVerifier) VerifyClaim(_ context.Context, c VerificationClaim) (Seal, error) {
	v.calls = append(v.calls, c)
	if v.err != nil {
		return Seal{}, v.err
	}
	if v.refute {
		return Seal{Verifier: v.Name(), Grade: GradeVerified, Approved: false, Why: "the claim is false"}, nil
	}
	if v.admits != nil && !v.admits[c.TargetSeq] {
		return Seal{}, fmt.Errorf("this verifier cannot speak to seq %d", c.TargetSeq)
	}
	return Seal{
		Verifier: v.Name(),
		Grade:    GradeVerified,
		Approved: true,
		Why:      "went and looked",
		Binding:  digestOf([]byte(v.canonical(c))),
		Token:    v.mac(c),
	}, nil
}

func (v *sealingVerifier) RecheckSeal(c VerificationClaim, s Seal) bool {
	return s.Token != "" && hmac.Equal([]byte(s.Token), []byte(v.mac(c)))
}

// sealing is a verifier that vouches for everything it is asked about.
func sealing() *sealingVerifier { return &sealingVerifier{} }

// newStore opens a hermetic store.
//
// WithRegistryRoot is not optional here, and the reason is the feature. Every store in
// this suite shares one scope ("test-seat"), and the canonical registry allows a scope
// exactly ONE directory — so without a per-test registry root, the second store built in
// the process would be refused as a second seat, which is precisely the enforcement being
// added. Rooting it in t.TempDir() gives each test its own canonical world, and keeps the
// suite out of the developer's real ~/.bashy.
func newStore(t *testing.T, opts ...Option) *Store {
	t.Helper()
	opts = append([]Option{
		WithScopeProvider(testScope("test-seat")),
		WithVerifier(verified()),
		WithRegistryRoot(t.TempDir()),
	}, opts...)
	s, err := Open(t.TempDir(), opts...)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	return s
}

// agent builds a stable, distinct identity. Name+Host, never an Episode: SameHolder
// matches on episode FIRST, so two test agents sharing one would be treated as the
// same logical steward and every singleton assertion here would pass vacuously.
func agent(name string) principal.Ref {
	return principal.Ref{Kind: principal.KindAgent, Name: name, Host: "test-host"}
}

var t0 = time.Date(2026, 7, 13, 12, 0, 0, 0, time.UTC)

func at(d time.Duration) time.Time { return t0.Add(d) }

// mustGrant mints a capability for `who` to perform `action`.
func mustGrant(t *testing.T, s *Store, who principal.Ref, action string, when time.Time) Grant {
	t.Helper()
	g, err := s.Authorize(context.Background(), GrantRequest{
		Action: action, Grantee: who, Actor: "qiangli", Reason: "test", Attended: true,
	}, when)
	if err != nil {
		t.Fatalf("Authorize(%s): %v", action, err)
	}
	return g
}

// mustClaim claims or fails the test, returning the epoch the holder must now present
// on every write. Returning the EPOCH rather than the View is deliberate: it is the
// token, and a test that forgets to carry it is a test that would not have caught a
// fencing bug.
//
// It mints a capability first, because CLAIMING IS AUTHORIZED — a vacant seat is still
// the seat of authority for the whole machine, and a lapsed one still has an incumbent
// who is about to be fenced.
func mustClaim(t *testing.T, s *Store, who principal.Ref, when time.Time) uint64 {
	t.Helper()
	g := mustGrant(t, s, who, ActionClaim, when)
	v, err := s.Claim(context.Background(), who, SeatRequest{GrantID: g.ID, Attended: true}, when)
	if err != nil {
		t.Fatalf("Claim(%s): %v", who.Name, err)
	}
	return v.Authority.Epoch
}

// mustRecord appends an entry or fails the test.
func mustRecord(t *testing.T, s *Store, e Entry, epoch uint64, when time.Time) Entry {
	t.Helper()
	out, err := s.Record(e, epoch, when)
	if err != nil {
		t.Fatalf("Record: %v", err)
	}
	return out
}

// evidenced is an entry whose success claim points at SOMETHING. Note what it is not:
// verified. Nobody has checked that command ran.
func evidenced(who principal.Ref, ws, summary string) Entry {
	return Entry{
		Actor: who, Kind: KindEffect, Workstream: ws, Summary: summary,
		Outcome:  OutcomeSuccess,
		Evidence: []Evidence{{Kind: "command", Ref: "go test ./..."}},
	}
}

// digestEvidence is evidence pinned to exact bytes: auditable, rehashable — and PROMOTING
// NOTHING, which is what several tests here exist to pin. A digest proves the bytes did not
// change; it says nothing about whether a check ran. See verification.go.
func digestEvidence() []Evidence {
	return []Evidence{{Kind: "file", Ref: "/tmp/test.log", Digest: digestOf([]byte("PASS"))}}
}

// mustTakeover seizes the seat with a fresh grant, returning the new epoch.
func mustTakeover(t *testing.T, s *Store, who principal.Ref, when time.Time) uint64 {
	t.Helper()
	g := mustGrant(t, s, who, ActionTakeover, when)
	v, err := s.Takeover(context.Background(), who, SeatRequest{GrantID: g.ID, Attended: true}, when)
	if err != nil {
		t.Fatalf("Takeover(%s): %v", who.Name, err)
	}
	return v.Authority.Epoch
}

// setFailpoint arms a named crash point for the duration of one test. See failpoint.
func setFailpoint(t *testing.T, stage string, err error) {
	t.Helper()
	prev := failpoint
	failpoint = func(s string) error {
		if s == stage {
			return err
		}
		return prev(s)
	}
	t.Cleanup(func() { failpoint = prev })
}

func journalBytes(t *testing.T, s *Store) []byte {
	t.Helper()
	b, err := os.ReadFile(s.journalPath())
	if err != nil {
		t.Fatalf("read journal: %v", err)
	}
	return b
}

// ─── singleton acquisition ────────────────────────────────────────────────────

// The seat is a SINGLETON. If two agents can both believe they hold it, every other
// guarantee in this package is decoration — so this is the first thing that must hold,
// and it must hold under a real race, not a polite sequential one.
func TestSeatIsSingletonUnderConcurrentClaims(t *testing.T) {
	s := newStore(t)

	const n = 16
	// Every racer is separately authorized, so what the race tests is the SEAT, not the
	// authorization: sixteen agents each holding a valid, unspent capability for the
	// vacant seat, all trying to spend it at once. Exactly one may win.
	grants := make([]Grant, n)
	for i := range n {
		grants[i] = mustGrant(t, s, agent(string(rune('a'+i))), ActionClaim, at(0))
	}

	var wg sync.WaitGroup
	won := make([]bool, n)
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := s.Claim(context.Background(), agent(string(rune('a'+i))),
				SeatRequest{GrantID: grants[i].ID, Attended: true}, at(0))
			if err == nil {
				won[i] = true
			}
		}()
	}
	wg.Wait()

	winners := 0
	for _, w := range won {
		if w {
			winners++
		}
	}
	if winners != 1 {
		t.Fatalf("exactly one agent must win the seat, got %d winners", winners)
	}

	// And the journal must agree: one claim event, not sixteen.
	rep, err := s.Replay()
	if err != nil {
		t.Fatalf("Replay: %v", err)
	}
	claims := 0
	for _, e := range rep.Entries {
		if e.Kind == KindSeatClaimed {
			claims++
		}
	}
	if claims != 1 {
		t.Fatalf("the journal records %d claims; a singleton has exactly 1", claims)
	}
}

// THE SAME-HOLDER CLAIM SHORTCUT IS GONE, and this is the test that keeps it gone.
//
// Claim used to treat "you already hold this seat and it is live" as an idempotent
// heartbeat: refresh the liveness cache, no epoch presented, no journal entry, no
// complaint. It reads as a harmless convenience and it is a hole, because it is A WAY TO
// REFRESH A HELD TENURE WITHOUT PRESENTING THE EPOCH — and the epoch is the only thing in
// the system that can tell a steward its tenure ended while it was away.
//
// The agent that most wants this shortcut is precisely the one that must not have it. A
// live holder renews through Heartbeat, which presents the token and is fenced like
// everything else.
func TestClaimNeverRefreshesALiveSeatEvenForItsOwnHolder(t *testing.T) {
	s := newStore(t)
	ep := mustClaim(t, s, agent("a"), at(0))

	// The holder returns, live, with a perfectly good capability — and CLAIM still refuses.
	g := mustGrant(t, s, agent("a"), ActionClaim, at(time.Minute))
	_, err := s.Claim(context.Background(), agent("a"),
		SeatRequest{GrantID: g.ID, Attended: true, Intent: "still here"}, at(time.Minute))

	var held *ErrHeld
	if !errors.As(err, &held) {
		t.Fatalf("a live seat is HELD, even by you — claim must not refresh it. got %v", err)
	}
	if !held.Yours {
		t.Fatal("the error must say the live holder is the caller, or it sends them looking for a takeover they do not need")
	}
	if !strings.Contains(held.Error(), "heartbeat") {
		t.Fatalf("the refusal must point at the way to actually renew (heartbeat --epoch): %v", held)
	}

	// Nothing moved: no new epoch, no new entry, no refreshed cache without a token.
	rep, _ := s.Replay()
	if n := len(rep.Entries); n != 1 {
		t.Fatalf("the refused claim must write nothing: expected 1 journal entry, got %d", n)
	}

	// And the honest path still works — because it presents the epoch.
	if err := s.Heartbeat(agent("a"), ep, at(2*time.Minute)); err != nil {
		t.Fatalf("the live holder renews with Heartbeat(epoch): %v", err)
	}
	v, _ := s.Status(at(2 * time.Minute))
	if v.Liveness != LivenessLive || v.Authority.Epoch != ep {
		t.Fatalf("heartbeat renews in place: liveness=%q epoch=%d (want live, %d)", v.Liveness, v.Authority.Epoch, ep)
	}
}

// The same hole from the zombie's side: a steward whose tenure ENDED, returning with the
// identity it always had, must not be able to claim its way back in without noticing.
// Identity is not authority — and a claim that refreshed on identity alone would let the
// zombie skip the fence entirely.
func TestZombieHolderCannotClaimItsWayBackIn(t *testing.T) {
	s := newStore(t)
	old := mustClaim(t, s, agent("a"), at(0))
	mustTakeover(t, s, agent("b"), at(TTL+time.Minute)) // a lapses; b seizes

	// `a` comes back believing it still holds the seat. b is live, so the claim is HELD —
	// and critically it is NOT "yours", because a is not the holder any more.
	g := mustGrant(t, s, agent("a"), ActionClaim, at(TTL+2*time.Minute))
	_, err := s.Claim(context.Background(), agent("a"),
		SeatRequest{GrantID: g.ID, Attended: true}, at(TTL+2*time.Minute))
	var held *ErrHeld
	if !errors.As(err, &held) {
		t.Fatalf("the seat is live under its new holder — the zombie's claim must be refused: %v", err)
	}
	if held.Yours {
		t.Fatal("a fenced zombie is NOT the holder; telling it the seat is 'yours' would be a lie with consequences")
	}
	// And its writes at the old epoch remain fenced, which is the whole point of the ladder.
	_, err = s.Record(evidenced(agent("a"), "ws", "i'm back"), old, at(TTL+3*time.Minute))
	var fenced *ErrFenced
	if !errors.As(err, &fenced) {
		t.Fatalf("the zombie's writes at its old epoch must be FENCED, got %v", err)
	}
}

func TestClaimRefusesALiveSeat(t *testing.T) {
	s := newStore(t)
	mustClaim(t, s, agent("a"), at(0))

	g := mustGrant(t, s, agent("b"), ActionClaim, at(time.Minute))
	_, err := s.Claim(context.Background(), agent("b"), SeatRequest{GrantID: g.ID, Attended: true}, at(time.Minute))
	var held *ErrHeld
	if !errors.As(err, &held) {
		t.Fatalf("claiming a live seat must fail with ErrHeld, got %v", err)
	}
}

// A lapse is the ONE ordinary way a seat changes hands without authorization — and it
// requires a heartbeat record we actually trust. See the adversarial suite for every
// way an untrustworthy one is refused.
func TestLapsedSeatIsClaimableAndFencesTheIncumbent(t *testing.T) {
	s := newStore(t)
	e1 := mustClaim(t, s, agent("a"), at(0))

	v, err := s.Status(at(TTL + time.Minute))
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if v.Liveness != LivenessLapsed {
		t.Fatalf("a heartbeat older than the TTL is LAPSED, got %q", v.Liveness)
	}
	if !v.Claimable {
		t.Fatal("a lapsed seat with a trustworthy heartbeat is the ordinary recovery path — it must be claimable")
	}

	e2 := mustClaim(t, s, agent("b"), at(TTL+2*time.Minute))
	if e2 <= e1 {
		t.Fatalf("a change of holder must bump the fencing epoch: %d → %d", e1, e2)
	}

	// The incumbent was never asked, and may come back at any moment. It is FENCED.
	_, err = s.Record(evidenced(agent("a"), "ws", "i'm back"), e1, at(TTL+3*time.Minute))
	var fenced *ErrFenced
	if !errors.As(err, &fenced) {
		t.Fatalf("the lapsed incumbent returning at its old epoch must be FENCED, got %v", err)
	}
}

func TestLiveHeartbeatKeepsTheSeat(t *testing.T) {
	s := newStore(t)
	ep := mustClaim(t, s, agent("a"), at(0))

	if err := s.Heartbeat(agent("a"), ep, at(TTL-time.Minute)); err != nil {
		t.Fatalf("Heartbeat: %v", err)
	}
	// The heartbeat moved the clock forward, so what would have lapsed has not.
	v, _ := s.Status(at(TTL + time.Minute))
	if v.Liveness != LivenessLive {
		t.Fatalf("a heartbeat inside the TTL keeps the seat live, got %q", v.Liveness)
	}
	g := mustGrant(t, s, agent("b"), ActionClaim, at(TTL+time.Minute))
	if _, err := s.Claim(context.Background(), agent("b"),
		SeatRequest{GrantID: g.ID, Attended: true}, at(TTL+time.Minute)); err == nil {
		t.Fatal("a live seat must not be claimable")
	}
}

func TestRecordingRefreshesLiveness(t *testing.T) {
	s := newStore(t)
	ep := mustClaim(t, s, agent("a"), at(0))
	mustRecord(t, s, evidenced(agent("a"), "ws", "did a thing"), ep, at(TTL-time.Minute))

	v, _ := s.Status(at(TTL + time.Minute))
	if v.Liveness != LivenessLive {
		t.Fatalf("writing to the journal is self-evidently being alive; liveness = %q", v.Liveness)
	}
}

func TestEpochIsMonotonicAcrossRelease(t *testing.T) {
	s := newStore(t)
	e1 := mustClaim(t, s, agent("a"), at(0))
	if err := s.Release(agent("a"), e1, "done", at(time.Minute)); err != nil {
		t.Fatalf("Release: %v", err)
	}
	e2 := mustClaim(t, s, agent("b"), at(2*time.Minute))
	if e2 <= e1 {
		t.Fatalf("the epoch ladder never resets — a release must not lower it: %d → %d", e1, e2)
	}
}

func TestReleaseIsOptionalForCorrectness(t *testing.T) {
	s := newStore(t)
	mustClaim(t, s, agent("a"), at(0))
	// 'a' vanishes. No release, no goodbye, no cooperation.
	e2 := mustClaim(t, s, agent("b"), at(TTL+time.Minute))
	if e2 != 2 {
		t.Fatalf("a successor must not need the predecessor's cooperation; epoch = %d", e2)
	}
}

func TestReleasingAVacantSeatIsANoOp(t *testing.T) {
	s := newStore(t)
	if err := s.Release(agent("a"), 1, "", at(0)); err != nil {
		t.Fatalf("releasing a seat nobody holds is a no-op, not an error: %v", err)
	}
}

// ─── fencing ──────────────────────────────────────────────────────────────────

// The returning zombie is the case the epoch exists for: a steward that lapsed, was
// replaced, and comes back mid-sentence believing it still holds the seat.
func TestReturningZombieIsFenced(t *testing.T) {
	s := newStore(t)
	zombieEpoch := mustClaim(t, s, agent("zombie"), at(0))
	mustRecord(t, s, evidenced(agent("zombie"), "ws", "before the lapse"), zombieEpoch, at(time.Minute))

	successorEpoch := mustClaim(t, s, agent("successor"), at(TTL+time.Minute))
	mustRecord(t, s, evidenced(agent("successor"), "ws", "took over"), successorEpoch, at(TTL+2*time.Minute))

	_, err := s.Record(evidenced(agent("zombie"), "ws", "as I was saying…"), zombieEpoch, at(TTL+3*time.Minute))
	var fenced *ErrFenced
	if !errors.As(err, &fenced) {
		t.Fatalf("a zombie presenting its old epoch must be FENCED, got %v", err)
	}
	if fenced.Presented != zombieEpoch || fenced.Current != successorEpoch {
		t.Fatalf("the fence must name both epochs: presented %d, current %d", fenced.Presented, fenced.Current)
	}
	// The error explains the ZOMBIE'S situation, not a stranger's. An agent that reads
	// "you are not the holder" may decide to re-claim — and overwrite its successor.
	if !strings.Contains(fenced.Error(), "tenure ended") {
		t.Fatalf("ErrFenced must explain the zombie to itself, got: %v", fenced)
	}
}

// Fencing is a property of the TOKEN, not of the identity. The same logical principal,
// returning with a stale token, is fenced exactly like a stranger — because "being
// yourself" is not a credential, and a tenure that ended is over no matter whose hand
// the old token is in.
func TestStaleTokenIsFencedEvenForTheSameLogicalPrincipal(t *testing.T) {
	s := newStore(t)
	a := agent("a")
	old := mustClaim(t, s, a, at(0))

	// 'a' lapses, and 'a' — the same logical agent, a new process — re-claims. New tenure,
	// new epoch. The OLD process is still out there holding `old`.
	fresh := mustClaim(t, s, a, at(TTL+time.Minute))
	if fresh == old {
		t.Fatalf("a re-claim after a lapse is a NEW tenure and must bump the epoch (%d)", old)
	}

	_, err := s.Record(evidenced(a, "ws", "from the old process"), old, at(TTL+2*time.Minute))
	var fenced *ErrFenced
	if !errors.As(err, &fenced) {
		t.Fatalf("the same principal presenting a stale token must be FENCED (not waved through), got %v", err)
	}
}

// Zero is not "whatever I hold". It is an absence, and it is refused — because the
// agent that would use it is precisely the agent that does not know its tenure ended.
func TestEpochZeroIsRefusedOnEveryMutation(t *testing.T) {
	s := newStore(t)
	a := agent("a")
	ep := mustClaim(t, s, a, at(0))
	seq := mustRecord(t, s, evidenced(a, "ws", "a thing"), ep, at(time.Minute)).Seq

	var noEpoch *ErrNoEpoch
	check := func(name string, err error) {
		t.Helper()
		if !errors.As(err, &noEpoch) {
			t.Fatalf("%s with epoch 0 must be refused with ErrNoEpoch, got %v", name, err)
		}
	}

	_, err := s.Record(evidenced(a, "ws", "x"), 0, at(2*time.Minute))
	check("Record", err)

	_, err = s.Decide(a, 0, "ws", "x", "why", nil, at(2*time.Minute))
	check("Decide", err)

	_, err = s.Attest(context.Background(), a, 0, Verification{TargetSeq: seq, Result: OutcomeSuccess, Method: "looked"}, nil, at(2*time.Minute))
	check("Attest", err)

	_, err = s.Transcript(a, 0, "ws", "x", strings.NewReader("hi"), at(2*time.Minute))
	check("Transcript", err)

	_, err = s.OpenWorkstream(a, 0, "ws2", "x", at(2*time.Minute))
	check("OpenWorkstream", err)

	_, err = s.UpdateWorkstream(a, 0, "ws", WorkstreamUpdate{Priority: P0}, at(2*time.Minute))
	check("UpdateWorkstream", err)

	_, err = s.CloseWorkstream(a, 0, "ws", "x", OutcomeSuccess, nil, at(2*time.Minute))
	check("CloseWorkstream", err)

	_, err = s.Checkpoint(a, 0, "x", at(2*time.Minute))
	check("Checkpoint", err)

	r, _ := s.Reconcile(context.Background(), at(2*time.Minute))
	_, err = s.RecordReconciliation(a, 0, r, at(2*time.Minute))
	check("RecordReconciliation", err)

	check("Heartbeat", s.Heartbeat(a, 0, at(2*time.Minute)))
	check("Release", s.Release(a, 0, "", at(2*time.Minute)))
}

// A stored entry never carries epoch 0, so replay refuses one that does: it was not
// written by this package, and an unfenced entry in the journal is exactly what a
// forger needs.
func TestReplayRefusesAnEntryWithEpochZero(t *testing.T) {
	s := newStore(t)
	ep := mustClaim(t, s, agent("a"), at(0))
	mustRecord(t, s, evidenced(agent("a"), "ws", "real"), ep, at(time.Minute))

	forged := `{"schema":"` + SchemaVersion + `","seq":3,"prev_hash":"x","id":"f","time":"2026-07-13T12:05:00Z",` +
		`"actor":{"name":"forger"},"epoch":0,"kind":"effect","summary":"unfenced","hash":"x"}` + "\n"
	f, err := os.OpenFile(s.journalPath(), os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	f.WriteString(forged)
	f.Close()

	rep, _ := s.Replay()
	if !rep.Corrupt {
		t.Fatal("an entry with epoch 0 must not replay as valid")
	}
	if len(rep.Entries) != 2 {
		t.Fatalf("the valid prefix before the forgery must survive: got %d entries", len(rep.Entries))
	}
}

func TestNonHolderCannotWrite(t *testing.T) {
	s := newStore(t)
	ep := mustClaim(t, s, agent("holder"), at(0))

	_, err := s.Record(evidenced(agent("stranger"), "ws", "sneaking in"), ep, at(time.Minute))
	var notHolder *ErrNotHolder
	if !errors.As(err, &notHolder) {
		t.Fatalf("a bystander must not write the host's authoritative record, got %v", err)
	}
}

func TestWriteToVacantSeatIsRejected(t *testing.T) {
	s := newStore(t)
	_, err := s.Record(evidenced(agent("a"), "ws", "no seat"), 1, at(0))
	var notHolder *ErrNotHolder
	if !errors.As(err, &notHolder) || !notHolder.Vacant {
		t.Fatalf("writing on a vacant seat must be rejected, got %v", err)
	}
}

// The epoch ladder must not be climbable by anyone already on it: a generic write may
// not forge a seat event and mint itself an epoch.
func TestRecordCannotForgeASeatEvent(t *testing.T) {
	s := newStore(t)
	ep := mustClaim(t, s, agent("a"), at(0))

	for _, k := range []Kind{KindSeatClaimed, KindSeatTakeover, KindSeatReleased} {
		_, err := s.Record(Entry{Actor: agent("a"), Kind: k, Summary: "forged"}, ep, at(time.Minute))
		if err == nil {
			t.Fatalf("Record must refuse to forge a %s seat event", k)
		}
	}
}

func TestAppendRefusesAnUnknownKind(t *testing.T) {
	s := newStore(t)
	ep := mustClaim(t, s, agent("a"), at(0))
	_, err := s.Record(Entry{Actor: agent("a"), Kind: Kind("whatever"), Summary: "x"}, ep, at(time.Minute))
	if err == nil || !strings.Contains(err.Error(), "unknown kind") {
		t.Fatalf("an entry of a kind no projection understands must be refused, got %v", err)
	}
}

// ─── takeover & authorization ─────────────────────────────────────────────────

func TestTakeoverRequiresACapability(t *testing.T) {
	s := newStore(t)
	mustClaim(t, s, agent("incumbent"), at(0))

	_, err := s.Takeover(context.Background(), agent("usurper"), SeatRequest{Attended: true}, at(time.Minute))
	var unauth *ErrUnauthorized
	if !errors.As(err, &unauth) {
		t.Fatalf("seizing a live seat with no capability must be refused, got %v", err)
	}
}

// CLAIMING IS AN AUTHORITY TRANSITION TOO, and this is the test that says so.
//
// It used to be free: any process that could see a vacant or lapsed seat became the
// steward by asking. The lapsed case is the one that gives it away — "lapsed" proves a
// heartbeat gap and nothing more, so an unattended agent that could claim a lapsed seat
// could wait out the TTL and depose a working steward. That is the takeover it was
// forbidden to perform, spelled differently and with no record of authorization.
func TestClaimRequiresACapability(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(t *testing.T, s *Store)
		when  time.Duration
	}{
		{
			name:  "a vacant seat is still the seat of authority for the whole machine",
			setup: func(*testing.T, *Store) {},
			when:  0,
		},
		{
			name:  "a lapsed seat has an incumbent, and claiming it FENCES them",
			setup: func(t *testing.T, s *Store) { mustClaim(t, s, agent("incumbent"), at(0)) },
			when:  TTL + time.Minute,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newStore(t)
			tc.setup(t, s)

			_, err := s.Claim(context.Background(), agent("worker"), SeatRequest{Attended: true}, at(tc.when))
			var unauth *ErrUnauthorized
			if !errors.As(err, &unauth) {
				t.Fatalf("claiming with no capability must be refused, got %v", err)
			}
			// And the refusal left no trace of the worker in the record.
			rep, _ := s.Replay()
			for _, e := range rep.Entries {
				if e.Kind == KindSeatClaimed && SameHolder(e.Actor, agent("worker")) {
					t.Fatal("the refused claim must not have reached the journal")
				}
			}
		})
	}
}

// A capability minted to CLAIM an empty seat must not be spendable on SEIZING an occupied
// one. They are different acts with different victims, and a grant is not a skeleton key.
func TestGrantIsBoundToItsAction(t *testing.T) {
	s := newStore(t)
	mustClaim(t, s, agent("incumbent"), at(0))

	claimGrant := mustGrant(t, s, agent("b"), ActionClaim, at(time.Minute))
	_, err := s.Takeover(context.Background(), agent("b"),
		SeatRequest{GrantID: claimGrant.ID, Attended: true}, at(2*time.Minute))
	var unauth *ErrUnauthorized
	if !errors.As(err, &unauth) || !strings.Contains(err.Error(), "not \"takeover\"") {
		t.Fatalf("a claim capability must not authorize a takeover, got %v", err)
	}

	// And the reverse: a takeover grant does not authorize a claim.
	s2 := newStore(t)
	tkGrant := mustGrant(t, s2, agent("b"), ActionTakeover, at(0))
	_, err = s2.Claim(context.Background(), agent("b"), SeatRequest{GrantID: tkGrant.ID, Attended: true}, at(0))
	if !errors.As(err, &unauth) || !strings.Contains(err.Error(), "not \"claim\"") {
		t.Fatalf("a takeover capability must not authorize a claim, got %v", err)
	}
}

func TestTakeoverSeizesALiveSeatAndRecordsItsAuthority(t *testing.T) {
	s := newStore(t)
	oldEpoch := mustClaim(t, s, agent("incumbent"), at(0))

	newEpoch := mustTakeover(t, s, agent("successor"), at(time.Minute))
	if newEpoch <= oldEpoch {
		t.Fatalf("a takeover must bump the epoch: %d → %d", oldEpoch, newEpoch)
	}

	v, _ := s.Status(at(2 * time.Minute))
	if !SameHolder(v.Authority.Holder, agent("successor")) {
		t.Fatalf("the seat must have changed hands, holder = %s", holderName(v.Authority.Holder))
	}
	if v.Authority.TakenOverFrom == nil || v.Authority.TakenOverFrom.Name != "incumbent" {
		t.Fatal("the record must say WHO was fenced — an unexplained seizure is indistinguishable from a hijack")
	}
	a := v.Authority.Authz
	if a == nil || a.Actor != "qiangli" || a.Provenance != ProvenanceOperatorAssertion {
		t.Fatalf("the record must carry the capability the seizure was performed under, got %+v", a)
	}

	// And the incumbent, who was never asked, is fenced from that instant.
	_, err := s.Record(evidenced(agent("incumbent"), "ws", "still working"), oldEpoch, at(3*time.Minute))
	var fenced *ErrFenced
	if !errors.As(err, &fenced) {
		t.Fatalf("the seized incumbent must be fenced, got %v", err)
	}
}

// A grant is SINGLE-USE, and the journal — not the grant file — is what makes it so.
func TestGrantCannotBeReplayed(t *testing.T) {
	for _, action := range []string{ActionClaim, ActionTakeover} {
		t.Run(action, func(t *testing.T) {
			s := newStore(t)
			spend := func(g Grant, when time.Time) error {
				req := SeatRequest{GrantID: g.ID, Attended: true}
				if action == ActionClaim {
					_, err := s.Claim(context.Background(), agent("b"), req, when)
					return err
				}
				_, err := s.Takeover(context.Background(), agent("b"), req, when)
				return err
			}
			if action == ActionTakeover {
				mustClaim(t, s, agent("a"), at(0))
			}

			g := mustGrant(t, s, agent("b"), action, at(time.Minute))
			if err := spend(g, at(2*time.Minute)); err != nil {
				t.Fatalf("first use of a grant must work: %v", err)
			}
			// Vacate, so the second attempt is refused for being a REPLAY rather than merely
			// for arriving at an occupied seat. The nonce is what must stop it.
			ep, _ := s.Status(at(2 * time.Minute))
			if err := s.Release(agent("b"), ep.Authority.Epoch, "", at(2*time.Minute+time.Second)); err != nil {
				t.Fatalf("Release: %v", err)
			}

			// Restore the grant file, exactly as a backup or a `cp` would. The journal still
			// remembers the seat event that consumed the nonce — and the journal is the
			// authority for consumption, not the file.
			if err := writeJSONAtomic(filepath.Join(s.grantDir(), g.ID+".json"), g); err != nil {
				t.Fatal(err)
			}
			err := spend(g, at(3*time.Minute))
			var unauth *ErrUnauthorized
			if !errors.As(err, &unauth) || !strings.Contains(err.Error(), "already been used") {
				t.Fatalf("a replayed grant must be refused even when its file is restored, got %v", err)
			}
		})
	}
}

// UNATTENDED ACQUISITION NEEDS A VERIFIER, NOT A RECEIPT — and this test is the
// correction of the previous revision's worst mistake.
//
// It used to demand an "external receipt" for an unattended takeover: a file, copied into
// the store, pinned by digest. That sounds like evidence and is not. A DIGEST IS
// INTEGRITY, NOT AUTHENTICITY — it proves the bytes did not change, and says nothing
// whatever about who wrote them. An agent can write a file, hash it, and hand it over as
// an approval; the digest verifies perfectly. So the control on the unattended path — the
// path that needs it most, because that is where the cron jobs and the runaway agent loops
// live — reduced to "the agent must write a file first".
//
// What actually authorizes an unattended transition is an attestation at GradeVerified,
// from a verifier the HOST injected, rooted outside the store the agent can write to.
func TestUnattendedAcquisitionRequiresAVerifiedGradeAttestation(t *testing.T) {
	// A receipt does NOT rescue an audit-grade attestation when nobody is attended.
	audit := auditOnly()
	s := newStore(t, WithVerifier(audit))
	mustClaim(t, s, agent("a"), at(0))

	approval := filepath.Join(t.TempDir(), "approval.txt")
	if err := os.WriteFile(approval, []byte("approved by oncall, ticket OPS-42"), 0o600); err != nil {
		t.Fatal(err)
	}
	g, err := s.Authorize(context.Background(), GrantRequest{
		Action: ActionTakeover, Grantee: agent("b"), Actor: "oncall", Reason: "wedged",
		Attended: true, ReceiptPath: approval, ReceiptIssuer: "ops:OPS-42",
	}, at(time.Minute))
	if err != nil {
		t.Fatalf("Authorize with a receipt: %v", err)
	}
	if g.Provenance != ProvenanceExternalReceipt {
		t.Fatalf("a receipt-backed grant must be labelled as such, got %q", g.Provenance)
	}

	_, err = s.Takeover(context.Background(), agent("b"),
		SeatRequest{GrantID: g.ID, Attended: false}, at(2*time.Minute))
	var unverified *ErrUnverified
	if !errors.As(err, &unverified) {
		t.Fatalf("an UNATTENDED seizure backed only by an audit-grade attestation must be refused — "+
			"a receipt's hash is integrity, never issuer authenticity, and a confirmation nobody made attests to "+
			"nothing. got %v", err)
	}

	// A host verifier that can establish authority OUTSIDE the store is what unlocks it.
	s2 := newStore(t, WithVerifier(verified()))
	mustClaim(t, s2, agent("a"), at(0))
	g2 := mustGrant(t, s2, agent("b"), ActionTakeover, at(time.Minute))
	if _, err := s2.Takeover(context.Background(), agent("b"),
		SeatRequest{GrantID: g2.ID, Attended: false}, at(2*time.Minute)); err != nil {
		t.Fatalf("an unattended seizure established by a TRUSTED verifier must be allowed: %v", err)
	}
}

// FAIL CLOSED. With no root of trust injected, there is nothing that could authorize an
// acquisition — so there is no acquisition. Not a warning, not an "unverified" label on a
// transition that happened anyway: a refusal.
//
// Reads keep working, which is the point of the split: a store you cannot get authority
// from is still a store you can inspect.
func TestNoVerifierMeansNoAuthorityAtAll(t *testing.T) {
	dir := t.TempDir()
	open := func(opts ...Option) *Store {
		s, err := Open(dir, append([]Option{WithScopeProvider(testScope("test-seat"))}, opts...)...)
		if err != nil {
			t.Fatalf("Open: %v", err)
		}
		return s
	}
	bare := open() // NO verifier

	var noVerifier *ErrNoVerifier

	// Minting is refused...
	_, err := bare.Authorize(context.Background(), GrantRequest{
		Action: ActionClaim, Grantee: agent("a"), Actor: "qiangli", Attended: true,
	}, at(0))
	if !errors.As(err, &noVerifier) {
		t.Fatalf("minting a capability with no root of trust must fail closed, got %v", err)
	}

	// ...and so is spending one, even a real one minted through a verifier that WAS
	// present. The capability is a bound on an authority, never the source of one: it does
	// not carry its own permission around in its pocket.
	withV := open(WithVerifier(verified()))
	g := mustGrant(t, withV, agent("a"), ActionClaim, at(0))

	_, err = bare.Claim(context.Background(), agent("a"), SeatRequest{GrantID: g.ID, Attended: true}, at(time.Minute))
	if !errors.As(err, &noVerifier) {
		t.Fatalf("spending a capability with no root of trust must fail closed, got %v", err)
	}
	_, err = bare.Takeover(context.Background(), agent("a"), SeatRequest{GrantID: g.ID, Attended: true}, at(time.Minute))
	if !errors.As(err, &noVerifier) {
		t.Fatalf("a takeover with no root of trust must fail closed, got %v", err)
	}

	// Nothing reached the journal, and reads still work.
	if v, err := bare.Status(at(time.Minute)); err != nil || !v.Authority.Vacant {
		t.Fatalf("no authority transition may have occurred; status = %+v (err %v)", v, err)
	}
}

// The verifier is asked at CONSUME time, not merely at mint time — because a mint-time
// attestation is a record IN THE STORE, and a record in the store is exactly what an
// agent with file access can fabricate. Re-asking the injected verifier at the moment
// authority actually moves is what makes the check unforgeable from disk.
func TestVerifierIsAskedWhenTheCapabilityIsSpent(t *testing.T) {
	v := verified()
	s := newStore(t, WithVerifier(v))
	mustClaim(t, s, agent("a"), at(0))

	before := len(v.calls)
	mustTakeover(t, s, agent("b"), at(time.Minute))

	var mint, consume int
	for _, c := range v.calls[before:] {
		switch c.Phase {
		case PhaseMint:
			mint++
		case PhaseConsume:
			consume++
		}
	}
	if mint != 1 || consume != 1 {
		t.Fatalf("the verifier must be asked at BOTH mint and consume (got mint=%d consume=%d): a capability that "+
			"authorized itself from store state would be one the agent could write", mint, consume)
	}
}

// A forged grant — one an agent simply WROTE into the store, complete with a
// self-attested approval — buys nothing, because the transition asks the injected
// verifier rather than reading the attestation off the disk.
func TestForgedGrantWithSelfAttestedApprovalIsRefused(t *testing.T) {
	deny := &testVerifier{grade: GradeVerified, approve: false}
	s := newStore(t, WithVerifier(deny))

	forged := Grant{
		SchemaVersion: SchemaVersion,
		ID:            "g-forged",
		Action:        ActionClaim,
		Grantee:       agent("rogue"),
		Scope:         s.Scope(),
		FromEpoch:     0,
		Actor:         "definitely-a-human",
		Provenance:    ProvenanceOperatorAssertion,
		IssuedAt:      at(0),
		ExpiresAt:     at(time.Hour),
		// The agent writes its own approval. Every static check passes.
		Attestation: &Attestation{
			Verifier: "cli-pty", Channel: "pty", Grade: GradeVerified, Approved: true, At: at(0),
		},
	}
	if err := writeJSONAtomic(filepath.Join(s.grantDir(), forged.ID+".json"), forged); err != nil {
		t.Fatal(err)
	}

	_, err := s.Claim(context.Background(), agent("rogue"),
		SeatRequest{GrantID: forged.ID, Attended: true}, at(time.Minute))
	var unverified *ErrUnverified
	if !errors.As(err, &unverified) {
		t.Fatalf("a grant an agent wrote for itself, with an approval it wrote for itself, must still be refused — "+
			"the root of trust is the injected verifier, not the bytes on disk. got %v", err)
	}
	if v, _ := s.Status(at(time.Minute)); !v.Authority.Vacant {
		t.Fatal("the forged capability must not have moved the seat")
	}
}

// The record is HONEST about what authorized a seizure. An audit-grade attestation is
// recorded as audit-grade, forever, so nothing downstream can read it as proof a human
// was present.
func TestAttestationGradeIsRecordedVerbatim(t *testing.T) {
	s := newStore(t, WithVerifier(auditOnly()))
	mustClaim(t, s, agent("a"), at(0))
	mustTakeover(t, s, agent("b"), at(time.Minute))

	changes, _, _, err := s.History()
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range changes {
		if c.Kind != KindSeatTakeover {
			continue
		}
		if c.Authz == nil || c.Authz.Attestation == nil {
			t.Fatal("a takeover must carry the attestation that authorized it")
		}
		if c.Authz.Attestation.Grade != GradeAudit {
			t.Fatalf("the grade must be recorded verbatim, got %q", c.Authz.Attestation.Grade)
		}
		if c.Authz.Verified() {
			t.Fatal("an AUDIT-grade attestation must never report itself as verified — that is the whole distinction")
		}
		if c.Authz.Provenance != ProvenanceOperatorAssertion {
			t.Fatalf("provenance must be recorded verbatim, got %q", c.Authz.Provenance)
		}
		if c.Authz.Receipt != nil {
			t.Fatal("an operator assertion has no receipt, and must not claim one")
		}
		return
	}
	t.Fatal("no takeover found in the history")
}

// ─── evidence: reference vs verification ──────────────────────────────────────

// The single most load-bearing rule. An agent writes fluent, confident prose about
// work it did not do; the only defense that scales is to refuse to promote an
// unevidenced claim into a fact.
func TestUnevidencedSuccessProjectsAsUnknown(t *testing.T) {
	s := newStore(t)
	ep := mustClaim(t, s, agent("a"), at(0))
	e := mustRecord(t, s, Entry{
		Actor: agent("a"), Kind: KindEffect, Workstream: "api",
		Summary: "shipped it 🎉", Outcome: OutcomeSuccess, // …and nothing to point at
	}, ep, at(time.Minute))

	if e.Outcome != OutcomeSuccess {
		t.Fatal("the CLAIM must be recorded faithfully — this is an honest record of what was asserted")
	}
	if e.EffectiveOutcome() != OutcomeUnknown {
		t.Fatalf("but no view may BELIEVE it: effective outcome = %q, want unknown", e.EffectiveOutcome())
	}

	board, _, _ := s.Board()
	ws := board.Workstreams[0]
	if ws.Outcome != OutcomeUnknown || ws.Confidence != ConfidenceUnknown {
		t.Fatalf("the board must show unknown/unknown, got %s/%s", ws.Outcome, ws.Confidence)
	}
	if !board.Degraded {
		t.Fatal("the board's headline must not hide an unestablished outcome")
	}
}

// The rule the earlier revision was missing. A reference is a POINTER, not a check —
// and it is exactly as easy for a model to fabricate as the prose it accompanies.
func TestEvidencedSuccessIsAssertedNotVerified(t *testing.T) {
	s := newStore(t)
	ep := mustClaim(t, s, agent("a"), at(0))
	mustRecord(t, s, evidenced(agent("a"), "api", "migrated the schema"), ep, at(time.Minute))

	board, _, _ := s.Board()
	ws := board.Workstreams[0]
	if ws.Outcome != OutcomeSuccess {
		t.Fatalf("an evidenced success keeps its outcome, got %q", ws.Outcome)
	}
	if ws.Confidence != ConfidenceAsserted {
		t.Fatalf("a claim NOBODY CHECKED is asserted, never verified: got %q. "+
			"'command:go test ./...' records that an agent SAYS it ran the tests", ws.Confidence)
	}
	if board.Asserted != 1 {
		t.Fatalf("the board headline must count unverified claims, got %d", board.Asserted)
	}
}

// …and this is the only thing that closes the gap: a check a TRUSTED VERIFIER vouched for.
//
// Note what the store needs before this test can pass at all — WithVerificationVerifier. A
// store with no way to check a claim cannot promote one, and that is the fail-closed default
// rather than a gap in the fixture.
func TestVerificationPromotesAClaimToVerified(t *testing.T) {
	s := newStore(t, WithVerificationVerifier(sealing()))
	a := agent("a")
	ep := mustClaim(t, s, a, at(0))
	target := mustRecord(t, s, evidenced(a, "api", "migrated the schema"), ep, at(time.Minute))

	if _, err := s.Attest(context.Background(), a, ep, Verification{
		TargetSeq: target.Seq, TargetHash: target.Hash,
		Result: OutcomeSuccess, Method: "re-ran the suite on a clean checkout",
	}, []Evidence{{Kind: "command", Ref: "go test ./...", Digest: "sha256:" + strings.Repeat("a", 64)}}, at(2*time.Minute)); err != nil {
		t.Fatalf("Attest: %v", err)
	}

	board, _, _ := s.Board()
	ws := board.Workstreams[0]
	if ws.Confidence != ConfidenceVerified {
		t.Fatalf("an attested claim is verified, got %q", ws.Confidence)
	}
	if board.Asserted != 0 {
		t.Fatalf("a verified claim is no longer merely asserted, got %d asserted", board.Asserted)
	}
}

// A verification can move a claim BACKWARDS. Degradation travels one way.
func TestVerificationCanRefuteAClaim(t *testing.T) {
	s := newStore(t)
	a := agent("a")
	ep := mustClaim(t, s, a, at(0))
	target := mustRecord(t, s, evidenced(a, "api", "shipped"), ep, at(time.Minute))

	if _, err := s.Attest(context.Background(), a, ep, Verification{
		TargetSeq: target.Seq, TargetHash: target.Hash,
		Result: OutcomeFailed, Method: "the endpoint 502s",
	}, nil, at(2*time.Minute)); err != nil {
		t.Fatalf("Attest: %v", err)
	}

	board, _, _ := s.Board()
	ws := board.Workstreams[0]
	if ws.Confidence != ConfidenceRefuted || ws.Outcome != OutcomeFailed {
		t.Fatalf("a refuted success must degrade to failed/refuted, got %s/%s", ws.Outcome, ws.Confidence)
	}
}

// An attestation names the exact BYTES it vouched for, or it is an attestation of
// whatever ends up at that sequence number.
func TestVerificationBindsToTheTargetHash(t *testing.T) {
	s := newStore(t)
	a := agent("a")
	ep := mustClaim(t, s, a, at(0))
	target := mustRecord(t, s, evidenced(a, "api", "shipped"), ep, at(time.Minute))

	_, err := s.Attest(context.Background(), a, ep, Verification{
		TargetSeq: target.Seq, TargetHash: "sha256:" + strings.Repeat("f", 64),
		Result: OutcomeSuccess, Method: "trust me",
	}, nil, at(2*time.Minute))
	if err == nil || !strings.Contains(err.Error(), "exact bytes") {
		t.Fatalf("attesting to a hash that is not what is at that seq must be refused, got %v", err)
	}
}

func TestVerificationRequiresAMethod(t *testing.T) {
	s := newStore(t)
	a := agent("a")
	ep := mustClaim(t, s, a, at(0))
	target := mustRecord(t, s, evidenced(a, "api", "shipped"), ep, at(time.Minute))

	_, err := s.Attest(context.Background(), a, ep, Verification{TargetSeq: target.Seq, Result: OutcomeSuccess}, nil, at(2*time.Minute))
	if err == nil {
		t.Fatal("an unexplained 'I verified it' is the same trust-me claim it is supposed to replace")
	}
}

// ─── PROMOTION IS NOT SOMETHING A CALLER CAN WRITE ────────────────────────────
//
// The three tests below are one argument in three moves, and each move is a hole a
// previous revision actually shipped. In every case the agent supplies BOTH the claim and
// the credential that vouches for it — the trust-me claim the package exists to refuse,
// laundered one field sideways.

// MOVE ONE: PROSE. "verify --method 're-ran the suite on a clean checkout'", typed by an
// agent that did no such thing, used to produce a green VERIFIED row.
//
// The verification is RECORDED — the log keeps its full value, and a human can read what
// was claimed — and it promotes NOTHING.
func TestVerificationCannotPromoteOnProseAlone(t *testing.T) {
	s := newStore(t)
	a := agent("a")
	ep := mustClaim(t, s, a, at(0))
	target := mustRecord(t, s, evidenced(a, "api", "migrated the schema"), ep, at(time.Minute))

	e, err := s.Attest(context.Background(), a, ep, Verification{
		TargetSeq: target.Seq, TargetHash: target.Hash,
		Result: OutcomeSuccess, Method: "re-ran the suite on a clean checkout, all green",
	}, nil, at(2*time.Minute)) // …and not one byte anybody can check
	if err != nil {
		t.Fatalf("the check is still worth RECORDING — the log is not the thing being defended: %v", err)
	}
	if e.Verifies.Sealed() {
		t.Fatal("nothing sealed this: no verifier was injected, and a sentence is not a credential")
	}
	board, _, _ := s.Board()
	if c := board.Workstreams[0].Confidence; c != ConfidenceAsserted {
		t.Fatalf("a verification whose entire backing is a sentence promotes NOTHING — an agent that would claim "+
			"success it did not earn will write a method string it did not run. Got %q, want asserted", c)
	}
}

// MOVE TWO: A DIGEST. This one is subtler and it fooled the last revision completely.
//
// A digest proves INTEGRITY — these bytes did not change — and says NOTHING about whether
// a check ran. The agent writes any file it likes, hashes it, and attaches it. It need not
// even write the file: nothing rehashes the evidence at promotion time, so thirty-two
// arbitrary bytes typed at the prompt did just as well. Both are tested here, because
// "arbitrary digest" is the version an agent would actually reach for.
func TestArbitraryDigestEvidenceDoesNotPromote(t *testing.T) {
	s := newStore(t)
	a := agent("a")
	ep := mustClaim(t, s, a, at(0))
	target := mustRecord(t, s, evidenced(a, "api", "shipped"), ep, at(time.Minute))

	// Bytes an agent invented, pinned to a hash of nothing in particular.
	made_up := []Evidence{{
		Kind:   "file",
		Ref:    "/tmp/definitely-passing-tests.log",
		Digest: "sha256:" + strings.Repeat("f", 64),
	}}
	if _, err := s.Attest(context.Background(), a, ep, Verification{
		TargetSeq: target.Seq, TargetHash: target.Hash,
		Result: OutcomeSuccess, Method: "the suite passed, see the log",
	}, made_up, at(2*time.Minute)); err != nil {
		t.Fatalf("recording it is fine — believing it is not: %v", err)
	}

	board, _, _ := s.Board()
	if c := board.Workstreams[0].Confidence; c != ConfidenceAsserted {
		t.Fatalf("a digest is integrity, NOT a check: it proves some bytes did not change and says nothing about "+
			"whether anybody looked. Got %q, want asserted", c)
	}

	// And the same for a digest over bytes that really do exist: existing is not checking.
	if _, err := s.Attest(context.Background(), a, ep, Verification{
		TargetSeq: target.Seq, TargetHash: target.Hash,
		Result: OutcomeSuccess, Method: "rehash it yourself",
	}, digestEvidence(), at(3*time.Minute)); err != nil {
		t.Fatalf("Attest: %v", err)
	}
	board, _, _ = s.Board()
	if c := board.Workstreams[0].Confidence; c != ConfidenceAsserted {
		t.Fatalf("real bytes, honestly hashed, still do not establish that a CHECK RAN. Got %q, want asserted", c)
	}
}

// MOVE THREE: A CREDENTIAL THE CALLER FILLED IN ITSELF.
//
// Verification.Adapter used to be a public *Attestation with an Approved bool and a Grade
// string, described in its own doc comment as "a trusted verification adapter's
// attestation". Nothing made it one. Any caller could write Approved=true, Grade=verified,
// and the board promoted the strand.
//
// The field is gone, and its replacement cannot be forged the same way: a Seal is minted by
// the store from an injected verifier's answer, and Attest REFUSES a verification that
// arrives carrying one.
func TestCallerSuppliedSealIsRefused(t *testing.T) {
	s := newStore(t, WithVerificationVerifier(sealing()))
	a := agent("a")
	ep := mustClaim(t, s, a, at(0))
	target := mustRecord(t, s, evidenced(a, "api", "shipped"), ep, at(time.Minute))

	// The agent writes its own credential for its own claim. This is the whole attack.
	_, err := s.Attest(context.Background(), a, ep, Verification{
		TargetSeq: target.Seq, TargetHash: target.Hash,
		Result: OutcomeSuccess, Method: "trust me",
		Seal: &Seal{Verifier: "ci", Grade: GradeVerified, Approved: true, Token: "looks-official"},
	}, nil, at(2*time.Minute))

	var supplied *ErrSealSupplied
	if !errors.As(err, &supplied) {
		t.Fatalf("a caller-supplied seal must be REFUSED, loudly — quietly overwriting it would hide a caller whose "+
			"next line deserves to be read. Got %v", err)
	}

	board, _, _ := s.Board()
	if c := board.Workstreams[0].Confidence; c != ConfidenceAsserted {
		t.Fatalf("nothing was recorded, so nothing was promoted: got %q", c)
	}
}

// The same rule, enforced at the PROJECTION as well as at the write — and this is the lock
// that actually matters.
//
// The board is a function of the JOURNAL, and the journal is a FILE ON THE AGENT'S DISK.
// So a verification can reach the log without ever passing through Attest: the agent
// appends it, or rewrites it, or a future bug lets one through. A projection that trusted
// the Approved bool it read out of that file would be checking the forger's own work.
//
// It does not. It asks the injected verifier whether the seal is one IT issued, for THIS
// claim — and a verifier that never issued it says no.
func TestBoardRefusesAForgedSealInTheJournal(t *testing.T) {
	v := sealing()
	a := agent("a")
	target := Entry{
		Seq: 2, Hash: "sha256:" + strings.Repeat("b", 64), Kind: KindEffect, Actor: a,
		Workstream: "api", Summary: "shipped", Outcome: OutcomeSuccess,
		Evidence: []Evidence{{Kind: "command", Ref: "go test ./..."}},
	}
	// A verification hand-written straight into the journal, wearing every field that used
	// to matter: a method, a matching target hash, a success result, digest-bound evidence,
	// and a seal that SAYS it is approved and verified. Exactly what a forger would write.
	forged := Entry{
		Seq: 3, Kind: KindVerification, Actor: a, Workstream: "api", Summary: "verified",
		Outcome: OutcomeSuccess, Epoch: 1,
		Evidence: []Evidence{{Kind: "file", Ref: "/tmp/ci.log", Digest: "sha256:" + strings.Repeat("c", 64)}},
		Verifies: &Verification{
			TargetSeq: 2, TargetHash: target.Hash, Result: OutcomeSuccess, Method: "asked CI, honest",
			Seal: &Seal{Verifier: "test-seal-verifier", Grade: GradeVerified, Approved: true, Token: "not-a-real-token"},
		},
	}
	entries := []Entry{target, forged}

	// With the trusted verifier asked: it does not recognize the token, so nothing moves.
	board := ProjectBoard(entries, v.RecheckSeal)
	if c := board.Workstreams[0].Confidence; c != ConfidenceAsserted {
		t.Fatalf("a seal the verifier never issued must promote NOTHING, however green its fields look. Got %q", c)
	}

	// And with no verifier at all, a store cannot check anything, so it promotes nothing —
	// which is the fail-closed default, not a special case.
	board = ProjectBoard(entries, nil)
	if c := board.Workstreams[0].Confidence; c != ConfidenceAsserted {
		t.Fatalf("with nothing able to check a claim, the board must not report one as checked. Got %q", c)
	}
}

// A REAL seal promotes — otherwise none of the above is a design, it is just a refusal.
//
// And it promotes only where it BELONGS: lifting a genuine token off one verification and
// pasting it onto another is the obvious next move, and the token commits to the claim, so
// it fails there too.
func TestASealedVerificationPromotesAndCannotBeMoved(t *testing.T) {
	v := sealing()
	s := newStore(t, WithVerificationVerifier(v))
	a := agent("a")
	ep := mustClaim(t, s, a, at(0))
	target := mustRecord(t, s, evidenced(a, "api", "migrated the schema"), ep, at(time.Minute))

	e, err := s.Attest(context.Background(), a, ep, Verification{
		TargetSeq: target.Seq, TargetHash: target.Hash,
		Result: OutcomeSuccess, Method: "asked the CI system",
	}, nil, at(2*time.Minute))
	if err != nil {
		t.Fatalf("Attest: %v", err)
	}
	if !e.Verifies.Sealed() {
		t.Fatal("the trusted verifier vouched for it — the seal must be in the record")
	}
	if len(v.calls) != 1 {
		t.Fatalf("the transition must actually ASK the root of trust, got %d calls", len(v.calls))
	}
	board, _, _ := s.Board()
	if c := board.Workstreams[0].Confidence; c != ConfidenceVerified {
		t.Fatalf("a seal a trusted verifier issued and still recognizes IS what verified means. Got %q", c)
	}

	// Now STEAL it: a genuine token, lifted off this verification and pasted onto a claim
	// nobody checked. The token commits to the claim, so the verifier does not recognize it
	// there — which is the property that makes a seal a seal rather than a password.
	other := Entry{
		Seq: 4, Hash: "sha256:" + strings.Repeat("d", 64), Kind: KindEffect, Actor: a,
		Workstream: "billing", Summary: "shipped billing too", Outcome: OutcomeSuccess,
		Evidence: []Evidence{{Kind: "command", Ref: "go test ./..."}},
	}
	pasted := Entry{
		Seq: 5, Kind: KindVerification, Actor: a, Workstream: "billing", Epoch: ep,
		Outcome: OutcomeSuccess, Summary: "verified",
		Verifies: &Verification{
			TargetSeq: 4, TargetHash: other.Hash, Result: OutcomeSuccess, Method: "asked the CI system",
			Seal: e.Verifies.Seal, // ← a genuine token, on somebody else's claim
		},
	}
	board = ProjectBoard([]Entry{other, pasted}, v.RecheckSeal)
	if c := board.Workstreams[0].Confidence; c != ConfidenceAsserted {
		t.Fatalf("a genuine token pasted onto a DIFFERENT claim must not promote it — the token commits to the "+
			"claim. Got %q", c)
	}
}

// A verifier that goes and looks and finds the claim FALSE is not the same as one that
// cannot say. The first is a refutation, and recording it as a success would put a claim
// the one trusted party actively refuted into the permanent record wearing a success label.
func TestARefutingVerifierBlocksASuccessVerification(t *testing.T) {
	s := newStore(t, WithVerificationVerifier(&sealingVerifier{refute: true}))
	a := agent("a")
	ep := mustClaim(t, s, a, at(0))
	target := mustRecord(t, s, evidenced(a, "api", "shipped"), ep, at(time.Minute))

	_, err := s.Attest(context.Background(), a, ep, Verification{
		TargetSeq: target.Seq, TargetHash: target.Hash,
		Result: OutcomeSuccess, Method: "asked CI",
	}, digestEvidence(), at(2*time.Minute))

	var refuted *ErrRefuted
	if !errors.As(err, &refuted) {
		t.Fatalf("the trusted verifier said the claim is FALSE — recording it as a success is not available. Got %v", err)
	}
	if !strings.Contains(err.Error(), "--result failed") {
		t.Fatalf("and the refusal must say what IS available, got %v", err)
	}
}

// A verifier that cannot speak to a claim leaves it exactly where a host with no verifier
// would: recorded, unsealed, asserted. That is the floor, not a new hole — and it is what
// keeps a narrow verifier (a CI adapter that knows nothing about a manual check) from
// blocking every honest record on the host.
func TestAVerifierThatCannotEstablishAClaimRecordsItUnsealed(t *testing.T) {
	s := newStore(t, WithVerificationVerifier(&sealingVerifier{admits: map[uint64]bool{}}))
	a := agent("a")
	ep := mustClaim(t, s, a, at(0))
	target := mustRecord(t, s, evidenced(a, "api", "shipped"), ep, at(time.Minute))

	e, err := s.Attest(context.Background(), a, ep, Verification{
		TargetSeq: target.Seq, TargetHash: target.Hash,
		Result: OutcomeSuccess, Method: "asked CI, which had never heard of this",
	}, digestEvidence(), at(2*time.Minute))
	if err != nil {
		t.Fatalf("a verifier with nothing to say must not block the record: %v", err)
	}
	if e.Verifies.Sealed() {
		t.Fatal("…and must not seal it either")
	}
	board, _, _ := s.Board()
	if c := board.Workstreams[0].Confidence; c != ConfidenceAsserted {
		t.Fatalf("unsealed is asserted, got %q", c)
	}
}

// Degradation travels one way, and this is the direction it travels in: REFUTING a claim
// needs no credential at all. We demand evidence to become more confident, never to
// become less — the cost of a false "verified" is unbounded, and the cost of a false
// "refuted" is a second look.
func TestRefutationNeedsNoEvidence(t *testing.T) {
	s := newStore(t)
	a := agent("a")
	ep := mustClaim(t, s, a, at(0))
	target := mustRecord(t, s, evidenced(a, "api", "shipped"), ep, at(time.Minute))

	if _, err := s.Attest(context.Background(), a, ep, Verification{
		TargetSeq: target.Seq, TargetHash: target.Hash,
		Result: OutcomeFailed, Method: "the endpoint 502s",
	}, nil, at(2*time.Minute)); err != nil {
		t.Fatalf("doubt is free: a refutation must not require digest-bound evidence: %v", err)
	}
	board, _, _ := s.Board()
	if c := board.Workstreams[0].Confidence; c != ConfidenceRefuted {
		t.Fatalf("the refutation must land, got %q", c)
	}
}

func TestFailureWithoutEvidenceStaysFailure(t *testing.T) {
	s := newStore(t)
	ep := mustClaim(t, s, agent("a"), at(0))
	e := mustRecord(t, s, Entry{
		Actor: agent("a"), Kind: KindEffect, Workstream: "api",
		Summary: "the migration blew up", Outcome: OutcomeFailed,
	}, ep, at(time.Minute))

	if e.EffectiveOutcome() != OutcomeFailed {
		t.Fatalf("degradation travels ONE WAY — an unevidenced failure is still a failure, got %q", e.EffectiveOutcome())
	}
}

func TestClosedWithoutEvidenceIsClosedAndUnknown(t *testing.T) {
	s := newStore(t)
	a := agent("a")
	ep := mustClaim(t, s, a, at(0))
	if _, err := s.OpenWorkstream(a, ep, "api", "the api migration", at(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CloseWorkstream(a, ep, "api", "all done!", OutcomeSuccess, nil, at(2*time.Minute)); err != nil {
		t.Fatal(err)
	}

	board, _, _ := s.Board()
	ws := board.Workstreams[0]
	if ws.State != WorkstreamClosed {
		t.Fatalf("state must be closed, got %q", ws.State)
	}
	if ws.Outcome != OutcomeUnknown {
		t.Fatalf("'closed' and 'verified done' are different facts: outcome = %q", ws.Outcome)
	}
	if ws.Lane != LaneDone {
		t.Fatalf("a closed strand sits in the done lane, got %q", ws.Lane)
	}
}

// ─── the Kanban board ─────────────────────────────────────────────────────────

func TestBoardIsAKanbanDerivedPurelyFromTheJournal(t *testing.T) {
	s := newStore(t)
	a := agent("a")
	ep := mustClaim(t, s, a, at(0))

	if _, err := s.OpenWorkstream(a, ep, "api", "the api migration", at(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.UpdateWorkstream(a, ep, "api", WorkstreamUpdate{
		Lane: LaneInProgress, Priority: P0, Owner: "qiangli",
		Agents: []string{"claude-opus"}, NextAction: "run the race gate",
		NextAt: at(time.Hour).Format(time.RFC3339),
		Links:  []Link{{Kind: "issue", Ref: "88"}, {Kind: "weave", Ref: "run-88"}},
	}, at(2*time.Minute)); err != nil {
		t.Fatal(err)
	}

	board, _, _ := s.Board()
	ws := board.Workstreams[0]
	if ws.Lane != LaneInProgress || ws.Priority != P0 || ws.Owner != "qiangli" {
		t.Fatalf("the Kanban fields must fold from the journal, got lane=%s pri=%s owner=%s", ws.Lane, ws.Priority, ws.Owner)
	}
	if ws.NextAction != "run the race gate" || ws.NextAt.IsZero() {
		t.Fatalf("next action/checkpoint must fold, got %q / %v", ws.NextAction, ws.NextAt)
	}
	if len(ws.Links) != 2 || len(ws.Agents) != 1 {
		t.Fatalf("links and agents must fold, got %v / %v", ws.Links, ws.Agents)
	}

	// A blocked strand shows as BLOCKED regardless of the lane anyone typed — a board
	// that lets you park a blocked item in "in-progress" hides the only thing worth
	// looking at.
	if _, err := s.UpdateWorkstream(a, ep, "api", WorkstreamUpdate{Blockers: []string{"waiting on review of #412"}}, at(3*time.Minute)); err != nil {
		t.Fatal(err)
	}
	board, _, _ = s.Board()
	if board.Workstreams[0].Lane != LaneBlocked || board.Blocked != 1 {
		t.Fatalf("live blockers must force the blocked lane, got %q (blocked=%d)", board.Workstreams[0].Lane, board.Blocked)
	}

	// And unblocking is as much of an event as blocking.
	if _, err := s.UpdateWorkstream(a, ep, "api", WorkstreamUpdate{Clear: []string{"blockers"}}, at(4*time.Minute)); err != nil {
		t.Fatal(err)
	}
	board, _, _ = s.Board()
	if board.Workstreams[0].Lane != LaneInProgress || board.Blocked != 0 {
		t.Fatalf("clearing the blockers must return the strand to its lane, got %q", board.Workstreams[0].Lane)
	}
}

func TestBoardRejectsAnInvalidLaneOrPriority(t *testing.T) {
	s := newStore(t)
	a := agent("a")
	ep := mustClaim(t, s, a, at(0))
	if _, err := s.UpdateWorkstream(a, ep, "api", WorkstreamUpdate{Lane: "wherever"}, at(time.Minute)); err == nil {
		t.Fatal("an invented lane must be refused, not silently stored")
	}
	if _, err := s.UpdateWorkstream(a, ep, "api", WorkstreamUpdate{Priority: "urgent"}, at(time.Minute)); err == nil {
		t.Fatal("an invented priority must be refused")
	}
}

func TestAnEvidencedEntrySettlesAnEarlierUnknown(t *testing.T) {
	s := newStore(t)
	a := agent("a")
	ep := mustClaim(t, s, a, at(0))
	mustRecord(t, s, Entry{Actor: a, Kind: KindEffect, Workstream: "api",
		Summary: "probably fine", Outcome: OutcomeSuccess}, ep, at(time.Minute))
	mustRecord(t, s, evidenced(a, "api", "actually verified the migration"), ep, at(2*time.Minute))

	board, _, _ := s.Board()
	ws := board.Workstreams[0]
	if ws.Outcome != OutcomeSuccess || ws.Confidence != ConfidenceAsserted {
		t.Fatalf("a later evidenced entry settles the earlier unknown, got %s/%s", ws.Outcome, ws.Confidence)
	}
	if board.Degraded {
		t.Fatal("an early unknown that a later entry settled is history, not a live problem")
	}
}

// ─── the journal as the only authority ────────────────────────────────────────

// The whole recovery story: seat.json is a cache, and the journal survives without it.
func TestAuthoritySurvivesLosingTheLivenessRecord(t *testing.T) {
	s := newStore(t)
	ep := mustClaim(t, s, agent("crashed"), at(0))
	mustRecord(t, s, evidenced(agent("crashed"), "api", "did real work"), ep, at(time.Minute))

	// The heartbeat file is gone — a crash, a cleanup script, a hostile `rm`.
	if err := os.Remove(s.seatPath()); err != nil {
		t.Fatal(err)
	}

	v, err := s.Status(at(2 * time.Minute))
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	// AUTHORITY survives — replayed from the journal.
	if v.Authority.Vacant || !SameHolder(v.Authority.Holder, agent("crashed")) || v.Authority.Epoch != ep {
		t.Fatalf("authority must replay from the journal without seat.json, got %+v", v.Authority)
	}
	// LIVENESS does not, and it is honest about it.
	if v.Liveness != LivenessUnknown {
		t.Fatalf("with no heartbeat record, liveness is UNKNOWN, got %q", v.Liveness)
	}
	// And the holder can restore it — the journal still says the seat is theirs.
	if err := s.Heartbeat(agent("crashed"), ep, at(3*time.Minute)); err != nil {
		t.Fatalf("the holder must be able to rebuild the liveness record from the journal: %v", err)
	}
	v, _ = s.Status(at(4 * time.Minute))
	if v.Liveness != LivenessLive {
		t.Fatalf("after heartbeating, liveness is live again, got %q", v.Liveness)
	}
}

func TestSeatLifecycleTouchesNoRepository(t *testing.T) {
	// A steward moves a MANDATE, not a checkout. If claiming the seat ever starts
	// touching a repository, `steward` and `handoff` have quietly become the same verb —
	// and the distinction is the whole reason both exist.
	repo := t.TempDir()
	if err := os.MkdirAll(filepath.Join(repo, ".git"), 0o700); err != nil {
		t.Fatal(err)
	}
	head := filepath.Join(repo, ".git", "HEAD")
	if err := os.WriteFile(head, []byte("ref: refs/heads/main\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	tracked := filepath.Join(repo, "file.txt")
	if err := os.WriteFile(tracked, []byte("original\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	before := snapshot(t, repo)

	s := newStore(t)
	a := agent("a")
	ep := mustClaim(t, s, a, at(0))
	mustRecord(t, s, evidenced(a, "api", "worked in that repo"), ep, at(time.Minute))
	if err := s.Release(a, ep, "done", at(2*time.Minute)); err != nil {
		t.Fatal(err)
	}
	ep2 := mustClaim(t, s, agent("b"), at(3*time.Minute))
	if _, err := s.Checkpoint(agent("b"), ep2, "", at(4*time.Minute)); err != nil {
		t.Fatal(err)
	}

	if after := snapshot(t, repo); after != before {
		t.Fatalf("the seat lifecycle must not touch a repository.\nbefore: %s\nafter:  %s", before, after)
	}
}

func snapshot(t *testing.T, dir string) string {
	t.Helper()
	var sb strings.Builder
	err := filepath.Walk(dir, func(p string, fi os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(dir, p)
		sb.WriteString(rel + "|")
		if !fi.IsDir() {
			b, err := os.ReadFile(p)
			if err != nil {
				return err
			}
			sb.WriteString(digestOf(b))
		}
		sb.WriteString("\n")
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return sb.String()
}

func TestReplayDetectsAnAlteredEntry(t *testing.T) {
	s := newStore(t)
	a := agent("a")
	ep := mustClaim(t, s, a, at(0))
	mustRecord(t, s, evidenced(a, "api", "the truth"), ep, at(time.Minute))
	mustRecord(t, s, evidenced(a, "api", "more truth"), ep, at(2*time.Minute))

	raw := journalBytes(t, s)
	tampered := strings.Replace(string(raw), "the truth", "a lie!!!!", 1)
	if err := os.WriteFile(s.journalPath(), []byte(tampered), 0o600); err != nil {
		t.Fatal(err)
	}

	rep, _ := s.Replay()
	if !rep.Corrupt {
		t.Fatal("an altered entry must break the chain — that is what the chain is FOR")
	}
	if rep.CorruptKind != CorruptInvalid {
		t.Fatalf("tampering is not a torn write: kind = %q", rep.CorruptKind)
	}
	if len(rep.Entries) != 1 {
		t.Fatalf("the valid prefix before the alteration survives: got %d entries", len(rep.Entries))
	}
}

func TestReplayChainsFromGenesis(t *testing.T) {
	s := newStore(t)
	a := agent("a")
	ep := mustClaim(t, s, a, at(0))
	mustRecord(t, s, evidenced(a, "api", "one"), ep, at(time.Minute))

	rep, _ := s.Replay()
	if rep.Entries[0].PrevHash != genesis {
		t.Fatalf("the first entry must chain from the public genesis root, got %q", rep.Entries[0].PrevHash)
	}
	prev := genesis
	for i, e := range rep.Entries {
		if e.PrevHash != prev {
			t.Fatalf("entry %d does not link to its predecessor", i)
		}
		if e.computeHash(prev) != e.Hash {
			t.Fatalf("entry %d's hash does not verify", i)
		}
		prev = e.Hash
	}
}

func TestEvidenceOrderDoesNotAffectTheHash(t *testing.T) {
	e1 := Entry{Actor: agent("a"), Epoch: 1, Kind: KindEffect, Summary: "x", Evidence: []Evidence{
		{Kind: "command", Ref: "go test"}, {Kind: "commit", Ref: "abc"},
	}}
	e2 := Entry{Actor: agent("a"), Epoch: 1, Kind: KindEffect, Summary: "x", Evidence: []Evidence{
		{Kind: "commit", Ref: "abc"}, {Kind: "command", Ref: "go test"},
	}}
	sortEvidence(e1.Evidence)
	sortEvidence(e2.Evidence)
	if e1.computeHash(genesis) != e2.computeHash(genesis) {
		t.Fatal("the same evidence in a different flag order must hash identically")
	}
}

// ─── transcripts are optional by contract ─────────────────────────────────────

func TestTranscriptDeletionDoesNotAffectProjections(t *testing.T) {
	s := newStore(t)
	a := agent("a")
	ep := mustClaim(t, s, a, at(0))
	if _, err := s.OpenWorkstream(a, ep, "api", "the migration", at(time.Minute)); err != nil {
		t.Fatal(err)
	}
	mustRecord(t, s, evidenced(a, "api", "did the thing"), ep, at(2*time.Minute))
	if _, err := s.Decide(a, ep, "api", "drop v1", "no callers in 90d", nil, at(3*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Transcript(a, ep, "api", "how we got here", strings.NewReader("a long conversation"), at(4*time.Minute)); err != nil {
		t.Fatal(err)
	}

	boardBefore, _, _ := s.Board()
	ckBefore := ProjectCheckpoint(mustReplay(t, s).Entries, 0, s.sealChecker())

	// Delete every artifact byte on the host.
	if err := os.RemoveAll(s.transcriptDir()); err != nil {
		t.Fatal(err)
	}

	boardAfter, _, _ := s.Board()
	ckAfter := ProjectCheckpoint(mustReplay(t, s).Entries, 0, s.sealChecker())

	if boardBefore.Digest != boardAfter.Digest {
		t.Fatal("deleting a transcript changed the board — transcripts are NOT authoritative, and this is the test that says so")
	}
	if ckBefore.Board.Digest != ckAfter.Board.Digest || ckBefore.JournalDigest != ckAfter.JournalDigest {
		t.Fatal("deleting a transcript changed a checkpoint projection")
	}
}

func mustReplay(t *testing.T, s *Store) *Replay {
	t.Helper()
	rep, err := s.Replay()
	if err != nil {
		t.Fatal(err)
	}
	return rep
}

func TestTamperedArtifactIsFlagged(t *testing.T) {
	s := newStore(t)
	a := agent("a")
	ep := mustClaim(t, s, a, at(0))
	e, err := s.Transcript(a, ep, "api", "the conversation", strings.NewReader("original"), at(time.Minute))
	if err != nil {
		t.Fatal(err)
	}

	path := filepath.Join(s.dir, filepath.FromSlash(e.Artifact.Path))
	if err := os.WriteFile(path, []byte("REWRITTEN"), 0o600); err != nil {
		t.Fatal(err)
	}

	r, err := s.Reconcile(context.Background(), at(2*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if len(r.TamperedArtifacts) != 1 {
		t.Fatal("an artifact whose bytes no longer match its digest is a LIE, and must be flagged")
	}
	if r.Health != HealthUnknown {
		t.Fatalf("tampering degrades health to unknown, got %q", r.Health)
	}
	// An ABSENT artifact, by contrast, is merely a gap.
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	r, _ = s.Reconcile(context.Background(), at(3*time.Minute))
	if len(r.MissingArtifacts) != 1 || len(r.TamperedArtifacts) != 0 {
		t.Fatal("a missing artifact is a gap, not a lie")
	}
	if r.Health == HealthUnknown {
		t.Fatal("a missing OPTIONAL artifact must not degrade health to unknown")
	}
}

// ─── reconcile ────────────────────────────────────────────────────────────────

// Reconcile must not claim it compared reality when it compared nothing.
func TestReconcileWithNoAdapterSaysItCheckedNothing(t *testing.T) {
	s := newStore(t)
	a := agent("a")
	ep := mustClaim(t, s, a, at(0))
	mustRecord(t, s, evidenced(a, "api", "shipped"), ep, at(time.Minute))

	r, err := s.Reconcile(context.Background(), at(2*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if r.RealityCompared {
		t.Fatal("with no adapter, NOTHING was compared against reality — saying otherwise is the most dangerous lie in the system")
	}
	if !strings.Contains(r.RealityNote, "NOTHING was compared") {
		t.Fatalf("the report must say so in prose, got %q", r.RealityNote)
	}
	if len(r.Asserted) != 1 {
		t.Fatalf("an unchecked reference must be listed as asserted, got %d", len(r.Asserted))
	}
	if r.Health != HealthDegraded {
		t.Fatalf("a claim nobody checked is not a clean bill of health, got %q", r.Health)
	}
}

// stubObserver is a host-supplied adapter that actually went and looked.
type stubObserver struct {
	name string
	obs  []Observation
	err  error
}

func (o stubObserver) Name() string { return o.name }
func (o stubObserver) Observe(context.Context, []Entry) ([]Observation, error) {
	return o.obs, o.err
}

func TestReconcileWithAnAdapterReportsWhatItFound(t *testing.T) {
	s := newStore(t)
	a := agent("a")
	ep := mustClaim(t, s, a, at(0))
	e := mustRecord(t, s, evidenced(a, "api", "shipped"), ep, at(time.Minute))

	r, err := s.Reconcile(context.Background(), at(2*time.Minute), stubObserver{
		name: "git",
		obs:  []Observation{{Seq: e.Seq, TargetHash: e.Hash, Result: OutcomeSuccess, Detail: "commit is on main"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !r.RealityCompared || len(r.Observations) != 1 {
		t.Fatal("an adapter that returned observations means reality WAS compared")
	}
	if r.Observations[0].Observer != "git" {
		t.Fatalf("the observation must name its adapter, got %q", r.Observations[0].Observer)
	}
	// …and an observation is still not a verification until it is RECORDED as one.
	if len(r.Asserted) != 1 {
		t.Fatal("an observation the steward has not journaled does not promote the claim")
	}
}

func TestReconcileReportsAFailedAdapterHonestly(t *testing.T) {
	s := newStore(t)
	mustClaim(t, s, agent("a"), at(0))
	r, err := s.Reconcile(context.Background(), at(time.Minute), stubObserver{name: "ci", err: errors.New("network is down")})
	if err != nil {
		t.Fatal(err)
	}
	if r.RealityCompared {
		t.Fatal("an adapter that FAILED compared nothing")
	}
	if len(r.ObserverErrors) != 1 || !strings.Contains(r.RealityNote, "FAILED") {
		t.Fatalf("the failure must be surfaced, got %+v / %q", r.ObserverErrors, r.RealityNote)
	}
}

func TestReconcileReportsUnknownForADamagedJournal(t *testing.T) {
	s := newStore(t)
	a := agent("a")
	ep := mustClaim(t, s, a, at(0))
	mustRecord(t, s, evidenced(a, "api", "real work"), ep, at(time.Minute))
	appendRaw(t, s, "{ not json at all")

	r, err := s.Reconcile(context.Background(), at(2*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if r.Health != HealthUnknown {
		t.Fatalf("a damaged RECORD is unknown, not merely degraded: got %q", r.Health)
	}
	if r.JournalEntries != 2 {
		t.Fatalf("the valid entries before the damage are still counted: got %d", r.JournalEntries)
	}
	if r.CorruptTail == "" {
		t.Fatal("the reconciliation must say where the record runs out")
	}
}

func TestRecordedReconciliationMirrorsItsVerdict(t *testing.T) {
	s := newStore(t)
	a := agent("a")
	ep := mustClaim(t, s, a, at(0))
	mustRecord(t, s, Entry{Actor: a, Kind: KindEffect, Workstream: "api",
		Summary: "done!", Outcome: OutcomeSuccess}, ep, at(time.Minute))

	r, _ := s.Reconcile(context.Background(), at(2*time.Minute))
	e, err := s.RecordReconciliation(a, ep, r, at(2*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if e.Outcome != OutcomeDegraded {
		t.Fatalf("a reconciliation that found unproven claims may never be replayed as a success, got %q", e.Outcome)
	}
	if !strings.Contains(e.Summary, "reality NOT compared") {
		t.Fatalf("the recorded entry must say whether reality was actually checked, got %q", e.Summary)
	}
}

func TestReconcileIsOKOnlyWhenEverythingHasBeenChecked(t *testing.T) {
	// "Checked" means a trusted verifier went and looked. On a host with none, health can
	// never reach ok — which is the honest verdict, not a missing feature.
	s := newStore(t, WithVerificationVerifier(sealing()))
	a := agent("a")
	ep := mustClaim(t, s, a, at(0))
	target := mustRecord(t, s, evidenced(a, "api", "shipped"), ep, at(time.Minute))

	r, _ := s.Reconcile(context.Background(), at(2*time.Minute))
	if r.Health != HealthDegraded {
		t.Fatalf("an unchecked claim is degraded, got %q", r.Health)
	}

	if _, err := s.Attest(context.Background(), a, ep, Verification{
		TargetSeq: target.Seq, TargetHash: target.Hash, Result: OutcomeSuccess,
		Method: "re-ran it and kept the log",
	}, digestEvidence(), at(3*time.Minute)); err != nil {
		t.Fatal(err)
	}
	r, _ = s.Reconcile(context.Background(), at(4*time.Minute))
	if r.Health != HealthOK {
		t.Fatalf("once every claim is attested, health is ok: got %q (%d unproven, %d asserted)",
			r.Health, len(r.Unproven), len(r.Asserted))
	}
}

// ─── checkpoints ──────────────────────────────────────────────────────────────

func TestCheckpointProjectionIsPure(t *testing.T) {
	s := newStore(t)
	a := agent("a")
	ep := mustClaim(t, s, a, at(0))
	mustRecord(t, s, evidenced(a, "api", "one"), ep, at(time.Minute))
	mustRecord(t, s, evidenced(a, "web", "two"), ep, at(2*time.Minute))
	entries := mustReplay(t, s).Entries

	first := ProjectCheckpoint(entries, 0, nil)
	for range 5 {
		if got := ProjectCheckpoint(entries, 0, nil); got.Board.Digest != first.Board.Digest {
			t.Fatal("the projection is not pure — same entries must always yield the same board")
		}
	}
}

func TestCheckpointIsReproducibleAndVerifiable(t *testing.T) {
	s := newStore(t)
	a := agent("a")
	ep := mustClaim(t, s, a, at(0))
	mustRecord(t, s, evidenced(a, "api", "work"), ep, at(time.Minute))

	ck, err := s.Checkpoint(a, ep, "before the risky bit", at(2*time.Minute))
	if err != nil {
		t.Fatalf("Checkpoint: %v", err)
	}
	v, err := s.VerifyCheckpoint(ck)
	if err != nil {
		t.Fatal(err)
	}
	if !v.Reproducible {
		t.Fatalf("a fresh checkpoint must re-derive: %s", v.Reason)
	}

	// And the JOURNAL remembers it was taken, even if the file is deleted.
	if err := os.RemoveAll(s.checkpointDir()); err != nil {
		t.Fatal(err)
	}
	_, cks, _, err := s.History()
	if err != nil {
		t.Fatal(err)
	}
	if len(cks) != 1 || cks[0].ID != ck.ID {
		t.Fatal("the file is a cache; the journal entry is the memory")
	}
}

func TestCheckpointStopsVerifyingIfHistoryIsRewritten(t *testing.T) {
	s := newStore(t)
	a := agent("a")
	ep := mustClaim(t, s, a, at(0))
	mustRecord(t, s, evidenced(a, "api", "work"), ep, at(time.Minute))
	ck, err := s.Checkpoint(a, ep, "", at(2*time.Minute))
	if err != nil {
		t.Fatal(err)
	}

	// Rewrite history beneath it.
	raw := string(journalBytes(t, s))
	raw = strings.Replace(raw, `"work"`, `"different work"`, 1)
	if err := os.WriteFile(s.journalPath(), []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}

	v, err := s.VerifyCheckpoint(ck)
	if err != nil {
		t.Fatal(err)
	}
	if v.Reproducible {
		t.Fatal("a checkpoint over rewritten history must STOP verifying — that is the whole point of the receipt")
	}
}

func TestCheckpointCarriesUnprovenClaimsForward(t *testing.T) {
	s := newStore(t)
	a := agent("a")
	ep := mustClaim(t, s, a, at(0))
	mustRecord(t, s, Entry{Actor: a, Kind: KindEffect, Workstream: "api",
		Summary: "done!", Outcome: OutcomeSuccess}, ep, at(time.Minute)) // unevidenced
	mustRecord(t, s, evidenced(a, "web", "shipped"), ep, at(2*time.Minute)) // asserted

	ck, err := s.Checkpoint(a, ep, "", at(3*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if len(ck.Unresolved) != 1 {
		t.Fatalf("a checkpoint that dropped its unknowns would look like a clean bill of health; got %v", ck.Unresolved)
	}
	if len(ck.Asserted) != 1 {
		t.Fatalf("it must also carry forward what was merely ASSERTED; got %v", ck.Asserted)
	}
}

// ─── follow / log / history ───────────────────────────────────────────────────

func TestFollowStreamsNewEntriesOnly(t *testing.T) {
	s := newStore(t)
	a := agent("a")
	ep := mustClaim(t, s, a, at(0))
	mustRecord(t, s, evidenced(a, "api", "before the follow"), ep, at(time.Minute))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	got := make(chan Entry, 8)
	done := make(chan error, 1)
	go func() {
		done <- s.Follow(ctx, Filter{}, 5*time.Millisecond, func(e Entry) error {
			got <- e
			return nil
		})
	}()

	time.Sleep(20 * time.Millisecond)
	mustRecord(t, s, evidenced(a, "api", "after the follow"), ep, at(2*time.Minute))

	select {
	case e := <-got:
		if e.Summary != "after the follow" {
			t.Fatalf("follow must start from the head, not replay the backlog; got %q", e.Summary)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("follow delivered nothing")
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("a cancelled follow is how follow ENDS, not how it fails: %v", err)
	}
}

func TestDegradedOnlyFilterSurfacesTheUnproven(t *testing.T) {
	s := newStore(t)
	a := agent("a")
	ep := mustClaim(t, s, a, at(0))
	mustRecord(t, s, evidenced(a, "api", "evidenced"), ep, at(time.Minute))
	mustRecord(t, s, Entry{Actor: a, Kind: KindEffect, Workstream: "api",
		Summary: "unevidenced", Outcome: OutcomeSuccess}, ep, at(2*time.Minute))

	entries, _, err := s.Log(Filter{DegradedOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Summary != "unevidenced" {
		t.Fatalf("--degraded is the 'what do I NOT know' query; got %d entries", len(entries))
	}
}

func TestHistoryReconstructsTheAuthorityLadder(t *testing.T) {
	s := newStore(t)
	e1 := mustClaim(t, s, agent("first"), at(0))
	if err := s.Release(agent("first"), e1, "handing off", at(time.Minute)); err != nil {
		t.Fatal(err)
	}
	mustClaim(t, s, agent("second"), at(2*time.Minute))
	mustTakeover(t, s, agent("third"), at(3*time.Minute))

	changes, _, _, err := s.History()
	if err != nil {
		t.Fatal(err)
	}
	want := []Kind{KindSeatClaimed, KindSeatReleased, KindSeatClaimed, KindSeatTakeover}
	if len(changes) != len(want) {
		t.Fatalf("expected %d seat events, got %d", len(want), len(changes))
	}
	for i, k := range want {
		if changes[i].Kind != k {
			t.Fatalf("event %d: want %s, got %s", i, k, changes[i].Kind)
		}
	}
	if changes[3].Authz == nil || changes[3].Authz.Actor != "qiangli" {
		t.Fatal("a takeover must record the capability it was performed under")
	}
	if changes[3].Epoch <= changes[2].Epoch {
		t.Fatal("the epoch ladder must be monotonic across the whole history")
	}
}

// ─── misc invariants ──────────────────────────────────────────────────────────

func TestEveryArtifactIsSchemaVersioned(t *testing.T) {
	s := newStore(t)
	a := agent("a")
	ep := mustClaim(t, s, a, at(0))
	mustRecord(t, s, evidenced(a, "api", "x"), ep, at(time.Minute))
	ck, err := s.Checkpoint(a, ep, "", at(2*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	g := mustGrant(t, s, agent("b"), ActionTakeover, at(3*time.Minute))

	if ck.SchemaVersion != SchemaVersion || g.SchemaVersion != SchemaVersion {
		t.Fatal("every stored artifact carries the schema it was written under")
	}
	var seat Seat
	if found, err := readJSON(s.seatPath(), &seat); err != nil || !found {
		t.Fatal("seat file must exist")
	}
	if seat.SchemaVersion != SchemaVersion {
		t.Fatal("the seat cache must be schema-versioned — an unversioned cache cannot be validated")
	}
	for _, e := range mustReplay(t, s).Entries {
		if e.Schema != SchemaVersion {
			t.Fatalf("entry seq %d has schema %q", e.Seq, e.Schema)
		}
	}
}

func TestParseEvidence(t *testing.T) {
	cases := []struct {
		in         string
		kind, ref  string
		digest     string
		wantDigest bool
	}{
		{in: "command:go test ./...", kind: "command", ref: "go test ./..."},
		{in: "commit:de6485c", kind: "commit", ref: "de6485c"},
		{in: "file:/tmp/o.log#sha256:abc", kind: "file", ref: "/tmp/o.log", digest: "sha256:abc", wantDigest: true},
		{in: "just some prose", kind: "note", ref: "just some prose"},
		// An unknown prefix is NOT silently promoted to a kind — it stays a note, whole.
		{in: "unknownkind:x", kind: "note", ref: "unknownkind:x"},
	}
	for _, c := range cases {
		ev, err := ParseEvidence(c.in)
		if err != nil {
			t.Fatalf("ParseEvidence(%q): %v", c.in, err)
		}
		if ev.Kind != c.kind || ev.Ref != c.ref {
			t.Fatalf("ParseEvidence(%q) = %s:%s, want %s:%s", c.in, ev.Kind, ev.Ref, c.kind, c.ref)
		}
		if c.wantDigest && ev.Digest != c.digest {
			t.Fatalf("ParseEvidence(%q) digest = %q, want %q", c.in, ev.Digest, c.digest)
		}
		if c.wantDigest && !ev.DigestBound() {
			t.Fatalf("ParseEvidence(%q) must be digest-bound", c.in)
		}
	}
	if _, err := ParseEvidence("   "); err == nil {
		t.Fatal("empty evidence must be rejected")
	}
}

func TestSameHolderMatchesTheLogicalAgentNotThePID(t *testing.T) {
	a := principal.Ref{Name: "agent", Host: "h", Episode: "ep-1"}
	b := principal.Ref{Name: "agent", Host: "h", Episode: "ep-1"}
	if !SameHolder(a, b) {
		t.Fatal("one logical agent runs many processes; they are the same holder")
	}
	c := principal.Ref{Name: "other", Host: "h", Episode: "ep-2"}
	if SameHolder(a, c) {
		t.Fatal("different agents are different holders")
	}
}

// appendRaw writes raw bytes to the end of the journal, simulating damage.
func appendRaw(t *testing.T, s *Store, raw string) {
	t.Helper()
	f, err := os.OpenFile(s.journalPath(), os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.WriteString(raw); err != nil {
		t.Fatal(err)
	}
}
