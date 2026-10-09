package fleet

// These tests are written against the review findings, one case per claim the
// store makes: the cap is a cap under concurrency, labels and handles share
// one address scope in both directions, an id is a UUID or it is nothing, and
// a retirement that reports archived mail is holding some.

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func instanceStore(t *testing.T) *InstanceStore {
	t.Helper()
	return NewInstanceStore(t.TempDir())
}

func testFamily() Family {
	return Family{Name: "esme-cfg", Display: "Esme", Policy: PolicySingle, Bindings: []string{"claude:opus5.5"}}
}

// mailOf is a MailSource standing in for the bus.
func mailOf(records ...string) MailSource {
	return func(Instance) ([]string, error) { return records, nil }
}

// Two fresh contexts in one family are two conversations: distinct UUIDs,
// distinct labels, and mailboxes that do not read each other's mail.
func TestTwoFreshContextsInOneFamilyAreDistinct(t *testing.T) {
	s := instanceStore(t)
	f := testFamily()
	a, err := s.Open(f, OpenOptions{MailFrom: 10})
	if err != nil {
		t.Fatal(err)
	}
	b, err := s.Open(f, OpenOptions{MailFrom: 20})
	if err != nil {
		t.Fatal(err)
	}
	if a.UUID == b.UUID {
		t.Fatalf("two fresh contexts share a uuid: %s", a.UUID)
	}
	if a.Label != "Esme" || b.Label != "Esme-2" {
		t.Fatalf("labels = %q, %q; want Esme, Esme-2", a.Label, b.Label)
	}
	if a.Accepts(b.MailAddress(), 30) || b.Accepts(a.MailAddress(), 30) {
		t.Fatal("one instance accepts the other's personal mail")
	}
	if !b.Accepts(b.MailAddress(), 20) || b.Accepts(b.MailAddress(), 19) {
		t.Fatal("the mailbox watermark is not the start of the mailbox")
	}
}

// Resume is a read: the UUID, the frozen configuration and the watermark all
// come back unchanged, which is what makes a restart a continuation.
func TestResumePreservesIdentityAndMailbox(t *testing.T) {
	s := instanceStore(t)
	opened, err := s.Open(testFamily(), OpenOptions{MailFrom: 42})
	if err != nil {
		t.Fatal(err)
	}
	// A second store over the same directory is the next process.
	resumed, err := NewInstanceStore(s.Dir()).Resume(opened.UUID)
	if err != nil {
		t.Fatal(err)
	}
	if resumed.UUID != opened.UUID || resumed.MailFrom != 42 || resumed.FamilyID != opened.FamilyID {
		t.Fatalf("resume changed the instance: %+v vs %+v", resumed, opened)
	}
}

// FINDING 1. The cap counts live instances, and it has to still count them
// when two openers race. Each goroutine opens through its own store handle
// over one directory, which is what two `bashy` processes in two checkouts
// are; without the interprocess lock both read 0 live and both wrote.
func TestOpenHoldsTheCapUnderConcurrentOpens(t *testing.T) {
	dir := t.TempDir()
	f := testFamily()
	const racers = 6

	var wg sync.WaitGroup
	errs := make([]error, racers)
	for i := range racers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, errs[i] = NewInstanceStore(dir).WithCap(1).Open(f, OpenOptions{})
		}()
	}
	wg.Wait()

	opened, capped := 0, 0
	for _, err := range errs {
		var full *CapError
		switch {
		case err == nil:
			opened++
		case errors.As(err, &full):
			capped++
			// FINDING: cap exhaustion must be actionable.
			msg := full.Error()
			for _, want := range []string{"1", "retire", InstanceCapEnv} {
				if !strings.Contains(msg, want) {
					t.Errorf("cap error does not say %q: %s", want, msg)
				}
			}
			if len(full.Live) == 0 {
				t.Error("cap error names no instance holding a slot")
			}
		default:
			t.Errorf("unexpected open failure: %v", err)
		}
	}
	if opened != 1 || capped != racers-1 {
		t.Fatalf("cap 1 admitted %d instances (%d refused) out of %d openers", opened, capped, racers)
	}
}

