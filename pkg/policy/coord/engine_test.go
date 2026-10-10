// Copyright (c) 2025 qiangli
// See LICENSE for licensing information

package coord

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/qiangli/yoke/pkg/principal"
	"github.com/qiangli/yoke/pkg/role"
)

// TestMain points the force audit at a scratch log: a test that forces an
// acquisition must never append to the developer's real audit chain.
func TestMain(m *testing.M) {
	home, err := os.MkdirTemp("", "coord-test-home")
	if err != nil {
		panic(err)
	}
	os.Setenv("BASHY_HOME", home)
	os.Unsetenv("BASHY_AUDIT")
	code := m.Run()
	os.RemoveAll(home)
	os.Exit(code)
}

func ledger(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("BASHY_COORD_DIR", dir)
	return dir
}

func req(ref string, h principal.Ref) Request {
	return Request{Ref: ParseRef(ref), Holder: h, Intent: "test"}
}

func isConflict(err error) bool {
	var c *Conflict
	return errors.As(err, &c)
}

func TestParseRef(t *testing.T) {
	for in, want := range map[string]Ref{
		"repo:yoke":       {"repo", "yoke"},
		"do1":             {"name", "do1"},
		"sprint:408":      {"sprint", "408"},
		"path:/w/a:b":     {"path", "/w/a:b"},
		`C:\work\yoke`:    {"name", `C:\work\yoke`},
		"  name:do1  ":    {"name", "do1"},
		"a:b":             {"name", "a:b"},
		"my-kind_2:x:y:z": {"my-kind_2", "x:y:z"},
	} {
		if got := ParseRef(in); got != want {
			t.Errorf("ParseRef(%q) = %+v, want %+v", in, got, want)
		}
	}
	if s := (Ref{"repo", "yoke"}).String(); s != "repo:yoke" {
		t.Fatalf("String = %q", s)
	}
}

func TestBuiltinKinds(t *testing.T) {
	for name, want := range map[string]struct {
		domain string
		match  MatchRule
	}{"name": {"name", MatchName}, "path": {"fs", MatchPath}, "repo": {"fs", MatchPath}} {
		k, ok := LookupKind(name)
		if !ok || k.domain() != want.domain || k.Match != want.match {
			t.Errorf("%s = %+v ok=%v", name, k, ok)
		}
	}
	if len(Kinds()) < 3 {
		t.Fatalf("Kinds() = %v", Kinds())
	}
}

func TestRepoAndPathShareADomain(t *testing.T) {
	ledger(t)
	ctx := context.Background()
	if _, err := AcquireRef(ctx, Request{Ref: Ref{"repo", "yoke"}, Members: []string{"/w/yoke"}, Holder: agentA()}); err != nil {
		t.Fatal(err)
	}
	_, err := AcquireRef(ctx, Request{Ref: Ref{"path", "/w/yoke/pkg/policy"}, Holder: agentB()})
	if !isConflict(err) {
		t.Fatalf("path under a claimed repo was admitted: %v", err)
	}
	if _, err := AcquireRef(ctx, Request{Ref: Ref{"path", "/w/other"}, Holder: agentB()}); err != nil {
		t.Fatalf("unrelated path blocked: %v", err)
	}
	// And the legacy holder-keyed project claim sits in the same domain.
	if _, err := Acquire(DefaultDir(), []string{"/w/yoke"}, agentC(), "", false); !isConflict(err) {
		t.Fatalf("legacy project claim ignored a keyed repo claim: %v", err)
	}
}

func TestDifferentDomainsNeverConflict(t *testing.T) {
	ledger(t)
	ctx := context.Background()
	RegisterKind(Kind{Name: "sprint", Match: MatchName})
	if _, err := AcquireRef(ctx, Request{Ref: Ref{"repo", "x"}, Members: []string{"/w/x"}, Holder: agentA()}); err != nil {
		t.Fatal(err)
	}
	// Same name and same member string as the repo claim, but other domains.
	for _, r := range []Ref{{"name", "x"}, {"sprint", "x"}} {
		if _, err := AcquireRef(ctx, Request{Ref: r, Members: []string{"/w/x"}, Holder: agentB()}); err != nil {
			t.Fatalf("%s conflicted across domains: %v", r, err)
		}
	}
}

