// Copyright (c) 2025 qiangli
// See LICENSE for licensing information

package todo

import (
	"bytes"
	"strings"
	"testing"

	"github.com/qiangli/yoke/pkg/issue"
)

func TestPriorityRankOrder(t *testing.T) {
	// Lower rank = more urgent: p0 < p1 < p2 < p3 < unset/unknown.
	order := []string{"p0", "p1", "p2", "p3", "", "nonsense"}
	prev := -1
	for _, p := range order {
		r := PriorityRank(p)
		if r < prev {
			t.Fatalf("PriorityRank(%q)=%d is out of order (prev=%d)", p, r, prev)
		}
		prev = r
	}
	if PriorityRank("p0") != 0 || PriorityRank("P0") != 0 {
		t.Errorf("p0 should rank 0 and be case-insensitive")
	}
	if PriorityRank("") != PriorityRank("bogus") {
		t.Errorf("unset and unrecognized should share the last rank")
	}
}

// TestListDefaultOrderByPriority: the DEFAULT list order is priority first, then
// by number — verified through the real list command, not a re-implementation.
func TestListDefaultOrderByPriority(t *testing.T) {
	t.Setenv("BASHY_TODO_DIR", t.TempDir())
	st, _ := UserStore("steward")

	// Add out of priority order; each gets an ascending Seq (1..4).
	if _, err := Add(st, "b2-first", "", "p2", nil, "", ""); err != nil { // Seq 1
		t.Fatal(err)
	}
	if _, err := Add(st, "p0-second", "", "p0", nil, "", ""); err != nil { // Seq 2
		t.Fatal(err)
	}
	if _, err := Add(st, "p1-third", "", "p1", nil, "", ""); err != nil { // Seq 3
		t.Fatal(err)
	}
	if _, err := Add(st, "p0-fourth", "", "p0", nil, "", ""); err != nil { // Seq 4
		t.Fatal(err)
	}

	var buf bytes.Buffer
	lc := newListCmd(func() (*issue.Store, string, error) { return st, "test", nil })
	lc.SetOut(&buf)
	lc.SetArgs(nil) // default order — no flag needed
	if err := lc.Execute(); err != nil {
		t.Fatal(err)
	}
	out := buf.String()

	// Expected title order: the two p0s (by Seq: p0-second then p0-fourth),
	// then p1-third, then b2-first.
	want := []string{"p0-second", "p0-fourth", "p1-third", "b2-first"}
	last := -1
	for _, title := range want {
		i := strings.Index(out, title)
		if i < 0 {
			t.Fatalf("title %q missing from list output:\n%s", title, out)
		}
		if i < last {
			t.Fatalf("--by-priority order wrong: %q appears before an earlier item\n%s", title, out)
		}
		last = i
	}
}
