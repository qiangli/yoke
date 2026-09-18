package principal

import (
	"path/filepath"
	"strings"
	"testing"
)

// withRoles registers a role table for one test, mirroring how pkg/bus
// bridges its HostRoles registry in.
func withRoles(t *testing.T, roles ...HostRole) {
	t.Helper()
	prev := roleSources
	RegisterRoleSource(func() []HostRole { return roles })
	t.Cleanup(func() { roleSources = prev })
}

// THE CONTRADICTION THIS CLOSES. Measured on a live host 2026-08-25:
// `bashy ping steward` resolved the role and posted to its seat topic while
// `bashy whois steward` said "names nothing on this host" — two commands
// resolving the same name giving opposite answers in the same second. The
// resolver must answer for every seat the addresser can post to.
func TestRoleResolvesLikeTheAddresser(t *testing.T) {
	withRoles(t, HostRole{Label: "steward", Topic: "steward.dragon-u501"})
	r, _ := testResolver(t, testEnv(t))

	for _, q := range []string{"steward", "Steward", "role:steward", "steward.dragon-u501"} {
		ans := r.Resolve(q)
		if !ans.Resolved {
			t.Fatalf("Resolve(%q) did not resolve — the resolver knows fewer names than the addresser", q)
		}
		if ans.Ambiguous() {
			t.Fatalf("Resolve(%q) ambiguous: %v", q, ans.Kinds())
		}
		res := ans.Matches[0]
		if res.Kind != KindRole || res.Name != "steward" {
			t.Fatalf("Resolve(%q) = kind %q name %q, want role/steward", q, res.Kind, res.Name)
		}
		// The seat's stable address must be in the answer — it is the fact a
		// sender needs, and the reason role mail survives a handover.
		var topic string
		for _, f := range res.Facts {
			if f[0] == "topic" {
				topic = f[1]
			}
		}
		if topic != "steward.dragon-u501" {
			t.Fatalf("Resolve(%q) topic fact = %q, want the seat topic", q, topic)
		}
		best, ok := res.Best()
		if !ok || best.Method != "mb" {
			t.Fatalf("Resolve(%q) best contact = %+v, want a live mb contact", q, best)
		}
	}
}

// A name that is both a role and something else is ambiguity to surface with
// exit 3, exactly as with two catalog kinds — never a silent pick.
func TestRoleAmbiguityWithAPersonSurfaces(t *testing.T) {
	withRoles(t, HostRole{Label: "localguy", Topic: "localguy.host-1"})
	r, _ := testResolver(t, testEnv(t)) // testEnv's LocalUser is "localguy"

	ans := r.Resolve("localguy")
	if !ans.Resolved || !ans.Ambiguous() {
		t.Fatalf("a role/person collision must surface as ambiguity, got %v", ans.Kinds())
	}
	// A typed query still breaks the tie.
	if ans := r.Resolve("role:localguy"); !ans.Resolved || ans.Ambiguous() || ans.Matches[0].Kind != KindRole {
		t.Fatalf("role:localguy = %v", ans.Kinds())
	}
}

// THE OPERATOR MUST BE REACHABLE. The person at the keyboard is the fallback
// relay when nothing else reaches an agent, and on most hosts they are in no
// catalog — but the OS vouches for them: this session runs as their account.
func TestLocalOperatorResolvesWithAnAskContact(t *testing.T) {
	r, _ := testResolver(t, testEnv(t))

	ans := r.Resolve("localguy")
	if !ans.Resolved {
		t.Fatal("the OS login must resolve as a person — a resolver that cannot reach the operator has no fallback")
	}
	if ans.Ambiguous() {
		t.Fatalf("expected one match, got %v", ans.Kinds())
	}
	res := ans.Matches[0]
	if res.Kind != KindPerson || res.Source != SourceHost || res.Confidence != Observed {
		t.Fatalf("operator = kind %q source %q confidence %q, want person/host/observed", res.Kind, res.Source, res.Confidence)
	}
	best, ok := res.Best()
	if !ok {
		t.Fatal("the operator resolved with no live contact — the whole point is a reach ladder")
	}
	if best.Method != "ask" || !strings.Contains(best.Address, "bashy ask") {
		t.Fatalf("best contact = %+v, want bashy ask", best)
	}
}