func TestMemberMatch(t *testing.T) {
	ledger(t)
	ctx := context.Background()
	RegisterKind(Kind{Name: "test-fleet", Domain: "test-hosts", Match: MatchMember})
	RegisterKind(Kind{Name: "test-box", Domain: "test-hosts", Match: MatchMember})
	if _, err := AcquireRef(ctx, Request{Ref: Ref{"test-fleet", "gpu"}, Members: []string{"h1", "h2"}, Holder: agentA()}); err != nil {
		t.Fatal(err)
	}
	if _, err := AcquireRef(ctx, Request{Ref: Ref{"test-box", "b"}, Members: []string{"h3"}, Holder: agentB()}); err != nil {
		t.Fatalf("disjoint members conflicted: %v", err)
	}
	if _, err := AcquireRef(ctx, Request{Ref: Ref{"test-box", "c"}, Members: []string{"h9", "h2"}, Holder: agentB()}); !isConflict(err) {
		t.Fatalf("shared member h2 was admitted: %v", err)
	}
}

type fakeProvider struct{ members map[string][]string }

func (fakeProvider) Kind() Kind {
	return Kind{Name: "test-project", Domain: "fs", Match: MatchPath}
}
func (p fakeProvider) Exists(n string) bool { _, ok := p.members[n]; return ok }
func (p fakeProvider) Members(n string) ([]string, error) {
	return p.members[n], nil
}

func TestProviderResolvesMembers(t *testing.T) {
	ledger(t)
	RegisterProvider(fakeProvider{members: map[string][]string{"bashy": {"/w/bashy", "/w/sh"}}})
	if k, ok := LookupKind("test-project"); !ok || k.Domain != "fs" {
		t.Fatal("RegisterProvider did not register the kind")
	}
	g, err := AcquireRef(context.Background(), Request{Ref: Ref{"test-project", "bashy"}, Holder: agentA()})
	if err != nil {
		t.Fatal(err)
	}
	if len(g.Claim.Members) != 2 {
		t.Fatalf("members = %v", g.Claim.Members)
	}
	if _, err := Acquire(DefaultDir(), []string{"/w/sh"}, agentB(), "", false); !isConflict(err) {
		t.Fatalf("provider-resolved member /w/sh not enforced: %v", err)
	}
}

func TestEpochBumpsOnNewAcquisitionOnly(t *testing.T) {
	ledger(t)
	ctx := context.Background()
	g1, err := AcquireRef(ctx, req("name:e1", agentA()))
	if err != nil || g1.Epoch != 1 {
		t.Fatalf("first grant = %+v %v", g1, err)
	}
	g2, err := AcquireRef(ctx, req("name:e1", agentA()))
	if err != nil || g2.Epoch != 1 {
		t.Fatalf("same-holder reacquire bumped the epoch: %+v %v", g2, err)
	}
	if g, err := Refresh(ctx, Ref{"name", "e1"}, agentA(), g1.Epoch); err != nil || g.Epoch != 1 {
		t.Fatalf("refresh = %+v %v", g, err)
	}
	if err := ReleaseRef(ctx, Ref{"name", "e1"}, agentA(), g1.Epoch); err != nil {
		t.Fatal(err)
	}
	g3, err := AcquireRef(ctx, req("name:e1", agentB()))
	if err != nil || g3.Epoch <= g1.Epoch {
		t.Fatalf("epoch went backwards across a release: %+v %v", g3, err)
	}
}

func TestStaleEpochIsFenced(t *testing.T) {
	ledger(t)
	ctx := context.Background()
	ref := Ref{"name", "fence"}
	old, err := AcquireRef(ctx, req("name:fence", agentA()))
	if err != nil {
		t.Fatal(err)
	}
	// B forces A out: a new acquisition, so a new epoch.
	taken, err := AcquireRef(ctx, Request{Ref: ref, Holder: agentB(), Force: true})
	if err != nil {
		t.Fatal(err)
	}
	if taken.Epoch <= old.Epoch {
		t.Fatalf("takeover epoch %d <= %d", taken.Epoch, old.Epoch)
	}
	if _, err := Refresh(ctx, ref, agentA(), old.Epoch); !errors.Is(err, ErrFenced) {
		t.Fatalf("stale Refresh = %v, want ErrFenced", err)
	}
	if err := ReleaseRef(ctx, ref, agentA(), old.Epoch); !errors.Is(err, ErrFenced) {
		t.Fatalf("stale Release = %v, want ErrFenced", err)
	}
	// A holder that re-presents a stale epoch of its own claim is fenced too.
	if _, err := Refresh(ctx, ref, agentB(), old.Epoch); !errors.Is(err, ErrFenced) {
		t.Fatalf("holder with stale epoch = %v, want ErrFenced", err)
	}
	// epoch 0 means "my current claim" — and only the holder may say it.
	if err := ReleaseRef(ctx, ref, agentA(), 0); !isConflict(err) {
		t.Fatalf("non-holder release with epoch 0 = %v, want Conflict", err)
	}
	if _, err := Refresh(ctx, ref, agentB(), 0); err != nil {
		t.Fatal(err)
	}
	if err := ReleaseRef(ctx, ref, agentB(), 0); err != nil {
		t.Fatal(err)
	}
	if err := ReleaseRef(ctx, ref, agentB(), 0); err != nil {
		t.Fatalf("releasing an absent claim errored: %v", err)
	}
	if _, err := Refresh(ctx, ref, agentB(), 0); !errors.Is(err, ErrNotHeld) {
		t.Fatalf("refresh of absent claim = %v, want ErrNotHeld", err)
	}
}

