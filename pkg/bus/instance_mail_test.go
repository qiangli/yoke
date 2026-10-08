package bus

// The REAL mail source for a retiring instance. fleet.Retire refuses a nil
// source rather than writing an empty archive, so the only thing that makes
// "retiring archives mail without deleting it" true is this function actually
// finding the records.

import (
	"strings"
	"testing"

	"github.com/qiangli/yoke/pkg/fleet"
)

func instanceOf(uuid string) fleet.Instance {
	return fleet.Instance{UUID: uuid, FamilyID: "single:claude:opus5.5@abc", Bindings: []string{"claude:opus5.5"}}
}

// Personal mail addressed to instance/<uuid> is collected, in sequence order,
// whether it sits in the materialized buffer or only on the durable timeline.
func TestInstanceMailCollectsPersonalMailFromBothStores(t *testing.T) {
	isolate(t)
	inst := instanceOf("11111111-2222-3333-4444-555555555555")
	addr := inst.MailAddress()

	// On the timeline only — addressed backlog that no subscription has
	// materialized yet.
	if err := Publish(Notification{Principal: "conductor:321", To: addr, Body: "review blockers"}); err != nil {
		t.Fatal(err)
	}
	// Already in the buffer, and ALREADY READ. An archive of only the unread
	// answers the wrong question.
	if err := AppendPending(addr, Pending{
		SchemaVersion: SchemaVersion, Seq: 9001, TS: "2026-10-08T00:00:00Z",
		Principal: "operator", To: addr, Body: "handoff accepted",
		Delivery: DeliveryQueued, ReadAt: "2026-10-08T00:01:00Z",
	}); err != nil {
		t.Fatal(err)
	}

	got, err := InstanceMail(inst)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("archived %d records, want 2: %v", len(got), got)
	}
	joined := strings.Join(got, "\n")
	for _, want := range []string{"review blockers", "handoff accepted"} {
		if !strings.Contains(joined, want) {
			t.Errorf("the archive lost %q: %v", want, got)
		}
	}
	// Sequence order, so the archive reads as the conversation it was.
	if !strings.Contains(got[0], "review blockers") {
		t.Errorf("records are not in sequence order: %v", got)
	}
}

// Another instance's mail is NOT this instance's. A reused label must not
// inherit a mailbox, and the archive is the place that would quietly leak one.
func TestInstanceMailIsScopedToItsOwnUUID(t *testing.T) {
	isolate(t)
	mine := instanceOf("11111111-2222-3333-4444-555555555555")
	theirs := instanceOf("99999999-8888-7777-6666-555555555555")

	if err := Publish(Notification{Principal: "operator", To: theirs.MailAddress(), Body: "not yours"}); err != nil {
		t.Fatal(err)
	}
	// Mail to the FAMILY name is role/legacy addressing, not personal mail.
	if err := Publish(Notification{Principal: "operator", To: "esme", Body: "legacy name"}); err != nil {
		t.Fatal(err)
	}

	got, err := InstanceMail(mine)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("an instance read %d records that were not addressed to it: %v", len(got), got)
	}
}

// Collecting mail must not MUTATE the bus: retirement is evidence collection.
// An earlier shape of this used SnapshotInbox, which creates a subscription
// and appends to the buffer as a side effect of reading.
func TestInstanceMailDoesNotMutateTheBus(t *testing.T) {
	isolate(t)
	inst := instanceOf("11111111-2222-3333-4444-555555555555")
	addr := inst.MailAddress()
	if err := Publish(Notification{Principal: "operator", To: addr, Body: "one"}); err != nil {
		t.Fatal(err)
	}

	before, err := ReadPending(addr)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := InstanceMail(inst); err != nil {
		t.Fatal(err)
	}
	after, err := ReadPending(addr)
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != len(before) {
		t.Errorf("collecting mail appended to the buffer: %d -> %d", len(before), len(after))
	}
	// Idempotent: a second collection reports the same records, so a retry
	// after a failed archive write cannot double the evidence.
	first, err := InstanceMail(inst)
	if err != nil {
		t.Fatal(err)
	}
	second, err := InstanceMail(inst)
	if err != nil {
		t.Fatal(err)
	}
	if len(first) != len(second) {
		t.Errorf("collection is not idempotent: %d then %d", len(first), len(second))
	}
}

// End to end: fleet.Retire with THIS source archives the real records, and
// reads them back under the old UUID.
func TestRetireArchivesRealMailThroughThisSource(t *testing.T) {
	isolate(t)
	dir := t.TempDir()
	t.Setenv(fleet.InstanceDirEnv, dir)
	store := fleet.NewInstanceStore(dir)

	inst, err := store.Open(fleet.Family{
		Name: "claude:opus5.5", Display: "Esme",
		Policy: fleet.PolicySingle, Bindings: []string{"claude:opus5.5"},
	}, fleet.OpenOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if err := Publish(Notification{Principal: "conductor:321", To: inst.MailAddress(), Body: "the last word"}); err != nil {
		t.Fatal(err)
	}

	archive, err := store.Retire(inst.UUID, InstanceMail)
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(archive) == "" {
		t.Fatal("retirement reported no archive")
	}
	kept, err := store.ArchivedMail(inst.UUID)
	if err != nil {
		t.Fatal(err)
	}
	if len(kept) != 1 || !strings.Contains(kept[0], "the last word") {
		t.Fatalf("the archive does not hold the real mail: %v", kept)
	}
}