// When the login db exists, `write` to the newest tty joins the ladder above
// ask — a logged-in terminal is a directly observed channel.
func TestLoginDBAddsAWriteContact(t *testing.T) {
	env := testEnv(t)
	env.LoginDB = filepath.Join(t.TempDir(), "sessions")
	writeFile(t, env.LoginDB, strings.Join([]string{
		"localguy ttys003 1756000000",
		"# comment rows and other users are ignored",
		"someoneelse ttys001 1756200000",
		"localguy ttys009 1756100000",
	}, "\n")+"\n")
	r, _ := testResolver(t, env)

	res := r.Resolve("localguy").Matches[0]
	var write *Contact
	for i := range res.Contacts {
		if res.Contacts[i].Method == "write" {
			write = &res.Contacts[i]
		}
	}
	if write == nil {
		t.Fatalf("no write contact with a login db present: %+v", res.Contacts)
	}
	// The newest session wins, and the crowd is reported rather than listed.
	if !strings.Contains(write.Address, "ttys009") {
		t.Fatalf("write contact = %q, want the newest tty ttys009", write.Address)
	}
	if !strings.Contains(write.Why, "2 ttys") {
		t.Fatalf("write contact why = %q, want the session count", write.Why)
	}
	// Without the db, no write contact — a channel must not be invented.
	r2, _ := testResolver(t, testEnv(t))
	for _, c := range r2.Resolve("localguy").Matches[0].Contacts {
		if c.Method == "write" {
			t.Fatal("write contact offered with no login db")
		}
	}
}

// A seat answer must name its CURRENT HOLDER. Knowing that `conductor:99` is
// an address is not the question anybody asks — they ask who is accountable for
// sprint 99 today, and an answer that omits it sends them off to look somewhere
// else for the same fact.
func TestWhoisNamesTheSeatsCurrentHolder(t *testing.T) {
	withRoles(t, HostRole{Label: "conductor:99", Topic: "conductor.99", Holder: "trestle"})
	r, _ := testResolver(t, testEnv(t))

	ans := r.Resolve("conductor:99")
	if !ans.Resolved || ans.Ambiguous() {
		t.Fatalf("a registered seat did not resolve cleanly: %+v", ans)
	}
	res := ans.Matches[0]
	if !strings.Contains(res.Summary, "trestle") {
		t.Errorf("summary = %q, want the current holder named", res.Summary)
	}
	var holder string
	for _, f := range res.Facts {
		if f[0] == "holder" {
			holder = f[1]
		}
	}
	if holder != "trestle" {
		t.Errorf("holder fact = %q, want trestle", holder)
	}
}

// A vacant seat says so rather than reading as held. The address stays valid —
// that is what makes mail survive a handover — but "who is in it" has an honest
// answer and it is not silence.
func TestWhoisReportsAVacantSeatAsVacant(t *testing.T) {
	withRoles(t, HostRole{Label: "steward", Topic: "steward.host-1"})
	r, _ := testResolver(t, testEnv(t))

	ans := r.Resolve("steward")
	if !ans.Resolved {
		t.Fatal("a vacant seat must still resolve as an address")
	}
	if !strings.Contains(strings.ToLower(ans.Matches[0].Summary), "vacant") {
		t.Errorf("summary = %q, want the vacancy stated", ans.Matches[0].Summary)
	}
}

// A HELD seat whose holder nothing can reach must not be reported as
// addressable. The transport rule already exists and `sprint ping` already
// enforces it by refusing; whois claimed the opposite about the same seat in
// the same second, which is the "two name systems disagree" defect in its
// sharpest form — and the one a human reads first was the wrong one.
func TestRoleSummarySaysWhenAHeldSeatIsUnreachable(t *testing.T) {
	t.Setenv("BASHY_ROOM_DIR", t.TempDir()) // no live members at all

	held := roleSummary(HostRole{Label: "conductor:1", Topic: "conductor.1", Holder: "nobody-live"})
	if strings.Contains(held, "addressable seat") {
		t.Errorf("a seat whose holder has no transport must not read as addressable: %q", held)
	}
	// It must say mail QUEUES rather than claiming the seat is unreachable:
	// the bus stores mail regardless of liveness, and an external CLI is live
	// only at its turn boundaries.
	if !strings.Contains(held, "QUEUES") {
		t.Errorf("the summary must say mail queues for the next read: %q", held)
	}
	if strings.Contains(held, "NOT reachable") {
		t.Errorf("must not claim unreachable — the inbox is durable: %q", held)
	}

	// A vacant seat is a different answer and keeps its own wording: mail waits
	// for a holder rather than going nowhere.
	vacant := roleSummary(HostRole{Label: "steward", Topic: "steward.host"})
	if !strings.Contains(vacant, "VACANT") {
		t.Errorf("vacant summary changed: %q", vacant)
	}
}