func TestAcquireWithStaleEpochIsFenced(t *testing.T) {
	ledger(t)
	ctx := context.Background()
	g, _ := AcquireRef(ctx, req("name:ae", agentA()))
	// A's own claim lapsed, B took over, B released, A comes back: fresh.
	if _, err := AcquireRef(ctx, Request{Ref: Ref{"name", "ae"}, Holder: agentA(), Epoch: g.Epoch + 7}); !errors.Is(err, ErrFenced) {
		t.Fatalf("mismatched epoch on re-acquire = %v", err)
	}
}

func TestUnknownIsNotTakeable(t *testing.T) {
	dir := ledger(t)
	ctx := context.Background()
	g, err := AcquireRef(ctx, req("name:unk", agentA()))
	if err != nil {
		t.Fatal(err)
	}
	c := g.Claim
	c.Heartbeat = time.Time{}
	if err := writeClaim(resourceClaimPath(dir, "unk"), c); err != nil {
		t.Fatal(err)
	}
	if got := c.Liveness(time.Now()); got != role.LivenessUnknown {
		t.Fatalf("liveness = %q", got)
	}
	if c.Stale(time.Now()) {
		t.Fatal("an UNKNOWN claim reads as stale (takeable)")
	}
	if _, err := AcquireRef(ctx, req("name:unk", agentB())); !isConflict(err) {
		t.Fatalf("unknown claim was taken without force: %v", err)
	}
	// A cross-key conflict in a shared domain treats unknown the same way.
	p := &Claim{SchemaVersion: SchemaVersion, Kind: "path", Resource: "/w/u", Members: []string{"/w/u"},
		Mode: ModeLease, Holder: agentA(), AcquiredAt: time.Now(), Epoch: 1}
	if err := writeClaim(keyedClaimPath(dir, Ref{"path", "/w/u"}), p); err != nil {
		t.Fatal(err)
	}
	if _, err := AcquireRef(ctx, req("repo:u", agentB())); err != nil {
		// repo:u defaults to member "u", not "/w/u": disjoint, so this is admitted.
		t.Fatalf("unexpected conflict: %v", err)
	}
	if _, err := AcquireRef(ctx, Request{Ref: Ref{"repo", "u2"}, Members: []string{"/w/u/sub"}, Holder: agentB()}); !isConflict(err) {
		t.Fatalf("unknown path claim was seized: %v", err)
	}
	if _, err := AcquireRef(ctx, Request{Ref: Ref{"repo", "u2"}, Members: []string{"/w/u/sub"}, Holder: agentB(), Force: true}); err != nil {
		t.Fatalf("force over unknown failed: %v", err)
	}
}

func TestLapsedKeyedClaimIsTakeable(t *testing.T) {
	dir := ledger(t)
	ctx := context.Background()
	g, _ := AcquireRef(ctx, req("repo:lapse", agentA()))
	backdate(t, keyedClaimPath(dir, Ref{"repo", "lapse"}), g.Claim)
	g2, err := AcquireRef(ctx, req("repo:lapse", agentB()))
	if err != nil {
		t.Fatalf("lapsed claim not takeable: %v", err)
	}
	if g2.Epoch <= g.Epoch {
		t.Fatalf("takeover epoch %d <= %d", g2.Epoch, g.Epoch)
	}
}

func TestKindTTLOverride(t *testing.T) {
	RegisterKind(Kind{Name: "test-short", Match: MatchName, TTL: time.Minute})
	c := &Claim{Kind: "test-short", Holder: agentA(), Mode: ModeLease,
		AcquiredAt: time.Now().Add(-10 * time.Minute), Heartbeat: time.Now().Add(-2 * time.Minute)}
	if got := c.Liveness(time.Now()); got != role.LivenessLapsed {
		t.Fatalf("liveness under 1m TTL = %q", got)
	}
}

