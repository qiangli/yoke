package role

import "testing"

// SPRINT-FACING CONTACT OUTPUT MUST LEAD WITH THE STABLE OBJECT NAME.
//
// "sprint 126" survives a manager handoff, a title edit and a pause/resume
// cycle; the short meet number does not — it is a reusable, display-only
// pointer. A contact string that shows only "meet #25 · bus conductor.126"
// forces a reader to already know which sprint #25 refers to, which is
// exactly the fact a stale or reused room number cannot be trusted to carry.
func TestContact_StringLeadsWithStableName(t *testing.T) {
	c := &Contact{Kind: "meet", Ref: "room-abc", Room: 25, Topic: "conductor.126", Name: "sprint 126"}
	got := c.String()
	want := "sprint 126 · meet #25 · bus conductor.126"
	if got != want {
		t.Fatalf("String() = %q, want %q", got, want)
	}
}

// Without a Room number the room is still addressed by its durable Ref, and
// the stable name still leads.
func TestContact_StringLeadsWithStableNameNoRoomNumber(t *testing.T) {
	c := &Contact{Kind: "meet", Ref: "room-abc", Topic: "conductor.126", Name: "sprint 126"}
	got := c.String()
	want := "sprint 126 · meet room-abc · bus conductor.126"
	if got != want {
		t.Fatalf("String() = %q, want %q", got, want)
	}
}

// A contact with no stable name (steward's singleton seat, or a legacy
// contact nothing has healed yet) renders exactly as before — the addition
// must not put an empty name in the output.
func TestContact_StringWithoutNameIsUnchanged(t *testing.T) {
	c := &Contact{Kind: "meet", Ref: "room-abc", Room: 7, Topic: "steward"}
	got := c.String()
	want := "meet #7 · bus steward"
	if got != want {
		t.Fatalf("String() = %q, want %q", got, want)
	}
}
