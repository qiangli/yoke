package redact

import (
	"strings"
	"testing"
)

// The headline property: a reader can still tell that two mentions are the same
// host, while learning nothing about which host. A flat placeholder would
// destroy that and turn a usable procedure into mush.
func TestScrub_PreservesCoReferenceDestroysIdentity(t *testing.T) {
	s := New(WithHost("workshop"), WithUser("svc-build"))

	got := s.String("ssh svc-build@workshop, then rsync from workshop:/tmp to workshop:/var")

	if strings.Contains(got, "workshop") || strings.Contains(got, "svc-build") {
		t.Fatalf("identity survived: %q", got)
	}
	hostTag := Tag(KindHost, "workshop")
	if n := strings.Count(got, hostTag); n != 3 {
		t.Errorf("host tag appears %d times, want 3 — co-reference was not preserved:\n%s", n, got)
	}
	if !strings.Contains(got, Tag(KindUser, "svc-build")) {
		t.Errorf("user not tagged: %s", got)
	}
	// Two different values must never collapse onto one tag.
	if Tag(KindHost, "workshop") == Tag(KindUser, "svc-build") {
		t.Error("distinct values produced one tag")
	}
}

// Tags must be stable across processes and machines — a fold written on one
// host and read on another has to line up.
func TestTag_StableAndNonReversible(t *testing.T) {
	a := Tag(KindHost, "workshop")
	if a != Tag(KindHost, "workshop") {
		t.Error("tag is not deterministic")
	}
	if a == Tag(KindHost, "workshop2") {
		t.Error("distinct values collided")
	}
	// Same text under different kinds must differ: a user named like a host
	// is not the same entity.
	if Tag(KindHost, "x-value") == Tag(KindUser, "x-value") {
		t.Error("kind is not part of the tag")
	}
	if strings.Contains(a, "workshop") {
		t.Errorf("tag %q leaks its input", a)
	}
}

// Longest-first ordering is correctness, not tidiness: replacing the short form
// first would leave a dangling ".local" and split one host into two entities.
func TestScrub_LongestLiteralWins(t *testing.T) {
	s := New(WithHost("workshop.local"))
	got := s.String("connect to workshop.local now")
	if strings.Contains(got, ".local") {
		t.Errorf("FQDN partially replaced, leaving a fragment: %q", got)
	}
	if strings.Contains(got, "workshop") {
		t.Errorf("host survived: %q", got)
	}
}

func TestScrub_Patterns(t *testing.T) {
	s := New()
	tests := []struct {
		name string
		in   string
		gone []string
	}{
		{"ipv4", "dial 192.168.1.42 now", []string{"192.168.1.42"}},
		{"ipv4 public", "dial 203.0.113.7", []string{"203.0.113.7"}},
		{"ipv6", "dial 2001:db8:85a3::8a2e:370:7334", []string{"2001:db8:85a3"}},
		{"mac", "iface aa:bb:cc:dd:ee:ff up", []string{"aa:bb:cc:dd:ee:ff"}},
		{"email", "mail alice@example.com ok", []string{"alice@example.com"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := s.String(tc.in)
			for _, g := range tc.gone {
				if strings.Contains(got, g) {
					t.Errorf("%q survived in %q", g, got)
				}
			}
		})
	}
}

// A private LAN address describes a specific network's topology, which is
// exactly the particular kind of fact that must not travel. Loopback describes
// every machine equally and stays legible.
func TestScrub_LoopbackKeptPrivateRangesNot(t *testing.T) {
	s := New()

	kept := s.String("serve on 127.0.0.1:8080 and ::1")
	if !strings.Contains(kept, "127.0.0.1") {
		t.Errorf("loopback was redacted: %q", kept)
	}

	for _, private := range []string{"10.0.0.5", "192.168.1.1", "172.16.9.9"} {
		got := s.String("dial " + private)
		if strings.Contains(got, private) {
			t.Errorf("%s survived; private ranges describe a real topology: %q", private, got)
		}
	}

	strict := New(KeepLoopback(false))
	if got := strict.String("serve on 127.0.0.1"); strings.Contains(got, "127.0.0.1") {
		t.Errorf("KeepLoopback(false) did not redact loopback: %q", got)
	}
}

// Tagging things that merely look like addresses would corrupt text without
// protecting anything. Real version strings have three parts, so the four-quad
// pattern cannot reach them, and a single colon is never an address.
func TestScrub_LeavesNonAddressesAlone(t *testing.T) {
	s := New()
	for _, in := range []string{
		"go 1.26.2 is the floor",        // three parts — not a dotted quad
		"see section 4.2 and 4.3",       // prose
		"at 10:30 we deploy",            // one colon — a time, not a MAC or v6
		"run the build and check tests", // nothing at all
	} {
		if got := s.String(in); got != in {
			t.Errorf("text was altered:\n in:  %q\n got: %q", in, got)
		}
	}
}