func TestAnnounceConflictsOnAcquireButHasNoTTL(t *testing.T) {
	dir := ledger(t)
	ctx := context.Background()
	g, err := AcquireRef(ctx, Request{Ref: Ref{"name", "ann"}, Holder: agentA(), Mode: ModeAnnounce})
	if err != nil {
		t.Fatal(err)
	}
	old := g.Claim
	old.AcquiredAt = time.Now().Add(-48 * time.Hour)
	old.Heartbeat = old.AcquiredAt
	if err := writeClaim(resourceClaimPath(dir, "ann"), old); err != nil {
		t.Fatal(err)
	}
	if got := old.Liveness(time.Now()); got != role.LivenessLive {
		t.Fatalf("announce liveness = %q, want live forever", got)
	}
	if _, err := AcquireRef(ctx, req("name:ann", agentB())); !isConflict(err) {
		t.Fatalf("announce did not conflict on Acquire: %v", err)
	}
	lapsed, _ := Lapsed(dir, time.Now())
	if len(lapsed) != 0 {
		t.Fatalf("announce was pruned: %v", lapsed)
	}
}

func TestModeMustBePermittedByKind(t *testing.T) {
	ledger(t)
	RegisterKind(Kind{Name: "test-leaseonly", Match: MatchName, Modes: []string{ModeLease}})
	if _, err := AcquireRef(context.Background(), Request{Ref: Ref{"test-leaseonly", "x"}, Holder: agentA(), Mode: ModeAnnounce}); err == nil {
		t.Fatal("announce accepted by a lease-only kind")
	}
	if _, err := AcquireRef(context.Background(), Request{Ref: Ref{"name", "x"}, Holder: agentA(), Mode: ModeAttached}); err == nil {
		t.Fatal("AcquireRef accepted an attached hold without a kernel lock")
	}
}

