// Copyright (c) 2025 qiangli
// See LICENSE for licensing information

package audit

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
)

func newLog(t *testing.T) (*Writer, string) {
	t.Helper()
	p := filepath.Join(t.TempDir(), "audit.jsonl")
	w, err := Open(p)
	if err != nil {
		t.Fatal(err)
	}
	return w, p
}

func rec(argv ...string) Record {
	return Record{Time: "2026-07-12T00:00:00Z", Argv: argv, Actor: Actor{UID: 501}}
}

// A written chain verifies, and the fields the caller did not set are filled.
func TestAppendAndVerify(t *testing.T) {
	w, p := newLog(t)
	for _, c := range [][]string{{"ls"}, {"rm", "-rf", "x"}, {"git", "push"}} {
		if _, err := w.Append(rec(c...)); err != nil {
			t.Fatalf("append %v: %v", c, err)
		}
	}
	f, _ := os.Open(p)
	defer f.Close()
	res := Verify(f)
	if !res.OK || res.Records != 3 {
		t.Fatalf("verify = %+v, want OK with 3 records", res)
	}
}

// The first record chains to the public genesis root, and seq starts at 1.
func TestGenesisRoot(t *testing.T) {
	w, _ := newLog(t)
	r, err := w.Append(rec("true"))
	if err != nil {
		t.Fatal(err)
	}
	if r.PrevHash != genesis {
		t.Fatalf("first prev_hash = %q, want genesis", r.PrevHash)
	}
	if r.Seq != 1 {
		t.Fatalf("first seq = %d, want 1", r.Seq)
	}
}

// The Decision slot carries a guard's deny verdict: it round-trips through
// the chain, is covered by the hash, and Effects stay in canonical sorted
// order. This is the seam a policy-applied guard decorator writes into
// (allow-only in practice until enforcement ships — Record.Decision).
func TestDenyDecisionIsChained(t *testing.T) {
	w, p := newLog(t)
	r := rec("curl", "https://x")
	r.Decision = "deny"
	r.Effects = []string{"net", "read"}
	stored, err := w.Append(r)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Decision != "deny" {
		t.Fatalf("decision = %q, want deny", stored.Decision)
	}
	raw, _ := os.ReadFile(p)
	if !strings.Contains(string(raw), `"decision":"deny"`) {
		t.Fatalf("deny not serialized: %s", raw)
	}
	res := Verify(strings.NewReader(string(raw)))
	if !res.OK {
		t.Fatalf("deny record broke the chain: %+v", res)
	}
	// Flipping the verdict after the fact must break verification — the
	// decision is tamper-evident like every other field.
	flipped := strings.Replace(string(raw), `"decision":"deny"`, `"decision":"allow"`, 1)
	if Verify(strings.NewReader(flipped)).OK {
		t.Fatal("an altered decision verified as intact")
	}
}

// Editing a single byte of any record must be caught, and caught AT that record
// — this is the whole point of the chain.
func TestTamperIsCaught(t *testing.T) {
	w, p := newLog(t)
	for i := range 5 {
		if _, err := w.Append(rec("cmd", string(rune('a'+i)))); err != nil {
			t.Fatal(err)
		}
	}
	raw, _ := os.ReadFile(p)
	lines := strings.Split(strings.TrimRight(string(raw), "\n"), "\n")
	// Alter the argument in record 3 (index 2) without touching its hash.
	lines[2] = strings.Replace(lines[2], `"cmd","c"`, `"cmd","HACKED"`, 1)
	if err := os.WriteFile(p, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	res := Verify(strings.NewReader(strings.Join(lines, "\n")))
	if res.OK {
		t.Fatal("tampered record verified as intact")
	}
	if res.BadSeq != 3 {
		t.Fatalf("break reported at seq %d, want 3", res.BadSeq)
	}
}

// Deleting a record breaks the chain at the following record (its prev_hash no
// longer matches).
func TestDeletionIsCaught(t *testing.T) {
	w, p := newLog(t)
	for i := range 4 {
		if _, err := w.Append(rec("cmd", string(rune('a'+i)))); err != nil {
			t.Fatal(err)
		}
	}
	raw, _ := os.ReadFile(p)
	lines := strings.Split(strings.TrimRight(string(raw), "\n"), "\n")
	// Drop record 2 (index 1).
	kept := append([]string{lines[0]}, lines[2:]...)
	res := Verify(strings.NewReader(strings.Join(kept, "\n")))
	if res.OK {
		t.Fatal("a chain with a deleted record verified as intact")
	}
}

// Concurrent writers to one log must not fork the chain or tear a line: the
// flock serializes them, and the result verifies with every record present.
func TestConcurrentAppendsStayChained(t *testing.T) {
	w, p := newLog(t)
	const writers, each = 6, 20
	var wg sync.WaitGroup
	for g := range writers {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for range each {
				if _, err := w.Append(rec("w", string(rune('0'+g)))); err != nil {
					t.Errorf("append: %v", err)
					return
				}
			}
		}(g)
	}
	wg.Wait()

	f, _ := os.Open(p)
	defer f.Close()
	res := Verify(f)
	if !res.OK {
		t.Fatalf("concurrent chain broke: %+v", res)
	}
	if res.Records != writers*each {
		t.Fatalf("records = %d, want %d (a torn/forked write lost some)", res.Records, writers*each)
	}
}

// The log file is owner-only (NIST AU-9).
func TestLogFileIsOwnerOnly(t *testing.T) {
	if runtime.GOOS == "windows" {
		// Windows has no unix permission bits: os.Chmod only toggles the
		// read-only attribute and os.Stat reports 0666/0444, so an owner-only
		// mode cannot be represented. AU-9 confinement on Windows is an ACL
		// concern, out of scope for this perm-bit assertion.
		t.Skip("unix permission bits do not apply on Windows (ACL-based file security)")
	}
	_, p := newLog(t)
	fi, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	if perm := fi.Mode().Perm(); perm&0o077 != 0 {
		t.Fatalf("audit log mode %#o is group/other-accessible", perm)
	}
}

func TestRedactMasksSecretsNotPaths(t *testing.T) {
	cases := []struct {
		in     []string
		masked int
		keep   string // a substring that must survive
	}{
		{[]string{"git", "clone", "https://github.com/a/b"}, 0, "github.com"},
		{[]string{"env", "AWS_SECRET_ACCESS_KEY=wJalrXUtnFEMI0987654321abcd"}, 1, "AWS_SECRET_ACCESS_KEY="},
		{[]string{"curl", "-H", "token", "--token", "ghp_ABCDEFGH1234567890abcd"}, 1, "curl"},
		{[]string{"ls", "-la", "/very/long/path/that/is/not/a/secret/at/all"}, 0, "/very/long/path"},
	}
	for _, c := range cases {
		out, n := Redact(c.in)
		if n != c.masked {
			t.Errorf("Redact(%v) masked %d, want %d -> %v", c.in, n, c.masked, out)
		}
		if !strings.Contains(strings.Join(out, " "), c.keep) {
			t.Errorf("Redact(%v) dropped %q: %v", c.in, c.keep, out)
		}
		if c.masked > 0 && !strings.Contains(strings.Join(out, " "), mask) {
			t.Errorf("Redact(%v) claimed a mask but none present: %v", c.in, out)
		}
	}
}