// A syntactically valid public address IS redacted even when the surrounding
// prose suggests it is something else.
//
// This is a deliberate asymmetry, not an oversight. The two errors are not
// equal: mangling a rare four-part version string costs legibility, while
// leaking a real address is the privacy failure the package exists to prevent.
// When the text alone cannot settle it, the scrubber redacts.
func TestScrub_AmbiguousQuadIsRedacted(t *testing.T) {
	s := New()
	if got := s.String("version 1.2.3.4 released"); !strings.Contains(got, "‹ip:") {
		t.Errorf("an ambiguous dotted quad was left intact: %q", got)
	}
}

// Home paths carry the username and must go, on both separator conventions.
func TestScrub_HomePaths(t *testing.T) {
	s := New(WithHomeDir("/Users/alice"), WithUser("alice"))
	got := s.String("cache at /Users/alice/.config/bashy")
	if strings.Contains(got, "alice") {
		t.Errorf("home path leaked the user: %q", got)
	}
	if !strings.Contains(got, "/.config/bashy") {
		t.Errorf("scrub ate the non-identifying remainder: %q", got)
	}

	win := New(WithHomeDir(`C:\Users\alice`))
	if got := win.String(`cache at C:\Users\alice\AppData`); strings.Contains(got, "alice") {
		t.Errorf("windows home path leaked: %q", got)
	}
}

// A two-character "username" matches half the English language; scrubbing it
// would shred the text while protecting nothing.
func TestScrub_IgnoresTooShortLiterals(t *testing.T) {
	s := New(WithUser("qi"), WithHost("a"))
	in := "quick inspection of a qi field"
	if got := s.String(in); got != in {
		t.Errorf("a too-short literal was applied:\n in:  %q\n got: %q", in, got)
	}
}

// Clean is what an export path asserts before letting content leave the host.
func TestClean(t *testing.T) {
	s := New(WithHost("workshop"))
	if !s.Clean("run the build and check the tests pass") {
		t.Error("Clean rejected text with no identity in it")
	}
	if s.Clean("ssh into workshop") {
		t.Error("Clean accepted text naming a host")
	}
	if s.Clean("dial 192.168.1.9") {
		t.Error("Clean accepted text carrying an address")
	}
}

func TestScrub_FindingsReportWhatHappened(t *testing.T) {
	s := New(WithHost("workshop"))
	_, found := s.Scrub("ssh workshop; ping 10.0.0.4; mail bob@example.com")

	kinds := map[Kind]int{}
	for _, f := range found {
		kinds[f.Kind]++
	}
	for _, want := range []Kind{KindHost, KindIP, KindEmail} {
		if kinds[want] == 0 {
			t.Errorf("no %s finding reported; got %+v", want, found)
		}
	}

	// Repeated values are reported once, not once per occurrence.
	_, repeat := s.Scrub("workshop and workshop and workshop")
	if len(repeat) != 1 {
		t.Errorf("got %d findings for one repeated value, want 1", len(repeat))
	}
}

// The scrubber must be a pure function of its input: two calls agree, and
// scrubbing already-scrubbed text changes nothing further.
func TestScrub_IdempotentAndDeterministic(t *testing.T) {
	s := New(WithHost("workshop"), WithUser("svc-build"))
	in := "ssh svc-build@workshop then dial 10.0.0.4"

	once := s.String(in)
	if again := s.String(in); once != again {
		t.Fatalf("not deterministic:\n %q\n %q", once, again)
	}
	if twice := s.String(once); twice != once {
		t.Errorf("not idempotent — a second pass altered the output:\n %q\n %q", once, twice)
	}
}

func TestScrub_EmptyAndNil(t *testing.T) {
	s := New(WithHost("workshop"))
	if got := s.String(""); got != "" {
		t.Errorf("empty input produced %q", got)
	}
	if got := s.Strings(nil); got != nil {
		t.Errorf("nil slice produced %v", got)
	}
	if got := s.Strings([]string{"ssh workshop", "ok"}); strings.Contains(got[0], "workshop") || got[1] != "ok" {
		t.Errorf("Strings = %v", got)
	}
}

// FromHost must never be a no-op: even if every host probe fails, the pattern
// passes still run, and those catch what a caller forgot to declare.
func TestFromHost_PatternPassesAlwaysRun(t *testing.T) {
	s := FromHost()
	if got := s.String("dial 198.51.100.23"); strings.Contains(got, "198.51.100.23") {
		t.Errorf("FromHost did not redact an address: %q", got)
	}
}