// A v1 record on disk — project claim and resource claim — must still list,
// read as the v2 kinds, and still block.
func TestV1RecordsAreReadable(t *testing.T) {
	dir := t.TempDir()
	now := time.Now().UTC()
	v1 := func(c map[string]any) {
		c["schema_version"] = "bashy-claim-v1"
		c["holder"] = agentA()
		c["acquired_at"], c["heartbeat"] = now, now
	}
	proj := map[string]any{"roots": []string{"/w/bashy", "/w/sh"}, "project": "bashy"}
	v1(proj)
	res := map[string]any{"resource": "do1", "mode": "lease"}
	v1(res)
	for path, rec := range map[string]map[string]any{
		claimPath(dir, agentA()):      proj,
		resourceClaimPath(dir, "do1"): res,
	} {
		b, _ := json.Marshal(rec)
		if err := os.WriteFile(path, b, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	got, err := List(dir)
	if err != nil || len(got) != 2 {
		t.Fatalf("List = %v %v", got, err)
	}
	for _, c := range got {
		switch {
		case c.Resource == "do1":
			if c.Kind != "name" || c.Ref() != (Ref{"name", "do1"}) {
				t.Errorf("v1 resource = %+v", c)
			}
		default:
			if c.Kind != "repo" || len(c.Members) != 2 || c.Members[1] != "/w/sh" {
				t.Errorf("v1 project = %+v", c)
			}
		}
	}
	if _, err := Acquire(dir, []string{"/w/sh"}, agentB(), "", false); !isConflict(err) {
		t.Fatalf("v1 project claim no longer blocks: %v", err)
	}
	if _, err := AcquireResource(dir, "do1", agentB(), "", false); !isConflict(err) {
		t.Fatalf("v1 resource claim no longer blocks: %v", err)
	}
	// Re-acquiring upgrades the record and keeps v1's tenure.
	g, err := AcquireResource(dir, "do1", agentA(), "", false)
	if err != nil || g.SchemaVersion != SchemaVersion || g.Epoch != 1 {
		t.Fatalf("upgrade = %+v %v", g, err)
	}
}

// memBackend honours the Backend contract: a CAS on Rev, a Rev that never
// repeats for a key, and an epoch high-water mark that survives deletion.
type memBackend struct {
	mu sync.Mutex
	m  map[string]*Claim
	// hwEpoch and hwRev are the per-key high-water marks kept across deletion.
	hwEpoch, hwRev map[string]uint64
	// afterLoad, when set, runs once right after the next Load returns its
	// snapshot: it lets a test interleave another writer into the window
	// between a caller's read and its commit.
	afterLoad func()
}

func (b *memBackend) Load(k string) (*Claim, error) {
	b.mu.Lock()
	var out *Claim
	if c := b.m[k]; c != nil {
		cp := *c
		out = &cp
	}
	hook := b.afterLoad
	b.afterLoad = nil
	b.mu.Unlock()
	if hook != nil {
		hook()
	}
	return out, nil
}

func (b *memBackend) CommitIfRev(k string, prev uint64, next *Claim) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	cur := b.m[k]
	var curRev uint64
	if cur != nil {
		curRev = cur.Rev
	}
	if curRev != prev {
		return ErrEpochMismatch
	}
	if b.hwEpoch == nil {
		b.hwEpoch, b.hwRev = map[string]uint64{}, map[string]uint64{}
	}
	if next == nil {
		if cur != nil {
			b.hwEpoch[k] = max(b.hwEpoch[k], cur.Epoch)
			b.hwRev[k] = max(b.hwRev[k], cur.Rev)
			delete(b.m, k)
		}
		return nil
	}
	if cur == nil {
		next.Epoch = max(next.Epoch, b.hwEpoch[k]+1)
		next.Rev = b.hwRev[k] + 1
	} else {
		next.Rev = cur.Rev + 1
	}
	cp := *next
	b.m[k] = &cp
	return nil
}

func TestCustomBackendOwnsItsKind(t *testing.T) {
	dir := ledger(t)
	mb := &memBackend{m: map[string]*Claim{}}
	RegisterKind(Kind{Name: "test-mem", Match: MatchName})
	RegisterBackend("test-mem", mb)
	t.Cleanup(func() { RegisterBackend("test-mem", nil) })
	ctx := context.Background()
	g, err := AcquireRef(ctx, req("test-mem:one", agentA()))
	if err != nil || g.Epoch != 1 {
		t.Fatalf("grant = %+v %v", g, err)
	}
	if _, err := AcquireRef(ctx, req("test-mem:one", agentB())); !isConflict(err) {
		t.Fatalf("custom backend did not exclude: %v", err)
	}
	if _, err := Refresh(ctx, Ref{"test-mem", "one"}, agentA(), 99); !errors.Is(err, ErrFenced) {
		t.Fatalf("custom backend fencing = %v", err)
	}
	if err := ReleaseRef(ctx, Ref{"test-mem", "one"}, agentA(), g.Epoch); err != nil {
		t.Fatal(err)
	}
	if len(mb.m) != 0 {
		t.Fatal("release left the record")
	}
	if entries, _ := filepath.Glob(filepath.Join(dir, "claim-test-mem-*")); len(entries) != 0 {
		t.Fatalf("custom kind leaked into the file store: %v", entries)
	}
}

func TestOneFilePerKey(t *testing.T) {
	dir := ledger(t)
	ctx := context.Background()
	for _, r := range []string{"repo:a", "path:/p", "name:n"} {
		if _, err := AcquireRef(ctx, req(r, agentA())); err != nil {
			t.Fatal(err)
		}
	}
	files, _ := filepath.Glob(filepath.Join(dir, "*.json"))
	if len(files) != 3 {
		t.Fatalf("files = %v", files)
	}
	all, _ := List(dir)
	if len(all) != 3 {
		t.Fatalf("List = %d", len(all))
	}
}

func TestAcquireRefWaitHonoursContext(t *testing.T) {
	ledger(t)
	if _, err := AcquireRef(context.Background(), req("name:w", agentA())); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 80*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := AcquireRef(ctx, Request{Ref: Ref{"name", "w"}, Holder: agentB(), Wait: 5 * time.Second})
	if !errors.Is(err, context.DeadlineExceeded) || time.Since(start) > 2*time.Second {
		t.Fatalf("err = %v after %v", err, time.Since(start))
	}
}

func TestAttachedRefRoundTrip(t *testing.T) {
	ledger(t)
	ctx := context.Background()
	g, l, err := AcquireAttachedRef(ctx, Request{Ref: Ref{"path", "/w/att"}, Holder: agentA()})
	if err != nil {
		t.Fatal(err)
	}
	defer l.Release()
	if g.Claim.Mode != ModeAttached || g.Epoch != 1 {
		t.Fatalf("grant = %+v", g)
	}
	if _, _, err := AcquireAttachedRef(ctx, Request{Ref: Ref{"path", "/w/att"}, Holder: agentB()}); !isConflict(err) {
		t.Fatalf("second attached hold = %v", err)
	}
	if err := ReleaseRef(ctx, Ref{"path", "/w/att"}, agentA(), 0); err == nil {
		t.Fatal("ReleaseRef dropped a kernel-held claim")
	}
	if err := ReleaseAttached(DefaultDir(), g.Claim, l); err != nil {
		t.Fatal(err)
	}
}