// FINDING 1. Two concurrent handle changes may not both succeed on one handle:
// a handle that resolves to two live instances is not an address.
func TestSetHandleUnderConcurrentChanges(t *testing.T) {
	dir := t.TempDir()
	s := NewInstanceStore(dir).WithCap(-1)
	f := testFamily()
	var ids []string
	for range 4 {
		inst, err := s.Open(f, OpenOptions{})
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, inst.UUID)
	}

	var wg sync.WaitGroup
	errs := make([]error, len(ids))
	for i, id := range ids {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, errs[i] = NewInstanceStore(dir).SetHandle(id, "e")
		}()
	}
	wg.Wait()

	took := 0
	for _, err := range errs {
		switch {
		case err == nil:
			took++
		case errors.Is(err, ErrHandleAmbiguous):
		default:
			t.Errorf("unexpected SetHandle failure: %v", err)
		}
	}
	if took != 1 {
		t.Fatalf("%d instances took the handle \"e\"; want 1", took)
	}
	if _, found, err := s.ResolveHandle("e"); err != nil || !found {
		t.Fatalf("the handle does not resolve to one instance: found=%v err=%v", found, err)
	}
}

// FINDING 1. Labels and handles are ONE address scope, so the collision check
// has to cross: a label may not take another instance's handle, a handle may
// not take another instance's label, and one instance may not hold the same
// name twice.
func TestLabelAndHandleCollideInBothDirections(t *testing.T) {
	s := instanceStore(t).WithCap(-1)
	f := testFamily()

	held, err := s.Open(f, OpenOptions{Label: "Alpha", Handle: "beta"})
	if err != nil {
		t.Fatal(err)
	}

	// A new LABEL against an existing HANDLE.
	if _, err := s.Open(f, OpenOptions{Label: "beta"}); !errors.Is(err, ErrHandleAmbiguous) {
		t.Errorf("opening label \"beta\" against the handle \"beta\": got %v", err)
	}
	// A new LABEL against an existing LABEL.
	if _, err := s.Open(f, OpenOptions{Label: "alpha"}); !errors.Is(err, ErrLabelHeld) {
		t.Errorf("opening label \"alpha\" against the label \"Alpha\": got %v", err)
	}
	// A new HANDLE against an existing LABEL.
	other, err := s.Open(f, OpenOptions{Label: "Gamma"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.SetHandle(other.UUID, "alpha"); !errors.Is(err, ErrLabelHeld) {
		t.Errorf("taking the handle \"alpha\" against the label \"Alpha\": got %v", err)
	}
	// A handle equal to the instance's OWN label.
	if _, err := s.SetHandle(other.UUID, "gamma"); !errors.Is(err, ErrHandleAmbiguous) {
		t.Errorf("taking a handle equal to one's own label: got %v", err)
	}
	// A derived label skips handles too.
	if label, err := s.NextLabel("beta"); err != nil || label != "beta-2" {
		t.Errorf("NextLabel over a live handle = %q, %v; want beta-2", label, err)
	}
	// An instance keeps its own handle when it sets it again.
	if _, err := s.SetHandle(held.UUID, "beta"); err != nil {
		t.Errorf("re-setting an instance's own handle: %v", err)
	}
}

// FINDING 4. An id is parsed as a UUID, not sanitized into a filename. The
// lossy mapping it replaced let these strings name records — and let two
// different ids name ONE record, which is two conversations in one mailbox.
func TestLookupsRefuseAnythingButAUUID(t *testing.T) {
	s := instanceStore(t)
	for _, id := range []string{"", "   ", "esme", "../../etc/passwd", "!!!", "instance", "../x"} {
		if _, err := s.Get(id); err == nil {
			t.Errorf("Get(%q) resolved", id)
		}
		if _, err := s.Resume(id); err == nil {
			t.Errorf("Resume(%q) resolved", id)
		}
		if _, err := s.Retire(id, mailOf()); err == nil {
			t.Errorf("Retire(%q) resolved", id)
		}
	}
	if _, err := s.Get("!!!"); !errors.Is(err, ErrInstanceMalformed) {
		t.Errorf("a non-UUID id is not reported as malformed: %v", err)
	}
	// No file was created by any of it, and nothing escaped the directory.
	entries, err := os.ReadDir(s.Dir())
	if err == nil {
		for _, e := range entries {
			if strings.HasSuffix(e.Name(), ext) {
				t.Errorf("a refused lookup left a record behind: %s", e.Name())
			}
		}
	}
}

// FINDING 4. One UUID is one instance however it is spelled: the canonical
// form is what reaches disk, so an upper-case or braced spelling resolves to
// the same record rather than a second one.
func TestUUIDLookupsAreCanonicalized(t *testing.T) {
	s := instanceStore(t)
	inst, err := s.Open(testFamily(), OpenOptions{})
	if err != nil {
		t.Fatal(err)
	}
	for _, spelling := range []string{
		strings.ToUpper(inst.UUID),
		"  " + inst.UUID + "  ",
		"{" + inst.UUID + "}",
		"urn:uuid:" + inst.UUID,
	} {
		got, err := s.Get(spelling)
		if err != nil {
			t.Fatalf("Get(%q): %v", spelling, err)
		}
		if got.UUID != inst.UUID {
			t.Fatalf("Get(%q) = %s, want %s", spelling, got.UUID, inst.UUID)
		}
	}
	if n := len(mustList(t, s)); n != 1 {
		t.Fatalf("the store holds %d records for one instance", n)
	}
}

// FINDING 4. A record is validated against the name it was read under. A
// hand-copied file would otherwise answer for an identity it does not hold —
// and the mailbox is keyed on exactly that identity.
func TestGetRefusesARecordWhoseUUIDDisagrees(t *testing.T) {
	s := instanceStore(t)
	inst, err := s.Open(testFamily(), OpenOptions{})
	if err != nil {
		t.Fatal(err)
	}
	impostor := "11111111-2222-3333-4444-555555555555"
	b, err := os.ReadFile(filepath.Join(s.Dir(), inst.UUID+ext))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(s.Dir(), impostor+ext), b, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Get(impostor); err == nil {
		t.Fatal("a copied record answered under another uuid")
	}
	// And it does not hold a label or occupy a cap slot either.
	if n := len(mustList(t, s)); n != 1 {
		t.Fatalf("the roster counts %d records; the copy must not be one of them", n)
	}
}

// FINDING 5. Retiring archives the instance's REAL mail. The version this
// replaces created an empty file and reported it as the archive, which reads
// as evidence of nothing having been received.
func TestRetireArchivesRealMail(t *testing.T) {
	s := instanceStore(t)
	inst, err := s.Open(testFamily(), OpenOptions{})
	if err != nil {
		t.Fatal(err)
	}
	records := []string{`{"seq":1,"body":"first"}`, `{"seq":2,"body":"second"}`}

	// With no mail source there is nothing to archive, and that is refused
	// rather than reported as an empty archive.
	if _, err := s.Retire(inst.UUID, nil); !errors.Is(err, ErrMailSourceRequired) {
		t.Fatalf("retiring with no mail source: got %v", err)
	}
	if got, err := s.Get(inst.UUID); err != nil || !got.Active() {
		t.Fatalf("a refused retirement retired the instance anyway: %+v %v", got, err)
	}

	archive, err := s.Retire(inst.UUID, mailOf(records...))
	if err != nil {
		t.Fatal(err)
	}
	if archive == "" {
		t.Fatal("retirement reported no archive path")
	}
	got, err := s.ArchivedMail(inst.UUID)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != len(records) || got[0] != records[0] || got[1] != records[1] {
		t.Fatalf("archived mail = %v, want %v", got, records)
	}

	// Retiring again is idempotent and must not double the evidence.
	if _, err := s.Retire(inst.UUID, mailOf(records...)); err != nil {
		t.Fatal(err)
	}
	if again, err := s.ArchivedMail(inst.UUID); err != nil || len(again) != len(records) {
		t.Fatalf("a second retirement changed the archive: %v %v", again, err)
	}
	// The record survives retirement, and so does the label it HELD.
	retired, err := s.Get(inst.UUID)
	if err != nil {
		t.Fatal(err)
	}
	if retired.Active() || retired.Label != "Esme" {
		t.Fatalf("retired record = %+v; want retired, still reporting its label", retired)
	}
}

// FINDING 5, and the Esme-2 acceptance. Retirement releases the LABEL and
// nothing else: the next holder is a new UUID, it is not delivered the old
// instance's mail, and the old instance's archive stays readable under the old
// UUID — which is what "the UUID keeps its delivery meaning" means.
func TestReusedLabelInheritsNoMail(t *testing.T) {
	s := instanceStore(t)
	f := testFamily()
	first, err := s.Open(f, OpenOptions{Label: "Esme", MailFrom: 0})
	if err != nil {
		t.Fatal(err)
	}
	oldMail := `{"seq":7,"to":"` + first.MailAddress() + `","body":"for the first Esme"}`
	if _, err := s.Retire(first.UUID, mailOf(oldMail)); err != nil {
		t.Fatal(err)
	}

	second, err := s.Open(f, OpenOptions{Label: "Esme", MailFrom: 100})
	if err != nil {
		t.Fatalf("the released label was not free: %v", err)
	}
	if second.UUID == first.UUID {
		t.Fatal("reusing a label reused the identity")
	}
	if second.Accepts(first.MailAddress(), 7) || second.Accepts(first.MailAddress(), 200) {
		t.Fatal("the new Esme accepts mail addressed to the retired one")
	}
	if second.Accepts(second.MailAddress(), 99) {
		t.Fatal("the new Esme opens onto a backlog below its watermark")
	}
	// The retired instance still answers for its own address, so late mail is
	// archivable rather than silently reassigned to the new holder.
	if !first.Accepts(first.MailAddress(), 7) {
		t.Fatal("a retired instance stopped answering for its own uuid")
	}
	if got, err := s.ArchivedMail(first.UUID); err != nil || len(got) != 1 || got[0] != oldMail {
		t.Fatalf("the retired instance's archive = %v, %v", got, err)
	}
	// And the live roster resolves the label to the NEW instance only.
	resolved, found, err := s.ResolveLabel("Esme")
	if err != nil || !found || resolved.UUID != second.UUID {
		t.Fatalf("ResolveLabel(Esme) = %+v, %v, %v", resolved, found, err)
	}
}

// A retired instance is not resumable: its mail is archived and its label may
// now mean somebody else.
func TestRetiredInstanceIsNotResumable(t *testing.T) {
	s := instanceStore(t)
	inst, err := s.Open(testFamily(), OpenOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Retire(inst.UUID, mailOf()); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Resume(inst.UUID); err == nil {
		t.Fatal("a retired instance resumed")
	}
	// It no longer counts against the cap.
	live, err := s.ActiveInstances(testFamily().ID())
	if err != nil || len(live) != 0 {
		t.Fatalf("retired instance still counted live: %v %v", live, err)
	}
}

// Bindings are immutable for the life of a context; selecting inside a
// predefined composite is not a rebinding.
func TestInstanceBindingsAreImmutable(t *testing.T) {
	s := instanceStore(t)
	composite := Family{Name: "ladder", Policy: PolicyCascade, Bindings: []string{"claude:opus5.5", "codex:gpt6-sol"}}
	inst, err := s.Open(composite, OpenOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if got, err := inst.Select("codex:gpt6-sol"); err != nil || got != "codex:gpt6-sol" {
		t.Fatalf("selecting inside the frozen set: %q, %v", got, err)
	}
	if _, err := inst.Select("glm:4.9"); !errors.Is(err, ErrBindingImmutable) {
		t.Fatalf("selecting outside the frozen set: %v", err)
	}
	if err := inst.Rebind(testFamily()); !errors.Is(err, ErrBindingImmutable) {
		t.Fatalf("Rebind: %v", err)
	}
	// A reordered ladder is a different family, so the frozen FamilyID stops
	// matching — that is the mechanism behind "a binding change needs a
	// handoff", and it needs no extra check to enforce.
	reordered := Family{Name: "ladder", Policy: PolicyCascade, Bindings: []string{"codex:gpt6-sol", "claude:opus5.5"}}
	if inst.FamilyID == reordered.ID() {
		t.Fatal("a reordered ladder kept the instance's family id")
	}
}

// The URN an external session exports is an AGENT principal whose name is the
// UUID — no new principal kind — and a non-UUID never produces one.
func TestInstanceURNIsAUUIDNamedAgent(t *testing.T) {
	inst := Instance{UUID: "11111111-2222-3333-4444-555555555555"}
	if got, want := inst.URN(), "dhnt:agent/11111111-2222-3333-4444-555555555555"; got != want {
		t.Fatalf("URN = %q, want %q", got, want)
	}
	if got := (Instance{UUID: "esme"}).URN(); got != "" {
		t.Fatalf("a label produced the URN %q", got)
	}
	if got, want := inst.MailAddress(), InstanceAddressPrefix+inst.UUID; got != want {
		t.Fatalf("MailAddress = %q, want %q", got, want)
	}
}

func mustList(t *testing.T, s *InstanceStore) []Instance {
	t.Helper()
	all, err := s.List()
	if err != nil {
		t.Fatal(err)
	}
	return all
}

// GAP 4. The mail source reads the bus, which has locks of its own; running it
// while holding the store lock is a lock-order hazard. Collection must happen
// BEFORE the lock, and the state is re-checked under it.
func TestRetireCollectsMailBeforeTakingTheStoreLock(t *testing.T) {
	s := instanceStore(t)
	inst, err := s.Open(testFamily(), OpenOptions{})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	src := func(Instance) ([]string, error) {
		// Another writer to the same store, run while the mail source is
		// still executing. If Retire holds the store lock here, this blocks
		// until the lock wait gives up.
		go func() {
			_, err := NewInstanceStore(s.Dir()).SetHandle(inst.UUID, "probe")
			done <- err
		}()
		select {
		case err := <-done:
			if err != nil {
				return nil, err
			}
		case <-time.After(3 * time.Second):
			return nil, errors.New("store lock was held while the mail source ran")
		}
		return []string{`{"seq":1}`}, nil
	}
	if _, err := s.Retire(inst.UUID, src); err != nil {
		t.Fatal(err)
	}
}

// GAP 4. A retry after a failed record write finds the archive already holding
// the records; archiving them again would double the evidence.
func TestRetireRetryDoesNotDoubleTheArchive(t *testing.T) {
	s := instanceStore(t)
	inst, err := s.Open(testFamily(), OpenOptions{})
	if err != nil {
		t.Fatal(err)
	}
	records := []string{`{"seq":1,"body":"a"}`, `{"seq":2,"body":"b"}`}
	// The first attempt got as far as the archive and no further.
	if _, err := s.ArchiveMail(inst.UUID, records); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Retire(inst.UUID, mailOf(append(records, `{"seq":3,"body":"c"}`)...)); err != nil {
		t.Fatal(err)
	}
	got, err := s.ArchivedMail(inst.UUID)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("archive = %v; want each of the 3 records exactly once", got)
	}
	retired, _ := s.Get(inst.UUID)
	if retired.Active() {
		t.Fatal("instance is still active after a successful retirement")
	}
}

// GAP 5. The archive location is derived from the UUID. A stored MailArchive
// path is data in a user-writable record and must never redirect the append.
func TestArchiveIgnoresAStoredMailArchivePath(t *testing.T) {
	s := instanceStore(t)
	inst, err := s.Open(testFamily(), OpenOptions{})
	if err != nil {
		t.Fatal(err)
	}
	elsewhere := filepath.Join(t.TempDir(), "elsewhere.jsonl")
	tampered := inst
	tampered.MailArchive = elsewhere
	if err := s.write(tampered); err != nil {
		t.Fatal(err)
	}
	archive, err := s.Retire(inst.UUID, mailOf(`{"seq":1,"body":"x"}`))
	if err != nil {
		t.Fatal(err)
	}
	want := s.archivePath(inst)
	if archive != want {
		t.Fatalf("archive = %q; want the UUID-derived %q", archive, want)
	}
	if _, err := os.Stat(elsewhere); !os.IsNotExist(err) {
		t.Fatalf("the stored MailArchive path was written to: %v", err)
	}
	if got, err := s.ArchivedMail(inst.UUID); err != nil || len(got) != 1 {
		t.Fatalf("archived mail = %v %v; want the one record under the derived path", got, err)
	}
}

// GAP 6. Bindings are compared case-insensitively everywhere else, so the
// configuration identity must be too; and ',' / '|' are the Config separators.
func TestFamilyConfigNormalizesBindings(t *testing.T) {
	a := Family{Name: "n", Policy: PolicySingle, Bindings: []string{"  Claude:Opus5.5 "}}
	b := Family{Name: "n", Policy: PolicySingle, Bindings: []string{"claude:opus5.5"}}
	if a.Config() != b.Config() || a.ID() != b.ID() {
		t.Fatalf("case/space variants are different families: %q vs %q", a.Config(), b.Config())
	}
}

func TestOpenRejectsBindingsThatForgeTheConfigSeparators(t *testing.T) {
	s := instanceStore(t)
	for _, bad := range []string{"claude:opus,codex:gpt", "claude:opus|x"} {
		_, err := s.Open(Family{Name: "n", Policy: PolicySingle, Bindings: []string{bad}}, OpenOptions{})
		if !errors.Is(err, ErrBindingMalformed) {
			t.Errorf("binding %q: got %v; want ErrBindingMalformed", bad, err)
		}
	}
	inst, err := s.Open(Family{Name: "n", Policy: PolicySingle, Bindings: []string{" Claude:Opus5.5 "}}, OpenOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if inst.Bindings[0] != "claude:opus5.5" {
		t.Fatalf("frozen binding = %q; want it normalized", inst.Bindings[0])
	}
}
